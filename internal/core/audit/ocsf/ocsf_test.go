package ocsf

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
)

// The OCSF 1.3.0 captions the table may use, by class and activity, so a typo
// in a name or a wrong number in one entry fails here rather than in a SIEM.
var known = map[int]struct {
	name       string
	activities map[int]string
}{
	1007: {"Process Activity", map[int]string{1: "Launch", 2: "Terminate", 99: "Other"}},
	3001: {"Account Change", map[int]string{1: "Create", 2: "Enable", 3: "Password Change", 4: "Password Reset", 5: "Disable", 6: "Delete", 99: "Other"}},
	3002: {"Authentication", map[int]string{1: "Logon", 2: "Logoff", 99: "Other"}},
	3005: {"User Access Management", map[int]string{1: "Assign Privileges", 2: "Revoke Privileges", 99: "Other"}},
	3006: {"Group Management", map[int]string{1: "Assign Privileges", 2: "Revoke Privileges", 3: "Add User", 4: "Remove User", 5: "Delete", 6: "Create", 99: "Other"}},
	6002: {"Application Lifecycle", map[int]string{1: "Install", 2: "Remove", 3: "Start", 4: "Stop", 5: "Restart", 8: "Update", 99: "Other"}},
	6003: {"API Activity", map[int]string{1: "Create", 2: "Read", 3: "Update", 4: "Delete", 99: "Other"}},
	6004: {"Web Resource Access Activity", map[int]string{1: "Access Grant", 2: "Access Deny", 99: "Other"}},
}

// TestR384_EveryActionMapsToOCSF asserts R-384: every catalogued action has an
// explicit row, every row is a catalogued action, and every row is a valid
// OCSF 1.3.0 class and activity.
func TestR384_EveryActionMapsToOCSF(t *testing.T) {
	for _, action := range audit.Actions {
		m, ok := classes[action]
		if !assert.True(t, ok, "%s has no OCSF mapping; add a row to classes", action) {
			continue
		}
		k, ok := known[m.ClassUID]
		require.True(t, ok, "%s: class %d is not one design 12 §6.2 uses", action, m.ClassUID)
		assert.Equal(t, k.name, m.ClassName, action)
		assert.Equal(t, k.activities[m.ActivityID], m.ActivityName, "%s: activity %d", action, m.ActivityID)
		assert.Contains(t, []int{1, 3, 6}, m.CategoryUID(), action)
		assert.Equal(t, m.ClassUID/1000, m.CategoryUID(), action)
		assert.NotEmpty(t, m.CategoryName(), action)
		assert.Equal(t, m.ClassUID*100+m.ActivityID, m.TypeUID(), action)
	}
	for action := range classes {
		assert.True(t, slices.Contains(audit.Actions, action),
			"%s is mapped but is not in audit.Actions; remove its row", action)
	}
	assert.Len(t, Table(), len(audit.Actions))
	assert.True(t, sort.SliceIsSorted(Table(), func(i, j int) bool { return Table()[i].Action < Table()[j].Action }))
}

// realistic is a line as row_to_json writes it: every column, nulls where
// unset, the txid the stream reads, and a timestamp with Postgres's offset.
const realistic = `{
	"id": 4211,
	"occurred_at": "2026-10-08T12:00:00.123456+00:00",
	"principal_kind": "token",
	"principal_id": "tok_01J9ZX",
	"on_behalf_of": "usr_01J9AB",
	"action": "grant.create",
	"app_id": "app_01HQ8X",
	"target_kind": "user",
	"target_id": "usr_01J9CD",
	"request_id": "req_01JA00",
	"detail": {"role": "app.operator", "count": 9007199254740993},
	"txid": "123",
	"schema_version": 2,
	"outcome": null,
	"source_ip": "203.0.113.7",
	"peer_ip": "10.0.0.2",
	"user_agent": "curl/8.7.1",
	"actor_name": "Ada Lovelace",
	"actor_email": "ada@example.com"
}`

func encode(t *testing.T, line string) map[string]any {
	t.Helper()
	out, err := Encode(json.RawMessage(line))
	require.NoError(t, err)
	var got map[string]any
	d := json.NewDecoder(strings.NewReader(string(out)))
	d.UseNumber()
	require.NoError(t, d.Decode(&got))
	return got
}

// TestR384_OCSFBaseFields asserts R-384: a line encodes to the base fields of
// design 12 §6.2.
func TestR384_OCSFBaseFields(t *testing.T) {
	got := encode(t, realistic)

	n := func(v any) string { return v.(json.Number).String() }
	assert.Equal(t, "3005", n(got["class_uid"]))
	assert.Equal(t, "User Access Management", got["class_name"])
	assert.Equal(t, "3", n(got["category_uid"]))
	assert.Equal(t, "Identity & Access Management", got["category_name"])
	assert.Equal(t, "1", n(got["activity_id"]))
	assert.Equal(t, "Assign Privileges", got["activity_name"])
	assert.Equal(t, "300501", n(got["type_uid"]))
	assert.Equal(t, "User Access Management: Assign Privileges", got["type_name"])
	assert.Equal(t, "1791460800123", n(got["time"]))
	assert.Equal(t, "1", n(got["severity_id"]))
	assert.Equal(t, "Informational", got["severity"])
	assert.Equal(t, "1", n(got["status_id"]))
	assert.Equal(t, "Success", got["status"])
	assert.NotContains(t, got, "status_detail")
	assert.Equal(t, "grant.create by Ada Lovelace on user usr_01J9CD", got["message"])

	md := got["metadata"].(map[string]any)
	assert.Equal(t, "4211", md["uid"])
	assert.Equal(t, SchemaVersion, md["version"])
	assert.Equal(t, map[string]any{"name": "Pando", "vendor_name": "Pando", "version": ProductVersion}, md["product"])
	assert.Equal(t, "req_01JA00", md["correlation_uid"])
	assert.Equal(t, "audit", md["log_name"])

	actor := got["actor"].(map[string]any)
	assert.Equal(t, "usr_01J9AB", actor["invoked_by"])
	assert.Equal(t, map[string]any{
		"uid": "tok_01J9ZX", "type_id": json.Number("99"), "type": "Token",
		"name": "Ada Lovelace", "email_addr": "ada@example.com",
	}, actor["user"])

	assert.Equal(t, map[string]any{"ip": "203.0.113.7"}, got["src_endpoint"])
	assert.Equal(t, map[string]any{"user_agent": "curl/8.7.1"}, got["http_request"])
	assert.Equal(t, map[string]any{"operation": "grant.create"}, got["api"])
	assert.Equal(t, []any{
		map[string]any{"type": "user", "uid": "usr_01J9CD"},
		map[string]any{"type": "app", "uid": "app_01HQ8X"},
	}, got["resources"])

	um := got["unmapped"].(map[string]any)
	assert.Equal(t, map[string]any{"role": "app.operator", "count": json.Number("9007199254740993")}, um["detail"],
		"detail is carried whole and exactly, large integers included")
	assert.Equal(t, "10.0.0.2", um["peer_ip"])
	assert.Equal(t, "2", n(um["schema_version"]))
	assert.Equal(t, "token", um["principal_kind"])
	assert.Equal(t, "usr_01J9AB", um["on_behalf_of"])

	// Every top-level field is one BaseFields documents, and the other way
	// round, so docs/audit-formats.md and Encode cannot disagree.
	documented := map[string]bool{}
	for _, f := range BaseFields {
		for _, name := range strings.Split(f.OCSF, ", ") {
			documented[strings.SplitN(name, ".", 2)[0]] = true
		}
	}
	for k := range got {
		assert.True(t, documented[k], "%s is encoded and not in BaseFields", k)
	}
	// status_detail is set only when the action did not succeed.
	denied := encode(t, strings.Replace(realistic, `"outcome": null`, `"outcome": "denied"`, 1))
	for k := range documented {
		assert.Contains(t, denied, k, "%s is in BaseFields and not encoded", k)
	}
}

// TestR384_OCSFOutcomes asserts R-384: denials and failures are failures, at
// the severities design 12 §6.2 gives them; a row from before R-379 derives
// its outcome from its action.
func TestR384_OCSFOutcomes(t *testing.T) {
	line := func(action, outcome string) string {
		return `{"id":1,"occurred_at":"2026-10-08T12:00:00Z","principal_kind":"anonymous","action":"` +
			action + `","outcome":` + outcome + `}`
	}
	denied := encode(t, line("session.denied", "null"))
	assert.Equal(t, json.Number("3"), denied["severity_id"])
	assert.Equal(t, json.Number("2"), denied["status_id"])
	assert.Equal(t, "denied", denied["status_detail"])
	assert.Equal(t, "Authentication: Logon", denied["type_name"])

	failed := encode(t, line("deploy.finish", `"failed"`))
	assert.Equal(t, json.Number("4"), failed["severity_id"])
	assert.Equal(t, "High", failed["severity"])
	assert.Equal(t, "failed", failed["status_detail"])
}

// TestR384_OCSFSparseLine asserts R-384 for a schema-version-1 line: no actor
// for an anonymous principal, empty fields omitted, the target read as the
// writer now fills it in, and an action this version does not know encoded
// rather than refused.
func TestR384_OCSFSparseLine(t *testing.T) {
	got := encode(t, `{"id":7,"occurred_at":"2025-01-01T00:00:00Z","principal_kind":"anonymous",
		"principal_id":null,"on_behalf_of":null,"action":"app.teleport","app_id":"app_X",
		"target_kind":null,"target_id":null,"request_id":null,"detail":null}`)
	assert.NotContains(t, got, "actor")
	assert.NotContains(t, got, "src_endpoint")
	assert.NotContains(t, got, "http_request")
	assert.NotContains(t, got["metadata"], "correlation_uid")
	assert.Equal(t, json.Number("600399"), got["type_uid"])
	assert.Equal(t, "app.teleport by anonymous on app app_X", got["message"])
	assert.Equal(t, []any{map[string]any{"type": "app", "uid": "app_X"}}, got["resources"])
	assert.Equal(t, map[string]any{"schema_version": json.Number("1"), "principal_kind": "anonymous"}, got["unmapped"])
}

// TestR194_OCSFAddsNothingToTheLine asserts R-194: the encoded event holds only
// what the line held. A detail value appears once, under unmapped.detail, and
// every string in the event is a value from the line or a fixed caption.
func TestR194_OCSFAddsNothingToTheLine(t *testing.T) {
	const marker = "detail-value-7f3a"
	line := strings.Replace(realistic, `"app.operator"`, `"`+marker+`"`, 1)
	out, err := Encode(json.RawMessage(line))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(out), marker))

	got := encode(t, line)
	assert.Equal(t, marker, got["unmapped"].(map[string]any)["detail"].(map[string]any)["role"])

	var source map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &source))
	fromLine := map[string]bool{}
	for _, v := range source {
		if s, ok := v.(string); ok {
			fromLine[s] = true
		}
	}
	fixed := map[string]bool{
		"Pando": true, "audit": true, SchemaVersion: true, ProductVersion: true, "Token": true,
		"User Access Management": true, "Identity & Access Management": true, "Assign Privileges": true,
		"User Access Management: Assign Privileges": true, "Informational": true, "Success": true,
		"4211": true, "app": true, "grant.create by Ada Lovelace on user usr_01J9CD": true,
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, c := range v {
				if path+k == "unmapped.detail" {
					continue
				}
				walk(path+k+".", c)
			}
		case []any:
			for _, c := range v {
				walk(path, c)
			}
		case string:
			assert.True(t, fromLine[v] || fixed[v], "%s = %q is not from the line", path, v)
		}
	}
	walk("", got)
}

// TestR384_ATerminalSessionCarriesItsProcess asserts the process object
// Process Activity requires: the command and the workload, for the start and
// the end of a session, and a name for a session with no command.
func TestR384_ATerminalSessionCarriesItsProcess(t *testing.T) {
	start := encode(t, `{"id": 7, "occurred_at": "2026-10-08T12:00:00Z", "principal_kind": "user",
		"principal_id": "usr_1", "action": "app.exec", "app_id": "app_1", "target_kind": "workload",
		"target_id": "web", "detail": {"command": ["sh", "-c", "id"], "workload": "web"}}`)
	assert.Equal(t, "1007", fmt.Sprint(start["class_uid"]))
	proc := start["process"].(map[string]any)
	assert.Equal(t, "sh", proc["name"])
	assert.Equal(t, "sh -c id", proc["cmd_line"])
	assert.Equal(t, map[string]any{"name": "web"}, proc["container"])

	end := encode(t, `{"id": 8, "occurred_at": "2026-10-08T12:01:00Z", "principal_kind": "user",
		"principal_id": "usr_1", "action": "app.exec.end", "detail": {"workload": "web", "reason": "client_closed"}}`)
	assert.Equal(t, "2", fmt.Sprint(end["activity_id"]))
	assert.Equal(t, "shell", end["process"].(map[string]any)["name"])

	other := encode(t, `{"id": 9, "occurred_at": "2026-10-08T12:01:00Z", "principal_kind": "system",
		"principal_id": "system", "action": "app.restart", "detail": {"command": ["x"]}}`)
	assert.NotContains(t, other, "process", "only Process Activity carries a process")
}
