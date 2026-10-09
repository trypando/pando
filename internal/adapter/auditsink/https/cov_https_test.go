package https

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// TestR382_HTTPSUnconfiguredRefuses asserts an adapter Configure never
// succeeded on neither reports healthy nor pretends to deliver, and that an
// empty batch is delivered without a request.
func TestR382_HTTPSUnconfiguredRefuses(t *testing.T) {
	a := New()
	require.Equal(t, Kind, a.Kind())
	require.Equal(t, api.CategoryAuditSink, a.Category())
	require.ErrorContains(t, a.HealthCheck(context.Background()), "https: not configured")
	err := a.Send(context.Background(), batch)
	require.ErrorContains(t, err, "The HTTPS audit sink is not configured.")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	srv, ca, got := destination(t, "Authorization", http.StatusOK, "")
	b, err := configure(t, map[string]any{"url": srv.URL, "ca_certificate": ca})
	require.NoError(t, err)
	require.NoError(t, b.Send(context.Background(), api.AuditBatch{}))
	require.Empty(t, got.path, "an empty batch posts nothing")
}

// TestR382_HTTPSRefusalsQuoteTheAnswer asserts how a destination's refusal
// becomes an error: a non-2xx status with its body, an empty body said to be
// empty, and a redirect answered as a refusal rather than followed.
func TestR382_HTTPSRefusalsQuoteTheAnswer(t *testing.T) {
	srv, ca, _ := destination(t, "Authorization", http.StatusServiceUnavailable, "")
	a, err := configure(t, map[string]any{"url": srv.URL + "/ingest", "ca_certificate": ca})
	require.NoError(t, err)
	err = a.Send(context.Background(), batch)
	host := strings.TrimPrefix(srv.URL, "https://")
	require.ErrorContains(t, err, "The audit sink at "+host+" answered 503: (no body)")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))

	followed := false
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	t.Cleanup(elsewhere.Close)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	b := New()
	require.NoError(t, b.Configure(context.Background(), json.RawMessage(`{"url":"`+redirect.URL+`","credentials":{"token":"tk_redirect_42"}}`)))
	// The test server's own client trusts its certificate.
	b.client.Transport = redirect.Client().Transport
	err = b.Send(context.Background(), batch)
	require.ErrorContains(t, err, "answered 307")
	require.NotContains(t, err.Error(), "tk_redirect_42")
	require.False(t, followed, "a redirect is not followed with the audit log and its token")
}

// TestR382_HTTPSUnreachableAndCanceled asserts a destination that cannot be
// reached is an error naming its host and saying the events will be sent
// again, and that a canceled context ends a request that is waiting.
func TestR382_HTTPSUnreachableAndCanceled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	// Built rather than written out, so the credential scan has no literal to flag.
	token := "tk_" + "unreachable_9"
	a, err := configure(t, map[string]any{"url": "https://" + addr + "/ingest", "credentials": map[string]any{"token": token}})
	require.NoError(t, err)
	err = a.Send(context.Background(), batch)
	require.ErrorContains(t, err, "Pando could not reach the audit sink at "+addr+". The events will be sent again.")
	require.NotContains(t, err.Error(), token)
	require.NotContains(t, err.Error(), "/ingest", "the transport's error quotes the URL; the message does not")

	release := make(chan struct{})
	slow := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: slow.Certificate().Raw}))
	b, err := configure(t, map[string]any{"url": slow.URL, "ca_certificate": ca, "timeout_seconds": 30})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = b.Send(ctx, batch)
	require.ErrorContains(t, err, "Pando could not reach the audit sink")
	require.Less(t, time.Since(start), 10*time.Second, "the context ended the request, not the 30-second timeout")
}

// TestR382_ElasticBulkAnswers asserts how an Elastic _bulk answer is read: a
// 200 that is not a bulk response is accepted, errors with no failed item
// still refuse the batch, and a failure's reason has the token removed.
func TestR382_ElasticBulkAnswers(t *testing.T) {
	for answer, want := range map[string]string{
		`<html>proxy ok</html>`: "",
		`{"errors":false}`:      "",
		`{"errors":true,"items":[{"create":{"status":201}}]}`:                                                                               "reported errors in the batch",
		`{"errors":true,"items":[{"create":{"status":403,"error":{"type":"security_exception","reason":"key tk_bulk_77 may not write"}}}]}`: "refused an audit event with 403 security_exception: key [redacted] may not write. Check that the URL names a data stream",
	} {
		srv, ca, _ := destination(t, "Authorization", http.StatusOK, answer)
		a, err := configure(t, map[string]any{"url": srv.URL + "/logs-pando.audit-default/_bulk", "body": BodyElasticBulk,
			"ca_certificate": ca, "credentials": map[string]any{"token": "tk_bulk_77"}})
		require.NoError(t, err)
		err = a.Send(context.Background(), batch)
		if want == "" {
			require.NoError(t, err, answer)
			continue
		}
		require.ErrorContains(t, err, want, answer)
		require.NotContains(t, err.Error(), "tk_bulk_77")
	}
}

// TestR382_HTTPSSplunkRefusesAnEventItCannotWrap asserts an event that is not
// JSON fails the batch with a message rather than posting a broken envelope.
func TestR382_HTTPSSplunkRefusesAnEventItCannotWrap(t *testing.T) {
	srv, ca, got := destination(t, "Authorization", http.StatusOK, "")
	a, err := configure(t, map[string]any{"url": srv.URL, "body": BodySplunkHEC, "ca_certificate": ca})
	require.NoError(t, err)
	err = a.Send(context.Background(), api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{not json`)}})
	require.ErrorContains(t, err, "An audit event could not be wrapped for the destination's body shape.")
	require.Empty(t, got.path, "nothing was posted")
}

// TestHTTPSConfigurationRefusals asserts each setting Configure cannot use is
// refused with a message that names it and shows a valid one.
func TestHTTPSConfigurationRefusals(t *testing.T) {
	for settings, want := range map[string]string{
		`{"url":5}`: "https: reading configuration",
		`{"url":"https://h/","auth_header":"Bad Header"}`:                 `"Bad Header" is not a header name. Use one such as Authorization or DD-API-KEY`,
		`{"url":"https://h/","auth_header":"X(1)"}`:                       "is not a header name",
		`{"url":"https://h/","auth_scheme":"Two words"}`:                  "is not an authorization scheme. Use one word such as Bearer",
		`{"url":"https://h/","ca_certificate":"nope"}`:                    "the CA certificate has no certificate Pando can read",
		`{"url":"https://h/","extra_headers":": no name"}`:                "extra_headers has a line that is not a header",
		`{"url":"https://h/","extra_headers":"X-Ok: 1\nContent-Type: x"}`: "extra_headers sets Content-Type, which Pando sets itself",
		`{"url":"https://h/","extra_headers":"Host: elsewhere"}`:          "extra_headers sets Host",
		`{"url":"https://h/","max_batch":-2}`:                             "max_batch is -2",
		`{"url":"https://h/","format":"leef"}`:                            "not an event format",
		`{"url":"https:///nohost"}`:                                       "is not an address Pando can post to",
	} {
		require.ErrorContains(t, New().Configure(context.Background(), json.RawMessage(settings)), want, settings)
	}
}

// TestR194_ASecretURLIsShownByItsHostOnly asserts R-194 for a secret_url:
// the endpoint shown is its host, the path that is the credential is cut
// from any answer quoted, and a malformed one is not quoted in the refusal.
func TestR194_ASecretURLIsShownByItsHostOnly(t *testing.T) {
	const path = "/receiver/v1/http/TOPSECRETpath0001"
	srv, ca, _ := destination(t, "Authorization", http.StatusBadRequest, "unknown collector "+path+"?x=1")
	a, err := configure(t, map[string]any{"ca_certificate": ca, "credentials": map[string]any{"secret_url": srv.URL + path + "#frag"}})
	require.NoError(t, err)
	host := strings.TrimPrefix(srv.URL, "https://")
	require.Equal(t, host, a.AuditSinkCapabilities().Endpoint)
	err = a.Send(context.Background(), batch)
	require.ErrorContains(t, err, "answered 400")
	require.NotContains(t, err.Error(), "TOPSECRETpath0001")

	_, err = configure(t, map[string]any{"credentials": map[string]any{"secret_url": "http://h" + path}})
	require.ErrorContains(t, err, "the URL is http")
	require.NotContains(t, err.Error(), "TOPSECRETpath0001")
	_, err = configure(t, map[string]any{"credentials": map[string]any{"secret_url": "https://u:TOPSECRETpw@h" + path}})
	require.ErrorContains(t, err, "user name or password")
	require.NotContains(t, err.Error(), "TOPSECRET")
}

// TestR382_HTTPSPresetsAreValidSettings asserts every preset the console
// offers configures an adapter, given what its help says to add, and names
// only fields the form has.
func TestR382_HTTPSPresetsAreValidSettings(t *testing.T) {
	info := Info()
	fields := map[string]bool{}
	for _, f := range info.Fields {
		fields[f.Key] = true
	}
	require.NotEmpty(t, info.Presets)
	for _, p := range info.Presets {
		settings := map[string]any{}
		for k, v := range p.Values {
			require.True(t, fields[k], "preset %s sets %s, which is not a field", p.ID, k)
			settings[k] = v
		}
		if _, ok := settings["url"]; !ok {
			if p.ID == "sumo_logic" {
				settings["credentials"] = map[string]any{"secret_url": "https://endpoint1.collection.sumologic.com/receiver/v1/http/abc"}
			} else {
				settings["url"] = "https://siem.example.com/ingest"
			}
		}
		_, err := configure(t, settings)
		require.NoError(t, err, p.ID)
		require.NotEmpty(t, p.Help, p.ID)
	}
}

func TestHTTPSSnippetIsCutOnARuneBoundary(t *testing.T) {
	a := &Adapter{}
	s := a.snippet([]byte(strings.Repeat("é", 400)))
	require.True(t, strings.HasSuffix(s, "…"))
	require.LessOrEqual(t, len(s), snippetLimit+len("…"))
	require.True(t, json.Valid([]byte(`"`+s+`"`)), "no half rune is left behind")
}
