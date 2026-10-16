package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
)

// TestR092_TheFetchGuardNamesPandosPortAndDatabase asserts what startup tells
// the repository fetch it may never reach: the port Pando listens on, and
// every host its database connection string names — not a Unix socket, which
// is not an address.
func TestR092_TheFetchGuardNamesPandosPortAndDatabase(t *testing.T) {
	g := sourceGuard(":8080", "postgres://pando@postgres:5432,db2.internal:5433/pando")
	require.Equal(t, []uint16{8080}, g.OwnPorts)
	require.Equal(t, []source.HostPort{{Host: "postgres", Port: 5432}, {Host: "db2.internal", Port: 5433}}, g.Database)

	g = sourceGuard("127.0.0.1:9000", "host=/var/run/postgresql dbname=pando")
	require.Equal(t, []uint16{9000}, g.OwnPorts)
	require.Empty(t, g.Database)

	g = sourceGuard("not an address", "%%%")
	require.Empty(t, g.OwnPorts)
	require.Empty(t, g.Database)
}
