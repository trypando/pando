package spec

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR166_AnAppsHostnameCanBeChosenWhereRoutingServesOne asserts that review
// can set the address an app is served at in subdomain mode, and refuses it —
// with the reason — where the address is a port Pando allocates.
func TestR166_AnAppsHostnameCanBeChosenWhereRoutingServesOne(t *testing.T) {
	s := &AppSpec{Routing: Routing{Mode: RoutingSubdomain, Hostname: "crew.pando.local"}}
	require.NoError(t, SetHostname(s, " Crew.Example.com. "))
	require.Equal(t, "crew.example.com", s.Routing.Hostname)

	err := SetHostname(s, "not a host")
	require.Equal(t, errs.ValidInvalid, errs.As(err).Code)
	require.Equal(t, "crew.example.com", s.Routing.Hostname, "unchanged by a refusal")

	port := &AppSpec{Routing: Routing{Mode: RoutingPort, Port: 9003}}
	err = SetHostname(port, "crew.example.com")
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "port")
	require.Empty(t, port.Routing.Hostname)
}

func TestReadsSecret(t *testing.T) {
	ref := "NTFY_TOPIC"
	s := &AppSpec{Workloads: []Workload{{Name: "web", Env: []EnvEntry{{Key: "NTFY_TOPIC", SecretRef: &ref}}}}}
	require.True(t, ReadsSecret(s, "NTFY_TOPIC"))
	require.False(t, ReadsSecret(s, "OTHER"))

	literal := "t"
	plain := &AppSpec{Workloads: []Workload{{Name: "web", Env: []EnvEntry{{Key: "NTFY_TOPIC", Value: &literal}}}}}
	require.False(t, ReadsSecret(plain, "NTFY_TOPIC"), "a literal with the same name is not the secret")

	slotted := &AppSpec{Slots: []Slot{{Key: "db", Resolution: &Resolution{Mode: ResolutionLiteral, SecretRef: "DB_URL"}}}}
	require.True(t, ReadsSecret(slotted, "DB_URL"))
	require.False(t, ReadsSecret(nil, "DB_URL"))
}
