package oidc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGraphBaseOnlyEverNamesMicrosoftGraph(t *testing.T) {
	const entra = "https://login.microsoftonline.com/t/v2.0"
	require.Equal(t, "https://graph.microsoft.com", graphBase(entra, ""))
	require.Equal(t, "https://graph.microsoft.com", graphBase(entra, "https://graph.windows.net/t/users/o/getMemberObjects"))
	require.Equal(t, "https://graph.microsoft.com", graphBase(entra, "https://attacker.example/steal"))
	require.Equal(t, "https://graph.microsoft.us", graphBase("https://login.microsoftonline.us/t/v2.0", "https://graph.microsoft.us/v1.0/x"))
	require.Equal(t, "", graphBase("https://example.okta.com", "https://graph.microsoft.com/v1.0/x"))
}

func TestOverageIsOnlyAClaimLeftOut(t *testing.T) {
	over, _ := overage(map[string]any{"groups": []any{}, "hasgroups": true}, "groups")
	require.False(t, over, "a groups claim that is present is the answer, even empty")
	over, _ = overage(map[string]any{"hasgroups": true}, "teams")
	require.False(t, over, "hasgroups is about the groups claim only")
	over, endpoint := overage(map[string]any{"_claim_names": map[string]any{"teams": "s"},
		"_claim_sources": map[string]any{"s": map[string]any{"endpoint": "https://e"}}}, "teams")
	require.True(t, over)
	require.Equal(t, "https://e", endpoint)
}
