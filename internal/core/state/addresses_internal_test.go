package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR165_NoAppTakesPandosOwnHostname asserts R-165's reserved hostnames: an
// app may not be reached at a hostname Pando serves the console on, in any
// case or with a trailing dot, because it would receive the console's traffic
// on the console's origin (2026-10-16 assessment, finding 2). Refused before
// the database is asked, so a nil querier is never reached.
func TestR165_NoAppTakesPandosOwnHostname(t *testing.T) {
	reserved := []string{"pando.example.com", "console.example.org."}
	for _, host := range []string{"pando.example.com", "PANDO.example.com", "console.example.org"} {
		r := spec.Routing{AdapterRef: "rte_traefik", Mode: spec.RoutingSubdomain, Hostname: host}
		err := checkAddress(context.Background(), nil, reserved, "app_1", r)
		require.Equal(t, errs.StateAddressTaken, errs.CodeOf(err), host)
		require.Contains(t, err.Error(), "Pando's own address")
	}

	a := &Apps{ReservedHostnames: func(context.Context) []string { return reserved }}
	require.Equal(t, reserved, a.reserved(context.Background()))
	require.Nil(t, (&Apps{}).reserved(context.Background()), "nil reserves none")
}
