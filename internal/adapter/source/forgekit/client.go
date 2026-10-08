package forgekit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// Client calls a forge's JSON API.
type Client struct {
	HTTP *http.Client

	// Provider names the forge in messages.
	Provider string

	// Authorize sets the request's credentials.
	Authorize func(*http.Request)
}

// Do sends a request and decodes a JSON answer into out, which may be nil. It
// returns the next page's address from a Link header, if there is one.
func (c Client) Do(ctx context.Context, method, url string, body io.Reader, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not build the request to the provider.", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Authorize != nil {
		c.Authorize(req)
	}
	client := c.HTTP
	if client == nil {
		client = HTTPClient(nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Pando could not reach %s's API.", c.Provider), err).
			WithRemedy("Check that this installation can reach the host, and its address in the connection's settings.")
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

	if err := StatusError(c.Provider, resp.StatusCode); err != nil {
		return "", err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return "", errs.Newf(errs.AdapterFailed, "%s's API answered in a form Pando could not read.", c.Provider)
		}
	}
	return NextLink(resp.Header.Get("Link")), nil
}

// Get is Do with GET.
func (c Client) Get(ctx context.Context, url string, out any) (string, error) {
	return c.Do(ctx, http.MethodGet, url, nil, out)
}

// StatusError turns a forge's HTTP status into a message a person can act on
// (R-105), or nil for a success.
//
// A forge refusing the connection's credential is an adapter failure, never
// AUTH_* or PERM_*: those are answered 401 and 403, which say the person
// calling Pando is not signed in or not allowed, and a client acts on that.
func StatusError(provider string, status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		return errs.Newf(errs.AdapterFailed,
			"%s did not accept this connection's credential. It may have expired or been revoked.", provider).
			WithRemedy("Create a new one and replace it in the connection's settings.")
	case status == http.StatusForbidden:
		return errs.Newf(errs.AdapterFailed,
			"%s accepted this connection's credential but it is not allowed to do this. It may be missing a scope or permission.", provider).
			WithRemedy("Give the credential permission to read repositories (and their metadata), then try again.")
	case status == http.StatusNotFound:
		return errs.Newf(errs.NotFound,
			"%s says this does not exist, or this connection's credential cannot see it.", provider)
	case status == http.StatusTooManyRequests:
		return errs.Newf(errs.AdapterUnavailable, "%s is limiting how fast this connection may call it. Try again in a few minutes.", provider)
	case status >= 500:
		return errs.Newf(errs.AdapterUnavailable, "%s answered %d. Try again shortly.", provider, status)
	default:
		return errs.Newf(errs.AdapterFailed, "%s answered %d.", provider, status)
	}
}

var (
	linkEntry = regexp.MustCompile(`<([^>]+)>([^<]*)`)
	relNext   = regexp.MustCompile(`;\s*rel="?([^";,]*\s)*next[\s"]?`)
)

// NextLink reads the rel="next" address out of a Link header. The address
// may itself hold commas — ?affiliation=owner,collaborator — so the header is
// read entry by entry from each "<", never split on ",".
func NextLink(header string) string {
	for _, m := range linkEntry.FindAllStringSubmatch(header, -1) {
		if relNext.MatchString(m[2] + " ") {
			return m[1]
		}
	}
	return ""
}

// Limit is the number of results a listing returns when the request does not
// say.
func Limit(n int) int {
	if n <= 0 || n > 1000 {
		return 100
	}
	return n
}

// Matches reports whether a repository's full name matches a listing query:
// every word in the query appears in it, without case.
func Matches(fullName, query string) bool {
	name := strings.ToLower(fullName)
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(name, w) {
			return false
		}
	}
	return true
}
