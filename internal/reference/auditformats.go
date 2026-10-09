package reference

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/audit/ocsf"
)

// AuditFormatsMarkdown is docs/audit-formats.md: the formats an audit stream,
// export or archive is written in (design 12 §6, R-384). The native columns
// come from audit.Line and the OCSF mapping from the ocsf package, so the page
// describes what the encoder does.
func AuditFormatsMarkdown() string {
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("# Audit event formats\n\n")
	b.WriteString("An audit stream or export writes one JSON object per line, in `native` or `ocsf` format.\n")
	b.WriteString("The archive is always native.\n\n")

	b.WriteString("## Native\n\n")
	b.WriteString("A native line is the `audit_events` row as Postgres's `row_to_json` writes it — the same\n")
	b.WriteString("shape the archive stores. Every column is present, and an unset one is `null`. The columns are\n")
	b.WriteString(list(lineColumns(), "`%s`") + ".\n\n")
	b.WriteString("A line also carries `txid`, the writing transaction's ID, which orders the stream (design 12\n")
	b.WriteString("§2). `schema_version` is `2` for events written since Pando recorded outcome, client address\n")
	b.WriteString("and actor name, and `null` before; read `null` as `1`. On a version 1 line, `outcome`,\n")
	b.WriteString("`source_ip`, `peer_ip`, `user_agent`, `actor_name` and `actor_email` are `null`. `detail` never\n")
	b.WriteString("holds a secret value (R-194).\n\n")

	fmt.Fprintf(&b, "## OCSF\n\nOCSF %s. One event per line. Fields absent from a line are omitted, not `null`.\n\n", ocsf.SchemaVersion)
	b.WriteString("### Base fields\n\n")
	b.WriteString("| OCSF | From |\n| --- | --- |\n")
	for _, f := range ocsf.BaseFields {
		names := strings.Split(f.OCSF, ", ")
		for i, n := range names {
			names[i] = "`" + n + "`"
		}
		fmt.Fprintf(&b, "| %s | %s |\n", strings.Join(names, ", "), f.From)
	}

	b.WriteString("\n### Process Activity fields\n\n")
	b.WriteString("A terminal session (`app.exec`, `app.exec.end`) is Process Activity, which also carries:\n\n")
	b.WriteString("| OCSF | From |\n| --- | --- |\n")
	for _, f := range ocsf.ProcessFields {
		fmt.Fprintf(&b, "| `%s` | %s |\n", f.OCSF, f.From)
	}

	b.WriteString("\n### Classes\n\n")
	b.WriteString("Each action's OCSF class and activity. A denied or failed action is the same activity as\n")
	b.WriteString("the attempt, with `status_id` 2. An action not listed here — one written by a newer Pando —\n")
	b.WriteString("is encoded as 6003 API Activity, 99 Other.\n\n")
	b.WriteString("| Action | Class | Activity | `type_uid` |\n| --- | --- | --- | --- |\n")
	for _, r := range ocsf.Table() {
		fmt.Fprintf(&b, "| `%s` | %d %s | %d %s | %d |\n",
			r.Action, r.ClassUID, r.ClassName, r.ActivityID, r.ActivityName, r.TypeUID())
	}
	return b.String()
}

// lineColumns is audit.Line's JSON names, in declaration order.
func lineColumns() []string {
	t := reflect.TypeOf(audit.Line{})
	cols := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			cols = append(cols, name)
		}
	}
	return cols
}
