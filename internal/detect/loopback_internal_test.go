package detect

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR105_TheCommandThatListenedOnLoopbackIsNotAnAnswer asserts R-105 for
// the question asked when an app listened only on 127.0.0.1: answering it with
// the command the trial already ran changes nothing, and is refused with what
// to change instead. It used to be accepted, and the deploy refused the app
// with the same explanation the question had given (issue #90).
func TestR105_TheCommandThatListenedOnLoopbackIsNotAnAnswer(t *testing.T) {
	loopback := Trial{Ran: true, Started: true, LoopbackPorts: []int{3000}}
	draft := Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}}

	// The command a generated Dockerfile starts the app with, from the plan.
	tried := loopbackCommand(loopback, draft, []string{"package.json — this looks like a Node.js project", startsWith + `["npm start"]`})
	require.Equal(t, "npm start", tried)

	p := Proposal{LoopbackCommand: tried}
	e := errs.As(p.CheckAnswers(map[string]string{KeyStartCommand: "  npm   start "}))
	require.NotNil(t, e)
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Message, "127.0.0.1")
	require.Contains(t, e.Message, "Valid answer:")
	require.Contains(t, e.Remedy, "0.0.0.0")

	require.NoError(t, p.CheckAnswers(map[string]string{KeyStartCommand: "npm start -- --host 0.0.0.0"}))

	// The workload's own command wins over the plan's, and a shell's -c line
	// is the command line.
	draft.Workloads[0].Command = []string{"/bin/sh", "-c", "node server.js"}
	require.Equal(t, "node server.js", loopbackCommand(loopback, draft, nil))

	// Nothing is recorded where the trial saw the app reachable, or no trial ran.
	reachable := Trial{Ran: true, Started: true, ObservedPorts: []int{3000}}
	require.Empty(t, loopbackCommand(reachable, draft, nil))
	require.Empty(t, loopbackCommand(Trial{}, draft, nil))
	require.NoError(t, Proposal{}.CheckAnswers(map[string]string{KeyStartCommand: "npm start"}))
}
