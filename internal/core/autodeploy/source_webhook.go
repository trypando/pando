package autodeploy

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// DeliverForSource takes a webhook a source connection's own webhook was sent
// — a GitHub App Pando created, which hears every repository it is installed
// on (R-142) — and starts a check of each app that deploys automatically from
// the repository it names and that the delivery concerns. As with an app's
// own webhook, the payload only says which apps to check now; each check
// reads the watched ref itself.
//
// A delivery that cannot be verified is refused the same way whatever the
// reason, so the endpoint does not tell a stranger which connections exist.
func (s *Service) DeliverForSource(ctx context.Context, sourceID string, d Delivery) (Outcome, error) {
	key, found, err := s.sourceSecret(ctx, sourceID)
	if err != nil {
		return Outcome{}, err
	}
	if !found || !signed(key, d) {
		return Outcome{}, errs.New(errs.AuthInvalid,
			"Pando could not verify this webhook. The signature does not match the source connection's webhook secret.").
			WithRemedy("The webhook belongs to the GitHub App Pando created for the connection. If the App's webhook secret was changed on GitHub, disconnect the connection and add it again.")
	}
	if d.Event == "ping" {
		return Outcome{Message: "Pando received the test delivery. Pushes and releases to the App's repositories will now start a check."}, nil
	}

	var payload struct {
		Repository struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
			SSHURL   string `json:"ssh_url"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	_ = json.Unmarshal(d.Body, &payload)
	r := payload.Repository
	names := map[string]bool{}
	for _, u := range []string{r.CloneURL, r.SSHURL, r.HTMLURL} {
		if k := repoKey(u); k != "" {
			names[k] = true
		}
	}
	if len(names) == 0 {
		return Outcome{Message: "This delivery names no repository, so Pando ignored it."}, nil
	}

	apps, err := s.Apps.WithAutoDeploy(ctx)
	if err != nil {
		return Outcome{}, err
	}
	checking := 0
	for _, app := range apps {
		if app.PinnedSpecID == "" {
			continue
		}
		pinned, found, err := s.Apps.RevisionByID(ctx, app.PinnedSpecID)
		if err != nil || !found || pinned.Body == nil {
			continue
		}
		src, ad := pinned.Body.Source, pinned.Body.Deploy.AutoDeploy
		if !ad.Enabled || !names[repoKey(src.URL)] || concerns(d, src, ad) != "" {
			continue
		}
		s.startCheck(ctx, app.ID)
		checking++
	}
	if checking == 0 {
		return Outcome{Message: "No app deploys automatically from " + r.FullName + " on this change, so Pando ignored this delivery."}, nil
	}
	return Outcome{Checking: true, Message: "Pando is checking the apps that deploy from " + r.FullName + " for something new to deploy."}, nil
}

func (s *Service) sourceSecret(ctx context.Context, sourceID string) (key secret.Value, found bool, err error) {
	if s.Sources == nil || sourceID == "" {
		return key, false, nil
	}
	return s.Sources.WebhookSecret(ctx, sourceID)
}

// repoKey is a repository address reduced to host and path, lower-cased, so
// the forms a git host gives one repository compare equal:
// https://github.com/Acme/API.git, git@github.com:acme/api.git and
// ssh://git@github.com/acme/api are all "github.com/acme/api".
func repoKey(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if !strings.Contains(addr, "://") {
		// scp-like: git@host:path
		at := strings.LastIndex(addr, "@")
		host, path, ok := strings.Cut(addr[at+1:], ":")
		if !ok {
			return ""
		}
		addr = "ssh://" + host + "/" + path
	}
	u, err := url.Parse(addr)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if path == "" {
		return ""
	}
	return strings.ToLower(u.Hostname() + "/" + path)
}
