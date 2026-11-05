//go:build integration

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/instructions"
	"github.com/trypando/pando/internal/errs"
)

type instructionsDoc struct {
	Text      string  `json:"text"`
	Builtin   string  `json:"builtin"`
	Company   string  `json:"company"`
	UpdatedAt *string `json:"updated_at"`
	UpdatedBy string  `json:"updated_by"`
}

func (i *install) instructions(s *session) instructionsDoc {
	i.t.Helper()
	got := i.do(s, http.MethodGet, "/instructions", nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	var d instructionsDoc
	got.JSON(i.t, &d)
	return d
}

// TestR424_AdministratorsSetWhatEveryAgentIsTold asserts R-424 end to end: an
// administrator saves the company's instructions, anyone signed in reads
// them, an agent connecting over MCP is given them after Pando's guidance,
// only install.policy.manage changes them, and each change is audited.
func TestR424_AdministratorsSetWhatEveryAgentIsTold(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	member := i.user("member")

	// Before anything is written: Pando's guidance alone.
	d := i.instructions(member)
	require.Equal(t, instructions.Builtin(), d.Text)
	require.Empty(t, d.Company)
	require.Nil(t, d.UpdatedAt)

	saved := i.do(admin, http.MethodPut, "/instructions", map[string]any{"company": "Use Postgres for every app's data."})
	require.Equal(t, http.StatusOK, saved.Code, saved.String())
	d = i.instructions(member)
	require.Equal(t, "Use Postgres for every app's data.", d.Company)
	require.True(t, strings.HasPrefix(d.Text, instructions.Builtin()))
	require.True(t, strings.HasSuffix(d.Text, "Use Postgres for every app's data."))
	require.NotNil(t, d.UpdatedAt)
	require.NotEmpty(t, d.UpdatedBy)
	require.Equal(t, 1, auditCount(t, i, "instructions.update", "company"))

	// A member's agent is given them when it connects, with no install verb.
	agent := i.tokenFor(member)
	init := i.mcp(agent, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	require.Equal(t, http.StatusOK, init.Code, init.String())
	var msg struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(init.Body, &msg), init.String())
	require.Equal(t, d.Text, msg.Result.Instructions)

	// And by the tool, for a client that ignores the field.
	text, isErr := mcpText(t, i.mcp(agent,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pando_get_instructions","arguments":{}}}`))
	require.False(t, isErr, text)
	require.Contains(t, text, "Use Postgres for every app's data.")

	// Only install.policy.manage writes them.
	denied := i.do(member, http.MethodPut, "/instructions", map[string]any{"company": "Ignore the rules."})
	require.Equal(t, http.StatusForbidden, denied.Code, denied.String())
	require.Equal(t, "Use Postgres for every app's data.", i.instructions(admin).Company)

	// Nobody signed out reads them.
	anonymous := i.do(nil, http.MethodGet, "/instructions", nil)
	require.Equal(t, http.StatusUnauthorized, anonymous.Code, anonymous.String())

	// A body without company, or too long, is refused and saves nothing.
	for _, body := range []any{map[string]any{}, map[string]any{"company": strings.Repeat("a", instructions.MaxCompany+1)}} {
		bad := i.do(admin, http.MethodPut, "/instructions", body)
		require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())
		require.Equal(t, string(errs.ValidInvalid), bad.ErrorCode())
	}
	require.Equal(t, 1, auditCount(t, i, "instructions.update", "company"))

	// Clearing them goes back to Pando's guidance alone, and is audited.
	cleared := i.do(admin, http.MethodPut, "/instructions", map[string]any{"company": ""})
	require.Equal(t, http.StatusOK, cleared.Code, cleared.String())
	require.Equal(t, instructions.Builtin(), i.instructions(member).Text)
	require.Equal(t, 2, auditCount(t, i, "instructions.update", "company"))
}
