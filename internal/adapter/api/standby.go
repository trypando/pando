package api

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/core/spec"
)

// Warm standby (R-419, R-420, notes-cluster.md "Warm standby").
//
// A standby is a stopped copy of an app on a second place the runtime can run
// it: the same plan, created and never started, with the app's volumes copied
// to it on an interval. It is started only when the app's place is lost, and
// then it is the app: one copy runs at a time (R-153).
//
// Places are the runtime's (R-256). Core never chooses one: it hands the
// runtime a plan and gets back an opaque Host string naming where the standby
// is, which it records, shows, and passes back.

// StandbyRuntime is implemented by a runtime adapter that reports
// SupportsStandby. Core checks the capability before asserting the interface,
// as it does for SelfUpgrader: the capability is the planner's and the
// console's answer, the interface is only how the call is made (R-254).
type StandbyRuntime interface {
	// ApplyStandby creates or refreshes the stopped standby of the bundle
	// somewhere other than where the bundle runs. Idempotent. Nothing of it
	// is started: a standby whose workloads ran would be a second copy of the
	// app, writing to its own data (R-153).
	ApplyStandby(ctx context.Context, req StandbyRequest) (StandbyHandle, error)

	// ShipVolumes copies the bundle's volumes to its standby, live and
	// incrementally: only what changed since the last copy is sent, the app is
	// never paused, and nothing about either host's filesystem is changed
	// (R-420). The standby keeps the last Keep copies.
	ShipVolumes(ctx context.Context, req ShipRequest) (ShipResult, error)

	// PromoteStandby starts the standby as the app, from the newest copy of
	// its volumes, and falls back to the copy before when the app does not
	// come up healthy (R-420). From then on the standby's host is the app's
	// home. The caller has already decided the app's host is lost (R-421);
	// this never checks, and never starts anything on the lost host.
	PromoteStandby(ctx context.Context, req PromoteRequest) (PromoteResult, error)

	// RemoveStandby removes the bundle's standby and every copy of its
	// volumes, wherever they are. Removing one that does not exist is not an
	// error.
	RemoveStandby(ctx context.Context, ref BundleRef) error

	// RemoveStaleCopy removes what is left of the bundle on host, a host that
	// was lost and has come back after the bundle's standby was promoted
	// elsewhere: its workloads, its network and its volumes. Never restarted:
	// R-148's restore does not apply to a copy that was failed over (R-419).
	// Refused for the bundle's current home.
	RemoveStaleCopy(ctx context.Context, ref BundleRef, host string) error
}

// StandbyRequest asks for a bundle's standby.
type StandbyRequest struct {
	// Bundle is the app's plan, as the reconciler would apply it.
	Bundle BundlePlan

	// Host is where the standby was before, or where core asks for it (the
	// host a failover left, once it is back). Empty lets the runtime choose.
	// A host that is no longer usable is replaced by the runtime's choice.
	Host string
}

// StandbyHandle says where a standby is.
type StandbyHandle struct {
	Host string

	// Home is where the bundle runs, the place whose loss starts this
	// standby, in the same names as Host. Core records it to know which
	// apps a lost place held (R-421); empty when the runtime cannot say.
	Home string
}

// ShipRequest asks for one copy of a bundle's volumes to be sent to its
// standby.
type ShipRequest struct {
	BundleID string
	// Host is the standby's host, as StandbyHandle named it.
	Host string
	// VolumeIDs are the bundle's volumes. Empty ships nothing.
	VolumeIDs []string
	// Keep is how many copies the standby keeps [P: 3]. Zero is
	// DefaultStandbyCopies.
	Keep int
}

// DefaultStandbyCopies is how many copies of an app's volumes its standby
// keeps (R-420 [P]).
const DefaultStandbyCopies = 3

// ShipResult is what one ship did.
type ShipResult struct {
	// Copies is how many copies the standby now holds, of the volume with the
	// fewest.
	Copies int
	// FilesSent and BytesSent are what crossed between hosts.
	FilesSent int
	BytesSent int64
	// Unchanged means nothing had changed since the last copy, so no new copy
	// was made.
	Unchanged bool
}

// PromoteRequest asks for a bundle's standby to be started as the app.
type PromoteRequest struct {
	// Bundle is the app's plan, with its environment resolved.
	Bundle BundlePlan
	// Host is the standby's host.
	Host string
	// HealthTimeout is how long each copy is given to come up healthy. Zero
	// is DefaultPromoteHealthTimeout.
	HealthTimeout time.Duration
	// PollEvery is how often health is looked at. Zero is two seconds.
	PollEvery time.Duration
}

// DefaultPromoteHealthTimeout is how long a promoted standby is given to come
// up healthy before the copy before is tried.
const DefaultPromoteHealthTimeout = 2 * time.Minute

// PromoteResult is what a promote did.
type PromoteResult struct {
	// Host is the app's home now.
	Host string
	// FormerHost is where the app ran before, which now holds a stale copy
	// (RemoveStaleCopy). Empty when the runtime did not know.
	FormerHost string
	// Copy is which copy the app runs from: 0 the newest, 1 the one before.
	Copy int
	// FellBack means a newer copy did not come up healthy first.
	FellBack bool
	// Healthy is false when no copy came up healthy in time. The app is then
	// left running from the oldest copy tried, for the reconciler and its
	// owner to deal with as any unhealthy app.
	Healthy bool
}

// --- services replication --------------------------------------------------

// ReplicaServices is implemented by a services adapter whose
// ServicesCapabilities.ReplicaTypes is not empty: it keeps a provisioned
// service's standby current through the engine's own replication, which loses
// seconds rather than an interval (R-420). A type it does not list is copied
// with the app's volumes instead, and the app's notice says so.
type ReplicaServices interface {
	// ApplyReplica returns the workloads that keep a replica of the service
	// on the standby, for the runtime to add to the standby's plan.
	ApplyReplica(ctx context.Context, req ReplicaRequest) (ProvisionResult, error)
	// PromoteReplica turns the replica into the primary, at failover.
	PromoteReplica(ctx context.Context, h ServiceHandle) error
}

// ReplicaRequest asks for a provisioned service's replica.
type ReplicaRequest struct {
	Handle ServiceHandle
	Type   spec.SlotType
	// Primary is the connection string of the service being replicated.
	Primary ProvisionResult
}

// Replicates reports whether the adapter replicates a slot type natively.
func (c ServicesCapabilities) Replicates(t spec.SlotType) bool {
	for _, have := range c.ReplicaTypes {
		if have == t {
			return true
		}
	}
	return false
}
