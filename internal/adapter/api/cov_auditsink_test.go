package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR382_AnAuditSinkThatIsNotRegisteredIsNotFound asserts the AuditSink
// accessor answers a reference nothing was registered under with not found,
// rather than a nil adapter core would call.
func TestR382_AnAuditSinkThatIsNotRegisteredIsNotFound(t *testing.T) {
	r := api.NewRegistry()
	s, ok := r.AuditSink("as_missing")
	require.False(t, ok)
	require.Nil(t, s)
}
