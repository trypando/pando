//go:build integration

// The CLI's half of R-058 and R-060 (issue #396): minting tokens, and sharing
// an app with a service token, all from `pando`.
package acceptance_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/cli"
)

// pando runs one CLI command in process against the stack as the holder of
// token (none when empty), with stdin supplied, and returns what it printed to
// standard output and to standard error.
func pando(t *testing.T, token, stdin string, args ...string) (string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(cli.EnvServer, strings.TrimSuffix(baseURL(), "/api/v1"))
	t.Setenv(cli.EnvToken, token)

	for _, c := range cli.Commands() {
		if c.Name() != args[0] {
			continue
		}
		var out, errOut bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&errOut)
		c.SetIn(strings.NewReader(stdin))
		c.SetArgs(args[1:])
		c.SilenceUsage, c.SilenceErrors = true, true
		require.NoError(t, c.Execute(), "pando %s\n%s", strings.Join(args, " "), errOut.String())
		return out.String(), errOut.String()
	}
	t.Fatalf("no %q command", args[0])
	return "", ""
}

var tokenID = regexp.MustCompile(`tok_[0-9A-Za-z]+`)

// createToken runs pando token create, approved with the administrator's
// password, and returns the token's ID and secret. The token is revoked when
// the test ends.
func createToken(t *testing.T, c *client, args ...string) (id, secret string) {
	t.Helper()
	args = append(append([]string{"token", "create"}, args...), "--username", "admin")
	out, errOut := pando(t, "", adminPassword+"\n", args...)
	id = tokenID.FindString(errOut)
	require.NotEmpty(t, id, errOut)
	secret = strings.TrimSpace(out)
	require.NotEmpty(t, secret)
	t.Cleanup(func() { c.do(t, http.MethodDelete, "/tokens/"+id, "") })
	return id, secret
}

// letTokensShareApps lifts host policy's default denial of app.grants.manage
// to tokens for one test, so the CLI can share an app, and restores the
// policy afterwards.
func letTokensShareApps(t *testing.T, c *client) {
	t.Helper()
	original, status := c.do(t, http.MethodGet, "/policy", "")
	require.Equal(t, http.StatusOK, status, original)

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(original), &doc))
	var kept []any
	if verbs, ok := doc["agent_disabled_verbs"].([]any); ok {
		for _, v := range verbs {
			if v != "app.grants.manage" {
				kept = append(kept, v)
			}
		}
	}
	doc["agent_disabled_verbs"] = kept
	lifted, err := json.Marshal(doc)
	require.NoError(t, err)

	body, status := c.do(t, http.MethodPut, "/policy", string(lifted))
	require.Equal(t, http.StatusOK, status, body)
	t.Cleanup(func() {
		body, status := c.do(t, http.MethodPut, "/policy", original)
		require.Equal(t, http.StatusOK, status, body)
	})
}

// bearer is a request to an app carrying only a token: no session cookie.
func bearer(t *testing.T, at, secret string) (map[string]any, int) {
	t.Helper()
	return throughProxy(t, nil, at, "/", map[string]string{"Authorization": "Bearer " + secret})
}

func assertionClaims(t *testing.T, headers map[string]string) map[string]any {
	t.Helper()
	parts := strings.Split(headers["x-pando-assertion"], ".")
	require.Len(t, parts, 3, "an assertion is a three-part JWT")
	return decodeSegment(t, parts[1])
}

// TestR060_AServiceTokenFromTheCLIReachesAnAppSharedWithIt asserts R-060
// through the CLI end to end: pando token create --service mints a service
// token that holds nothing, pando token list --service shows it, pando grant
// add --token shares an app with it, and it then reaches the app through the
// proxy as its own principal, its Authorization header removed before the app
// sees the request (R-053).
func TestR060_AServiceTokenFromTheCLIReachesAnAppSharedWithIt(t *testing.T) {
	c := login(t)
	appID, at := deployEcho(t, c, "cli-service-token-"+stamp(), 9013)
	letTokensShareApps(t, c)

	// The CLI's own credential, as pando login would store it.
	_, cliToken := createToken(t, c, "acceptance CLI "+stamp())
	name := "acceptance agent " + stamp()
	id, secret := createToken(t, c, name, "--service")

	list, _ := pando(t, cliToken, "", "token", "list", "--service")
	require.Contains(t, list, id)
	require.Contains(t, list, name)

	_, status := bearer(t, at, secret)
	require.NotEqual(t, http.StatusOK, status, "a fresh service token holds nothing")

	pando(t, cliToken, "", "grant", "add", appID, "--token", id)

	echoed, status := bearer(t, at, secret)
	require.Equal(t, http.StatusOK, status, "%v", echoed)
	headers := echoedHeaders(t, echoed)
	require.Empty(t, headers["authorization"], "the token is not forwarded to the app")
	claims := assertionClaims(t, headers)
	require.Equal(t, id, claims["sub"], "the token is its own principal")
	require.Equal(t, appID, claims["aud"])
}

// TestR058_ADelegatedTokenFromTheCLIActsAsItsOwner asserts R-058 through the
// CLI: pando token create mints a token that reaches its owner's app as the
// owner.
func TestR058_ADelegatedTokenFromTheCLIActsAsItsOwner(t *testing.T) {
	c := login(t)
	_, at := deployEcho(t, c, "cli-delegated-token-"+stamp(), 9012)
	adminID := c.get(t, "/me")["user_id"].(string)

	_, secret := createToken(t, c, "acceptance laptop "+stamp())

	echoed, status := bearer(t, at, secret)
	require.Equal(t, http.StatusOK, status, "%v", echoed)
	headers := echoedHeaders(t, echoed)
	require.Empty(t, headers["authorization"])
	require.Equal(t, adminID, assertionClaims(t, headers)["sub"], "a delegated token acts as its owner")
}
