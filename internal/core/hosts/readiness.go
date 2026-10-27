package hosts

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// What an install needs before a second host joins (R-417): nothing a host
// must share may live only on one host's disk. Uploaded sources are in the
// database and keys are handed over at join; what is left is what the
// operator chooses, and is checked here.

// BackupDestinations is the slice of *api.Registry the check reads.
type BackupDestinations interface {
	Default(c api.Category) (string, bool)
	Backup(ref string) (api.BackupAdapter, bool)
}

// PolicyLoader reads host policy as it is now.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// RoutingAdapters is the slice of *api.Registry the certificate check reads.
type RoutingAdapters interface {
	ByCategory(c api.Category) []string
	Routing(ref string) (api.RoutingAdapter, bool)
}

// Readiness checks an install is ready for a second host to join.
type Readiness struct {
	backups  BackupDestinations
	policy   PolicyLoader
	registry planner.InstallRegistries

	// Routing is the adapter registry, for the check that a second control
	// host's edge can share certificates with this one's (O-68). Nil skips
	// it.
	Routing RoutingAdapters
}

// NewReadiness returns the check. backups is the adapter registry, policy the
// host policy store, and registry the install's image registry
// (*imageregistry.Service).
func NewReadiness(backups BackupDestinations, policy PolicyLoader, registry planner.InstallRegistries) *Readiness {
	return &Readiness{backups: backups, policy: policy, registry: registry}
}

// problem is one reason the install is not ready, with what fixes it.
type problem struct{ message, remedy string }

// ReadyToJoin refuses a join, whatever its roles, while backups, audit
// archives or app images would stay on this host alone (R-417), and a
// control host's while its edge could not share this one's certificates
// (O-68). Every reason is said at once, so one round of changes is enough.
func (r *Readiness) ReadyToJoin(ctx context.Context, roles []string) error {
	var found []problem
	add := func(p *problem) {
		if p != nil {
			found = append(found, *p)
		}
	}

	add(r.defaultBackup())

	doc, err := r.policy.Load(ctx)
	if err != nil {
		return err
	}
	add(r.auditArchives(doc))

	reg, err := r.registry.CurrentRegistry(ctx)
	if err != nil {
		return err
	}
	if reg == nil || !reg.Configured() {
		add(&problem{
			message: "This install has no image registry, and a host other than this one receives app images only through one.",
			remedy:  "Set PANDO_REGISTRY_URL, with PANDO_REGISTRY_USERNAME and PANDO_REGISTRY_PASSWORD, where Pando is deployed, or configure an image registry adapter.",
		})
	}
	if len(roles) == 0 || slices.Contains(roles, state.HostRoleControl) {
		edges, err := r.edgeCertificates(ctx)
		if err != nil {
			return err
		}
		found = append(found, edges...)
	}
	return notReady(found)
}

// edgeCertificates refuses a routing adapter whose edge orders its own
// certificates where Pando cannot order them instead (R-254: a capability,
// not a kind). A Traefik Pando runs is not one: in a cluster Pando's leader
// orders each certificate once and every control host's edge serves it.
func (r *Readiness) edgeCertificates(ctx context.Context) ([]problem, error) {
	if r.Routing == nil {
		return nil, nil
	}
	refs := r.Routing.ByCategory(api.CategoryRouting)
	sort.Strings(refs)
	var found []problem
	for _, ref := range refs {
		routing, ok := r.Routing.Routing(ref)
		if !ok {
			continue
		}
		caps, err := routing.Capabilities(ctx)
		if err != nil {
			return nil, err
		}
		if caps.CertificatesPerEdge {
			found = append(found, problem{
				message: fmt.Sprintf("The routing adapter %q uses a Traefik that orders its own certificates, so each control host's Traefik would order every hostname's certificate and fail the challenges another host receives.", ref),
				remedy:  fmt.Sprintf("Let Pando run Traefik for %q, so Pando orders each certificate once and every control host serves it, or clear its certificate resolver and supply certificates to Traefik yourself.", ref),
			})
		}
	}
	return found, nil
}

const onHostDisk = "keeps them on this host's disk, which is lost with the host"

func (r *Readiness) defaultBackup() *problem {
	ref, ok := r.backups.Default(api.CategoryBackup)
	if !ok {
		return &problem{
			message: "This install has no backup destination.",
			remedy:  "Configure a backup destination that keeps backups off this host, such as an object store, and make it the default.",
		}
	}
	if r.offHost(ref) {
		return nil
	}
	return &problem{
		message: fmt.Sprintf("The default backup destination %q %s.", ref, onHostDisk),
		remedy:  "Make a backup destination that keeps backups off this host the default.",
	}
}

func (r *Readiness) auditArchives(doc policy.Document) *problem {
	switch doc.AuditArchive {
	case audit.ArchiveKeep, "":
		return &problem{
			message: "Host policy keeps audit archives under Pando's directory on this host (audit_archive is keep).",
			remedy:  "Set audit_archive to export in host policy, or set PANDO_POLICY_AUDIT_ARCHIVE=export where Pando is deployed.",
		}
	case audit.ArchiveExport:
		// Exported to the default destination, which defaultBackup checks.
		if ref := doc.AuditArchiveDestination; ref != "" && !r.offHost(ref) {
			return &problem{
				message: fmt.Sprintf("Host policy exports audit archives to the backup destination %q, which %s.", ref, onHostDisk),
				remedy:  "Set audit_archive_destination to a backup destination that keeps backups off this host, or clear it to use the default.",
			}
		}
	}
	return nil
}

// offHost reports whether the destination keeps bundles off this host, as its
// capabilities say (R-254). One that is not running cannot say so.
func (r *Readiness) offHost(ref string) bool {
	b, ok := r.backups.Backup(ref)
	return ok && b.Capabilities().OffHost
}

func notReady(found []problem) error {
	switch len(found) {
	case 0:
		return nil
	case 1:
		return errs.New(errs.StateInvalid, "This install is not ready for a second host. "+found[0].message).
			WithRemedy(found[0].remedy)
	}
	messages := make([]string, len(found))
	remedies := make([]string, len(found))
	for i, p := range found {
		messages[i] = fmt.Sprintf("(%d) %s", i+1, p.message)
		remedies[i] = fmt.Sprintf("(%d) %s", i+1, p.remedy)
	}
	return errs.Newf(errs.StateInvalid, "This install is not ready for a second host, for %d reasons. %s",
		len(found), strings.Join(messages, " ")).
		WithRemedy(strings.Join(remedies, " "))
}
