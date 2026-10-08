package api

import (
	"context"
	"encoding/json"
	"time"
)

// The administrative AI functions (R-343 … R-346): drafting access, drafting
// policy, searching the audit log, and answering from the reference.
//
// Every one of them proposes and none of them applies. The adapter is handed
// what core decided it may see — a verb catalog, a policy document, a list of
// people — and returns a draft, a filter or an answer. Core checks the draft
// against the closed set it was drawn from, runs the filter itself, and leaves
// applying anything to a person using the ordinary endpoints under their own
// authority. That is R-027 for these functions: an adapter never touches
// authorization, audit, or state, so there is no draft it can return that
// creates a role or reads a record.

// VerbInfo is one verb an access or policy draft may name.
type VerbInfo struct {
	Name string `json:"name"`

	// Scope is "install" or "app". A role holds verbs of one scope (R-080).
	Scope string `json:"scope"`

	Meaning string `json:"meaning,omitempty"`
}

// RoleInfo is an existing role, so a draft does not duplicate one or take a
// built-in's name (R-082).
type RoleInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Scope   string   `json:"scope"`
	Builtin bool     `json:"builtin"`
	Verbs   []string `json:"verbs,omitempty"`
}

// PersonInfo is an account, so "Ben Meeker" can be resolved to an ID. No
// credential and nothing else about the account is included.
type PersonInfo struct {
	ID string `json:"id"`

	// Username is what the person signs in with, and often the only name an
	// account has: "admin" in a question is this, not a display name.
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
}

// GroupInfo is an existing group.
type GroupInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AppInfo is an app, so a question can name one.
type AppInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// LookupLimit is the most results one lookup returns. Core asks its stores
// for no more, and aikit cuts a longer answer to it, so what a lookup puts in
// a prompt does not grow with the installation (O-54).
const LookupLimit = 20

// Lookup is how an adapter finds the people, apps and groups a request names,
// rather than being handed every one (O-54). Each function searches by a
// name, username, email or slug, case-insensitively, and returns at most
// LookupLimit matches.
//
// Callbacks core passes in, not a store: they run as the person who asked,
// and return only what that person could read through the API. An adapter
// sees no more by calling them than its caller could see, and never touches
// state or authorization itself (R-027). A nil function is a lookup the
// person may not make, and its tool is not offered.
//
// Only an adapter whose capabilities say LooksUp is given one. Any other is
// given what core found by searching the words of the request itself
// (design 10 §10.6).
type Lookup struct {
	People func(ctx context.Context, query string) ([]PersonInfo, error)
	Apps   func(ctx context.Context, query string) ([]AppInfo, error)
	Groups func(ctx context.Context, query string) ([]GroupInfo, error)
}

// AccessRequest asks for a role and group draft (R-343).
type AccessRequest struct {
	Description string

	// Verbs are every verb the requester could grant. A draft naming one not
	// listed is refused by core, so a model cannot widen what a person asked
	// it to draft past what that person could do themselves.
	Verbs []VerbInfo
	Roles []RoleInfo

	// Groups and People are not every group and account (O-54). They are the
	// ones the draft so far names and, for an adapter that cannot look
	// things up, those matching words in Description — at most LookupLimit
	// of each.
	Groups []GroupInfo
	People []PersonInfo

	// Lookup finds anyone else, for an adapter that LooksUp. Nil otherwise.
	Lookup *Lookup

	// Current is the draft so far, when a person is refining one: what an
	// earlier call drafted, with whatever they changed by hand. Description is
	// then the change they asked for, and the answer is the whole draft again.
	Current *AccessDraft

	Model string
}

// RoleDraft is a proposed custom role.
type RoleDraft struct {
	Name  string   `json:"name"`
	Scope string   `json:"scope"`
	Verbs []string `json:"verbs"`
}

// GroupDraft is a proposed group. Members are account IDs from the request.
type GroupDraft struct {
	Name    string   `json:"name"`
	Members []string `json:"members,omitempty"`
}

// AccessDraft is what DraftAccess proposes. Either part may be absent.
type AccessDraft struct {
	Role  *RoleDraft  `json:"role,omitempty"`
	Group *GroupDraft `json:"group,omitempty"`

	// Reply says what was drafted and why, or why nothing was, to the person
	// who asked. Held to R-105.
	Reply string `json:"reply"`
	Model string `json:"model,omitempty"`
}

// PolicyField is one host policy field as a policy draft sees it.
type PolicyField struct {
	Key string `json:"key"`

	// Type is "boolean", "whole number", "list" or "text".
	Type string `json:"type"`

	// Meaning is what the field does, in the words the Policy screen uses, so
	// a request can be matched to the setting a person means.
	Meaning string `json:"meaning,omitempty"`

	// Fixed is set when the startup configuration sets this field (R-271).
	// Core refuses a change to it whatever the model returns; it is included
	// so the model can say so rather than propose it.
	Fixed bool `json:"fixed,omitempty"`
}

// PolicyRequest asks for a policy change (R-344).
type PolicyRequest struct {
	Description string

	// Current is the effective policy document, startup fields included.
	Current json.RawMessage
	Fields  []PolicyField
	Verbs   []VerbInfo

	// Draft is the document so far, when a person is refining a proposal:
	// Current with the changes already proposed and kept. Description is then
	// the change they asked for, and Changes are relative to Draft.
	Draft json.RawMessage

	Model string
}

// PolicyDraft is a proposed change to host policy: each field it would set,
// and the value it would set it to.
type PolicyDraft struct {
	Changes map[string]json.RawMessage `json:"changes"`
	Reply   string                     `json:"reply"`
	Model   string                     `json:"model,omitempty"`
}

// AuditSearchRequest asks for audit filters (R-345).
type AuditSearchRequest struct {
	Question string

	// Now is when the question was asked, in UTC, so "last month" means
	// something.
	Now time.Time

	// People and Apps are those matching words in Question, for an adapter
	// that cannot look things up; empty otherwise (O-54). At most
	// LookupLimit of each.
	People []PersonInfo
	Apps   []AppInfo

	// Actions are every action Pando records, from its own catalog rather
	// than from the log (O-54).
	Actions []string

	// Lookup finds people and apps, for an adapter that LooksUp.
	Lookup *Lookup

	Model string
}

// AuditFilter is one audit query, as the audit screen's filters express it.
// Every field narrows; an empty one does not.
type AuditFilter struct {
	// Actions are action names or prefixes, any of which matches.
	Actions       []string   `json:"actions,omitempty"`
	AppID         string     `json:"app_id,omitempty"`
	PrincipalID   string     `json:"principal_id,omitempty"`
	PrincipalKind string     `json:"principal_kind,omitempty"`
	TargetKind    string     `json:"target_kind,omitempty"`
	TargetID      string     `json:"target_id,omitempty"`
	Involving     string     `json:"involving,omitempty"`
	Since         *time.Time `json:"since,omitempty"`
	Until         *time.Time `json:"until,omitempty"`
}

// AuditSearch is the filter a question became.
type AuditSearch struct {
	Filter AuditFilter `json:"filter"`

	// Note says what the filter cannot answer — that successful use of an
	// app is not recorded, say — so the summary is not read as more than it
	// is.
	Note  string `json:"note,omitempty"`
	Model string `json:"model,omitempty"`
}

// AuditRecordView is one audit record as a summary sees it. Detail carries no
// secret value (R-194): none is ever written to the log.
type AuditRecordView struct {
	At            time.Time      `json:"at"`
	Action        string         `json:"action"`
	PrincipalKind string         `json:"principal_kind"`
	PrincipalID   string         `json:"principal_id"`
	AppID         string         `json:"app_id,omitempty"`
	TargetKind    string         `json:"target_kind,omitempty"`
	TargetID      string         `json:"target_id,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
}

// AuditSummaryRequest asks for a summary of the records a filter found.
type AuditSummaryRequest struct {
	Question string
	Filter   AuditFilter
	Records  []AuditRecordView

	// Truncated is set when more records matched than were sent.
	Truncated bool

	// People and Apps are those the records and the filter name, by ID, and
	// that the person who asked may see: bounded by the records, not by the
	// installation (O-54).
	People []PersonInfo
	Apps   []AppInfo

	Model string
}

// AuditSummary is a few sentences about the records, from the records alone.
type AuditSummary struct {
	Summary string `json:"summary"`
	Model   string `json:"model,omitempty"`
}

// ReferenceRequest asks a question of the generated reference (R-346).
type ReferenceRequest struct {
	Question string

	// Reference is the API, CLI and MCP reference as GET /reference serves it,
	// in Markdown.
	Reference string

	Model string
}

// ReferenceAnswer is an answer and what it rests on.
type ReferenceAnswer struct {
	Answer string `json:"answer"`

	// Cites are the endpoints, commands and tools the answer relies on, as
	// the reference names them: "POST /api/v1/groups", "pando policy set".
	Cites []string `json:"cites,omitempty"`

	// Covered is false when the reference does not cover the question, and
	// the answer says so rather than guess.
	Covered bool   `json:"covered"`
	Model   string `json:"model,omitempty"`
}
