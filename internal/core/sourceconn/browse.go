package sourceconn

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// ListRepositories lists the repositories a connection can read, for a
// person to pick one rather than type its address. Repositories the source
// allowlist refuses are left out (R-092): offering one that cannot be added is
// offering an error.
func (s *Service) ListRepositories(ctx context.Context, id string, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	c, err := s.usable(ctx, id)
	if err != nil {
		return nil, err
	}
	if !c.Capabilities.ListRepositories {
		return nil, errs.Newf(errs.ValidInvalid,
			"The source connection %q cannot list repositories: it signs in with a %s, which has no access to the host's API.",
			c.Name, methodName(c.Capabilities.Method)).
			WithRemedy("Enter the repository's address instead.")
	}
	repos, err := c.adapter.ListRepositories(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]api.Repository, 0, len(repos))
	for _, r := range repos {
		if s.Policy != nil {
			if err := s.Policy.AllowsSource(ctx, spec.Source{Type: spec.SourceGit, URL: r.URL}); err != nil {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ListBranches lists a repository's branches through a connection.
func (s *Service) ListBranches(ctx context.Context, id, repoURL string) ([]string, error) {
	if repoURL == "" {
		return nil, errs.New(errs.ValidInvalid, "Say which repository's branches to list.").
			WithRemedy("Pass the repository's address as url.")
	}
	c, err := s.usable(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.adapter.Covers(repoURL) == 0 {
		return nil, errs.Newf(errs.ValidInvalid,
			"The source connection %q is not for %s.", c.Name, repoURL).
			WithRemedy("Choose the connection for the host and owner the repository is on.")
	}
	if s.Policy != nil {
		if err := s.Policy.AllowsSource(ctx, spec.Source{Type: spec.SourceGit, URL: repoURL}); err != nil {
			return nil, err
		}
	}
	if !c.Capabilities.ListRepositories {
		return nil, errs.Newf(errs.ValidInvalid,
			"The source connection %q cannot list branches: it signs in with a %s, which has no access to the host's API.",
			c.Name, methodName(c.Capabilities.Method)).
			WithRemedy("Enter the branch's name instead.")
	}
	return c.adapter.ListBranches(ctx, repoURL)
}

func (s *Service) usable(ctx context.Context, id string) (Connection, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	if !c.Usable() {
		reason := c.Problem
		if reason == "" {
			reason = "It has not been authorized yet."
		}
		return Connection{}, errs.Newf(errs.StateInvalid,
			"The source connection %q cannot be used. %s", c.Name, reason).
			WithRemedy("Fix or authorize it under Adapters in the console.")
	}
	// A token that has expired is renewed and stored before the listing,
	// and the connection is built again with it, rather than the adapter
	// refreshing on its own and the new token being lost (api.SourceAdapter.Refresh).
	rotated, err := c.adapter.Refresh(ctx)
	if err != nil {
		return Connection{}, err
	}
	if len(rotated) == 0 || !s.stored(c.ID) {
		return c, nil
	}
	for field, v := range rotated {
		if err := s.Credentials.Put(ctx, c.ID, field, v); err != nil {
			return Connection{}, err
		}
	}
	return s.Get(ctx, id)
}

func methodName(m string) string {
	switch m {
	case "ssh":
		return "SSH key"
	case "token":
		return "token"
	default:
		return m
	}
}
