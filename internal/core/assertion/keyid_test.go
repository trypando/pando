package assertion_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/assertion"
)

// TestR051_AKeysIDIsDerivedFromTheKey asserts that the kid a replica
// publishes is the one any other replica derives from the same public key, so
// every replica names a peer's key alike without being told.
func TestR051_AKeysIDIsDerivedFromTheKey(t *testing.T) {
	a, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	b, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)

	key := a.SigningKey()
	require.Equal(t, key.ID, assertion.KeyID(key.Public))
	require.Equal(t, a.SigningKeyID(), key.ID)
	require.NotEqual(t, key.ID, assertion.KeyID(b.SigningKey().Public), "a different key, a different kid")
}
