package https

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

type captured struct {
	path, contentType, auth, extra string
	body                           string
}

// destination is a TLS server answering status and body, recording each
// request. Its certificate is returned as PEM for ca_certificate.
func destination(t *testing.T, header string, status int, answer string) (*httptest.Server, string, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = captured{r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get(header), r.Header.Get("X-Source"), string(b)}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return srv, ca, got
}

func configure(t *testing.T, settings map[string]any) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	a := New()
	return a, a.Configure(context.Background(), raw)
}

var batch = api.AuditBatch{
	Events: []json.RawMessage{json.RawMessage(`{"id":1,"action":"app.deploy"}`), json.RawMessage(`{"id":2,"action":"app.use"}`)},
	IDs:    []int64{1, 2},
}

// TestR382_HTTPSBodiesMatchEachProvider asserts R-382: each body shape and
// auth scheme is what its provider's ingest endpoint reads.
func TestR382_HTTPSBodiesMatchEachProvider(t *testing.T) {
	cases := []struct {
		name, body, header, scheme, path string
		wantType, wantAuth, wantBody     string
	}{
		{"generic", BodyNDJSON, "Authorization", "", "/ingest", "application/x-ndjson", "Bearer tk_1",
			`{"id":1,"action":"app.deploy"}` + "\n" + `{"id":2,"action":"app.use"}` + "\n"},
		{"datadog", BodyJSONArray, "DD-API-KEY", SchemeNone, "/api/v2/logs?ddsource=pando", "application/json", "tk_1",
			`[{"id":1,"action":"app.deploy"},{"id":2,"action":"app.use"}]`},
		{"splunk", BodySplunkHEC, "Authorization", "Splunk", "/services/collector/event", "application/json", "Splunk tk_1",
			`{"event":{"id":1,"action":"app.deploy"},"sourcetype":"pando:audit","source":"pando"}` + "\n" +
				`{"event":{"id":2,"action":"app.use"},"sourcetype":"pando:audit","source":"pando"}` + "\n"},
		{"elastic", BodyElasticBulk, "Authorization", "ApiKey", "/logs-pando.audit-default/_bulk", "application/x-ndjson", "ApiKey tk_1",
			"{\"create\":{}}\n" + `{"id":1,"action":"app.deploy"}` + "\n{\"create\":{}}\n" + `{"id":2,"action":"app.use"}` + "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, ca, got := destination(t, c.header, http.StatusOK, `{"errors":false}`)
			a, err := configure(t, map[string]any{
				"url": srv.URL + c.path, "body": c.body, "auth_header": c.header, "auth_scheme": c.scheme,
				"ca_certificate": ca, "extra_headers": "X-Source: pando\n", "format": "ocsf",
				"credentials": map[string]any{"token": "tk_1"},
			})
			require.NoError(t, err)
			require.NoError(t, a.HealthCheck(context.Background()))
			require.NoError(t, a.Send(context.Background(), batch))
			require.Equal(t, c.path, got.path)
			require.Equal(t, c.wantType, got.contentType)
			require.Equal(t, c.wantAuth, got.auth)
			require.Equal(t, "pando", got.extra)
			require.Equal(t, c.wantBody, got.body)

			caps := a.AuditSinkCapabilities()
			require.Equal(t, "https", caps.Transport)
			require.Equal(t, strings.TrimPrefix(srv.URL, "https://"), caps.Endpoint)
			require.Equal(t, api.AuditFormatOCSF, caps.Format)
			require.Equal(t, DefaultMaxBatch, caps.MaxBatch)
		})
	}
	require.NoError(t, Info().Validate())
}

// TestR382_ElasticItemFailuresFailTheBatch asserts R-382's contract with
// Send: Elasticsearch answers 200 with per-item errors, and a batch with one
// is not counted as delivered.
func TestR382_ElasticItemFailuresFailTheBatch(t *testing.T) {
	srv, ca, _ := destination(t, "Authorization", http.StatusOK,
		`{"errors":true,"items":[{"create":{"status":201}},{"create":{"status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse"}}}]}`)
	a, err := configure(t, map[string]any{"url": srv.URL + "/logs-pando.audit-default/_bulk", "body": BodyElasticBulk, "ca_certificate": ca})
	require.NoError(t, err)
	err = a.Send(context.Background(), batch)
	require.ErrorContains(t, err, "mapper_parsing_exception")
}

// TestR194_ATokenNeverAppearsInAnError asserts R-194: a destination that
// echoes the token, or a secret URL's path, back in a refusal does not put
// either in the error.
func TestR194_ATokenNeverAppearsInAnError(t *testing.T) {
	const token = "tk_supersecret_0123456789"
	srv, ca, _ := destination(t, "Authorization", http.StatusUnauthorized, "bad token: Bearer "+token+" "+strings.Repeat("x", 400))
	a, err := configure(t, map[string]any{"url": srv.URL, "ca_certificate": ca, "credentials": map[string]any{"token": token}})
	require.NoError(t, err)
	err = a.Send(context.Background(), batch)
	require.ErrorContains(t, err, "401")
	require.ErrorContains(t, err, "[redacted]")
	require.NotContains(t, err.Error(), token)
	require.Less(t, len(err.Error()), 450, "the answer is quoted only in part")

	// A Sumo Logic style URL whose path is the credential.
	const path = "/receiver/v1/http/ZaVnC4dhaV2sEcretPath"
	srv2, ca2, got := destination(t, "Authorization", http.StatusForbidden, "no source at "+path)
	b, err := configure(t, map[string]any{"ca_certificate": ca2, "credentials": map[string]any{"secret_url": srv2.URL + path}})
	require.NoError(t, err)
	require.Equal(t, strings.TrimPrefix(srv2.URL, "https://"), b.AuditSinkCapabilities().Endpoint)
	err = b.Send(context.Background(), batch)
	require.Error(t, err)
	require.Equal(t, path, got.path)
	require.NotContains(t, err.Error(), "ZaVnC4dhaV2sEcretPath")

	// Unreachable: the transport's error would quote the URL; the message must not.
	c, err := configure(t, map[string]any{"credentials": map[string]any{"secret_url": "https://127.0.0.1:1" + path}})
	require.NoError(t, err)
	err = c.Send(context.Background(), batch)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "ZaVnC4dhaV2sEcretPath")

	// Configure's refusals do not quote a secret URL either.
	_, err = configure(t, map[string]any{"credentials": map[string]any{"secret_url": "ftp://h" + path}})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "ZaVnC4dhaV2sEcretPath")
}

// TestR190_CredentialsInPlainConfigAreRefused asserts R-190: a token, a secret
// URL, a password in the URL, or an auth header in extra_headers — each a
// credential in the unencrypted configuration — is refused.
func TestR190_CredentialsInPlainConfigAreRefused(t *testing.T) {
	for settings, want := range map[string]string{
		`{"url":"https://h/","token":"x"}`:                                                "set it as a credential",
		`{"secret_url":"https://h/x"}`:                                                    "set it as a credential",
		`{"url":"https://user:hunter2@h/"}`:                                               "user name or password",
		`{"url":"https://h/","extra_headers":"Authorization: x"}`:                         "carries the token",
		`{"url":"https://h/","auth_header":"DD-API-KEY","extra_headers":"dd-api-key: x"}`: "carries the token",
	} {
		a := New()
		err := a.Configure(context.Background(), json.RawMessage(settings))
		require.ErrorContains(t, err, want, settings)
		require.NotContains(t, err.Error(), "hunter2")
	}
}

func TestHTTPSURLIsChecked(t *testing.T) {
	for settings, want := range map[string]string{
		`{}`:                                  "no url",
		`{"url":"http://h/ingest"}`:           "unencrypted",
		`{"url":"ftp://h/"}`:                  "not an address",
		`{"url":"https://h/","body":"xml"}`:   "not a body shape",
		`{"url":"https://h/","start":"soon"}`: "not a starting point",
		`{"url":"https://h/","extra_headers":"not a header"}`:             "not a header",
		`{"url":"https://h/","credentials":{"secret_url":"https://h/x"}}`: "both",
	} {
		require.ErrorContains(t, New().Configure(context.Background(), json.RawMessage(settings)), want, settings)
	}
	a := New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"url":"http://h:8080/ingest","allow_http":true,"start":"now"}`)))
	require.Equal(t, "h:8080", a.AuditSinkCapabilities().Endpoint)
	require.True(t, a.AuditSinkCapabilities().StartAtNow)
}
