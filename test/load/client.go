package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to Pando through the load balancer. One transport for the
// whole run, as a fleet of browsers behind one NAT would look to the
// balancer: many connections, kept alive.
type Client struct {
	Root string // http://host:port, no trailing slash, no /api/v1
	HTTP *http.Client
}

func NewClient(root string, timeout time.Duration) *Client {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        20_000,
		MaxIdleConnsPerHost: 20_000,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		Root: strings.TrimSuffix(strings.TrimSuffix(root, "/"), "/api/v1"),
		HTTP: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// A redirect is an answer (the proxy sending an anonymous visitor
			// to sign in); following it would time the sign-in page instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *Client) API(path string) string { return c.Root + "/api/v1" + path }

// Credential is how a request authenticates: a session cookie, a bearer
// token, or neither.
type Credential struct {
	Cookie string
	Bearer string
}

func (cr Credential) apply(req *http.Request) {
	if cr.Cookie != "" {
		// A cookie a client sends carries only its name and value; Secure,
		// HttpOnly and SameSite are attributes the server sets.
		req.AddCookie(&http.Cookie{Name: "pando_session", Value: cr.Cookie}) //nolint:gosec // G124: see above.
	}
	if cr.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+cr.Bearer)
	}
}

// Response is what a request came back with.
type Response struct {
	Status  int
	Body    []byte
	Latency time.Duration
	Cookies []*http.Cookie
}

// Do sends one request. The body is read to the end, because a response is
// not served until it is, and the latency includes it.
func (c *Client) Do(ctx context.Context, method, rawURL, host string, cred Credential, body string) (Response, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return Response{}, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if host != "" {
		req.Host = host
	}
	cred.apply(req)
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{Latency: time.Since(start)}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Body: b, Latency: time.Since(start), Cookies: resp.Cookies()}, err
}

// JSON sends an API request and decodes a 2xx answer into out, if given.
func (c *Client) JSON(ctx context.Context, method, path string, cred Credential, body string, out any) (int, error) {
	resp, err := c.Do(ctx, method, c.API(path), "", cred, body)
	if err != nil {
		return 0, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return resp.Status, fmt.Errorf("%s %s: %d %.300s", method, path, resp.Status, resp.Body)
	}
	if out != nil && len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp.Status, fmt.Errorf("%s %s: decoding: %w", method, path, err)
		}
	}
	return resp.Status, nil
}

// AwaitHealthy waits for /healthz to answer 200.
func (c *Client) AwaitHealthy(ctx context.Context, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		resp, err := c.Do(ctx, http.MethodGet, c.Root+"/healthz", "", Credential{}, "")
		if err == nil && resp.Status == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("pando at %s did not become healthy within %s: %w", c.Root, within, err)
			}
			return fmt.Errorf("pando at %s did not become healthy within %s: /healthz answered %d", c.Root, within, resp.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ClaimSetup makes the first administrator on a fresh install (R-046), the
// way test/replicas does. An install already claimed is left alone.
func (c *Client) ClaimSetup(ctx context.Context, username, password string) error {
	var setup struct {
		Needed bool `json:"needed"`
	}
	if _, err := c.JSON(ctx, http.MethodGet, "/setup", Credential{}, "", &setup); err != nil {
		return err
	}
	if !setup.Needed {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	_, err := c.JSON(ctx, http.MethodPost, "/setup", Credential{}, string(body), nil)
	return err
}

// SignIn returns a session cookie for a local account.
func (c *Client) SignIn(ctx context.Context, username, password string) (string, Response, error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := c.Do(ctx, http.MethodPost, c.API("/sessions"), "", Credential{}, string(body))
	if err != nil {
		return "", resp, err
	}
	if resp.Status != http.StatusOK {
		return "", resp, fmt.Errorf("signing in as %s: %d %.300s", username, resp.Status, resp.Body)
	}
	for _, ck := range resp.Cookies {
		if ck.Name == "pando_session" {
			return ck.Value, resp, nil
		}
	}
	return "", resp, errors.New("signing in set no session cookie")
}

// hostOf is the host part of the client's root, for port-mode requests.
func (c *Client) hostOf() string {
	u, err := url.Parse(c.Root)
	if err != nil {
		return "localhost"
	}
	return u.Hostname()
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
