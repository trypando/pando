//go:build integration

package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	corehosts "github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/errs"
)

// TestR415_AControlRoleJoinTokenNeedsEveryInstallVerb asserts R-415 and
// R-080a: a control host is handed every key the installation has, so minting
// its token needs every install verb, while install.cluster.manage alone mints
// one for the app role.
func TestR415_AControlRoleJoinTokenNeedsEveryInstallVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	ops, _ := i.holding("ops", authz.InstallClusterManage)

	requireMissingVerbs(t, i.do(ops, http.MethodPost, "/cluster/join-tokens", map[string]any{"roles": []string{"control"}}))
	requireMissingVerbs(t, i.do(ops, http.MethodPost, "/cluster/join-tokens", map[string]any{}))

	app := i.do(ops, http.MethodPost, "/cluster/join-tokens", map[string]any{"roles": []string{"app"}})
	require.Equal(t, http.StatusCreated, app.Code, app.String())

	both := i.do(i.admin(), http.MethodPost, "/cluster/join-tokens", map[string]any{"roles": []string{"control", "app"}, "max_uses": 2, "expires_hours": 2})
	require.Equal(t, http.StatusCreated, both.Code, both.String())
	var out struct {
		Token struct {
			ID      string   `json:"id"`
			Roles   []string `json:"roles"`
			MaxUses int      `json:"max_uses"`
		} `json:"token"`
		Join string `json:"join"`
	}
	both.JSON(t, &out)
	require.Equal(t, []string{"app", "control"}, out.Token.Roles)
	require.Equal(t, 2, out.Token.MaxUses)
	d, err := corehosts.Decode(out.Join)
	require.NoError(t, err)
	require.Equal(t, "https://pando.example.com", d.Address)

	// Listed without the token itself.
	list := i.do(ops, http.MethodGet, "/cluster/join-tokens", nil)
	require.Equal(t, http.StatusOK, list.Code, list.String())
	require.Contains(t, list.String(), out.Token.ID)
	require.NotContains(t, list.String(), strings.Split(out.Join, ".")[4])

	revoked := i.do(ops, http.MethodDelete, "/cluster/join-tokens/"+out.Token.ID, nil)
	require.Equal(t, http.StatusNoContent, revoked.Code, revoked.String())
	missing := i.do(ops, http.MethodDelete, "/cluster/join-tokens/jt_nobody", nil)
	require.Equal(t, string(errs.NotFound), missing.ErrorCode(), missing.String())

	audit := i.do(i.admin(), http.MethodGet, "/audit?action=cluster.join_token.create", nil)
	require.Equal(t, http.StatusOK, audit.Code, audit.String())
	require.Contains(t, audit.String(), out.Token.ID)
}

// TestR414_TheClusterNeedsItsOwnVerb asserts R-414 and R-080: listing hosts and
// tokens is install.cluster.manage's, not anybody's who can see the install.
func TestR414_TheClusterNeedsItsOwnVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	viewer, _ := i.holding("viewer", authz.InstallView)

	for _, path := range []string{"/cluster/hosts", "/cluster/join-tokens"} {
		got := i.do(viewer, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, got.Code, got.String())
	}
	removed := i.do(viewer, http.MethodDelete, "/cluster/hosts/host_x", nil)
	require.Equal(t, http.StatusForbidden, removed.Code, removed.String())

	hostsList := i.do(i.admin(), http.MethodGet, "/cluster/hosts", nil)
	require.Equal(t, http.StatusOK, hostsList.Code, hostsList.String())
	require.JSONEq(t, `{"hosts":[]}`, hostsList.String())
	gone := i.do(i.admin(), http.MethodDelete, "/cluster/hosts/host_nobody", nil)
	require.Equal(t, string(errs.NotFound), gone.ErrorCode(), gone.String())
}
