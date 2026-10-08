package forgekit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// tokenServer answers the token endpoint with body, as JSON or form-encoded.
func tokenServer(t *testing.T, status int, contentType, body string, check func(url.Values)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if check != nil {
			check(r.PostForm)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func provider(srv *httptest.Server) OAuth {
	return OAuth{
		Provider: "Forge", ClientID: "cid", ClientSecret: secret.New("csecret-s3cret"),
		DeviceURL: srv.URL + "/device", AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token",
		Scopes: []string{"read", "write"},
	}
}

func TestTokensRoundTrip(t *testing.T) {
	in := Tokens{Access: secret.New("a"), Refresh: secret.New("r"), ExpiresAt: now}
	out := TokensFrom(in.Fields())
	if out.Access.Reveal() != "a" || out.Refresh.Reveal() != "r" || !out.ExpiresAt.Equal(now) {
		t.Fatalf("round trip = %+v", out)
	}
	f := Tokens{Access: secret.New("a")}.Fields()
	if _, ok := f[FieldRefreshToken]; ok || !f[FieldTokenExpiresAt].IsZero() {
		t.Fatalf("fields without refresh or expiry = %v", f)
	}
	if _, ok := f[FieldTokenExpiresAt]; !ok {
		t.Fatal("an absent expiry is not cleared")
	}
}

func TestFresh(t *testing.T) {
	ctx := context.Background()
	o := OAuth{Provider: "Forge", ClientID: "cid", TokenURL: "http://127.0.0.1:1/token"}

	if _, _, err := o.Fresh(ctx, Tokens{}, now); errs.CodeOf(err) != errs.StateInvalid {
		t.Errorf("no access token: %v", err)
	}

	noExpiry := Tokens{Access: secret.New("a")}
	got, rotated, err := o.Fresh(ctx, noExpiry, now)
	if err != nil || rotated || got.Access.Reveal() != "a" {
		t.Errorf("no expiry: %+v %v %v", got, rotated, err)
	}

	valid := Tokens{Access: secret.New("a"), Refresh: secret.New("r"), ExpiresAt: now.Add(time.Hour)}
	got, rotated, err = o.Fresh(ctx, valid, now)
	if err != nil || rotated || got.Access.Reveal() != "a" {
		t.Errorf("not expired: %+v %v %v", got, rotated, err)
	}

	expiredNoRefresh := Tokens{Access: secret.New("a"), ExpiresAt: now.Add(-time.Hour)}
	_, _, err = o.Fresh(ctx, expiredNoRefresh, now)
	if e := errs.As(err); e == nil || e.Code != errs.StateInvalid || e.Remedy == "" {
		t.Errorf("expired with no refresh token: %v", err)
	}

	srv := tokenServer(t, 200, "application/json", `{"access_token":"a2","expires_in":3600}`, func(f url.Values) {
		if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "r" ||
			f.Get("client_id") != "cid" || f.Get("client_secret") != "csecret-s3cret" || f.Get("scope") != "read write" {
			t.Errorf("refresh form = %v", f)
		}
	})
	// Within a minute of expiry counts as expired.
	soon := Tokens{Access: secret.New("a"), Refresh: secret.New("r"), ExpiresAt: now.Add(30 * time.Second)}
	got, rotated, err = provider(srv).Fresh(ctx, soon, now)
	if err != nil || !rotated || got.Access.Reveal() != "a2" || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("refreshed: %+v %v %v", got, rotated, err)
	}
	if got.Refresh.Reveal() != "r" {
		t.Error("a provider that does not rotate refresh tokens lost the old one")
	}

	revoked := tokenServer(t, 400, "application/json", `{"error":"invalid_grant"}`, nil)
	_, _, err = provider(revoked).Fresh(ctx, soon, now)
	if errs.CodeOf(err) != errs.StateInvalid || strings.Contains(err.Error(), "csecret-s3cret") {
		t.Errorf("revoked: %v", err)
	}
}

func TestBeginDevice(t *testing.T) {
	srv := tokenServer(t, 200, "application/json",
		`{"device_code":"dc","user_code":"UC","verification_uri":"https://forge/device","expires_in":600}`,
		func(f url.Values) {
			if f.Get("client_id") != "cid" || f.Get("scope") != "read write" {
				t.Errorf("device form = %v", f)
			}
		})
	auth, err := provider(srv).BeginDevice(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Mode != api.AuthorizationDevice || auth.UserCode != "UC" || auth.VerificationURL != "https://forge/device" ||
		auth.Flow.Reveal() != "dc" || auth.Interval != 5*time.Second || !auth.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("authorization = %+v", auth)
	}

	if _, err := (OAuth{Provider: "Forge"}).BeginDevice(context.Background(), now); err == nil {
		t.Error("device authorization without a device endpoint")
	}
	o := provider(srv)
	o.ClientID = ""
	if _, err := o.BeginDevice(context.Background(), now); err == nil {
		t.Error("device authorization without a client ID")
	}
	refused := tokenServer(t, 400, "application/json", `{"error":"invalid_client","error_description":"device flow disabled"}`, nil)
	if _, err := provider(refused).BeginDevice(context.Background(), now); err == nil || !strings.Contains(err.Error(), "device flow disabled") {
		t.Errorf("refused: %v", err)
	}
}

func TestPollDeviceErrorMapping(t *testing.T) {
	for body, check := range map[string]func(api.AuthorizationResult, error) bool{
		`{"error":"authorization_pending"}`: func(r api.AuthorizationResult, err error) bool { return err == nil && r.Pending && !r.SlowDown },
		`{"error":"slow_down"}`:             func(r api.AuthorizationResult, err error) bool { return err == nil && r.Pending && r.SlowDown },
		`{"error":"expired_token"}`:         func(_ api.AuthorizationResult, err error) bool { return errs.CodeOf(err) == errs.StateInvalid },
		`{"error":"access_denied"}`:         func(_ api.AuthorizationResult, err error) bool { return errs.CodeOf(err) == errs.StateInvalid },
		`{"error":"unsupported_grant_type"}`: func(_ api.AuthorizationResult, err error) bool {
			return errs.CodeOf(err) == errs.AdapterFailed
		},
		`{"access_token":"at","refresh_token":"rt","expires_in":60}`: func(r api.AuthorizationResult, err error) bool {
			return err == nil && !r.Pending && r.Credentials[FieldAccessToken].Reveal() == "at" &&
				r.Credentials[FieldRefreshToken].Reveal() == "rt" && r.ExpiresAt.Equal(now.Add(time.Minute))
		},
	} {
		srv := tokenServer(t, 400, "application/json", body, func(f url.Values) {
			if f.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || f.Get("device_code") != "dc" {
				t.Errorf("poll form = %v", f)
			}
		})
		res, err := provider(srv).PollDevice(context.Background(), secret.New("dc"), now)
		if !check(res, err) {
			t.Errorf("%s: %+v, %v", body, res, err)
		}
		if err != nil && strings.Contains(err.Error(), "csecret-s3cret") {
			t.Errorf("%s: error carries the client secret", body)
		}
	}

	down := tokenServer(t, 503, "text/plain", "down", nil)
	if _, err := provider(down).PollDevice(context.Background(), secret.New("dc"), now); errs.CodeOf(err) != errs.AdapterUnavailable {
		t.Errorf("5xx: %v", err)
	}
	empty := tokenServer(t, 200, "application/json", `{}`, nil)
	if _, err := provider(empty).PollDevice(context.Background(), secret.New("dc"), now); errs.CodeOf(err) != errs.AdapterFailed {
		t.Errorf("no token: %v", err)
	}
	garbage := tokenServer(t, 200, "text/html", "<html>", nil)
	if _, err := provider(garbage).PollDevice(context.Background(), secret.New("dc"), now); errs.CodeOf(err) != errs.AdapterFailed {
		t.Errorf("unreadable: %v", err)
	}
}

func TestFormEncodedTokenResponse(t *testing.T) {
	srv := tokenServer(t, 200, "application/x-www-form-urlencoded",
		"access_token=at&refresh_token=rt&expires_in=120&token_type=bearer", nil)
	res, err := provider(srv).PollDevice(context.Background(), secret.New("dc"), now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials[FieldAccessToken].Reveal() != "at" || !res.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("result = %+v", res)
	}
	pending := tokenServer(t, 200, "application/x-www-form-urlencoded", "error=authorization_pending", nil)
	res, err = provider(pending).PollDevice(context.Background(), secret.New("dc"), now)
	if err != nil || !res.Pending {
		t.Fatalf("pending form: %+v %v", res, err)
	}
}

func TestWebFlow(t *testing.T) {
	var verifier string
	srv := tokenServer(t, 200, "application/json", `{"access_token":"at"}`, func(f url.Values) {
		if f.Get("grant_type") != "authorization_code" || f.Get("code") != "c" || f.Get("code_verifier") != verifier ||
			f.Get("redirect_uri") != "https://pando/cb" {
			t.Errorf("exchange form = %v", f)
		}
	})
	o := provider(srv)
	o.AuthorizeURL += "?tenant=x"
	auth, err := o.BeginWeb(api.AuthorizationRequest{Mode: api.AuthorizationWeb, RedirectURL: "https://pando/cb", State: "st"},
		url.Values{"prompt": {"consent"}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.AuthorizeURL)
	q := u.Query()
	verifier = auth.Flow.Reveal()
	sum := sha256.Sum256([]byte(verifier))
	if q.Get("tenant") != "x" || q.Get("state") != "st" || q.Get("prompt") != "consent" || q.Get("scope") != "read write" ||
		q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL = %s", auth.AuthorizeURL)
	}
	if strings.Contains(auth.AuthorizeURL, verifier) || strings.Contains(auth.AuthorizeURL, "csecret-s3cret") {
		t.Fatal("the authorize URL carries the verifier or the client secret")
	}
	res, err := o.CompleteWeb(context.Background(), api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: auth.Flow, Code: "c", RedirectURL: "https://pando/cb",
	}, now)
	if err != nil || res.Credentials[FieldAccessToken].Reveal() != "at" || !res.ExpiresAt.IsZero() {
		t.Fatalf("complete = %+v %v", res, err)
	}

	if _, err := o.BeginWeb(api.AuthorizationRequest{State: "st"}, nil); err == nil {
		t.Error("web authorization began without a redirect URL")
	}
	refused := tokenServer(t, 400, "application/json", `{"error":"invalid_grant"}`, nil)
	if _, err := provider(refused).CompleteWeb(context.Background(), api.AuthorizationCompletion{Flow: secret.New("v")}, now); errs.CodeOf(err) != errs.StateInvalid {
		t.Errorf("refused exchange: %v", err)
	}
}

func TestBasicClientAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "cid" || pass != "csecret-s3cret" {
			t.Errorf("basic auth = %q %q %v", user, pass, ok)
		}
		_, _ = fmt.Fprint(w, `{"access_token":"at"}`)
	}))
	defer srv.Close()
	o := provider(srv)
	o.BasicClientAuth = true
	if _, err := o.CompleteWeb(context.Background(), api.AuthorizationCompletion{Flow: secret.New("v")}, now); err != nil {
		t.Fatal(err)
	}
}
