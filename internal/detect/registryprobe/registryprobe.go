// Package registryprobe looks for images a project already publishes.
//
// R-094 tier 1, the top of the confidence ladder: an image the maintainers
// publish themselves is their own answer to "how is this built", already built
// and already shipped. Nothing Pando infers about the source beats it, and
// running it skips the build entirely.
package registryprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// Probe checks public registries for an image matching a source repository.
type Probe struct {
	// HTTP is the client used for registry calls. A short timeout on purpose:
	// this is a best-effort lookup on the path of every app anyone adds, and a
	// slow registry must not hold up detection.
	HTTP *http.Client

	// IncludeDockerHub turns on the Docker Hub half of R-094 tier 1. Off by
	// default, and the two registries are not equivalent:
	//
	// On ghcr.io a namespace belongs to the GitHub account of the same name.
	// Only the owner of github.com/acme can publish ghcr.io/acme/*, so finding
	// an image there really is the maintainers' own image.
	//
	// On Docker Hub the namespace is an unrelated account. Anyone may register
	// the Docker Hub user "acme" regardless of who owns github.com/acme, so a
	// match proves only that the names are the same. Defaulting this on would
	// mean Pando proposing to run a stranger's image, described as the
	// project's own — and proposals are reviewed by someone R-005 says may not
	// know what a container registry is.
	//
	// See docs/design/notes-registry-tier-namespaces.md.
	IncludeDockerHub bool
}

// New returns a probe with sensible timeouts.
func New() *Probe {
	return &Probe{HTTP: &http.Client{Timeout: 8 * time.Second}}
}

// Published reports images published for a source repository.
//
// Only under the same owner and name as the repository. On ghcr.io that is a
// real correspondence — the namespace belongs to the GitHub account of the same
// name — though it still does not prove the image was built from the commit
// being deployed, which is why the proposal goes through review before anything
// is pinned (R-098). Docker Hub has no such correspondence at all and is off
// unless IncludeDockerHub is set.
func (p *Probe) Published(ctx context.Context, src spec.Source) ([]detect.PublishedImage, error) {
	owner, name, ok := ownerAndName(src.URL)
	if !ok {
		return nil, nil
	}

	// A subdir means the app is one project inside a larger repository, and the
	// repository's own published image is not it.
	if strings.TrimSpace(src.Subdir) != "" {
		return nil, nil
	}

	var found []detect.PublishedImage
	if p.existsOnGHCR(ctx, owner, name) {
		found = append(found, detect.PublishedImage{
			Ref:      fmt.Sprintf("ghcr.io/%s/%s:latest", owner, name),
			Registry: "ghcr.io",
		})
	}
	if p.IncludeDockerHub && p.existsOnDockerHub(ctx, owner, name) {
		found = append(found, detect.PublishedImage{
			Ref:      fmt.Sprintf("docker.io/%s/%s:latest", owner, name),
			Registry: "docker.io",
		})
	}
	return found, nil
}

// ownerAndName pulls owner/name out of a git URL.
//
// Handles the https and ssh forms of the hosts people actually use. Anything
// else returns false, and tier 1 is simply skipped — an unrecognized URL is not
// an error, it is a repository Pando has no registry convention for.
func ownerAndName(raw string) (owner, name string, ok bool) {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	if raw == "" {
		return "", "", false
	}

	if after, found := strings.CutPrefix(raw, "git@"); found {
		// git@github.com:owner/name
		_, path, split := strings.Cut(after, ":")
		if !split {
			return "", "", false
		}
		return splitPath(path)
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	switch strings.ToLower(u.Host) {
	case "github.com", "www.github.com", "gitlab.com", "codeberg.org":
	default:
		return "", "", false
	}
	return splitPath(strings.TrimPrefix(u.Path, "/"))
}

func splitPath(path string) (owner, name string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), true
}

// existsOnGHCR checks the OCI registry API.
//
// Anonymous pulls need a token even for public images, so this is two calls:
// one for a token scoped to the repository, one for the manifest.
func (p *Probe) existsOnGHCR(ctx context.Context, owner, name string) bool {
	repo := owner + "/" + name

	token := p.ghcrToken(ctx, repo)
	if token == "" {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		fmt.Sprintf("https://ghcr.io/v2/%s/manifests/latest", repo), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// Without this, a multi-architecture image — which is most of them —
	// answers 404 rather than returning its index.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

func (p *Probe) ghcrToken(ctx context.Context, repo string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://ghcr.io/token?scope="+url.QueryEscape("repository:"+repo+":pull"), nil)
	if err != nil {
		return ""
	}

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.Token
}

// existsOnDockerHub checks Hub's own API, which answers without a token.
func (p *Probe) existsOnDockerHub(ctx context.Context, owner, name string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", owner, name), nil)
	if err != nil {
		return false
	}

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	// A repository can exist with no tags pushed, which is not an image.
	var body struct {
		IsPrivate bool `json:"is_private"`
		PullCount int  `json:"pull_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return !body.IsPrivate
}

var _ detect.RegistryProbe = (*Probe)(nil)
