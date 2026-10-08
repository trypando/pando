package github

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// maxBranches caps a branch listing, so a repository with tens of thousands
// of branches does not turn one form into hundreds of API calls.
const maxBranches = 1000

// client is GitHub's REST API, authorized with token.
func (a *Adapter) client(token secret.Value) forgekit.Client {
	return forgekit.Client{
		HTTP:     a.http,
		Provider: provider,
		Authorize: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token.Reveal())
			r.Header.Set("Accept", "application/vnd.github+json")
			r.Header.Set("X-GitHub-Api-Version", apiVersion)
		},
	}
}

// apiToken is the token to call the API with. An OAuth token is used as
// stored: core calls Refresh and configures the connection again before a
// listing, so refreshing here would spend a refresh token core never learns
// the replacement for.
func (a *Adapter) apiToken(ctx context.Context) (secret.Value, error) {
	if !a.configured {
		return secret.Value{}, notConfigured()
	}
	switch a.cfg.Method {
	case forgekit.MethodSSH:
		return secret.Value{}, errs.New(errs.ValidInvalid,
			"A GitHub connection with a deploy key cannot list repositories or branches: a deploy key reads one repository over SSH and cannot call GitHub's API.").
			WithRemedy("Enter the repository's address instead, or connect with a GitHub App or a personal access token to pick from a list.")
	case forgekit.MethodApp:
		tok, _, err := a.installationToken(ctx)
		return tok, err
	case forgekit.MethodOAuth:
		tokens := forgekit.TokensFrom(a.cfg.Credentials)
		if tokens.Access.IsZero() {
			return secret.Value{}, errs.New(errs.StateInvalid, "This GitHub connection has not been authorized yet.").
				WithRemedy("Authorize it under Adapters in the console, or with pando source authorize.")
		}
		if !tokens.ExpiresAt.IsZero() && !a.now().Before(tokens.ExpiresAt) {
			return secret.Value{}, errs.Newf(errs.StateInvalid,
				"This GitHub connection's authorization expired at %s and was not renewed.",
				tokens.ExpiresAt.UTC().Format(time.RFC3339)).
				WithRemedy("Authorize the connection again under Adapters in the console, or with pando source authorize.")
		}
		return tokens.Access, nil
	default:
		return a.cfg.Credential(forgekit.FieldToken), nil
	}
}

type repo struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ListRepositories lists what the installation, user or token can read,
// limited to the connection's owner when it has one.
func (a *Adapter) ListRepositories(ctx context.Context, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	token, err := a.apiToken(ctx)
	if err != nil {
		return nil, err
	}
	c := a.client(token)
	limit := forgekit.Limit(req.Limit)

	next := a.apiURL + "/user/repos?per_page=100&affiliation=owner%2Ccollaborator%2Corganization_member"
	if a.cfg.Method == forgekit.MethodApp {
		next = a.apiURL + "/installation/repositories?per_page=100"
	}

	out := []api.Repository{}
	for next != "" && len(out) < limit {
		var page []repo
		if a.cfg.Method == forgekit.MethodApp {
			var wrapped struct {
				Repositories []repo `json:"repositories"`
			}
			next, err = c.Get(ctx, next, &wrapped)
			page = wrapped.Repositories
		} else {
			next, err = c.Get(ctx, next, &page)
		}
		if err != nil {
			return nil, err
		}
		for _, r := range page {
			if a.cfg.Scope != "" && !strings.EqualFold(ownerOf(r), a.cfg.Scope) {
				continue
			}
			if !forgekit.Matches(r.FullName, req.Query) {
				continue
			}
			out = append(out, api.Repository{
				URL:           r.CloneURL,
				FullName:      r.FullName,
				DefaultBranch: r.DefaultBranch,
				Private:       r.Private,
			})
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func ownerOf(r repo) string {
	if r.Owner.Login != "" {
		return r.Owner.Login
	}
	owner, _, _ := strings.Cut(r.FullName, "/")
	return owner
}

// ListBranches lists the repository's branches.
func (a *Adapter) ListBranches(ctx context.Context, repoURL string) ([]string, error) {
	if !a.configured {
		return nil, notConfigured()
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	seg := u.Segments()
	if len(seg) != 2 {
		return nil, errs.Newf(errs.ValidInvalid,
			"%q is not a GitHub repository address. One names an owner and a repository, such as https://%s/acme/api.", repoURL, a.cfg.Host)
	}
	token, err := a.apiToken(ctx)
	if err != nil {
		return nil, err
	}
	c := a.client(token)

	next := a.apiURL + "/repos/" + url.PathEscape(seg[0]) + "/" + url.PathEscape(seg[1]) + "/branches?per_page=100"
	out := []string{}
	for next != "" && len(out) < maxBranches {
		var page []struct {
			Name string `json:"name"`
		}
		next, err = c.Get(ctx, next, &page)
		if err != nil {
			return nil, err
		}
		for _, b := range page {
			out = append(out, b.Name)
		}
	}
	return out, nil
}
