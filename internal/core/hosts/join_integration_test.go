//go:build integration

package hosts_test

import (
	"bytes"
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/secret"
)

// readiness answers the first join's question only once released, or once
// that join's context ends; every later one at once.
type readiness struct {
	asked   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (r *readiness) ReadyToJoin(ctx context.Context, _ []string) error {
	if r.asked.Add(1) > 1 {
		return nil
	}
	close(r.entered)
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dbCluster is a join service over a real database.
func dbCluster(t *testing.T) (*hosts.Service, *state.Cluster, *state.DB) {
	t.Helper()
	db, _ := statetest.Connect(t)
	c := state.NewCluster(db, bytes.Repeat([]byte{7}, tokenkey.Size))
	a, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	return &hosts.Service{Store: c, Authority: a, Version: "1.2.3"}, c, db
}

func joinAs(t *testing.T, tok secret.Value, name string) hosts.JoinRequest {
	return hosts.JoinRequest{Secret: tok, Name: name, CSR: csrFor(t, name), Platform: "linux/amd64", Version: "1.2.3", Address: name + ":7443"}
}

// TestR415_AHungReadinessCheckHoldsNoTokenLockOrConnection asserts R-415's
// join under load: the checks a join makes before it spends its token's use
// (readiness, the runtime, issuing its certificates) hold no database
// connection and no lock on the token, so a check that hangs keeps no other
// join on that token waiting and cannot drain the pool; and a check that
// never answers is given up on.
func TestR415_AHungReadinessCheckHoldsNoTokenLockOrConnection(t *testing.T) {
	ctx := context.Background()
	svc, c, db := dbCluster(t)
	issued, err := c.CreateJoinToken(ctx, []string{"app"}, 2, "usr_ada", time.Now().Add(time.Hour))
	require.NoError(t, err)
	ready := &readiness{entered: make(chan struct{}), release: make(chan struct{})}
	svc.Readiness = ready

	hung := make(chan error, 1)
	released := false
	t.Cleanup(func() {
		if !released {
			close(ready.release)
			<-hung
		}
	})
	go func() {
		_, err := svc.Join(ctx, joinAs(t, issued.Secret, "edge-1"))
		hung <- err
	}()
	<-ready.entered
	require.Zero(t, db.Stat().AcquiredConns(), "a join waiting on its checks holds no connection")

	other, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := svc.Join(other, joinAs(t, issued.Secret, "edge-2"))
	require.NoError(t, err, "another join on the token is not kept waiting")
	require.Equal(t, "edge-2", res.Host.Name)

	released = true
	close(ready.release)
	require.NoError(t, <-hung)
	toks, err := c.JoinTokens(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, toks[0].Uses)
}
