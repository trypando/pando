package subscription

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Headers on every webhook delivery (R-369).
const (
	HeaderSignature = "Pando-Signature"
	HeaderTimestamp = "Pando-Timestamp"
	HeaderEventID   = "Pando-Event-Id"
	HeaderEvent     = "Pando-Event"
	HeaderDelivery  = "Pando-Delivery-Id"
)

// Envelope is the body of a webhook delivery. Its shape is the contract with
// whoever receives it, documented in docs/events.md; data is the catalogued
// fields of the event and nothing else.
type Envelope struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	App        *EnvelopeApp   `json:"app,omitempty"`
	Actor      Actor          `json:"actor"`
	Data       map[string]any `json:"data"`

	// Link is where in the console to look, when Pando's external_url is
	// set (R-369).
	Link string `json:"link,omitempty"`
}

// EnvelopeApp names the app an event is about.
type EnvelopeApp struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// Actor is who caused an event: a person, a token, or Pando itself.
type Actor struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
}

// envelopeOf builds the body for one event.
func envelopeOf(e state.Event, app *EnvelopeApp, link string) Envelope {
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	return Envelope{
		ID: e.ID, Type: e.Name, OccurredAt: e.OccurredAt.UTC(), App: app,
		Actor: Actor{Kind: e.ActorKind, ID: e.ActorID, OnBehalfOf: e.OnBehalfOf},
		Data:  data, Link: link,
	}
}

// keyPrefix marks a signing key, so one pasted into the wrong place is
// recognizable.
const keyPrefix = "whsec_"

// NewKey returns a fresh signing key: 32 random bytes.
func NewKey() (secret.Value, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal, "Could not make a signing key.", err)
	}
	return secret.New(keyPrefix + base64.RawURLEncoding.EncodeToString(b)), nil
}

// Sign returns the signature header for a body sent at timestamp: the
// HMAC-SHA256, keyed by the whole signing key as given, of the timestamp in
// Unix seconds, a full stop, and the body exactly as sent.
//
// The timestamp is inside what is signed so a receiver that refuses old
// timestamps refuses a replayed delivery, and the event ID is in a header so a
// receiver can drop a delivery it already has (R-369).
func Sign(key secret.Value, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key.Reveal()))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is Sign's other half: what a receiver does, here so the
// documentation's example and the tests check the same thing Pando sends.
func Verify(key secret.Value, header string, timestamp int64, body []byte, now time.Time, tolerance time.Duration) bool {
	if d := now.Sub(time.Unix(timestamp, 0)); d > tolerance || d < -tolerance {
		return false
	}
	want := Sign(key, timestamp, body)
	for _, part := range strings.Split(header, ",") {
		if hmac.Equal([]byte(strings.TrimSpace(part)), []byte(want)) {
			return true
		}
	}
	return false
}

// Result is how one attempt went.
type Result struct {
	StatusCode int
	Err        error
	Duration   time.Duration
}

// OK reports a 2xx answer.
func (r Result) OK() bool { return r.Err == nil && r.StatusCode >= 200 && r.StatusCode < 300 }

// Message is what went wrong, in a line, for the delivery log.
func (r Result) Message() string {
	switch {
	case r.Err != nil:
		// An adapter's envelope says what happened in its message; the code
		// in front of it is for machines.
		if e := errs.As(r.Err); e != nil {
			return e.Message
		}
		return r.Err.Error()
	case r.OK():
		return ""
	default:
		return fmt.Sprintf("The endpoint answered %d %s.", r.StatusCode, http.StatusText(r.StatusCode))
	}
}

// webhookRequest is one delivery as it goes on the wire.
type webhookRequest struct {
	URL         string
	Method      string
	ContentType string
	// Headers are the subscription's own, already opened (R-375). Pando's
	// headers are set after them and cannot be overridden: a custom header
	// never carries a Pando- name, which is refused when it is saved.
	Headers    map[string]string
	Body       []byte
	Key        secret.Value
	DeliveryID string
	Event      string
	EventID    string
}

// post sends one signed delivery. The signature is over the body exactly as
// sent, whether that is Pando's envelope or a subscription's own template.
func post(ctx context.Context, client *http.Client, w webhookRequest, now time.Time) Result {
	method := w.Method
	if method == "" {
		method = http.MethodPost
	}
	contentType := w.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	ts := now.Unix()
	req, err := http.NewRequestWithContext(ctx, method, w.URL, bytes.NewReader(w.Body))
	if err != nil {
		return Result{Err: sentence("The webhook address could not be used: " + err.Error())}
	}
	for name, value := range w.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Pando-Webhooks/1")
	req.Header.Set(HeaderEvent, w.Event)
	req.Header.Set(HeaderEventID, w.EventID)
	req.Header.Set(HeaderDelivery, w.DeliveryID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(w.Key, ts, w.Body))

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return Result{Err: describeSendError(err), Duration: elapsed}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return Result{StatusCode: resp.StatusCode, Duration: elapsed}
}

// describeSendError turns a transport error into a sentence for the delivery
// log. The private-address refusal is named as itself, because the fix is a
// policy setting rather than anything at the far end.
func describeSendError(err error) error {
	if errors.Is(err, errPrivateAddress) {
		return sentence("Pando did not send this: the webhook's address resolves to a private, loopback or link-local address, which host policy does not allow (allow_private_webhooks).")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return sentence("The endpoint did not answer within 15 seconds.")
	}
	return sentence("Pando could not reach the endpoint: " + err.Error())
}

var errPrivateAddress = errors.New("private address refused")

// sentence is an error written for the delivery log: a whole sentence a
// person reads, capitalized and punctuated as one (R-105).
type sentence string

func (s sentence) Error() string { return string(s) }

// forbidden is the set of addresses a webhook may not reach unless policy
// allows it (R-372): private, loopback, link-local (169.254.169.254 among
// them), unspecified and multicast.
func forbidden(a netip.Addr) bool {
	a = a.Unmap()
	return egress.Private(a) || a.IsLoopback() || a.IsUnspecified() || a.IsMulticast()
}

// newClient returns the HTTP client webhooks are sent with.
//
// The check is made on the address actually dialed, after DNS, in the
// dialer's Control hook. Checking a name before connecting is not enough: a
// name can resolve to a public address when it is checked and a private one
// when it is used. Redirects are not followed — a 3xx is an answer, and a
// failed one — so a public endpoint cannot bounce a delivery inward. No proxy
// from the environment either, for the same reason.
func newClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if forbidden(ip) && !allowPrivate {
				return errPrivateAddress
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Resolver looks a host name up. net.DefaultResolver in production.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// CheckURL refuses a webhook address before it is saved, so a person finds out
// now rather than from a delivery log (R-372). The dialer checks again at every
// send; this is the readable half, not the enforcing one.
func CheckURL(ctx context.Context, raw string, allowPrivate bool, resolver Resolver) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errs.Newf(errs.ValidInvalid,
			"%q is not a webhook address Pando can send to. Use a full http or https URL, such as https://example.com/hooks/pando.", raw)
	}
	if u.User != nil {
		return "", errs.New(errs.ValidInvalid,
			"A webhook address cannot carry a username or password: the address is shown to anyone who can see the subscription. "+
				"Check the Pando-Signature header to know a delivery came from Pando.")
	}
	if u.Fragment != "" {
		u.Fragment = ""
	}
	if allowPrivate {
		return u.String(), nil
	}

	host := u.Hostname()
	addrs := []netip.Addr{}
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, ip)
	} else if resolver != nil {
		found, err := resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return "", errs.Newf(errs.ValidInvalid,
				"Pando could not look up %s, the webhook's host. Check the address is spelled correctly and resolves from the Pando server.", host)
		}
		addrs = found
	}
	for _, a := range addrs {
		if forbidden(a) {
			return "", errs.Newf(errs.PolicyWebhookPrivateAddress,
				"The webhook address %s resolves to %s, a private, loopback or link-local address. Host policy does not let webhooks reach those.", u.Host, a.Unmap()).
				WithRemedy("Use an address reachable on the internet, or ask whoever administers this installation to turn on allow_private_webhooks in host policy.")
		}
	}
	return u.String(), nil
}
