package subscription

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/core/state"
)

// RetrySchedule is how long to wait after each failed attempt before the next
// [P]. The first attempt is immediate; after the last of these fails the
// delivery is marked failed and can be sent again by hand. About a day in all,
// which is also how long an endpoint may fail everything before it is turned
// off — so a delivery outlives a short outage and does not outlive a dead one.
var RetrySchedule = []time.Duration{
	time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour,
}

// Disabling thresholds [P]: an endpoint that has failed every attempt for a
// day, and at least five times, is turned off (R-370). Both, so neither one
// slow failure in a quiet install nor a burst of them in a busy one does it.
const (
	DisableAfter       = 24 * time.Hour
	DisableMinFailures = 5
)

// Retention is how long the outbox keeps an event once it has nothing left
// to deliver [P]. The audit log is the history; this is a queue (R-366).
const Retention = 30 * 24 * time.Hour

// Dispatcher routes the outbox and sends what it routes.
type Dispatcher struct {
	Events        *state.Events
	Subscriptions *state.Subscriptions
	Deliveries    *state.Deliveries
	Keys          *state.SubscriptionSecrets

	// Tokens resolves an account token that owns a subscription (R-060).
	Tokens interface {
		Active(ctx context.Context, tokenID string) (state.Token, bool, error)
	}

	// ExternalURL is how a browser reaches Pando, for the link in each
	// delivery. Empty sends none.
	ExternalURL string

	// Who Pando's own failure notifications go to (announce.go). Each is
	// optional; without one, those people are not told.
	Deployments Deployments
	Holders     VerbHolders
	TokenOwners TokenOwners

	Authz    Authorizer
	Policy   PolicyLoader
	Apps     Apps
	Users    Users
	Groups   Groups
	Registry *api.Registry

	// Notifier tells a subscription's owner it was turned off. Optional.
	Notifier interface {
		Notify(ctx context.Context, n api.Notification) error
	}

	Audit  func(ctx context.Context, e audit.Event)
	Clock  clock.Clock
	Logger *zap.Logger

	// Client sends webhooks. Nil builds the guarded client (R-372); tests
	// supply their own only to reach a local server, with policy allowing it.
	Client *http.Client

	// Interval is how often the outbox is read when it was empty last time.
	Interval time.Duration

	once          sync.Once
	guarded, open *http.Client
	health        map[string]bool
}

func (d *Dispatcher) now() time.Time {
	if d.Clock == nil {
		return time.Now().UTC()
	}
	return d.Clock.Now()
}

func (d *Dispatcher) logger() *zap.Logger {
	if d.Logger == nil {
		return zap.NewNop()
	}
	return d.Logger
}

// client is the client one send uses. Whether private addresses are allowed
// is read from policy for each send, so turning the setting off takes effect
// on the next delivery rather than the next restart.
func (d *Dispatcher) client(ctx context.Context) *http.Client {
	if d.Client != nil {
		return d.Client
	}
	d.once.Do(func() {
		d.guarded, d.open = newClient(false), newClient(true)
	})
	if d.allowPrivate(ctx) {
		return d.open
	}
	return d.guarded
}

func (d *Dispatcher) allowPrivate(ctx context.Context) bool {
	if d.Policy == nil {
		return false
	}
	doc, err := d.Policy.Load(ctx)
	return err == nil && doc.AllowPrivateWebhooks
}

// Run routes and delivers until ctx ends, and prunes the outbox hourly.
func (d *Dispatcher) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	c := d.Clock
	if c == nil {
		c = clock.System{}
	}
	lastPrune := time.Time{}
	for {
		busy := d.Pass(ctx)
		if d.now().Sub(lastPrune) > time.Hour {
			if n, err := d.Events.Prune(ctx, d.now().Add(-Retention)); err != nil {
				d.logger().Warn("could not prune delivered events", zap.Error(err))
			} else if n > 0 {
				d.logger().Info("pruned delivered events", zap.Int64("events", n))
			}
			lastPrune = d.now()
		}
		if busy {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-c.After(interval):
		}
	}
}

// Pass routes what is waiting and sends what is due, once. It reports
// whether there was anything to do, so Run keeps going while there is.
func (d *Dispatcher) Pass(ctx context.Context) bool {
	routed, err := d.route(ctx)
	if err != nil {
		d.logger().Warn("could not route events", zap.Error(err))
	}
	d.announce(ctx, routed)
	sent, err := d.deliver(ctx)
	if err != nil {
		d.logger().Warn("could not send deliveries", zap.Error(err))
	}
	return len(routed) > 0 || sent > 0
}

// route queues a delivery for every enabled subscription an event matches.
// Authorization is not decided here but at send time (R-368), so a grant
// revoked between the two still stops the delivery.
func (d *Dispatcher) route(ctx context.Context) ([]state.Event, error) {
	subs, err := d.Subscriptions.List(ctx, state.SubscriptionFilter{EnabledOnly: true})
	if err != nil {
		return nil, err
	}
	return d.Events.Route(ctx, 100, func(_ context.Context, e state.Event) ([]string, error) {
		return Matching(subs, e), nil
	})
}

// Matching is which subscriptions an event goes to: those whose filter names
// it, on its app or install-wide, made before the event happened. An event
// with no app reaches install-wide subscriptions only. A test event is never
// routed; it was made for one subscription.
func Matching(subs []state.Subscription, e state.Event) []string {
	if e.Name == events.SubscriptionTest {
		return nil
	}
	var out []string
	for _, s := range subs {
		if !s.Enabled || !events.MatchAny(s.Events, e.Name) {
			continue
		}
		if s.AppID != "" && s.AppID != e.AppID {
			continue
		}
		// A subscription hears what happens after it is made. An event
		// still waiting to be routed when it was made is older than it.
		if e.OccurredAt.Before(s.CreatedAt) {
			continue
		}
		out = append(out, s.ID)
	}
	return out
}

// deliver sends up to a batch of due deliveries, a few at a time.
func (d *Dispatcher) deliver(ctx context.Context) (int, error) {
	due, err := d.Deliveries.Claim(ctx, d.now(), 2*time.Minute, 32)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, dl := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(dl state.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			d.send(ctx, dl)
		}(dl)
	}
	wg.Wait()
	return len(due), nil
}

// send makes one attempt at one delivery and records how it went.
func (d *Dispatcher) send(ctx context.Context, dl state.Delivery) {
	log := d.logger().With(zap.String("delivery_id", dl.ID), zap.String("subscription_id", dl.SubscriptionID))
	sub, ok, err := d.Subscriptions.Get(ctx, dl.SubscriptionID)
	if err != nil || !ok {
		return // deleted under us; its deliveries went with it
	}
	e, ok, err := d.Events.Get(ctx, dl.EventID)
	if err != nil || !ok {
		log.Warn("a delivery's event could not be read", zap.Error(err))
		return
	}

	if reason, allowed := d.authorized(ctx, sub); !allowed {
		if err := d.Deliveries.Drop(ctx, dl.ID, reason); err != nil {
			log.Warn("could not record a dropped delivery", zap.Error(err))
		}
		return
	}

	attempt := dl.Attempts + 1
	app := appOf(ctx, d.Apps, e.AppID)
	var result Result
	switch sub.Destination {
	case state.DestinationWebhook:
		result = d.webhook(ctx, sub, dl, e, app)
	case state.DestinationNotify:
		result = d.notify(ctx, sub, e, app)
	}

	now := d.now()
	rec := state.DeliveryAttempt{Attempt: attempt, AttemptedAt: now, Error: result.Message(),
		DurationMS: int(result.Duration / time.Millisecond)}
	if result.StatusCode != 0 {
		code := result.StatusCode
		rec.StatusCode = &code
	}

	if result.OK() {
		if err := d.Deliveries.Record(ctx, dl.ID, rec, state.DeliverySucceeded, nil); err != nil {
			log.Warn("could not record a delivery", zap.Error(err))
		}
		if err := d.Subscriptions.Succeeded(ctx, sub.ID); err != nil {
			log.Warn("could not record a delivery", zap.Error(err))
		}
		return
	}

	status, next := state.DeliveryPending, (*time.Time)(nil)
	if inRound := attempt - dl.RoundBase; inRound > len(RetrySchedule) {
		status = state.DeliveryFailed
	} else {
		at := now.Add(RetrySchedule[inRound-1])
		next = &at
	}
	if err := d.Deliveries.Record(ctx, dl.ID, rec, status, next); err != nil {
		log.Warn("could not record a delivery", zap.Error(err))
	}
	since, count, err := d.Subscriptions.Failed(ctx, sub.ID, now)
	if err != nil {
		log.Warn("could not record a delivery", zap.Error(err))
		return
	}
	if now.Sub(since) >= DisableAfter && count >= DisableMinFailures {
		d.disable(ctx, sub, since, count)
	}
}

// authorized is R-368: a delivery is made as its owner, now. An owner who
// left, was suspended, or lost sight of the app gets nothing, and the
// delivery log says so.
func (d *Dispatcher) authorized(ctx context.Context, sub state.Subscription) (string, bool) {
	p, reason, ok := d.ownerPrincipal(ctx, sub)
	if !ok {
		return reason, false
	}
	if sub.AppID == "" {
		if ok, err := d.Authz.AllowsInstall(ctx, p, authz.InstallEventsManage); err != nil || !ok {
			return "Not sent: the subscription's owner no longer holds install.events.manage, which an install-wide subscription needs.", false
		}
		return "", true
	}
	if ok, err := d.Authz.Allows(ctx, p, sub.AppID, authz.AppView); err != nil || !ok {
		return "Not sent: the subscription's owner can no longer see this app.", false
	}
	return "", true
}

// ownerPrincipal is the subscription's owner as authorization sees them: a
// person and their live groups, or an account token that is neither revoked
// nor expired (R-060, R-368).
func (d *Dispatcher) ownerPrincipal(ctx context.Context, sub state.Subscription) (authz.Principal, string, bool) {
	const unchecked = "Pando could not check the subscription's owner, so it did not send this."
	if sub.OwnerTokenID != "" {
		if d.Tokens == nil {
			return authz.Principal{}, unchecked, false
		}
		tok, ok, err := d.Tokens.Active(ctx, sub.OwnerTokenID)
		if err != nil {
			return authz.Principal{}, unchecked, false
		}
		if !ok {
			return authz.Principal{}, "Not sent: the account token that owns this subscription was revoked or has expired.", false
		}
		return authz.Principal{Kind: authz.KindToken, ID: tok.ID, TokenID: tok.ID}, "", true
	}
	user, ok, err := d.Users.ByID(ctx, sub.OwnerID)
	if err != nil {
		return authz.Principal{}, unchecked, false
	}
	if !ok || user.Status != "active" {
		return authz.Principal{}, "Not sent: the subscription's owner is no longer an active account.", false
	}
	groups, err := d.Groups.GroupsForUser(ctx, user.ID)
	if err != nil {
		return authz.Principal{}, unchecked, false
	}
	return authz.Principal{Kind: authz.KindUser, ID: user.ID, UserID: user.ID, Groups: groups,
		AdapterID: user.AdapterID, Status: user.Status}, "", true
}

// webhook makes one attempt at a webhook delivery: the subscription's
// secrets opened, its body rendered, signed and sent (R-369, R-375).
func (d *Dispatcher) webhook(ctx context.Context, sub state.Subscription, dl state.Delivery, e state.Event, app *EnvelopeApp) Result {
	secrets, err := d.Keys.All(ctx, sub.ID)
	if err != nil {
		return Result{Err: sentence("Pando could not open this webhook's signing key and headers: " + err.Error())}
	}
	key, ok := secrets[state.SigningKeyField]
	if !ok {
		return Result{Err: sentence("This webhook has no signing key. Rotate it to make one.")}
	}
	headers := map[string]string{}
	for _, name := range sub.HeaderNames {
		if v, ok := secrets[state.HeaderField(name)]; ok {
			headers[name] = v.Reveal()
		}
	}
	body, err := bodyFor(sub, e, app, LinkFor(d.ExternalURL, e.AppID))
	if err != nil {
		return Result{Err: err}
	}
	return post(ctx, d.client(ctx), webhookRequest{
		URL: sub.URL, Method: sub.Method, ContentType: sub.ContentType, Headers: headers, Body: body,
		Key: key, DeliveryID: dl.ID, Event: e.Name, EventID: e.ID,
	}, d.now())
}

// notify sends an event through a notification adapter. One that reaches
// people sends to the owner; one that posts to a channel ignores recipients.
func (d *Dispatcher) notify(ctx context.Context, sub state.Subscription, e state.Event, app *EnvelopeApp) Result {
	if d.Registry == nil {
		return Result{Err: sentence("Pando has no notification adapters configured.")}
	}
	adapter, ok := d.Registry.Notify(sub.AdapterID)
	if !ok {
		return Result{Err: sentence("The notification adapter " + sub.AdapterID + " is not configured on this installation any more. Choose another, or delete this subscription.")}
	}
	n := Describe(e, app)
	n.Link = LinkFor(d.ExternalURL, e.AppID)
	if adapter.Capabilities().Audience == api.AudiencePeople {
		r := api.Recipient{UserID: sub.OwnerID}
		if user, ok, err := d.Users.ByID(ctx, sub.OwnerID); err == nil && ok {
			r.Email = user.Email
		}
		n.Recipients = []api.Recipient{r}
	}
	start := time.Now()
	err := adapter.Notify(ctx, n)
	return Result{StatusCode: okIf(err), Err: err, Duration: time.Since(start)}
}

func okIf(err error) int {
	if err != nil {
		return 0
	}
	return http.StatusOK
}

// disable turns a failing subscription off, once, and tells its owner.
func (d *Dispatcher) disable(ctx context.Context, sub state.Subscription, since time.Time, count int) {
	reason := "Every delivery since " + since.UTC().Format(time.RFC3339) + " failed, " +
		itoa(count) + " attempts in a row. Pando turned this subscription off so it stops retrying. " +
		"Fix the endpoint, then turn the subscription on; its failed deliveries can be sent again from its delivery log."
	changed, err := d.Subscriptions.Disable(ctx, sub.ID, reason)
	if err != nil || !changed {
		return
	}
	if d.Audit != nil {
		d.Audit(ctx, audit.Event{
			PrincipalKind: audit.KindSystem, PrincipalID: authz.System().ID,
			Action: "subscription.disable", AppID: sub.AppID, TargetKind: "subscription", TargetID: sub.ID,
			Detail: map[string]any{"subscription_id": sub.ID, "reason": reason, "failures": count},
		})
	}
	d.logger().Warn("turned off a subscription whose endpoint keeps failing",
		zap.String("subscription_id", sub.ID), zap.Int("failures", count))
	if d.Notifier != nil {
		what := sub.URL
		if what == "" {
			what = sub.AdapterID
		}
		_ = d.Notifier.Notify(ctx, api.Notification{
			Kind:       api.NotifySubscriptionDisabled,
			AppID:      sub.AppID,
			Recipients: []api.Recipient{{UserID: sub.OwnerID}},
			Subject:    "Pando turned off your subscription to " + what,
			Body:       reason,
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// WatchAdapters checks every adapter's health every interval and records a
// change as an event: adapter.unhealthy when a check starts failing,
// adapter.recovered when it passes again. An adapter healthy since start says
// nothing.
func (d *Dispatcher) WatchAdapters(ctx context.Context, interval time.Duration) {
	c := d.Clock
	if c == nil {
		c = clock.System{}
	}
	for {
		d.CheckAdapters(ctx)
		select {
		case <-ctx.Done():
			return
		case <-c.After(interval):
		}
	}
}

// CheckAdapters is one round of WatchAdapters.
func (d *Dispatcher) CheckAdapters(ctx context.Context) {
	if d.Registry == nil {
		return
	}
	if d.health == nil {
		d.health = map[string]bool{}
	}
	for ref, err := range d.Registry.HealthCheckAll(ctx) {
		healthy := err == nil
		was, seen := d.health[ref]
		d.health[ref] = healthy
		if seen && was == healthy || !seen && healthy {
			continue
		}
		category := ""
		if a, ok := d.Registry.Get(ref); ok {
			category = string(a.Category())
		}
		e := state.Event{Name: events.AdapterRecovered, Data: map[string]any{"adapter_id": ref, "category": category}}
		if !healthy {
			e.Name = events.AdapterUnhealthy
			e.Data["reason"] = err.Error()
		}
		if _, err := d.Events.Emit(ctx, e); err != nil {
			d.logger().Warn("could not record an adapter's health", zap.String("adapter", ref), zap.Error(err))
		}
	}
}
