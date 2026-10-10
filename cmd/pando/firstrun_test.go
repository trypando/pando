package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/secret"
)

func firstRunLog(first bootstrap.Result, adminPasswordSet bool) []observer.LoggedEntry {
	core, logs := observer.New(zapcore.InfoLevel)
	logFirstRun(zap.New(core), first, adminPasswordSet)
	return logs.All()
}

// TestR046_TheSetupTokenIsPrintedOnTheStartThatMadeIt asserts the one
// credential Pando logs on purpose (issue #130): the setup token, on the
// warning that the installation is not set up yet, and only on the start that
// made it. Later starts say when it was made and how to get a new one.
func TestR046_TheSetupTokenIsPrintedOnTheStartThatMadeIt(t *testing.T) {
	made := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	logs := firstRunLog(bootstrap.Result{Unclaimed: true, SetupToken: secret.New("tok-123"), SetupTokenMade: made}, false)
	require.Len(t, logs, 1)
	require.Equal(t, zapcore.WarnLevel, logs[0].Level)
	require.Equal(t, "this installation is not set up yet", logs[0].Message)
	require.Equal(t, "tok-123", logs[0].ContextMap()["setup_token"])

	logs = firstRunLog(bootstrap.Result{Unclaimed: true, SetupTokenMade: made}, false)
	require.Len(t, logs, 1)
	require.NotContains(t, logs[0].ContextMap(), "setup_token")
	require.Equal(t, made, logs[0].ContextMap()["setup_token_made"])
	require.Contains(t, logs[0].ContextMap()["note"], "pando admin setup-token")
}

func TestFirstRunLogsWhatItDidWithoutAPassword(t *testing.T) {
	logs := firstRunLog(bootstrap.Result{Created: true}, true)
	require.Len(t, logs, 1)
	require.Equal(t, "first run: created an administrator account", logs[0].Message)
	for _, v := range logs[0].ContextMap() {
		require.NotContains(t, v, "password:", "the supplied password is never logged")
	}

	logs = firstRunLog(bootstrap.Result{}, true)
	require.Len(t, logs, 1)
	require.Equal(t, "PANDO_ADMIN_PASSWORD was set and ignored", logs[0].Message)

	require.Empty(t, firstRunLog(bootstrap.Result{}, false), "an installation already set up says nothing")
}
