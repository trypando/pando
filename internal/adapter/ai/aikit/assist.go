package aikit

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
)

// The administrative functions (R-343 … R-346), as tasks any provider can run.
// Each has one tool the model answers through, because each has one answer: a
// draft, a filter, a summary, an answer. Drafting access and searching the
// audit log may also have the lookup tools (O-54), which is the only loop
// these have; nothing is read from a repository.
//
// Every value these return is checked again by core. The schemas below are
// how the model is told what shape to answer in, not what Pando accepts.

// Task is one administrative request: what the model is told, what it is
// asked, and the tool it answers through.
type Task struct {
	System string
	User   string
	Tool   Tool

	// Lookup, when set, is what the lookup tools run (O-54).
	Lookup *api.Lookup
}

// Tools are the task's tools: the one it answers through, then the lookups.
func (t Task) Tools() []Tool {
	return append([]Tool{t.Tool}, LookupTools(t.Lookup)...)
}

// listsNote says what the lists in a prompt are, now that none is whole.
func listsNote(l *api.Lookup) string {
	if l != nil {
		return " " + LookupNote
	}
	return " " + MatchNote
}

// Voice is the house style every answer here is held to (R-105), said once.
const Voice = "Write for the person who asked, in plain sentences. Be specific and self-contained: " +
	"someone reading only your answer should understand it. No apology, no exclamation mark, " +
	"no \"Error:\" prefix, and do not restate the question."

// AnswerThroughTool is added to every task's system prompt. The call is asked
// for rather than forced: current models refuse forced tool use.
const AnswerThroughTool = " Answer only by calling the submit tool, once. Do not answer in prose."

func submitTool(properties map[string]any, required []string) Tool {
	return Tool{
		Name:        ToolSubmit,
		Description: "Submit your answer. Call this exactly once; it is the only way to answer.",
		Properties:  properties,
		Required:    required,
	}
}

// AccessTask drafts a role and, optionally, a group (R-343).
func AccessTask(req api.AccessRequest) Task {
	system := "You draft access for Pando, a self-hosted app platform, from an administrator's description. " +
		"You may draft one custom role, one group, or both. A role has a scope, install or app, and holds " +
		"verbs of that scope only — never a mix. Use only verbs from the catalog given, by exact name. " +
		"Never reuse the name of an existing role or group. Group members are account IDs; " +
		"leave out anyone you cannot identify and say so. If the description asks for something the " +
		"catalog cannot express, draft what it can and say what it cannot. You draft; an administrator " +
		"reviews and creates." + listsNote(req.Lookup) + " " + Voice

	user := "## Description\n\n" + req.Description +
		"\n\n## Verb catalog\n\n" + JSONBlock(req.Verbs) +
		"\n\n## Existing roles\n\n" + JSONBlock(req.Roles) +
		"\n\n## Existing groups\n\n" + JSONBlock(req.Groups) +
		"\n\n## Accounts\n\n" + JSONBlock(req.People)
	if req.Current != nil {
		user = "## The draft so far\n\nA person is refining this draft; some of it they may have changed by hand. " +
			"The description below is the change they want. Submit the whole draft again with that change made, " +
			"and keep everything else as it is.\n\n" + JSONBlock(req.Current) + "\n\n" + user
	}

	verbs := make([]string, 0, len(req.Verbs))
	for _, v := range req.Verbs {
		verbs = append(verbs, v.Name)
	}
	verbItems := map[string]any{"type": "string"}
	if len(verbs) > 0 {
		verbItems["enum"] = verbs
	}

	schema := map[string]any{
		"role": map[string]any{
			"type":        "object",
			"description": "The role to create. Omit when the description needs no new role.",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "description": "A short name, such as \"Release manager\"."},
				"scope": map[string]any{"type": "string", "enum": []string{"install", "app"}},
				"verbs": map[string]any{"type": "array", "items": verbItems},
			},
			"required": []string{"name", "scope", "verbs"},
		},
		"group": map[string]any{
			"type":        "object",
			"description": "The group to create. Omit when the description names no group of people.",
			"properties": map[string]any{
				"name":    map[string]any{"type": "string"},
				"members": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Account IDs."},
			},
			"required": []string{"name"},
		},
		"reply": map[string]any{"type": "string", "description": "One to three sentences: what you drafted and why, and anything you could not draft."},
	}

	return Task{System: system, User: user, Tool: submitTool(schema, []string{"reply"}), Lookup: req.Lookup}
}

// PolicyTask proposes changes to host policy (R-344).
func PolicyTask(req api.PolicyRequest) Task {
	system := "You propose changes to the host policy of Pando, a self-hosted app platform, from an " +
		"administrator's description. Propose only fields from the list given, with values of the " +
		"field's type. Each field says what it does; find the setting the person means by that. A field " +
		"marked fixed is set in Pando's startup configuration and cannot be changed here: never propose " +
		"it, and say in your reply that it is fixed. Change nothing the description does not ask for. " +
		"In your reply, name each setting the way its meaning does (\"Turned off terminal access for the " +
		"whole installation\"), not by its field name. You propose; an administrator reviews and saves. " + Voice

	user := "## Description\n\n" + req.Description +
		"\n\n## Current policy\n\n```json\n" + string(req.Current) + "\n```" +
		"\n\n## Fields\n\n" + JSONBlock(req.Fields) +
		"\n\n## Verbs, for disabled_verbs\n\n" + JSONBlock(req.Verbs)
	if len(req.Draft) > 0 {
		user += "\n\n## The draft so far\n\nA person is refining a proposal. This is the policy with the " +
			"changes they have kept so far. The description is the change they want now: propose changes " +
			"relative to this draft, and only for what the description asks.\n\n```json\n" + string(req.Draft) + "\n```"
	}

	schema := map[string]any{
		"changes": map[string]any{
			"type": "object",
			"description": "Each field to change, keyed by field name, with its new value. " +
				"An empty object when nothing should change.",
		},
		"reply": map[string]any{"type": "string", "description": "One to three sentences: what you changed and why, and anything you did not change."},
	}

	return Task{System: system, User: user, Tool: submitTool(schema, []string{"changes", "reply"})}
}

// AuditSearchTask turns a question into one audit filter (R-345).
func AuditSearchTask(req api.AuditSearchRequest) Task {
	system := "You turn a question about Pando's audit log into one filter. Resolve people to account " +
		"IDs by username, name or email, and apps to app IDs; when the question " +
		"names who did something, set principal_id to that account's ID; resolve relative times against the current time " +
		"given, in UTC, and write times in RFC 3339. Actions are names or prefixes from the list given; " +
		"any of several matches. Leave a field out rather than guess it. Using an app is recorded as " +
		"app.use, once per visit (a browser session, or a token's use within twelve hours), and a refused " +
		"use as app.use.denied; for what someone accessed or used, filter on app.use. Visitors who were " +
		"not signed in are app.use with principal_kind anonymous, unless host policy turns that off. " +
		"Use the note for what the filter cannot answer, such as individual requests within a visit, " +
		"which are not recorded." + listsNote(req.Lookup) + " " + Voice

	user := "## Question\n\n" + req.Question +
		"\n\n## Current time\n\n" + req.Now.UTC().Format(time.RFC3339) +
		"\n\n## Accounts\n\n" + JSONBlock(req.People) +
		"\n\n## Apps\n\n" + JSONBlock(req.Apps) +
		"\n\n## Actions\n\n" + JSONBlock(req.Actions)

	str := map[string]any{"type": "string"}
	schema := map[string]any{
		"filter": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"actions":        map[string]any{"type": "array", "items": str},
				"app_id":         str,
				"principal_id":   str,
				"principal_kind": map[string]any{"type": "string", "enum": []string{"user", "token", "system", "anonymous"}},
				"target_kind":    str,
				"target_id":      str,
				"involving":      map[string]any{"type": "string", "description": "An ID that appears as the principal, the app or the target."},
				"since":          map[string]any{"type": "string", "description": "RFC 3339, UTC."},
				"until":          map[string]any{"type": "string", "description": "RFC 3339, UTC."},
			},
		},
		"note": map[string]any{"type": "string", "description": "What this filter cannot answer, if anything. Empty otherwise."},
	}

	return Task{System: system, User: user, Tool: submitTool(schema, []string{"filter"}), Lookup: req.Lookup}
}

// AuditSummaryTask summarizes the records core found (R-345).
func AuditSummaryTask(req api.AuditSummaryRequest) Task {
	system := "You summarize audit records from Pando for the person who asked a question of the audit " +
		"log. Use only the records given: say nothing they do not show. Name people and apps as the " +
		"lists given name them. If the records are truncated, say the summary covers the most recent " +
		"ones. If there are none, say that nothing matched. Two to five sentences. " + Voice

	user := "## Question\n\n" + req.Question +
		"\n\n## Filter\n\n" + JSONBlock(req.Filter) +
		"\n\n## Records, newest first\n\n" + JSONBlock(req.Records) +
		fmt.Sprintf("\n\nTruncated: %t", req.Truncated) +
		"\n\n## Accounts\n\n" + JSONBlock(req.People) +
		"\n\n## Apps\n\n" + JSONBlock(req.Apps)

	schema := map[string]any{"summary": map[string]any{"type": "string"}}

	return Task{System: system, User: user, Tool: submitTool(schema, []string{"summary"})}
}

// ReferenceTask answers from the generated reference (R-346).
func ReferenceTask(req api.ReferenceRequest) Task {
	system := "You answer \"How can I…\" questions about Pando, a self-hosted app platform, from its " +
		"generated reference: the HTTP API, the pando CLI and the MCP tools. Answer only from the " +
		"reference given. Say which endpoint, command or tool to use and how, with a short example " +
		"where it helps. Describe how to do it; you do nothing yourself. If the reference does not " +
		"cover the question, set covered to false and say so plainly rather than guess. " + Voice

	user := "## Question\n\n" + req.Question + "\n\n## Reference\n\n" + req.Reference

	schema := map[string]any{
		"answer":  map[string]any{"type": "string", "description": "Markdown. Short."},
		"cites":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Each endpoint, command or tool the answer relies on, as the reference writes it."},
		"covered": map[string]any{"type": "boolean"},
	}

	return Task{System: system, User: user, Tool: submitTool(schema, []string{"answer", "covered"})}
}

// JSONBlock renders v for a prompt.
func JSONBlock(v any) string {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil || string(body) == "null" {
		return "(none)"
	}
	return "```json\n" + strings.TrimSpace(string(body)) + "\n```"
}
