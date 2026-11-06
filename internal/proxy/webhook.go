package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
)

// Webhook routes (R-425, design 06 §4.3).
//
// A route is a way to authenticate, not a way around the proxy. A request with
// no Pando credential that matches one is verified here and becomes a webhook
// principal, and goes through everything after step 4 like any other request.
// One that fails is answered 401 and never reaches the app, the sign-in page or
// CheckData.

const (
	// webhookBodyLimit and webhookBodyIdle bound what an hmac_sha256 route
	// reads before it can decide (R-428) [P], R-411's limits.
	webhookBodyLimit = 8 << 20
	webhookBodyIdle  = time.Minute

	// webhookReplayWindow is how long an accepted signature is refused (R-433)
	// [P]. Longer than any sender's retries, and the limit of the protection:
	// these signatures carry no timestamp.
	webhookReplayWindow = 7 * 24 * time.Hour

	// webhookClaimLifetime is how long a claim outlives a replica that died
	// holding it (R-433) [P].
	webhookClaimLifetime = 5 * time.Minute

	// webhookSettleTimeout bounds recording what the app answered, on a
	// context that outlives the caller's.
	webhookSettleTimeout = 5 * time.Second
)

// WebhookStore holds routes' secrets and the signatures seen on them.
type WebhookStore interface {
	Secret(ctx context.Context, appID, route string) (secret.Value, bool, error)
	Claim(ctx context.Context, appID, route string, digest []byte, now, expires time.Time) (state.WebhookClaim, error)
	Accept(ctx context.Context, appID, route string, digest []byte, expires time.Time) error
	Release(ctx context.Context, appID, route string, digest []byte) error
}

// WebhookPolicy is what host policy says about routes. *policy.Evaluator is one.
type WebhookPolicy interface {
	WebhookRoutesDisabled(ctx context.Context) bool
	AuditsAnonymousDenials(ctx context.Context) bool
}

// webhookDelivery is a request verified against a route.
type webhookDelivery struct {
	route spec.WebhookRoute

	// digest is the signature's, on a route that checks for replays.
	digest []byte
}

// webhookRoute finds the route a request with no credential matches.
//
// The escaped path as sent is compared with a path validation keeps free of
// anything that needs escaping, so `..`, an empty segment and an encoded slash
// never match rather than being cleaned up into a path that does (R-425). A
// request asking to upgrade does not match either: a delivery is good for one
// request, and an upgraded connection would outlive it (R-429).
func (p *Proxy) webhookRoute(r *http.Request, s *spec.AppSpec, prefix string) (spec.WebhookRoute, bool) {
	if p.Webhooks == nil || s == nil || len(s.WebhookRoutes) == 0 || r.Header.Get("Upgrade") != "" {
		return spec.WebhookRoute{}, false
	}
	path, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok {
		return spec.WebhookRoute{}, false
	}
	route, ok := s.WebhookRouteFor(r.Method, path)
	if !ok {
		return spec.WebhookRoute{}, false
	}
	if p.WebhookPolicy != nil && p.WebhookPolicy.WebhookRoutesDisabled(r.Context()) {
		return spec.WebhookRoute{}, false // R-431: the app's ordinary access decides.
	}
	return route, true
}

// verifyWebhook is step 4a for a request matching a route. It reports the
// verified delivery, or false once it has answered the request.
//
// The anonymous limit comes first (design 06 §4.3): before a body is read, so a
// flood cannot make Pando hold one per request, and before a secret is checked,
// so guessing one is limited. A verified request is not charged again at 5b.
func (p *Proxy) verifyWebhook(w http.ResponseWriter, r *http.Request, route spec.WebhookRoute, appID string) (*webhookDelivery, bool) {
	if !p.allowAnonymous(w, r, authz.Anonymous(), appID) {
		p.countRefused(r, appID)
		return nil, false
	}

	key, found, err := p.Webhooks.Secret(r.Context(), appID, route.Name)
	if err != nil {
		log.From(r.Context()).Error("could not read a webhook route's secret", zap.Error(err))
		p.countRefused(r, appID)
		p.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return nil, false
	}
	if !found {
		p.refuseWebhook(w, r, route, appID, "no_secret")
		return nil, false
	}

	v := route.Verification
	proof := r.Header.Get(v.Header)
	if proof == "" {
		p.refuseWebhook(w, r, route, appID, "no_proof")
		return nil, false
	}
	if v.Kind == spec.WebhookHeader {
		if !sameSecret(proof, key.Reveal()) {
			p.refuseWebhook(w, r, route, appID, "bad_secret")
			return nil, false
		}
		return &webhookDelivery{route: route}, true
	}

	body, status := readWebhookBody(w, r)
	if status != 0 {
		p.countRefused(r, appID)
		p.fail(w, r, status, "This webhook delivery's body is over 8 MB or stopped arriving.")
		return nil, false
	}
	signature, ok := verifySignature(proof, v.Prefix, key.Reveal(), body)
	if !ok {
		p.refuseWebhook(w, r, route, appID, "bad_signature")
		return nil, false
	}
	delivery := &webhookDelivery{route: route}
	if route.ChecksReplays() {
		// Of the decoded signature, so the same one written in another case
		// is the same delivery.
		digest := sha256.Sum256(signature)
		delivery.digest = digest[:]
	}
	return delivery, true
}

// sameSecret compares digests, so the comparison takes the same time whatever
// the lengths of the two values.
func sameSecret(proof, key string) bool {
	a, b := sha256.Sum256([]byte(proof)), sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// verifySignature checks a header's hex HMAC-SHA256 of the body and returns the
// decoded signature.
func verifySignature(proof, prefix, key string, body []byte) ([]byte, bool) {
	encoded, ok := strings.CutPrefix(proof, prefix)
	if !ok {
		return nil, false
	}
	signature, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return signature, hmac.Equal(signature, mac.Sum(nil))
}

// readWebhookBody reads the whole body under R-428's limits and puts it back
// for the workload. It returns the status to refuse with, or 0.
func readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	if r.Body == nil {
		return nil, 0
	}
	rc := http.NewResponseController(w)
	body, err := io.ReadAll(&idleLimited{
		body: http.MaxBytesReader(w, r.Body, webhookBodyLimit), rc: rc,
	})
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge
		}
		return nil, http.StatusRequestTimeout
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	return body, 0
}

// idleLimited ends a read once no byte has arrived for webhookBodyIdle.
type idleLimited struct {
	body io.Reader
	rc   *http.ResponseController
}

func (b *idleLimited) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(webhookBodyIdle))
	return b.body.Read(p)
}

// refuseWebhook answers a request that matched a route and failed (R-430). The
// sender is a visitor who is not signed in, so the audit follows that rule.
func (p *Proxy) refuseWebhook(w http.ResponseWriter, r *http.Request, route spec.WebhookRoute, appID, reason string) {
	p.countRefused(r, appID)
	if p.WebhookPolicy == nil || p.WebhookPolicy.AuditsAnonymousDenials(r.Context()) {
		p.auditWebhook(r, route, appID, "app.webhook.refused", reason)
	}
	p.fail(w, r, http.StatusUnauthorized,
		"This webhook delivery could not be verified. Check the secret and the header the sender uses.")
}

// countRefused counts a request refused before the data check, which every
// other request reaches, so a refused delivery is still seen.
func (p *Proxy) countRefused(r *http.Request, appID string) {
	p.count(r, appID, authz.KindAnonymous, false)
}

// auditWebhook records what happened to a delivery. audit_events has no
// webhook kind: the sender is anonymous, and the route is its ID.
func (p *Proxy) auditWebhook(r *http.Request, route spec.WebhookRoute, appID, action, reason string) {
	if p.Auditor == nil {
		return
	}
	detail := map[string]any{"route": route.Name, "path": r.URL.Path}
	if reason != "" {
		detail["reason"] = reason
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		detail["remote_addr"] = host
	}
	if err := p.Auditor.Write(r.Context(), audit.Event{
		PrincipalKind: audit.KindAnonymous,
		PrincipalID:   authz.Webhook(appID, route.Name).ID,
		Action:        action,
		AppID:         appID,
		Detail:        detail,
	}); err != nil {
		log.From(r.Context()).Warn("could not record a webhook delivery", zap.Error(err))
	}
}

// claimWebhook is step 10a: it takes the delivery's signature for this request
// and returns the transport that settles it, or false once it has answered.
//
// Only an accepted signature is a replay (R-433). One still in flight may yet
// fail, so the copy is told to come back rather than that all is well.
func (p *Proxy) claimWebhook(w http.ResponseWriter, r *http.Request, d *webhookDelivery, appID string, next http.RoundTripper) (http.RoundTripper, bool) {
	now := p.now()
	claim, err := p.Webhooks.Claim(r.Context(), appID, d.route.Name, d.digest, now, now.Add(webhookClaimLifetime))
	if err != nil {
		log.From(r.Context()).Error("could not claim a webhook delivery", zap.Error(err))
		w.Header().Set("Retry-After", "30")
		p.fail(w, r, http.StatusServiceUnavailable, "This webhook delivery could not be recorded. Send it again.")
		return nil, false
	}
	switch claim {
	case state.WebhookAccepted:
		p.auditWebhook(r, d.route, appID, "app.webhook.replayed", "")
		w.WriteHeader(http.StatusOK)
		return nil, false
	case state.WebhookInFlight:
		w.Header().Set("Retry-After", "30")
		p.fail(w, r, http.StatusConflict, "A copy of this webhook delivery is still being handled. Send it again.")
		return nil, false
	}
	return &settling{next: next, proxy: p, delivery: d, appID: appID}, true
}

// settling records what the app answered a claimed delivery: 2xx keeps the
// signature for the window, and anything else gives it up, so the sender's retry
// is forwarded (R-433).
type settling struct {
	next     http.RoundTripper
	proxy    *Proxy
	delivery *webhookDelivery
	appID    string
}

func (s *settling) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(r)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), webhookSettleTimeout)
	defer cancel()
	d, store := s.delivery, s.proxy.Webhooks
	var settled error
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		settled = store.Accept(ctx, s.appID, d.route.Name, d.digest, s.proxy.now().Add(webhookReplayWindow))
	} else {
		settled = store.Release(ctx, s.appID, d.route.Name, d.digest)
	}
	if settled != nil {
		log.From(ctx).Warn("could not settle a webhook delivery", zap.Error(settled))
	}
	return resp, err
}
