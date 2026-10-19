package registrykit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// GitHub Container Registry does not delete through the registry API: a
// DELETE of a manifest is refused. Its images are package versions, deleted
// through GitHub's REST API (docs.github.com/en/rest/packages/packages) with
// the same token Pando pushes with, which needs delete:packages as well as
// write:packages.

// ghcrHost is the registry whose images are deleted through GitHub's API.
const ghcrHost = "ghcr.io"

// githubAPI is GitHub's REST API.
const githubAPI = "https://api.github.com"

// ghcrDeleteRemedy is what to change when GitHub refuses a delete.
const ghcrDeleteRemedy = "Give the token Pando signs in to ghcr.io with the delete:packages scope, " +
	"and admin access to the app's packages, then delete the app's packages in GitHub."

// packages is GitHub's REST API for one owner's container packages.
type packages struct {
	api   string
	token string
	owner string

	// base is where the owner's packages are: /orgs/<owner> or /user. Found
	// on the first answer, as an owner is one or the other.
	base string
}

type packageVersion struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"` // the manifest digest
	Metadata struct {
		Container struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

// ghcrPackage splits a repository on ghcr.io into its owner and its package
// name: ghcr.io/acme/pando/apps/app_1 is the package pando/apps/app_1 owned
// by acme.
func ghcrPackage(host, repository string) (owner, pkg string) {
	path := strings.TrimPrefix(repository, host+"/")
	owner, pkg, _ = strings.Cut(path, "/")
	return owner, pkg
}

// deleteGHCR deletes a deleted app's images on ghcr.io. A repository of the
// app's own (prefix empty) is a package of the app's own, deleted whole:
// GitHub refuses to delete a package's last tagged version, and the package's
// untagged versions — the platform images under an index — are the app's too.
// In one shared repository only the versions tagged for the app are deleted.
func (r *Registry) deleteGHCR(ctx context.Context, repository, prefix string) (int, error) {
	auth, err := r.PullAuth(ctx)
	if err != nil {
		return 0, err
	}
	if auth == nil || auth.Password.IsZero() {
		return 0, errs.New(errs.AdapterFailed,
			"Pando has no token for ghcr.io, and GitHub deletes a package only for a token that may.").
			WithRemedy("Set the image registry's username and a token with write:packages and delete:packages as its password.")
	}
	owner, pkg := ghcrPackage(r.host, repository)
	p := &packages{api: r.packagesAPI, token: auth.Password.Reveal(), owner: owner}

	versions, err := p.versions(ctx, pkg)
	if err != nil || len(versions) == 0 {
		return 0, err
	}
	if prefix == "" {
		if err := p.delete(ctx, "/packages/container/"+url.PathEscape(pkg)); err != nil {
			return 0, err
		}
		return len(versions), nil
	}
	deleted := 0
	for _, v := range versions {
		if !taggedFor(v.Metadata.Container.Tags, prefix) {
			continue
		}
		if err := p.delete(ctx, fmt.Sprintf("/packages/container/%s/versions/%d", url.PathEscape(pkg), v.ID)); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func taggedFor(tags []string, prefix string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// versions lists every version of a container package. None for a package
// that does not exist: an app that never pushed.
func (p *packages) versions(ctx context.Context, pkg string) ([]packageVersion, error) {
	var out []packageVersion
	// GitHub serves at most 100 a page and 10,000 in all.
	for page := 1; page <= 100; page++ {
		path := fmt.Sprintf("/packages/container/%s/versions?per_page=100&page=%d", url.PathEscape(pkg), page)
		var batch []packageVersion
		found, err := p.get(ctx, path, &batch)
		if err != nil || !found {
			return out, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// get reads one answer, finding where the owner's packages are on the first
// call: an organization's, or the token's own user's.
func (p *packages) get(ctx context.Context, path string, into any) (bool, error) {
	bases := []string{p.base}
	if p.base == "" {
		bases = []string{"/orgs/" + url.PathEscape(p.owner), "/user"}
	}
	for _, base := range bases {
		// The token's own packages only when the owner is the token's user:
		// another user's package of the same name is not this app's.
		if base == "/user" && p.base == "" && !p.tokenIsOwner(ctx) {
			continue
		}
		resp, err := p.do(ctx, http.MethodGet, base+path)
		if err != nil {
			return false, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			continue
		case resp.StatusCode != http.StatusOK:
			return false, p.refused(resp.StatusCode, "list")
		}
		p.base = base
		if err := json.Unmarshal(body, into); err != nil {
			return false, errs.Wrap(errs.AdapterFailed, "GitHub answered with a list of package versions Pando could not read.", err)
		}
		return true, nil
	}
	return false, nil
}

// tokenIsOwner reports whether the token signs in as the package owner.
func (p *packages) tokenIsOwner(ctx context.Context) bool {
	resp, err := p.do(ctx, http.MethodGet, "/user")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var user struct {
		Login string `json:"login"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user) != nil {
		return false
	}
	return strings.EqualFold(user.Login, p.owner)
}

func (p *packages) delete(ctx context.Context, path string) error {
	resp, err := p.do(ctx, http.MethodDelete, p.base+path)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	}
	return p.refused(resp.StatusCode, "delete")
}

func (p *packages) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.api+path, nil)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not build a request to GitHub's packages API.", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Pando could not reach GitHub's API to delete a deleted app's images from ghcr.io.", err).
			WithRemedy("Check that Pando can reach api.github.com.")
	}
	return resp, nil
}

func (p *packages) refused(status int, what string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errs.Newf(errs.AdapterFailed,
			"GitHub refused to %s a deleted app's images on ghcr.io (HTTP %d).", what, status).
			WithRemedy(ghcrDeleteRemedy)
	case http.StatusBadRequest:
		// A package's last tagged version, or a public one downloaded more
		// than 5,000 times: GitHub's rules, not the token's.
		return errs.Newf(errs.AdapterFailed,
			"GitHub would not %s a deleted app's image on ghcr.io (HTTP 400), which it refuses for a package's last tagged version and for a public one downloaded more than 5,000 times.", what).
			WithRemedy("Delete the app's images in the package's settings on GitHub.")
	}
	return errs.Newf(errs.AdapterFailed,
		"GitHub answered HTTP %d when Pando asked it to %s a deleted app's images on ghcr.io.", status, what).
		WithRemedy("Try again later, or delete the app's packages in GitHub.")
}

// ghcrScopesNote probes a classic token's scopes, which GitHub returns in
// X-OAuth-Scopes on any answer. A fine-grained token has no such header and
// says nothing here.
func (r *Registry) ghcrScopesNote(ctx context.Context) string {
	auth, err := r.PullAuth(ctx)
	if err != nil || auth == nil || auth.Password.IsZero() {
		return ""
	}
	p := &packages{api: r.packagesAPI, token: auth.Password.Reveal()}
	resp, err := p.do(ctx, http.MethodGet, "/user")
	if err != nil {
		return ""
	}
	_ = resp.Body.Close()
	scopes, present := resp.Header["X-Oauth-Scopes"]
	if !present {
		return ""
	}
	for _, s := range strings.Split(strings.Join(scopes, ","), ",") {
		if strings.TrimSpace(s) == "delete:packages" {
			return ""
		}
	}
	return "The token Pando signs in to ghcr.io with does not have the delete:packages scope, so a deleted app's images will stay on GitHub. Add the scope to the token."
}
