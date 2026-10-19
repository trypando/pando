package forgekit_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// TestSSHKeyStepWritesTheKeyItNames asserts the key step's commands and
// paste line name the same file, default to an Ed25519 key, and ask for the
// host keys only when told how to scan them.
func TestSSHKeyStepWritesTheKeyItNames(t *testing.T) {
	plain := forgekit.SSHKeyStep(forgekit.SSHKey{Label: "Deploy key", Title: "Add a deploy key", File: "pando-deploy",
		Add: []string{"Add it."}})
	require.Equal(t, "ssh", plain.ID)
	require.Equal(t, &api.Condition{Key: "method", Values: []string{forgekit.MethodSSH}}, plain.ShownWhen)
	require.Equal(t, []api.Action{
		{Text: "Run `ssh-keygen -t ed25519 -f pando-deploy -N \"\"`."},
		{Text: "Add it."},
		{Text: "Paste `pando-deploy` below."},
	}, plain.Do)
	require.Equal(t, []string{forgekit.FieldSSHPrivateKey, forgekit.FieldSSHPassphrase, "known_hosts"}, plain.Fields)

	scanned := forgekit.SSHKeyStep(forgekit.SSHKey{File: "k", Type: "-t rsa -b 3072", KeyScan: forgekit.KeyScanAnyHost})
	require.Equal(t, "Run `ssh-keygen -t rsa -b 3072 -f k -N \"\"`.", scanned.Do[0].Text)
	require.Equal(t, "Run `ssh-keyscan` with the host's name, on a machine you trust, and paste its output below.",
		scanned.Do[len(scanned.Do)-1].Text)
}

// TestSharedForgeSteps asserts the method, scope and OAuth steps ask the
// settings the forges' Info declares.
func TestSharedForgeSteps(t *testing.T) {
	require.Equal(t, []string{"method", "host"}, forgekit.MethodStep().Fields)
	scope := forgekit.ScopeStep("Owner", "Choose an owner")
	require.Equal(t, api.Step{ID: "scope", Label: "Owner", Title: "Choose an owner", Fields: []string{"scope"}}, scope)

	links := []api.Link{{Label: "Open", URL: "https://{host}/apps"}}
	oauth := forgekit.OAuthStep("OAuth app", "Register it", links, "Open it.", forgekit.SetCallback("Redirect URI"))
	require.Equal(t, []string{"client_id", forgekit.FieldClientSecret}, oauth.Fields)
	require.Equal(t, links, oauth.Links)
	require.Equal(t, "Set **Redirect URI** to your Pando address plus `/api/v1/sources/callback`.", oauth.Do[1].Text)
	require.Equal(t, []string{forgekit.MethodOAuth}, oauth.ShownWhen.Values)
}
