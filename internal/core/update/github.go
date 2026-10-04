package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ReleasesURL is where the release workflow publishes. A renamed repository
// redirects, and the client follows.
const ReleasesURL = "https://api.github.com/repos/trypando/pando/releases?per_page=100"

// GitHub lists releases from GitHub's API.
//
// The request carries a User-Agent naming the version, which GitHub requires,
// and nothing else about the installation (R-349).
type GitHub struct {
	HTTP    *http.Client
	URL     string
	Version string
}

// NewGitHub is a GitHub source with the timeouts an unattended outbound call
// needs: without them a black-holed route holds the check open forever.
func NewGitHub(version string) *GitHub {
	return &GitHub{
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
			},
		},
		URL:     ReleasesURL,
		Version: version,
	}
}

// maxBody bounds what a response may make the server hold: a hundred releases'
// notes are a few hundred kilobytes.
const maxBody = 8 << 20

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Body        string    `json:"body"`
}

// Releases implements Source.
func (g *GitHub) Releases(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pando/"+strings.TrimPrefix(g.Version, "v"))

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}

	var raw []githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("GitHub's release list did not parse: %w", err)
	}
	out := make([]Release, 0, len(raw))
	for _, r := range raw {
		if r.Draft {
			continue
		}
		out = append(out, Release{
			Version:     strings.TrimPrefix(r.TagName, "v"),
			Prerelease:  r.Prerelease,
			PublishedAt: r.PublishedAt.UTC(),
			URL:         r.HTMLURL,
			Notes:       r.Body,
			Security:    HasSecurityFix(r.Body),
		})
	}
	return out, nil
}
