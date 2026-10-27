//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type standbys struct {
	homed    map[string][]liveness.HomedApp
	promoted []string
}

func (s *standbys) AppsHomedOn(_ context.Context, hostID string) ([]liveness.HomedApp, error) {
	return s.homed[hostID], nil
}

func (s *standbys) PromoteStandby(_ context.Context, appID string) error {
	s.promoted = append(s.promoted, appID)
	return nil
}

// answeringWitness refuses marking a host lost while answering is set, as
// liveness.Witness does for a host heard from lately.
type answeringWitness struct{ answering *bool }

func (w answeringWitness) Silent(context.Context, state.Host) error {
	if *w.answering {
		return errs.New(errs.StateInvalid, "Host app-2 has been heard from in the last 30s, so it is not gone.")
	}
	return nil
}

// TestR421_APersonMarksAHostLostWithTheClusterVerb asserts R-421's manual
// path: with install.cluster.manage a person says a host is gone and its
// apps' standbys start, audited as them; without the verb nothing happens;
// and a host that still answers is refused.
func TestR421_APersonMarksAHostLostWithTheClusterVerb(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	store := state.NewCluster(i.db, testTokenKey)
	h, err := store.AddHost(ctx, state.Host{Name: "app-2", Roles: []string{"app"}, Address: "10.0.0.2:7443", CertSHA256: strings.Repeat("e", 64)})
	require.NoError(t, err)

	sb := &standbys{homed: map[string][]liveness.HomedApp{h.ID: {{AppID: "app_gone", HasStandby: true}}}}
	answering := true
	i.Server.HostLoss = &liveness.Service{
		Store: store, Apps: state.NewApps(i.db), Audit: audit.New(i.db.Pool), Logger: zap.NewNop(),
		Homes: sb, Failover: sb,
		Witness: answeringWitness{answering: &answering},
	}

	viewer, _ := i.holding("viewer", authz.InstallView)
	denied := i.do(viewer, http.MethodPost, "/cluster/hosts/"+h.ID+"/lost", nil)
	require.Equal(t, http.StatusForbidden, denied.Code, denied.String())
	lost, err := store.LostHosts(ctx)
	require.NoError(t, err)
	require.Empty(t, lost, "refused without the verb")

	ops, _ := i.holding("ops", authz.InstallClusterManage)
	refused := i.do(ops, http.MethodPost, "/cluster/hosts/"+h.ID+"/lost", nil)
	require.Equal(t, string(errs.StateInvalid), refused.ErrorCode(), refused.String())
	require.Contains(t, refused.String(), "has been heard from")

	answering = false
	got := i.do(ops, http.MethodPost, "/cluster/hosts/"+h.ID+"/lost", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var out liveness.HostLoss
	got.JSON(t, &out)
	require.Equal(t, h.ID, out.HostID)
	require.False(t, out.AlreadyLost)
	require.Equal(t, []liveness.SkippedApp{{AppID: "app_gone", Reason: liveness.SkipGone}}, out.Skipped)

	lost, err = store.LostHosts(ctx)
	require.NoError(t, err)
	require.True(t, lost[h.ID])

	missing := i.do(ops, http.MethodPost, "/cluster/hosts/host_nobody/lost", nil)
	require.Equal(t, string(errs.NotFound), missing.ErrorCode(), missing.String())

	events := i.do(i.admin(), http.MethodGet, "/audit?action=cluster.host.lost", nil)
	require.Equal(t, http.StatusOK, events.Code, events.String())
	require.Contains(t, events.String(), h.ID)
	require.Contains(t, events.String(), `"manual"`)

	// Without the service wired, the route says so.
	i.Server.HostLoss = nil
	unwired := i.do(ops, http.MethodPost, "/cluster/hosts/"+h.ID+"/lost", nil)
	require.Equal(t, string(errs.Internal), unwired.ErrorCode(), unwired.String())
}
