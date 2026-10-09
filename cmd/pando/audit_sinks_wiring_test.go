package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	sinkhttps "github.com/trypando/pando/internal/adapter/auditsink/https"
	sinksyslog "github.com/trypando/pando/internal/adapter/auditsink/syslog"
)

// TestR382_TheAuditSinkKindsAreBuiltAndListed asserts both kinds of audit
// sink are buildable, offered to the forms that configure adapters, and built
// at startup only when the config file declares one.
func TestR382_TheAuditSinkKindsAreBuiltAndListed(t *testing.T) {
	assert.IsType(t, sinksyslog.New(), newAuditSinkAdapter(sinksyslog.Kind))
	assert.IsType(t, sinkhttps.New(), newAuditSinkAdapter(sinkhttps.Kind))
	assert.Nil(t, newAuditSinkAdapter("kafka"), "a kind this build has not is nil")

	assert.NotNil(t, newAdapter(string(adapterapi.CategoryAuditSink), sinkhttps.Kind, nil))
	assert.Nil(t, newAdapter(string(adapterapi.CategoryAuditSink), "kafka", nil),
		"a nil interface stays nil, so registration reports the kind as unknown")

	listed := map[string]bool{}
	for _, k := range adapterKinds() {
		if k.Category == adapterapi.CategoryAuditSink {
			listed[k.Kind] = true
		}
	}
	assert.Equal(t, map[string]bool{"syslog": true, "https": true}, listed)

	registry := adapterapi.NewRegistry()
	require.NoError(t, registry.Register("as_declared", sinksyslog.New()))
	declared := declaredAuditSinks(registry)
	require.Len(t, declared, 1)
	assert.Equal(t, "as_declared", declared[0].ID)
	assert.Empty(t, declaredAuditSinks(adapterapi.NewRegistry()))
}
