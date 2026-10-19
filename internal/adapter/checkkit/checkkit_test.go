package checkkit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/secret"
)

func TestMergeGivesConfigureWhatItReads(t *testing.T) {
	got, err := Merge(json.RawMessage(`{"host":"mail.example.com"}`),
		map[string]secret.Value{"api_key": secret.New("k"), "empty": {}}, []string{"password", "api_key"})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(got, &m))
	require.Equal(t, "mail.example.com", m["host"])
	require.Equal(t, map[string]any{"api_key": "k", "password": Stored}, m["credentials"])

	plain, err := Merge(nil, nil, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(plain))

	_, err = Merge(json.RawMessage(`[1]`), nil, nil)
	require.Error(t, err)
}

func TestMergeLeavesOutACredentialSentEmpty(t *testing.T) {
	got, err := Merge(nil, map[string]secret.Value{"password": {}}, []string{"password", "token"})
	require.NoError(t, err)
	require.JSONEq(t, `{"credentials":{"token":"stored"}}`, string(got))
}

func TestMergeTakesANullConfiguration(t *testing.T) {
	got, err := Merge(json.RawMessage(`null`), nil, []string{"password"})
	require.NoError(t, err)
	require.JSONEq(t, `{"credentials":{"password":"stored"}}`, string(got))
}
