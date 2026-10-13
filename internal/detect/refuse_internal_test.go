package detect

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR097_AnAppThatStopsForAMountItIsGivenIsNotRefused asserts R-097. Nginx
// Proxy Manager checks for /etc/letsencrypt, says it is not mounted and exits
// 0. The same trial gives it that volume (mountsAskedFor), so the clean exit is
// the app missing something Pando now provides, not a command-line program.
func TestR097_AnAppThatStopsForAMountItIsGivenIsNotRefused(t *testing.T) {
	asked := Trial{Ran: true, Started: true, Exited: true,
		Log: "❯ Checking paths ...\nERROR: /etc/letsencrypt is not mounted! Check your docker configuration.\n"}
	require.Nil(t, exitedRefusal(asked))

	draft := applyImageVolumes(Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}},
		Trial{ImageVolumes: mountsAskedFor(asked.Log)})
	require.Len(t, draft.Workloads[0].Mounts, 1)
	require.Equal(t, "/etc/letsencrypt", draft.Workloads[0].Mounts[0].Path)

	// A program that printed its result and finished is still refused.
	require.NotNil(t, exitedRefusal(Trial{Ran: true, Started: true, Exited: true, Log: "usage: tool [flags]\n"}))
}
