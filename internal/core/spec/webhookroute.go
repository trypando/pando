package spec

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// Webhook routes (R-425, issue #377).
//
// A route lets a verified delivery through to one method and path of an app,
// private or not. The proxy decides on it; the spec only declares it, so a
// route is reviewed and versioned like anything else about how the app runs
// (R-020). The secret is not here: it is kept by the secrets adapter under the
// app and the route's name (R-427).

// WebhookVerificationKind is how a route proves its sender (R-426).
type WebhookVerificationKind string

const (
	// WebhookHMACSHA256 compares a header against the hex HMAC-SHA256 of the
	// raw body.
	WebhookHMACSHA256 WebhookVerificationKind = "hmac_sha256"

	// WebhookHeader compares a header against the secret.
	WebhookHeader WebhookVerificationKind = "header"
)

// WebhookVerification says where a delivery's proof is and what it is.
type WebhookVerification struct {
	Kind   WebhookVerificationKind `json:"kind"`
	Header string                  `json:"header"`

	// Prefix comes before the digest in the header, such as sha256=.
	// hmac_sha256 only.
	Prefix string `json:"prefix,omitempty"`
}

// WebhookRoute is one path an app receives deliveries on.
type WebhookRoute struct {
	// Name is unique in the app. A delivery's assertion carries
	// webhook:<name> (R-429).
	Name   string `json:"name"`
	Method string `json:"method"`

	// Path is exact, and is what the app receives: under path routing, the
	// path after the app's prefix (R-167).
	Path string `json:"path"`

	Verification WebhookVerification `json:"verification"`

	// AllowRepeats turns the replay check off, for a sender that repeats
	// identical bodies (R-433). hmac_sha256 only.
	AllowRepeats bool `json:"allow_repeats,omitempty"`
}

// ChecksReplays reports whether a delivery to this route may be refused as a
// replay. A header route cannot: a captured request holds the secret (R-433).
func (r WebhookRoute) ChecksReplays() bool {
	return r.Verification.Kind == WebhookHMACSHA256 && !r.AllowRepeats
}

// WebhookRouteFor returns the route for a method and an exact path.
func (s *AppSpec) WebhookRouteFor(method, path string) (WebhookRoute, bool) {
	for _, r := range s.WebhookRoutes {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return WebhookRoute{}, false
}

var (
	webhookRouteName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

	// A path Go's URL leaves as it is, so the proxy can compare it with the
	// request's escaped path: nothing that needs escaping, and no %, which
	// keeps an encoded slash from ever matching (R-425).
	webhookRoutePath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+/?$`)

	// RFC 9110's token, which is what a header name is.
	webhookHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// webhookReservedHeaders may not carry a proof: Pando's own namespace is
// stripped from every request (R-053), and the rest describe the connection
// or are Pando's credential (R-173).
var webhookReservedHeaders = map[string]bool{
	"cookie": true, "host": true, "connection": true, "upgrade": true,
	"content-length": true, "transfer-encoding": true,
}

func validateWebhookRoutes(s *AppSpec, add func(*errs.Error)) {
	names := map[string]bool{}
	targets := map[string]bool{}
	for _, r := range s.WebhookRoutes {
		if !webhookRouteName.MatchString(r.Name) {
			add(errs.Newf(errs.ValidInvalid, "Webhook route name %q is not valid.", r.Name).
				WithRemedy("Use lowercase letters, digits and hyphens, such as noodle, up to 63 characters."))
		}
		if names[r.Name] {
			add(errs.Newf(errs.ValidInvalid, "This app has two webhook routes named %q.", r.Name).
				WithRemedy("Give each route its own name."))
		}
		names[r.Name] = true

		target := r.Method + " " + r.Path
		if targets[target] {
			add(errs.Newf(errs.ValidInvalid, "This app has two webhook routes for %s.", target).
				WithRemedy("Remove one of them."))
		}
		targets[target] = true

		validateWebhookTarget(r, add)
		validateWebhookVerification(r, add)
	}
}

func validateWebhookTarget(r WebhookRoute, add func(*errs.Error)) {
	switch r.Method {
	case "POST", "PUT", "PATCH":
	default:
		add(errs.Newf(errs.ValidInvalid, "Webhook route %q has method %q.", r.Name, r.Method).
			WithRemedy("Use POST, PUT or PATCH."))
	}
	if !webhookRoutePath.MatchString(r.Path) || hasDotSegment(r.Path) {
		add(errs.Newf(errs.ValidInvalid, "Webhook route %q has path %q, which cannot be matched exactly.", r.Name, r.Path).
			WithRemedy("Use the exact path the app receives, such as /api/v1/webhook/noodle. Letters, digits, hyphens, dots, underscores and tildes only, with no wildcards, query or empty segments."))
	}
	if r.Path == "/.pando" || strings.HasPrefix(r.Path, "/.pando/") {
		add(errs.Newf(errs.ValidInvalid, "Webhook route %q has a path under /.pando, which Pando keeps for itself.", r.Name).
			WithRemedy("Use a path the app serves."))
	}
}

func hasDotSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func validateWebhookVerification(r WebhookRoute, add func(*errs.Error)) {
	v := r.Verification
	switch v.Kind {
	case WebhookHMACSHA256:
	case WebhookHeader:
		if v.Prefix != "" {
			add(errs.Newf(errs.ValidInvalid, "Webhook route %q has a prefix, which only hmac_sha256 uses.", r.Name).
				WithRemedy("Remove the prefix, or use hmac_sha256."))
		}
		if r.AllowRepeats {
			add(errs.Newf(errs.ValidInvalid, "Webhook route %q sets allow_repeats, which only hmac_sha256 uses.", r.Name).
				WithRemedy("Remove allow_repeats. A header route has no replay check."))
		}
	default:
		add(errs.Newf(errs.ValidInvalid, "Webhook route %q has verification %q.", r.Name, v.Kind).
			WithRemedy("Use hmac_sha256 or header."))
	}
	lower := strings.ToLower(v.Header)
	if !webhookHeaderName.MatchString(v.Header) || webhookReservedHeaders[lower] || strings.HasPrefix(lower, "x-pando-") {
		add(errs.Newf(errs.ValidInvalid, "Webhook route %q cannot verify with header %q.", r.Name, v.Header).
			WithRemedy("Name the header the sender puts its signature or secret in, such as X-Hub-Signature-256. Cookie, Host, connection headers and X-Pando-* cannot be used."))
	}
}

// compareWebhookRoutes is destructive whichever way it goes: adding a route
// lets requests into a private app, and removing one stops a sender (design 01
// §4).
func compareWebhookRoutes(old, next *AppSpec, add func(Class, string, string, any, any)) {
	if reflect.DeepEqual(old.WebhookRoutes, next.WebhookRoutes) ||
		len(old.WebhookRoutes)+len(next.WebhookRoutes) == 0 {
		return
	}
	add(Destructive, "webhook_routes",
		fmt.Sprintf("Which webhook deliveries reach this app changed: %d routes before, %d after.",
			len(old.WebhookRoutes), len(next.WebhookRoutes)),
		old.WebhookRoutes, next.WebhookRoutes)
}
