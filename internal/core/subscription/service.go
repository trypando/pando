// Package subscription delivers Pando's events to the people who asked for
// them: webhooks and notification channels, retried until they arrive or
// plainly fail (issue #50, R-366 – R-374, design 11).
//
// The service is what the API calls, and through it the CLI, MCP and console
// (R-261). The dispatcher is the loop that routes the outbox and sends. The
// router sends Pando's own notifications to the people they are for, as each
// person's preferences allow.
package subscription

import (
	"context"
	"encoding/json"
	"net"
	"strings"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// Authorizer is the part of authz.Authorizer the service uses.
type Authorizer interface {
	CheckControl(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) error
	CheckInstall(ctx context.Context, p authz.Principal, verb authz.Verb) error
	Allows(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) (bool, error)
	AllowsInstall(ctx context.Context, p authz.Principal, verb authz.Verb) (bool, error)
}

// PolicyLoader reads host policy.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Apps looks an app up.
type Apps interface {
	ByID(ctx context.Context, appID string) (state.App, bool, error)
}

// Users looks an account up.
type Users interface {
	ByID(ctx context.Context, userID string) (state.User, bool, error)
}

// Groups resolves an account's groups, live (R-079).
type Groups interface {
	GroupsForUser(ctx context.Context, userID string) ([]string, error)
}

// Service manages subscriptions.
type Service struct {
	Subscriptions *state.Subscriptions
	Deliveries    *state.Deliveries
	Events        *state.Events
	Keys          *state.SubscriptionSecrets

	// ExternalURL is how a browser reaches Pando, for links. Empty sends none.
	ExternalURL string
	Prefs         *state.NotificationPreferences

	Authz    Authorizer
	Policy   PolicyLoader
	Apps     Apps
	Registry *api.Registry

	// Resolver looks webhook hosts up when one is saved. Nil uses the
	// system resolver.
	Resolver Resolver

	Audit  func(ctx context.Context, e audit.Event)
	Clock  clock.Clock
	Logger *zap.Logger
}

func (s *Service) audit(ctx context.Context, p authz.Principal, action string, sub state.Subscription, detail map[string]any) {
	if s.Audit == nil {
		return
	}
	s.Audit(ctx, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: action, AppID: sub.AppID, TargetKind: "subscription", TargetID: sub.ID, Detail: detail,
	})
}

func (s *Service) resolver() Resolver {
	if s.Resolver != nil {
		return s.Resolver
	}
	return net.DefaultResolver
}

// CreateRequest is a new subscription.
type CreateRequest struct {
	// AppID is the app it is about; empty subscribes install-wide.
	AppID       string   `json:"app_id,omitempty"`
	Events      []string `json:"events"`
	Destination string   `json:"destination"`
	URL         string   `json:"url,omitempty"`
	AdapterID   string   `json:"adapter_id,omitempty"`
	Description string   `json:"description,omitempty"`

	// How a webhook is sent (R-375). Header values are secret.Value: they
	// are read from the request, sealed, and never written back out.
	Method          string                  `json:"method,omitempty"`
	ContentType     string                  `json:"content_type,omitempty"`
	Headers         map[string]secret.Value `json:"headers,omitempty"`
	PayloadTemplate string                  `json:"payload_template,omitempty"`
}

// Created is a subscription as it was made, with its signing key — shown this
// once and never again (R-371).
type Created struct {
	state.Subscription
	SigningKey *secretOnce `json:"signing_key,omitempty"`
}

// secretOnce is a key that is meant to be shown: the one response that
// carries a signing key marshals it in the clear, on purpose, and nothing
// else can, because secret.Value refuses to anywhere else (R-194).
type secretOnce struct{ v secret.Value }

func (k *secretOnce) MarshalJSON() ([]byte, error) { return jsonMarshal(k.v.Reveal()) }

// Reveal returns the key, for the CLI's own tests and nothing else.
func (k *secretOnce) Reveal() string { return k.v.Reveal() }

// ownerOf is who a new subscription belongs to: the person, for a person or a
// delegated token acting for one (R-058), or an account token itself
// (R-060), which is bounded by its own grants like any principal.
func ownerOf(p authz.Principal) (userID, tokenID string, err error) {
	switch {
	case p.UserID != "":
		return p.UserID, "", nil
	case p.Kind == authz.KindToken && p.TokenID != "":
		return "", p.TokenID, nil
	}
	return "", "", errs.New(errs.PermDenied, "A subscription needs an owner. Sign in, or use an API token.")
}

// owns reports whether p is the subscription's owner.
func owns(p authz.Principal, sub state.Subscription) bool {
	if sub.OwnerID != "" {
		return p.UserID == sub.OwnerID
	}
	return p.Kind == authz.KindToken && p.UserID == "" && p.TokenID == sub.OwnerTokenID
}

// mayCreate is the R-368 check: seeing the app for an app subscription, the
// install verb for an install-wide one. A denial is audited by the check.
func (s *Service) mayCreate(ctx context.Context, p authz.Principal, appID string) error {
	if appID == "" {
		return s.Authz.CheckInstall(ctx, p, authz.InstallEventsManage)
	}
	if err := s.Authz.CheckControl(ctx, p, appID, authz.AppView); err != nil {
		return notFound()
	}
	return nil
}

func notFound() error {
	return errs.New(errs.NotFound, "No subscription or app with that ID is visible to you.")
}

// checkEvents refuses a filter that names nothing.
func checkEvents(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if !events.ValidPattern(p) {
			return nil, errs.Newf(errs.ValidUnknownEvent,
				"%q is not an event Pando sends, and matches none. Use an event name such as deploy.failed, a prefix such as deploy.*, or * for everything.", p).
				WithRemedy("GET /api/v1/events lists every event, or see docs/events.md.")
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errs.New(errs.ValidInvalid,
			"A subscription needs at least one event to listen for, such as deploy.failed, deploy.*, or * for everything.")
	}
	return out, nil
}

// checkDestination validates where a subscription sends and returns the
// normalized URL. A notification adapter that reaches people needs a person
// to reach: an account token's subscription may send to a webhook or a
// channel, and not to an inbox or an email address it does not have.
func (s *Service) checkDestination(ctx context.Context, destination, rawURL, adapterID string, tokenOwned bool) (string, error) {
	switch destination {
	case state.DestinationWebhook:
		allow := false
		if s.Policy != nil {
			doc, err := s.Policy.Load(ctx)
			if err != nil {
				return "", err
			}
			allow = doc.AllowPrivateWebhooks
		}
		return CheckURL(ctx, rawURL, allow, s.resolver())
	case state.DestinationNotify:
		if s.Registry == nil {
			return "", errs.New(errs.StateInvalid, "Pando has no notification adapters configured.")
		}
		a, ok := s.Registry.Notify(adapterID)
		if !ok {
			return "", errs.Newf(errs.ValidInvalid,
				"%q is not a notification adapter configured on this installation. Valid answers are the destinations GET /api/v1/events lists.", adapterID)
		}
		if tokenOwned && a.Capabilities().Audience == api.AudiencePeople {
			return "", errs.Newf(errs.ValidInvalid,
				"%s sends to people, and this subscription belongs to an account token, which is not a person and has no inbox or email address. "+
					"Send to a webhook or a channel such as Slack, or make the subscription signed in as a person.", adapterID)
		}
		return "", nil
	default:
		return "", errs.Newf(errs.ValidInvalid,
			"%q is not a destination. Valid answers: webhook (Pando posts each event to a URL) or notify (a notification adapter such as Slack or email delivers it).", destination)
	}
}

// requestOptions validates how a webhook is sent. They are refused on a
// notify subscription, where the adapter decides the request.
type requestOptions struct {
	method, contentType, template string
	headerNames                   []string
	headers                       map[string]secret.Value
}

func checkRequestOptions(destination, method, contentType, template string, headers map[string]secret.Value) (requestOptions, error) {
	if destination != state.DestinationWebhook {
		if method != "" || contentType != "" || template != "" || len(headers) > 0 {
			return requestOptions{}, errs.New(errs.ValidInvalid,
				"Method, content type, headers and a body template are for a webhook. A notification adapter lays out its own message.")
		}
		return requestOptions{}, nil
	}
	m, err := checkMethod(method)
	if err != nil {
		return requestOptions{}, err
	}
	ct, err := checkContentType(contentType)
	if err != nil {
		return requestOptions{}, err
	}
	if err := checkTemplate(template, ct); err != nil {
		return requestOptions{}, err
	}
	plain := make(map[string]string, len(headers))
	for k, v := range headers {
		plain[k] = v.Reveal()
	}
	names, canonical, err := checkHeaders(plain)
	if err != nil {
		return requestOptions{}, err
	}
	sealed := make(map[string]secret.Value, len(canonical))
	for k, v := range canonical {
		sealed[k] = secret.New(v)
	}
	return requestOptions{method: m, contentType: ct, template: template, headerNames: names, headers: sealed}, nil
}

// putHeaders replaces a webhook's header values with these.
func (s *Service) putHeaders(ctx context.Context, subscriptionID string, headers map[string]secret.Value) error {
	if err := s.Keys.DeleteHeaders(ctx, subscriptionID); err != nil {
		return err
	}
	for name, value := range headers {
		if err := s.Keys.Put(ctx, subscriptionID, state.HeaderField(name), value); err != nil {
			return err
		}
	}
	return nil
}

// Create makes a subscription. A webhook gets a signing key, returned here once.
func (s *Service) Create(ctx context.Context, p authz.Principal, req CreateRequest) (Created, error) {
	ownerUser, ownerToken, err := ownerOf(p)
	if err != nil {
		return Created{}, err
	}
	if err := s.mayCreate(ctx, p, req.AppID); err != nil {
		return Created{}, err
	}
	names, err := checkEvents(req.Events)
	if err != nil {
		return Created{}, err
	}
	target, err := s.checkDestination(ctx, req.Destination, req.URL, req.AdapterID, ownerToken != "")
	if err != nil {
		return Created{}, err
	}
	opts, err := checkRequestOptions(req.Destination, req.Method, req.ContentType, req.PayloadTemplate, req.Headers)
	if err != nil {
		return Created{}, err
	}

	sub := state.Subscription{
		ID: id.New(id.Subscription), OwnerID: ownerUser, OwnerTokenID: ownerToken, AppID: req.AppID, Events: names,
		Destination: req.Destination, Description: strings.TrimSpace(req.Description), CreatedBy: p.ID,
		Method: opts.method, ContentType: opts.contentType, PayloadTemplate: opts.template, HeaderNames: opts.headerNames,
	}
	if req.Destination == state.DestinationWebhook {
		sub.URL = target
	} else {
		sub.AdapterID = req.AdapterID
	}

	// The audit names headers and never their values.
	s.audit(ctx, p, "subscription.create", sub, map[string]any{
		"events": names, "destination": sub.Destination, "url": sub.URL, "adapter_id": sub.AdapterID,
		"method": sub.Method, "headers": sub.HeaderNames, "templated": sub.PayloadTemplate != "",
	})
	created, err := s.Subscriptions.Create(ctx, sub)
	if err != nil {
		return Created{}, err
	}
	out := Created{Subscription: created}
	if created.Destination == state.DestinationWebhook {
		key, err := NewKey()
		if err == nil {
			err = s.Keys.Put(ctx, created.ID, state.SigningKeyField, key)
		}
		if err == nil {
			err = s.putHeaders(ctx, created.ID, opts.headers)
		}
		if err != nil {
			// A webhook nobody can verify, or missing the headers its
			// receiver needs, is not one to keep.
			_ = s.Subscriptions.Delete(ctx, created.ID)
			return Created{}, err
		}
		out.SigningKey = &secretOnce{key}
	}
	return out, nil
}

// visible returns a subscription the caller may manage: their own, or anybody's
// with install.events.manage. Anything else is not found, not forbidden — a
// subscription's existence is not something to confirm to whoever guesses its ID.
func (s *Service) visible(ctx context.Context, p authz.Principal, subscriptionID string) (state.Subscription, error) {
	sub, ok, err := s.Subscriptions.Get(ctx, subscriptionID)
	if err != nil {
		return state.Subscription{}, err
	}
	if !ok {
		return state.Subscription{}, notFound()
	}
	if owns(p, sub) || p.Kind == authz.KindSystem {
		return sub, nil
	}
	if ok, err := s.Authz.AllowsInstall(ctx, p, authz.InstallEventsManage); err == nil && ok {
		return sub, nil
	}
	return state.Subscription{}, notFound()
}

// Get returns one subscription.
func (s *Service) Get(ctx context.Context, p authz.Principal, subscriptionID string) (state.Subscription, error) {
	return s.visible(ctx, p, subscriptionID)
}

// ListRequest narrows a list.
type ListRequest struct {
	AppID string
	// Everyone lists every person's subscriptions, which needs
	// install.events.manage. Otherwise the list is the caller's own.
	Everyone bool
}

// List returns the caller's subscriptions, or everybody's.
func (s *Service) List(ctx context.Context, p authz.Principal, req ListRequest) ([]state.Subscription, error) {
	f := state.SubscriptionFilter{AppID: req.AppID}
	if req.Everyone {
		if err := s.Authz.CheckInstall(ctx, p, authz.InstallEventsManage); err != nil {
			return nil, err
		}
		return s.Subscriptions.List(ctx, f)
	}
	user, token, err := ownerOf(p)
	if err != nil {
		return []state.Subscription{}, nil
	}
	f.OwnerID = user
	if token != "" {
		f.OwnerID = token
	}
	return s.Subscriptions.List(ctx, f)
}

// UpdateRequest changes a subscription. Nil fields are left alone. The
// destination's kind and the app are fixed: a different one is a different
// subscription. Headers, when given, replace every header the webhook had.
type UpdateRequest struct {
	Events          *[]string                `json:"events,omitempty"`
	URL             *string                  `json:"url,omitempty"`
	AdapterID       *string                  `json:"adapter_id,omitempty"`
	Description     *string                  `json:"description,omitempty"`
	Enabled         *bool                    `json:"enabled,omitempty"`
	Method          *string                  `json:"method,omitempty"`
	ContentType     *string                  `json:"content_type,omitempty"`
	Headers         *map[string]secret.Value `json:"headers,omitempty"`
	PayloadTemplate *string                  `json:"payload_template,omitempty"`
}

func or(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// Update changes a subscription.
func (s *Service) Update(ctx context.Context, p authz.Principal, subscriptionID string, req UpdateRequest) (state.Subscription, error) {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return state.Subscription{}, err
	}
	patch := state.SubscriptionPatch{Description: req.Description, Enabled: req.Enabled}
	detail := map[string]any{}
	if req.Events != nil {
		names, err := checkEvents(*req.Events)
		if err != nil {
			return state.Subscription{}, err
		}
		patch.Events = &names
		detail["events"] = names
	}
	if req.URL != nil {
		if sub.Destination != state.DestinationWebhook {
			return state.Subscription{}, errs.New(errs.ValidInvalid,
				"This subscription sends through a notification adapter, so it has no URL to change. Change adapter_id instead, or make a webhook subscription.")
		}
		target, err := s.checkDestination(ctx, sub.Destination, *req.URL, "", sub.OwnerTokenID != "")
		if err != nil {
			return state.Subscription{}, err
		}
		patch.URL = &target
		detail["url"] = target
	}
	if req.AdapterID != nil {
		if sub.Destination != state.DestinationNotify {
			return state.Subscription{}, errs.New(errs.ValidInvalid,
				"This subscription is a webhook, so it has no notification adapter to change. Change url instead.")
		}
		if _, err := s.checkDestination(ctx, sub.Destination, "", *req.AdapterID, sub.OwnerTokenID != ""); err != nil {
			return state.Subscription{}, err
		}
		patch.AdapterID = req.AdapterID
		detail["adapter_id"] = *req.AdapterID
	}

	var headers map[string]secret.Value
	if req.Method != nil || req.ContentType != nil || req.PayloadTemplate != nil || req.Headers != nil {
		if req.Headers != nil {
			headers = *req.Headers
		}
		opts, err := checkRequestOptions(sub.Destination, or(req.Method, sub.Method), or(req.ContentType, sub.ContentType),
			or(req.PayloadTemplate, sub.PayloadTemplate), headers)
		if err != nil {
			return state.Subscription{}, err
		}
		patch.Method, patch.ContentType, patch.PayloadTemplate = &opts.method, &opts.contentType, &opts.template
		detail["method"], detail["templated"] = opts.method, opts.template != ""
		if req.Headers != nil {
			patch.HeaderNames = &opts.headerNames
			headers = opts.headers
			detail["headers"] = opts.headerNames
		}
	}
	if req.Enabled != nil {
		detail["enabled"] = *req.Enabled
	}
	s.audit(ctx, p, "subscription.update", sub, detail)
	if req.Headers != nil {
		if err := s.putHeaders(ctx, sub.ID, headers); err != nil {
			return state.Subscription{}, err
		}
	}
	return s.Subscriptions.Update(ctx, sub.ID, patch)
}

// Delete removes a subscription, its key and its delivery log.
func (s *Service) Delete(ctx context.Context, p authz.Principal, subscriptionID string) error {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return err
	}
	s.audit(ctx, p, "subscription.delete", sub, nil)
	return s.Subscriptions.Delete(ctx, sub.ID)
}

// RotateKey replaces a webhook's signing key and returns the new one, once.
// Deliveries signed with the old key and not yet sent are signed with the new
// one when they go, because a delivery is signed as it is sent.
func (s *Service) RotateKey(ctx context.Context, p authz.Principal, subscriptionID string) (Created, error) {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return Created{}, err
	}
	if sub.Destination != state.DestinationWebhook {
		return Created{}, errs.New(errs.ValidInvalid,
			"Only a webhook has a signing key. This subscription sends through a notification adapter.")
	}
	key, err := NewKey()
	if err != nil {
		return Created{}, err
	}
	s.audit(ctx, p, "subscription.key.rotate", sub, nil)
	if err := s.Keys.Put(ctx, sub.ID, state.SigningKeyField, key); err != nil {
		return Created{}, err
	}
	return Created{Subscription: sub, SigningKey: &secretOnce{key}}, nil
}

// Test sends a subscription.test event to one subscription, whatever its
// filter says, so a person can check an endpoint before relying on it.
func (s *Service) Test(ctx context.Context, p authz.Principal, subscriptionID string) (state.Delivery, error) {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return state.Delivery{}, err
	}
	if !sub.Enabled {
		return state.Delivery{}, errs.New(errs.StateInvalid,
			"This subscription is turned off, so a test would wait until it is on. Turn it on, then send a test.")
	}
	s.audit(ctx, p, "subscription.test", sub, nil)
	return s.Events.EmitRouted(ctx, state.Event{
		Name: events.SubscriptionTest, AppID: sub.AppID,
		ActorKind: string(p.Kind), ActorID: p.ID, OnBehalfOf: p.UserID,
		Data: map[string]any{"subscription_id": sub.ID},
	}, sub.ID)
}

// ListDeliveries lists a subscription's recent deliveries.
func (s *Service) ListDeliveries(ctx context.Context, p authz.Principal, subscriptionID, before string, limit int) ([]state.Delivery, error) {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.Deliveries.List(ctx, sub.ID, before, limit)
}

// DeliveryDetail is one delivery, every attempt at it, and what was sent.
type DeliveryDetail struct {
	state.Delivery
	// Payload is the body as it is sent: JSON when it is JSON, otherwise
	// the text of a templated body.
	Payload any `json:"payload"`
}

// Delivery returns one delivery in full.
func (s *Service) Delivery(ctx context.Context, p authz.Principal, subscriptionID, deliveryID string) (DeliveryDetail, error) {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return DeliveryDetail{}, err
	}
	d, ok, err := s.Deliveries.Get(ctx, deliveryID)
	if err != nil {
		return DeliveryDetail{}, err
	}
	if !ok || d.SubscriptionID != sub.ID {
		return DeliveryDetail{}, errs.New(errs.NotFound, "This subscription has no delivery with that ID.")
	}
	e, ok, err := s.Events.Get(ctx, d.EventID)
	if err != nil {
		return DeliveryDetail{}, err
	}
	out := DeliveryDetail{Delivery: d}
	if !ok {
		return out, nil
	}
	if sub.Destination != state.DestinationWebhook {
		out.Payload = envelopeOf(e, appOf(ctx, s.Apps, e.AppID), LinkFor(s.ExternalURL, e.AppID))
		return out, nil
	}
	body, err := bodyFor(sub, e, appOf(ctx, s.Apps, e.AppID), LinkFor(s.ExternalURL, e.AppID))
	switch {
	case err != nil:
		out.Payload = err.Error()
	case json.Valid(body):
		out.Payload = json.RawMessage(body)
	default:
		out.Payload = string(body)
	}
	return out, nil
}

// Redeliver sends a delivery again, with the whole retry schedule ahead of it.
func (s *Service) Redeliver(ctx context.Context, p authz.Principal, subscriptionID, deliveryID string) (state.Delivery, error) {
	sub, err := s.visible(ctx, p, subscriptionID)
	if err != nil {
		return state.Delivery{}, err
	}
	d, ok, err := s.Deliveries.Get(ctx, deliveryID)
	if err != nil {
		return state.Delivery{}, err
	}
	if !ok || d.SubscriptionID != sub.ID {
		return state.Delivery{}, errs.New(errs.NotFound, "This subscription has no delivery with that ID.")
	}
	s.audit(ctx, p, "subscription.redeliver", sub, map[string]any{"delivery_id": d.ID, "event_id": d.EventID})
	by := p.UserID
	if by == "" {
		by = p.ID
	}
	if err := s.Deliveries.Redeliver(ctx, d.ID, by); err != nil {
		return state.Delivery{}, err
	}
	got, _, err := s.Deliveries.Get(ctx, d.ID)
	return got, err
}

// appOf names the app an event is about, for the envelope. An app that cannot
// be read is named by its ID alone rather than holding the delivery up.
func appOf(ctx context.Context, apps Apps, appID string) *EnvelopeApp {
	if appID == "" {
		return nil
	}
	out := &EnvelopeApp{ID: appID}
	if apps == nil {
		return out
	}
	if app, ok, err := apps.ByID(ctx, appID); err == nil && ok {
		out.Name, out.Slug = app.Name, app.Slug
	}
	return out
}
