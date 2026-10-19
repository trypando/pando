package smtp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/secret"
)

// A configuration that could never send is refused as it is saved, counting a
// password already stored as there.
func TestASavedConfigurationIsCheckedAsItIsSaved(t *testing.T) {
	check := Info().Check
	require.NotNil(t, check)

	plain := json.RawMessage(`{"host":"mail.example.com","from":"pando@example.com","security":"none","username":"pando"}`)
	require.ErrorContains(t, check(plain, map[string]secret.Value{"password": secret.New("pw")}, nil), "unencrypted")

	ok := json.RawMessage(`{"host":"mail.example.com","from":"pando@example.com","username":"pando"}`)
	require.NoError(t, check(ok, nil, []string{"password"}))
	require.ErrorContains(t, check(ok, nil, nil), "no password")
	// Sent empty, a stored password is being removed.
	require.ErrorContains(t, check(ok, map[string]secret.Value{"password": {}}, []string{"password"}), "no password")
}
