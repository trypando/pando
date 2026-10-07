package deploy

import (
	"context"
	"fmt"
	"io"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// ServiceSecretPrefix namespaces the connection strings Pando generates.
//
// A prefix rather than the bare slot key so a provisioned DATABASE_URL cannot
// collide with a secret someone set by hand under the same name — and so that
// looking at an app's secrets makes it obvious which ones Pando owns and will
// overwrite.
const ServiceSecretPrefix = "pando.service."

// provisioned is what a deploy needs to add to the bundle for one app's
// provisioned slots.
type provisioned struct {
	workloads []api.WorkloadPlan
	volumes   []api.VolumePlan

	// connections is the DSN per slot key, for resolveEnv.
	connections map[string]secret.Value

	// names is the workload name per slot key, so the app's own workloads can
	// be ordered after the services they depend on.
	names map[string]string
}

// provision fills every provisioned slot in the spec (R-131).
//
// Runs on every deploy, not only the first, because the workloads it returns
// are the service. Skipping it on a redeploy would produce a bundle without the
// database in it, and the runtime would dutifully converge to that.
//
// The database row is what makes this idempotent rather than the adapter: the
// row holds the service ID and the secret key, and the stored connection string
// carries the credentials the data on disk was created with.
func (r *Runner) provision(ctx context.Context, s *spec.AppSpec, sink io.Writer) (provisioned, error) {
	out := provisioned{
		connections: map[string]secret.Value{},
		names:       map[string]string{},
	}
	if r.services == nil {
		return out, nil
	}

	for _, slot := range s.Slots {
		if slot.Resolution == nil || slot.Resolution.Mode != spec.ResolutionProvisioned {
			continue
		}

		adapter, ref, ok := r.registry.ServicesFor(slot.Type)
		if !ok {
			return provisioned{}, errs.Newf(errs.PlanAdapterNotConfigured,
				"Nothing on this installation can provision a %s for %s.", slot.Type.DisplayName(), slot.Key).
				WithDetail("slot_key", slot.Key).
				WithRemedy("Connect this slot to an instance you already run, or paste a connection string.")
		}

		existing, found, err := r.services.Get(ctx, s.AppID, slot.Key)
		if err != nil {
			return provisioned{}, err
		}

		serviceID := existing.ID

		// The name a secret is filed under, not the secret — "pando.service.db"
		// and never a password. "Key" is this codebase's word for the lookup
		// name throughout state.Secrets (Put, Get and Keys all take one, and
		// Keys is documented as returning names and never values); the value
		// itself is only ever a secret.Value, which renders as [redacted] in
		// every marshaler (R-194).
		//
		// Worth saying because a static analyzer reads the name and assumes the
		// worse meaning: CodeQL's clear-text-logging heuristic treats anything
		// matching /secretkey/ as key material, so this name reaching an error
		// message reads to it as a password reaching a log.
		secretKey := existing.SecretKey

		var prior secret.Value
		if found {
			// Reading the stored DSN is what lets the adapter return the same
			// credentials it returned the first time.
			prior, err = r.secretStore.Get(ctx, s.AppID, secretKey)
			if err != nil {
				return provisioned{}, err
			}
		} else {
			serviceID = r.services.NewID()
			secretKey = ServiceSecretPrefix + slot.Key
			fmt.Fprintf(sink, "=> Provisioning a %s for %s\n", slot.Type.DisplayName(), slot.Key)
		}

		res, err := adapter.Provision(ctx, api.ProvisionRequest{
			AppID: s.AppID, BundleID: s.AppID, SlotKey: slot.Key,
			Type: slot.Type, ServiceID: serviceID, ExistingSecret: prior,
		})
		if err != nil {
			return provisioned{}, err
		}

		// Store before recording the row. A secret with no row is an unused
		// value; a row with no secret is an app that cannot start and whose
		// password is gone.
		if !found {
			if err := r.secretStore.Put(ctx, s.AppID, secretKey, res.ConnectionSecret); err != nil {
				return provisioned{}, err
			}
			if err := r.services.Record(ctx, state.ServiceInstance{
				ID: serviceID, AppID: s.AppID, SlotKey: slot.Key, SlotType: slot.Type,
				AdapterRef: ref, Handle: res.Handle.Handle, SecretKey: secretKey,
			}); err != nil {
				return provisioned{}, err
			}
		}

		out.workloads = append(out.workloads, res.Workloads...)
		out.volumes = append(out.volumes, res.Volumes...)
		out.connections[slot.Key] = res.ConnectionSecret
		if len(res.Workloads) > 0 {
			out.names[slot.Key] = res.Workloads[0].Name
		}
	}
	return out, nil
}

// ServiceShapes is what an app's provisioned services should look like, for the
// reconciler's drift comparison.
//
// Without it the reconciler's idea of "what should be running" contains the
// app's workloads and not the database standing next to them, and two things go
// wrong on every tick: a provisioned service that was killed is never restored,
// because nothing wants it (R-148), and the one that is running is reported as
// a workload the spec does not declare — an app told every fifteen seconds that
// its own database is a stranger.
//
// Reads nothing sensitive and writes nothing. Provision is called without the
// stored connection string, which changes only the generated password, and
// environment is dropped here anyway: the reconciler's comparison is shape, and
// it must never be a reason to decrypt a secret (R-193). A slot with no
// recorded instance is skipped rather than provisioned — this runs every
// fifteen seconds for every app and is not where a database gets created.
func (r *Runner) ServiceShapes(ctx context.Context, s *spec.AppSpec) (api.BundlePlan, error) {
	var shape api.BundlePlan
	if r.services == nil {
		return shape, nil
	}

	for _, slot := range s.Slots {
		if slot.Resolution == nil || slot.Resolution.Mode != spec.ResolutionProvisioned {
			continue
		}

		existing, found, err := r.services.Get(ctx, s.AppID, slot.Key)
		if err != nil {
			return api.BundlePlan{}, err
		}
		if !found {
			continue
		}

		adapter, _, ok := r.registry.ServicesFor(slot.Type)
		if !ok {
			continue
		}

		res, err := adapter.Provision(ctx, api.ProvisionRequest{
			AppID: s.AppID, BundleID: s.AppID, SlotKey: slot.Key,
			Type: slot.Type, ServiceID: existing.ID,
		})
		if err != nil {
			return api.BundlePlan{}, err
		}

		for _, w := range res.Workloads {
			w.Env = nil
			shape.Workloads = append(shape.Workloads, w)
		}
		shape.Volumes = append(shape.Volumes, res.Volumes...)
	}
	return shape, nil
}

// Environments is the environment each of an app's workloads runs with, keyed
// by workload name: its own workloads' and its provisioned services'.
//
// For the reconciler, at the moment it is about to apply a correction (R-148).
// Its comparison is shape, and environment is deliberately absent from that
// (R-193) — but a shape is not something to start. A workload it re-created
// from the shape came up with no environment at all: a provisioned Redis whose
// password arrives in REDIS_PASSWORD exited on every start, and an app lost
// every variable and secret it was deployed with.
//
// Resolved the way a deploy resolves them, with one difference: a slot with no
// recorded instance is skipped, as in ServiceShapes. Restoring what exists is
// the reconciler's job; creating a database is a deploy's.
func (r *Runner) Environments(ctx context.Context, s *spec.AppSpec) (map[string]map[string]secret.Value, error) {
	secrets, err := r.secrets.Resolve(ctx, s.AppID)
	if err != nil {
		return nil, err
	}

	svcs := provisioned{connections: map[string]secret.Value{}, names: map[string]string{}}
	if r.services != nil {
		for _, slot := range s.Slots {
			if slot.Resolution == nil || slot.Resolution.Mode != spec.ResolutionProvisioned {
				continue
			}
			existing, found, err := r.services.Get(ctx, s.AppID, slot.Key)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			adapter, _, ok := r.registry.ServicesFor(slot.Type)
			if !ok {
				continue
			}
			prior, err := r.secretStore.Get(ctx, s.AppID, existing.SecretKey)
			if err != nil {
				return nil, err
			}
			res, err := adapter.Provision(ctx, api.ProvisionRequest{
				AppID: s.AppID, BundleID: s.AppID, SlotKey: slot.Key,
				Type: slot.Type, ServiceID: existing.ID, ExistingSecret: prior,
			})
			if err != nil {
				return nil, err
			}
			svcs.workloads = append(svcs.workloads, res.Workloads...)
			svcs.connections[slot.Key] = res.ConnectionSecret
		}
	}

	out := make(map[string]map[string]secret.Value, len(s.Workloads)+len(svcs.workloads))
	for _, w := range svcs.workloads {
		out[w.Name] = w.Env
	}
	for _, w := range s.Workloads {
		env, err := resolveEnv(s, w, secrets, svcs)
		if err != nil {
			return nil, err
		}
		out[w.Name] = env
	}
	return out, nil
}

// dependsOn is every provisioned workload an app workload should start after.
//
// Start order, not readiness — Docker's depends_on does not wait for a health
// check and neither does this. What actually covers the race is the restart
// policy: an app that exits because Postgres was still running initdb comes
// back a second later and connects. Ordering removes the common case; the
// restart policy removes the rest.
func (p provisioned) dependsOn(s *spec.AppSpec, w spec.Workload) []string {
	if len(p.names) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range w.Env {
		if e.SlotRef == nil {
			continue
		}
		name, ok := p.names[*e.SlotRef]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
