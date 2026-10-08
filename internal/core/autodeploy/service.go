// Package autodeploy is the service behind an app's automatic deploy settings
// and its webhook (R-141, R-142).
//
// The settings live in the spec, so changing them writes a revision the next
// deploy ships, as every other edit does (R-152). What this adds is one place
// every client changes them through — the console, the CLI and MCP all call
// the same endpoints (R-261) — and the per-app webhook a git host calls to
// make the next check happen now rather than at the next poll.
//
// The webhook starts nothing a poll could not. It is verified, and then it
// asks the auto-deploy job to check the app (reconciler.AutoDeploy.CheckApp),
// which reads the watched ref itself and goes the ordinary way. What the
// request says about commits is never trusted: a forged payload naming a
// commit cannot ship it.
package autodeploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/specgate"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Checker checks one app for something new to deploy, now.
type Checker interface {
	CheckApp(ctx context.Context, appID, delivery string) error
}

// PolicyLoader reads host policy as it is now.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Service reads and changes auto-deploy settings, and takes webhooks.
type Service struct {
	Apps    *state.Apps
	Checks  *state.AutoDeployChecks
	Secrets *state.AutoDeploySecrets
	Policy  PolicyLoader

	// Authz is what the spec gate asks (R-158, R-184).
	Authz specgate.Authorizer

	// Checker is the auto-deploy job.
	Checker Checker

	Audit  func(ctx context.Context, e audit.Event)
	Logger *zap.Logger
}

// View is an app's auto-deploy as the API shows it.
type View struct {
	// Settings are the newest revision's: what the next deploy ships.
	Settings spec.AutoDeploy `json:"settings"`

	// Deployed are the pinned revision's: what is in effect now.
	Deployed spec.AutoDeploy `json:"deployed"`

	// Pending is whether Settings are saved and not yet deployed.
	Pending bool `json:"pending"`

	// Paused is whether approval now stops an app that auto-deploys (R-158).
	Paused bool `json:"paused"`

	// LastCheck is the last time auto-deploy looked, or nil if it has not.
	LastCheck *state.AutoDeployCheck `json:"last_check"`

	// WebhookSecretSet is whether the app has a webhook secret, and so
	// whether its webhook does anything. The secret itself is shown once,
	// when it is made.
	WebhookSecretSet bool `json:"webhook_secret_set"`

	// WebhookURL is where the git host sends webhooks. The HTTP layer fills
	// it in, since only it knows the address Pando is reached at.
	WebhookURL string `json:"webhook_url"`
}

// Get is an app's auto-deploy settings, last check and webhook.
func (s *Service) Get(ctx context.Context, app state.App) (View, error) {
	var v View
	pinned, newest, err := s.revisions(ctx, app)
	if err != nil {
		return v, err
	}
	if pinned.Body != nil {
		v.Deployed = pinned.Body.Deploy.AutoDeploy
		v.Settings = v.Deployed
		if v.Deployed.Enabled {
			doc, err := s.Policy.Load(ctx)
			if err != nil {
				return v, err
			}
			v.Paused = approval.BlocksAutoDeploy(doc, app.ID, pinned.Body)
		}
	}
	if newest.Body != nil {
		v.Settings = newest.Body.Deploy.AutoDeploy
		v.Pending = v.Settings != v.Deployed
	}
	check, found, err := s.Checks.ByApp(ctx, app.ID)
	if err != nil {
		return v, err
	}
	if found {
		v.LastCheck = &check
	}
	if v.WebhookSecretSet, err = s.Secrets.Has(ctx, app.ID); err != nil {
		return v, err
	}
	return v, nil
}

// Saved is the outcome of Set.
type Saved struct {
	Changed  bool   `json:"changed"`
	Revision int    `json:"revision,omitempty"`
	SpecID   string `json:"spec_id,omitempty"`
}

// Set changes an app's auto-deploy settings: a new revision built on the
// newest one, which the next deploy ships (R-152). The same validation and
// the same gate as any spec save, so a setting refused there is refused here
// in the same words (R-158).
func (s *Service) Set(ctx context.Context, p authz.Principal, app state.App, ad spec.AutoDeploy) (Saved, error) {
	pinned, newest, err := s.revisions(ctx, app)
	if err != nil {
		return Saved{}, err
	}
	base := newest
	if base.Body == nil {
		return Saved{}, errs.New(errs.ValidInvalid, "This app isn't configured yet, so it has no deploy settings to change.").
			WithRemedy("Accept its setup first, then turn on automatic deploys.")
	}
	if ad.Enabled && ad.Trigger == "" {
		ad.Trigger = spec.TriggerBranchUpdated
	}
	if base.Body.Deploy.AutoDeploy == ad {
		return Saved{Changed: false, Revision: base.Revision, SpecID: base.ID}, nil
	}

	next := *base.Body
	next.Deploy.AutoDeploy = ad
	if err := spec.Validate(&next); err != nil {
		return Saved{}, err
	}
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return Saved{}, err
	}
	if _, err := specgate.Check(ctx, s.Authz, p, specgate.Change{
		AppID: app.ID, Policy: doc, Pinned: pinned.Body, Next: &next,
	}); err != nil {
		return Saved{}, err
	}

	rev, err := s.Apps.CreateRevision(ctx, app.ID, &next, spec.OriginEdited, p.ID)
	if err != nil {
		return Saved{}, err
	}
	s.audit(ctx, p, "app.auto_deploy.configure", app.ID, map[string]any{
		"enabled": ad.Enabled, "trigger": string(ad.Trigger), "branch": ad.Branch,
		"tag_pattern": ad.TagPattern, "revision": rev.Revision,
	})
	return Saved{Changed: true, Revision: rev.Revision, SpecID: rev.ID}, nil
}

// Secret is a webhook secret, shown once (R-194 holds everywhere else: the
// one response that carries it marshals it in the clear, on purpose).
type Secret struct{ v secret.Value }

func (k Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + k.v.Reveal() + `"`), nil
}

// Reveal returns the secret, for tests.
func (k Secret) Reveal() string { return k.v.Reveal() }

// secretPrefix marks the secret, so one pasted into the wrong place is
// recognizable.
const secretPrefix = "pdwh_"

// RotateSecret makes the app a new webhook secret, replacing any it had, and
// returns it — the only time it is shown. The old one stops verifying at once.
func (s *Service) RotateSecret(ctx context.Context, p authz.Principal, app state.App) (Secret, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Secret{}, errs.Wrap(errs.Internal, "Could not make a webhook secret.", err)
	}
	// URL-safe base64 has no characters a JSON string must escape, so
	// MarshalJSON can quote it as it is.
	v := secret.New(secretPrefix + base64.RawURLEncoding.EncodeToString(b))
	s.audit(ctx, p, "app.auto_deploy.webhook_secret", app.ID, map[string]any{"change": "rotate"})
	if err := s.Secrets.Put(ctx, app.ID, v); err != nil {
		return Secret{}, err
	}
	return Secret{v}, nil
}

// RemoveSecret removes the app's webhook secret, which turns its webhook off.
// Polling carries on.
func (s *Service) RemoveSecret(ctx context.Context, p authz.Principal, app state.App) error {
	s.audit(ctx, p, "app.auto_deploy.webhook_secret", app.ID, map[string]any{"change": "remove"})
	return s.Secrets.Delete(ctx, app.ID)
}

// revisions are the app's pinned revision and its newest, either zero when
// there is none.
func (s *Service) revisions(ctx context.Context, app state.App) (pinned, newest state.Revision, err error) {
	if app.PinnedSpecID != "" {
		if pinned, _, err = s.Apps.RevisionByID(ctx, app.PinnedSpecID); err != nil {
			return
		}
	}
	newest = pinned
	list, err := s.Apps.ListRevisions(ctx, app.ID)
	if err != nil || len(list) == 0 || list[0].ID == pinned.ID || list[0].Revision < pinned.Revision {
		return
	}
	latest, found, err := s.Apps.RevisionByID(ctx, list[0].ID)
	if err == nil && found {
		newest = latest
	}
	return
}

func (s *Service) audit(ctx context.Context, p authz.Principal, action, appID string, detail map[string]any) {
	if s.Audit == nil {
		return
	}
	s.Audit(ctx, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        action,
		AppID:         appID,
		TargetKind:    "app",
		TargetID:      appID,
		Detail:        detail,
	})
}
