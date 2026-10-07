package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/hostagent"
)

func TestTheNewAuthorityCommandPrintsAnAuthorityTheAdapterReads(t *testing.T) {
	cmd := hostAgentCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"new-authority"})
	require.NoError(t, cmd.Execute())
	as, err := hostagent.ParseAuthorities(out.Bytes())
	require.NoError(t, err)
	require.Len(t, as, 1)
}

// agentEnv sets what the runtime adapter gives an agent's container.
func agentEnv(t *testing.T, listen string) {
	t.Helper()
	caPEM, err := hostagent.NewAuthority()
	require.NoError(t, err)
	as, err := hostagent.ParseAuthorities(caPEM)
	require.NoError(t, err)
	certPEM, keyPEM, err := as.IssueServer("app-1")
	require.NoError(t, err)
	t.Setenv(multidocker.EnvAgentCert, string(certPEM))
	t.Setenv(multidocker.EnvAgentKey, string(keyPEM))
	t.Setenv(multidocker.EnvAgentAuthorities, string(as.PoolPEM()))
	t.Setenv(multidocker.EnvAgentPool, "10.213.0.0/16")
	t.Setenv(multidocker.EnvAgentListen, listen)
}

func runServe(ctx context.Context) error {
	cmd := hostAgentCmd()
	cmd.SetArgs([]string{"serve"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.ExecuteContext(ctx)
}

// TestR023_TheHostAgentRefusesToStartWithoutWhatItChecksWith asserts that
// the agent does not start without its certificate, the authority it checks
// Pando's certificate against, or the app range it forwards into: started
// without any of them it would carry connections it cannot check.
func TestR023_TheHostAgentRefusesToStartWithoutWhatItChecksWith(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		unset, env, value, says string
	}{
		"no certificate": {unset: multidocker.EnvAgentCert, says: "without its certificate"},
		"no authority":   {unset: multidocker.EnvAgentAuthorities, says: "without its certificate, key or authority"},
		"a bad key":      {env: multidocker.EnvAgentKey, value: "not a key", says: "did not start"},
		"a bad range":    {env: multidocker.EnvAgentPool, value: "everywhere", says: "is not an address range"},
		"a bad address":  {env: multidocker.EnvAgentListen, value: "nowhere:at:all", says: "could not listen"},
	} {
		t.Run(name, func(t *testing.T) {
			agentEnv(t, "127.0.0.1:0")
			if c.unset != "" {
				t.Setenv(c.unset, "")
			}
			if c.env != "" {
				t.Setenv(c.env, c.value)
			}
			err := runServe(ctx)
			require.ErrorContains(t, err, c.says)
		})
	}
}

func TestTheHostAgentServesUntilItIsStopped(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	agentEnv(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()

	require.Eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 10*time.Second, 20*time.Millisecond, "the agent listens on the address it was given")
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
}
