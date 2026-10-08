package forgekit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trypando/pando/internal/errs"
)

func TestNextLink(t *testing.T) {
	for header, want := range map[string]string{
		"": "",
		`<https://api.example.com/x?page=2>; rel="next", <https://api.example.com/x?page=9>; rel="last"`: "https://api.example.com/x?page=2",
		`<https://api.example.com/x?page=1>; rel="prev", <https://api.example.com/x?page=3>; rel="next"`: "https://api.example.com/x?page=3",
		`<https://api.example.com/x?page=9>; rel="last"`:                                                 "",
		`<https://api.example.com/x?page=2>;rel=next`:                                                    "https://api.example.com/x?page=2",
	} {
		if got := NextLink(header); got != want {
			t.Errorf("NextLink(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestLimitAndMatches(t *testing.T) {
	for in, want := range map[int]int{0: 100, -1: 100, 5: 5, 1000: 1000, 1001: 100} {
		if got := Limit(in); got != want {
			t.Errorf("Limit(%d) = %d, want %d", in, got, want)
		}
	}
	if !Matches("acme/Platform/api", "PLAT api") || !Matches("acme/api", "") || Matches("acme/api", "web") {
		t.Error("Matches")
	}
}

func TestClientDoAndStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok-s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Link", `<http://x/ok?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"name":"api"}`))
		case "/bad":
			_, _ = w.Write([]byte(`not json`))
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := Client{Provider: "Forge", Authorize: func(r *http.Request) { r.Header.Set("Authorization", "token tok-s3cret") }}
	var out struct{ Name string }
	next, err := c.Get(context.Background(), srv.URL+"/ok", &out)
	if err != nil || out.Name != "api" || next != "http://x/ok?page=2" {
		t.Fatalf("Get = %q, %+v, %v", next, out, err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/bad", &out); errs.CodeOf(err) != errs.AdapterFailed {
		t.Errorf("unreadable body: %v", err)
	}
	for path, code := range map[string]errs.Code{
		"/forbidden": errs.AdapterFailed,
		"/limited":   errs.AdapterUnavailable,
		"/broken":    errs.AdapterUnavailable,
		"/missing":   errs.NotFound,
	} {
		_, err := c.Get(context.Background(), srv.URL+path, nil)
		if errs.CodeOf(err) != code {
			t.Errorf("%s: %v, want %s", path, err, code)
		}
		if err != nil && strings.Contains(err.Error(), "tok-s3cret") {
			t.Errorf("%s: error carries the token", path)
		}
	}
	anon := Client{Provider: "Forge"}
	_, err = anon.Get(context.Background(), srv.URL+"/ok", nil)
	if e := errs.As(err); e == nil || !strings.Contains(e.Message, "did not accept") || e.Remedy == "" {
		t.Errorf("401: %v", err)
	}
	if _, err := c.Get(context.Background(), "http://127.0.0.1:1/unreachable", nil); errs.CodeOf(err) != errs.AdapterUnavailable {
		t.Errorf("unreachable: %v", err)
	}
	if err := StatusError("Forge", 204); err != nil {
		t.Errorf("204: %v", err)
	}
	if err := StatusError("Forge", 418); errs.CodeOf(err) != errs.AdapterFailed {
		t.Errorf("418: %v", err)
	}
}
