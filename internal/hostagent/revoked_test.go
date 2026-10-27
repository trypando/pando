package hostagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// nothingToSay answers no view, as a host that cannot stop its apps does.
type nothingToSay struct{}

func (nothingToSay) Answer(string) []byte { return nil }

// TestR415_APingWithARevokedCertificateIsRefused asserts R-415 on the agent:
// Member is asked about the certificate as well as the host it names, so a
// certificate revoked with a removed host is refused whoever it claims to
// be. And a Liveness with no view to give refuses the ping (R-153).
func TestR415_APingWithARevokedCertificateIsRefused(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	cert, err := as.IssueHostClient(peerID)
	require.NoError(t, err)
	probe := ClientTLS(as, cert, "host-1")
	revoked := Fingerprint256(cert.Certificate[0])
	live := &fakeLiveness{}
	addr := agentOn(ctx, t, as, &Agent{Liveness: live, Member: func(_, sha string) bool { return sha != revoked }})
	_, err = Ping(ctx, addr, probe, peerID)
	require.ErrorContains(t, err, "is not in this cluster")
	_, err = Ping(ctx, addr, hostTLSFor(t, as, peerID), peerID)
	require.NoError(t, err, "another certificate of the same host")

	quiet := agentOn(ctx, t, as, &Agent{Liveness: nothingToSay{}})
	_, err = Ping(ctx, quiet, probe, peerID)
	require.ErrorContains(t, err, "cannot stop its own apps")
	require.True(t, Contacted(err))
}
