package incus

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// TestR105_AnIncusThatFailsIsReportedNotIgnored asserts R-105 and R-254:
// whichever call Incus fails, the adapter returns a classified error naming
// what it was doing, never a success and never a panic.
func TestR105_AnIncusThatFailsIsReportedNotIgnored(t *testing.T) {
	boom := errors.New("incus said no")
	vol := api.VolumeHandle{VolumeID: "vol_1", Handle: "pando-vol-1"}
	operations := map[string]func(a *Adapter) error{
		"apply": func(a *Adapter) error {
			p := webPlan("app_2")
			p.Volumes = []api.VolumePlan{{VolumeID: "vol_2"}}
			_, err := a.Apply(ctx, p)
			return err
		},
		"observe": func(a *Adapter) error {
			_, err := a.Observe(ctx, api.BundleRef{BundleID: "app_1"})
			return err
		},
		"destroy": func(a *Adapter) error {
			return a.Destroy(ctx, api.BundleRef{BundleID: "app_1"}, api.DestroyOptions{})
		},
		"destroy volume": func(a *Adapter) error { return a.DestroyVolume(ctx, vol) },
		"create volume": func(a *Adapter) error {
			_, err := a.CreateVolume(ctx, api.VolumeRequest{VolumeID: "vol_3", BundleID: "app_1"})
			return err
		},
		"snapshot": func(a *Adapter) error { return a.SnapshotVolume(ctx, vol, io.Discard) },
		"capacity": func(a *Adapter) error {
			_, err := a.Capacity(ctx)
			return err
		},
		"largest fit": func(a *Adapter) error {
			a.config.Target = ""
			_, err := a.LargestFitFor(ctx, "app_1")
			return err
		},
		"in use": func(a *Adapter) error {
			_, err := a.InUse(ctx)
			return err
		},
		"health": func(a *Adapter) error { return a.HealthCheck(ctx) },
		"trial": func(a *Adapter) error {
			_, err := a.Trial(ctx, api.TrialRequest{TrialID: "tr_9", Image: "nginx"})
			return err
		},
	}
	methods := []string{
		"Server", "Members", "Resources", "Instances", "Instance", "InstanceState", "CreateInstance",
		"UpdateInstance", "SetState", "DeleteInstance", "Network", "ACL",
		"Profile", "Volume", "Volumes", "CreateVolume", "DeleteVolume", "VolumeFiles", "ImageAlias",
	}
	failedSomething := map[string]bool{}
	for _, method := range methods {
		for name, op := range operations {
			f := newFakeIncus()
			f.members = nil
			a, _, _ := testAdapter(t, f, func(c *Config) { c.StoragePool = "" })
			p := webPlan("app_1")
			p.Volumes = []api.VolumePlan{{VolumeID: "vol_1"}}
			_, err := a.Apply(ctx, p)
			require.NoError(t, err)
			f.files["default/pando-vol-1"] = newMemFS(t)

			f.fail = map[string]error{method: boom}
			err = op(a)
			if err == nil {
				continue
			}
			failedSomething[method] = true
			e := errs.As(err)
			require.NotNil(t, e, "%s failing %s returned an unclassified error: %v", method, name, err)
			require.NotEmpty(t, e.Message, "%s failing %s", method, name)
		}
	}
	// Some calls are made only on a first deploy or a changed setting, which
	// this table does not repeat; most must have been exercised.
	require.GreaterOrEqual(t, len(failedSomething), len(methods)-2, "exercised: %v", failedSomething)
}
