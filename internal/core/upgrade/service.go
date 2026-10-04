package upgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// Defaults for R-359's [P] values.
const (
	DefaultReadyTimeout = 5 * time.Minute
	DefaultSoak         = 24 * time.Hour
	// A running outcome older than this is a helper that died: it no longer
	// blocks another upgrade.
	staleRunning = 30 * time.Minute
)

// DatabaseURLEnv carries the owner's database URL to the helper. Not a
// PANDO_ variable, which the helper would read as configuration.
const DatabaseURLEnv = "UPGRADE_DATABASE_URL"

// Service is the in-place upgrade (R-355 – R-362).
type Service struct {
	Version string
	Updates interface {
		Status(ctx context.Context) (update.Status, error)
	}
	// Verify checks the target version's image signature and returns its
	// digest (R-357).
	Verify func(ctx context.Context, version string) (string, error)
	// Runtime is the runtime Pando runs on, with its capabilities.
	Runtime func(ctx context.Context) (api.RuntimeAdapter, error)
	Policy  func(ctx context.Context) (policy.Document, error)
	// CanCopy checks the database account may take the rollback copy.
	CanCopy func(ctx context.Context) error
	// Backup takes and records a full backup, returning its ID (R-358).
	Backup func(ctx context.Context, p authz.Principal, passphrase secret.Value) (string, error)
	// Recipients are the people holding install.upgrade (R-362).
	Recipients func(ctx context.Context) ([]string, error)
	// DropSnapshot removes the rollback copy after the soak.
	DropSnapshot func(ctx context.Context) error
	Notify       func(ctx context.Context, n api.Notification) error
	Audit        func(ctx context.Context, e audit.Event)

	DatabaseURL  secret.Value
	WorkDir      string
	Port         string
	ReadyTimeout time.Duration
	Soak         time.Duration
	Clock        clock.Clock
	Logger       *zap.Logger
}

// Plan is what an upgrade to a version would do, or every reason it cannot.
type Plan struct {
	Current  string `json:"current"`
	Target   string `json:"target"`
	Possible bool   `json:"possible"`
	// Reasons say why it is not possible, each with what to change (R-105).
	Reasons []string `json:"reasons"`
	Image   string   `json:"image,omitempty"`
	Tag     string   `json:"tag,omitempty"`
	// Breaking are the versions in between that may break what works now
	// (R-360), with their notes. Non-empty means the target version must be
	// typed to confirm.
	Breaking []update.Release `json:"breaking"`
	// Unavailable while it lasts: every app, because the proxy is in Pando.
	Note string `json:"note"`
}

// Request starts an upgrade.
type Request struct {
	Version string `json:"version"`
	// ConfirmBreaking is the target version, typed, when Plan.Breaking is not
	// empty (R-360).
	ConfirmBreaking string `json:"confirm_breaking,omitempty"`
	// Passphrase encrypts the full backup taken first (R-358).
	Passphrase secret.Value `json:"passphrase"`
	// SkipBackup says, explicitly, to go ahead without one.
	SkipBackup bool `json:"skip_backup,omitempty"`
	// Automatic is the schedule's (R-361): no backup, only the copy.
	Automatic bool `json:"-"`
}

const outageNote = "Every app is unreachable while Pando restarts, usually for under a minute, because every request to an app passes through Pando (R-023). Apps keep running."

// PlanFor reports what upgrading to version would do.
func (s *Service) PlanFor(ctx context.Context, version string) (Plan, error) {
	version = strings.TrimPrefix(version, "v")
	p := Plan{Current: strings.TrimPrefix(s.Version, "v"), Target: version, Reasons: []string{}, Breaking: []update.Release{}, Note: outageNote}
	no := func(r string) { p.Reasons = append(p.Reasons, r) }

	doc, err := s.Policy(ctx)
	if err != nil {
		return Plan{}, err
	}
	if !doc.UpgradeInPlace {
		no("In-place upgrades are off. Turn on \"Let Pando upgrade itself\" on the Policy screen, or set PANDO_POLICY_UPGRADE_IN_PLACE=true where Pando is deployed, which also locks it there.")
	}

	st, err := s.Updates.Status(ctx)
	if err != nil {
		return Plan{}, err
	}
	var target *update.Release
	for i, r := range st.Releases {
		if r.Version == version {
			target = &st.Releases[i]
		}
	}
	switch {
	case st.Development:
		no("This is a development build, which has no version to upgrade from. Run a released image to upgrade in place.")
	case target == nil:
		no(fmt.Sprintf("%s is not a release newer than %s on the %s channel, as of the last update check.", version, p.Current, st.Channel))
	default:
		for _, r := range st.Releases {
			if r.Breaking && semver.Compare("v"+r.Version, "v"+version) <= 0 {
				p.Breaking = append(p.Breaking, r)
			}
		}
	}

	rt, err := s.Runtime(ctx)
	if err != nil {
		no("Pando could not reach its runtime to check how it is running: " + err.Error())
		return s.finishPlan(p), nil
	}
	caps, err := rt.Capabilities(ctx)
	if err != nil {
		no("Pando could not read its runtime's capabilities: " + err.Error())
		return s.finishPlan(p), nil
	}
	up, ok := rt.(api.SelfUpgrader)
	if !caps.SupportsSelfUpgrade || !ok {
		no("Pando is not running in a container its runtime manages, so it cannot replace itself. Upgrade by replacing the pando binary with the new release's.")
		return s.finishPlan(p), nil
	}
	self, err := up.Self(ctx)
	if err != nil {
		no(errs.As(err).Message)
		return s.finishPlan(p), nil
	}
	p.Image = self.Image
	if target != nil {
		tag, reason := MovingTag(self.Image, version)
		if reason != "" {
			no(reason)
		}
		p.Tag = tag
	}
	if err := s.CanCopy(ctx); err != nil {
		no(errs.As(err).Message + " " + errs.As(err).Remedy)
	}
	if o, _ := ReadOutcome(OutcomePath(s.WorkDir)); o != nil && o.State == StateRunning && s.Clock.Since(o.StartedAt) < staleRunning {
		no(fmt.Sprintf("An upgrade to %s is already under way, started %s.", o.To, o.StartedAt.Format(time.RFC3339)))
	}
	return s.finishPlan(p), nil
}

func (s *Service) finishPlan(p Plan) Plan {
	p.Possible = len(p.Reasons) == 0
	return p
}

// Start begins an upgrade and returns once the helper is running; the helper
// stops this process shortly after (R-359).
func (s *Service) Start(ctx context.Context, p authz.Principal, req Request) (Outcome, error) {
	plan, err := s.PlanFor(ctx, req.Version)
	if err != nil {
		return Outcome{}, err
	}
	if !plan.Possible {
		return Outcome{}, errs.New(errs.ValidInvalid, fmt.Sprintf("Pando cannot upgrade itself to %s: %s", plan.Target, strings.Join(plan.Reasons, " "))).
			WithDetail("reasons", plan.Reasons)
	}
	if len(plan.Breaking) > 0 && strings.TrimPrefix(req.ConfirmBreaking, "v") != plan.Target {
		versions := make([]string, len(plan.Breaking))
		for i, r := range plan.Breaking {
			versions[i] = r.Version
		}
		return Outcome{}, errs.New(errs.ValidInvalid,
			fmt.Sprintf("Upgrading to %s crosses %s, which may break something that works now (R-360).", plan.Target, strings.Join(versions, ", "))).
			WithRemedy(fmt.Sprintf("Read the Upgrade notes of each, then confirm by sending confirm_breaking: %q.", plan.Target)).
			WithDetail("breaking", versions)
	}

	o := Outcome{
		ID: id.New(id.Upgrade), From: plan.Current, To: plan.Target, Tag: plan.Tag,
		StartedAt: s.Clock.Now(), StartedBy: p.ID, Automatic: req.Automatic, State: StateRunning,
	}
	event := func(action string, detail map[string]any) {
		s.Audit(ctx, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: action, TargetKind: "upgrade", TargetID: o.ID, Detail: detail,
		})
	}

	// Verified before anything else (R-357): a signature that does not check
	// stops the upgrade before a backup is spent on it.
	digest, err := s.Verify(ctx, plan.Target)
	if err != nil {
		event("upgrade.refused", map[string]any{"from": o.From, "to": o.To, "reason": "signature"})
		return Outcome{}, errs.Wrap(errs.ValidInvalid,
			fmt.Sprintf("Pando %s's image did not verify as signed by Pando's release workflow, so Pando will not run it: %s", plan.Target, err), err)
	}
	o.Image = Repository + "@" + digest

	switch {
	case req.Automatic:
		// Nobody is here to supply a passphrase (R-361); the copy is the
		// rollback.
	case req.SkipBackup:
		o.SkipBackup = true
		event("upgrade.backup_skipped", map[string]any{"from": o.From, "to": o.To})
	default:
		if req.Passphrase.Reveal() == "" {
			return Outcome{}, errs.New(errs.ValidInvalid, "An upgrade takes a full backup first, and it needs a passphrase, which Pando does not keep.").
				WithRemedy("Send a passphrase, or skip the backup explicitly with skip_backup: true.")
		}
		backupID, err := s.Backup(ctx, p, req.Passphrase)
		if err != nil {
			event("upgrade.refused", map[string]any{"from": o.From, "to": o.To, "reason": "backup"})
			return Outcome{}, errs.Wrap(errs.As(err).Code,
				"The backup taken before the upgrade failed, so Pando did not upgrade: "+errs.As(err).Message, err).
				WithRemedy("Fix the backup and try again, or skip it explicitly.")
		}
		o.BackupID = backupID
	}

	rt, err := s.Runtime(ctx)
	if err != nil {
		return Outcome{}, err
	}
	up := rt.(api.SelfUpgrader) // PlanFor checked SupportsSelfUpgrade
	self, err := up.Self(ctx)
	if err != nil {
		return Outcome{}, err
	}
	if err := up.PullImage(ctx, o.Image); err != nil {
		return Outcome{}, err
	}

	path := OutcomePath(s.WorkDir)
	if err := WriteOutcome(path, o); err != nil {
		return Outcome{}, errs.Wrap(errs.Internal, "Could not record the upgrade before starting it.", err)
	}
	if _, err := up.StartHelper(ctx, api.HelperSpec{
		Args: []string{"upgrade-helper",
			"--container", self.ID,
			"--outcome", path,
			"--port", s.port(),
			"--ready-timeout", s.readyTimeout().String(),
		},
		Env: map[string]secret.Value{DatabaseURLEnv: s.DatabaseURL},
	}); err != nil {
		o.State, o.Reason, o.FinishedAt = StateFailed, "The upgrade helper did not start, so nothing was changed: "+err.Error(), s.Clock.Now()
		o.Recorded = true
		_ = WriteOutcome(path, o)
		event("upgrade.failed", map[string]any{"from": o.From, "to": o.To, "reason": o.Reason})
		return o, err
	}
	event("upgrade.start", map[string]any{
		"from": o.From, "to": o.To, "image": o.Image, "automatic": o.Automatic,
		"backup_id": o.BackupID, "backup_skipped": o.SkipBackup,
	})
	return o, nil
}

// Last is the most recent upgrade's outcome, or nil.
func (s *Service) Last(context.Context) (*Outcome, error) {
	return ReadOutcome(OutcomePath(s.WorkDir))
}

func (s *Service) port() string {
	if s.Port == "" {
		return "8080"
	}
	return s.Port
}

func (s *Service) readyTimeout() time.Duration {
	if s.ReadyTimeout <= 0 {
		return DefaultReadyTimeout
	}
	return s.ReadyTimeout
}

func (s *Service) soak() time.Duration {
	if s.Soak <= 0 {
		return DefaultSoak
	}
	return s.Soak
}
