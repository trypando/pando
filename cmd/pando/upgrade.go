package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/subscription"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// upgradeDeps is what serve already holds that the upgrade needs.
type upgradeDeps struct {
	cfg        *config.Config
	updates    *update.Checker
	registry   *adapterapi.Registry
	policy     func(context.Context) (policy.Document, error)
	backups    *state.Backups
	backup     *backup.Service
	authzStore *state.AuthzStore
	auditor    *audit.Writer
	notify     subscription.Router
	logger     *zap.Logger
}

// newUpgradeService wires the in-place upgrade (R-355 – R-362).
func newUpgradeService(d upgradeDeps) *upgrade.Service {
	sigstoreDir := filepath.Join(d.cfg.Server.WorkDir, "sigstore")
	verifier := &update.ImageVerifier{
		Repository: "index.docker.io/" + update.Image,
		Trusted: func(ctx context.Context) (root.TrustedMaterial, error) {
			return update.TrustedRoot(ctx, sigstoreDir)
		},
		Remote: []remote.Option{remote.WithUserAgent("pando/" + buildVersion)},
	}
	write := func(ctx context.Context, e audit.Event) {
		if err := d.auditor.Write(ctx, e); err != nil {
			d.logger.Error("could not write an audit event", zap.String("action", e.Action), zap.Error(err))
		}
	}
	_, port, _ := net.SplitHostPort(d.cfg.Server.Addr)

	return &upgrade.Service{
		Version: buildVersion,
		Updates: d.updates,
		Verify:  verifier.Verify,
		Runtime: func(context.Context) (adapterapi.RuntimeAdapter, error) {
			if ref, ok := d.registry.Default(adapterapi.CategoryRuntime); ok {
				if rt, ok := d.registry.Runtime(ref); ok {
					return rt, nil
				}
			}
			return nil, errors.New("no runtime adapter is configured")
		},
		Policy:  d.policy,
		CanCopy: func(ctx context.Context) error { return state.CanCopyDatabase(ctx, d.cfg.Database.URL) },
		// The same steps as POST /backups: the bundle, its record, its
		// audit event (R-358).
		Backup: func(ctx context.Context, p authz.Principal, passphrase secret.Value) (string, error) {
			id := d.backups.NewID()
			created, err := d.backup.Create(ctx, id, backup.CreateRequest{Passphrase: passphrase})
			if err != nil {
				write(ctx, audit.Event{PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
					Action: "backup.failed", TargetKind: "backup", TargetID: id})
				return "", err
			}
			if err := d.backups.Record(ctx, state.Backup{
				ID: id, Kind: "dr_bundle", AdapterRef: created.AdapterRef, ObjectName: created.ObjectName,
				SizeBytes: created.SizeBytes, Manifest: created.Manifest, RetainUntil: created.RetainUntil, CreatedBy: p.ID,
			}); err != nil {
				return "", err
			}
			write(ctx, audit.Event{PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
				Action: "backup.create", TargetKind: "backup", TargetID: id,
				Detail: map[string]any{"destination": created.AdapterRef, "size_bytes": created.SizeBytes, "reason": "upgrade"}})
			return id, nil
		},
		Recipients: func(ctx context.Context) ([]string, error) {
			return d.authzStore.InstallVerbHolders(ctx, authz.InstallUpgrade)
		},
		DropSnapshot: func(ctx context.Context) error { return state.DropSnapshot(ctx, d.cfg.Database.URL) },
		Notify:       d.notify.Notify,
		Audit:        write,
		DatabaseURL:  secret.New(d.cfg.Database.URL),
		WorkDir:      d.cfg.Server.WorkDir,
		Port:         port,
		Clock:        clock.System{},
		Logger:       d.logger,
	}
}

// upgradeHelperCmd is the helper an in-place upgrade starts beside Pando
// (R-359). Hidden: nobody runs it by hand, and it does nothing useful without
// the outcome file Pando wrote before starting it.
func upgradeHelperCmd() *cobra.Command {
	var containerID, outcome, port string
	var readyTimeout time.Duration
	cmd := &cobra.Command{
		Use:    "upgrade-helper",
		Short:  "Replace Pando's container with a newer release (started by an in-place upgrade)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dbURL := os.Getenv(upgrade.DatabaseURLEnv)
			if dbURL == "" {
				return errs.New(errs.ValidInvalid, "The upgrade helper needs "+upgrade.DatabaseURLEnv+".")
			}
			r, err := docker.NewReplacer()
			if err != nil {
				return err
			}
			old, err := r.Inspect(ctx, containerID)
			if err != nil {
				return err
			}
			o := upgrade.RunHelper(ctx, outcome,
				&dockerSwap{r: r, old: old, port: port},
				snapshotDB{url: dbURL},
				readyTimeout, clock.System{})
			_, _ = cmd.OutOrStdout().Write([]byte(o.State + " " + o.Reason + "\n"))
			return nil
		},
	}
	cmd.Flags().StringVar(&containerID, "container", "", "the container to replace")
	cmd.Flags().StringVar(&outcome, "outcome", "", "where the upgrade is recorded")
	cmd.Flags().StringVar(&port, "port", "8080", "the port Pando listens on inside its container")
	cmd.Flags().DurationVar(&readyTimeout, "ready-timeout", upgrade.DefaultReadyTimeout, "how long the new version has to report ready")
	return cmd
}

// dockerSwap is upgrade.Swap over the Docker adapter's Replacer.
type dockerSwap struct {
	r     *docker.Replacer
	old   docker.Old
	port  string
	newID string
}

func (s *dockerSwap) Stop(ctx context.Context) error { return s.r.Stop(ctx, s.old) }
func (s *dockerSwap) Recreate(ctx context.Context, image string) error {
	id, err := s.r.Recreate(ctx, s.old, image)
	s.newID = id
	return err
}
func (s *dockerSwap) Ready(ctx context.Context, timeout time.Duration) error {
	return s.r.Ready(ctx, s.newID, s.port, timeout)
}
func (s *dockerSwap) Logs(ctx context.Context) string {
	if s.newID == "" {
		return ""
	}
	return s.r.Logs(ctx, s.newID)
}
func (s *dockerSwap) Discard(ctx context.Context)       { s.r.Discard(ctx, s.newID) }
func (s *dockerSwap) Restore(ctx context.Context) error { return s.r.Restore(ctx, s.old) }
func (s *dockerSwap) Finish(ctx context.Context, image, tag string) error {
	return s.r.Finish(ctx, s.old, image, tag)
}

// snapshotDB is upgrade.Database over the state store's copy.
type snapshotDB struct{ url string }

func (d snapshotDB) Snapshot(ctx context.Context) error { return state.Snapshot(ctx, d.url) }
func (d snapshotDB) Restore(ctx context.Context) error  { return state.RestoreSnapshot(ctx, d.url) }
