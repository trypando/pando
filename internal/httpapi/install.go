package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/log"
)

// InstallVerbs reports the installation-wide verbs a principal holds.
//
// Read by GET /me so the console can decide what to offer (R-265) from the
// server's answer rather than by inferring administration from some other
// response. Inference is what it did before install verbs existed — it treated
// a non-empty GET /apps as "this person is an administrator" — and that is a
// second opinion about authorization living in a client (R-261).
type InstallVerbs interface {
	InstallVerbsFor(ctx context.Context, p authz.Principal) ([]string, error)
}

// requireInstall checks an installation-wide verb (O-17).
//
// Unlike requireControl there is nothing to resolve first and nothing to hide:
// an install-level endpoint's existence is not a disclosure, so a caller who
// holds nothing gets a denial that names the verb rather than a not-found.
func (s *Server) requireInstall(w http.ResponseWriter, r *http.Request, verb authz.Verb) (authz.Principal, bool) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return p, false
	}
	if err := s.Authz.CheckInstall(r.Context(), p, verb); err != nil {
		Error(w, r, err)
		return p, false
	}
	return p, true
}

// requireSelfOrInstall allows an action on one's own account, and otherwise
// requires an installation-wide verb.
//
// The shape the user endpoints need: reading and changing your own account is
// self-service and needs no administrative power, while doing either to someone
// else is administration. Both halves matter — without the first, an install
// with one administrator cannot let anyone manage their own account; without
// the second, *any* signed-in account could suspend the administrator, which is
// what these endpoints allowed before O-17 was resolved.
//
// A delegated token acts as its owner here as everywhere else (R-058), so an
// agent may act on its owner's account and no other.
func (s *Server) requireSelfOrInstall(w http.ResponseWriter, r *http.Request, userID string, verb authz.Verb) (authz.Principal, bool) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return p, false
	}

	// Self, and only for a principal that has a user at all. An account token
	// is nobody's account (R-060), so p.UserID is empty and the comparison must
	// not match a caller who passed an empty ID.
	if p.UserID != "" && p.UserID == userID {
		return p, true
	}

	if err := s.Authz.CheckInstall(r.Context(), p, verb); err != nil {
		Error(w, r, err)
		return p, false
	}
	return p, true
}

// handleListUsers returns a page of accounts, newest first, with the
// installation role each holds (issue #72: `limit`, `cursor`, `q`).
//
// The role travels with the account rather than behind a second request per
// row: the accounts screen exists to answer "who can do what here", and an
// answer that takes N+1 requests is one the console would be tempted to cache
// and get wrong.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}

	page, err := pageFrom(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	users, next, total, err := s.Users.ListPage(r.Context(), page)
	if err != nil {
		Error(w, r, err)
		return
	}
	roles, err := s.Grants.InstallRolesByPrincipal(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, accountView(u, roles[u.ID]))
	}
	JSON(w, http.StatusOK, withTotal(map[string]any{"users": out, "next_cursor": next}, total))
}

// accountView is an account as the API shows it, in the list and on its own:
// one shape, so that a client reading one account does not need the list to
// learn what role it holds.
func accountView(u state.User, installRoleID string) map[string]any {
	v := map[string]any{
		"id":                   u.ID,
		"adapter_id":           u.AdapterID,
		"external_id":          u.ExternalID,
		"email":                u.Email,
		"display_name":         u.DisplayName,
		"status":               u.Status,
		"must_change_password": u.MustChangePassword,
		"created_at":           u.CreatedAt,

		// Empty when the account holds nothing install-wide, which is most
		// of them. Not null: absent and empty are the same thing to a
		// client, and only one of them is an answer.
		"install_role_id": installRoleID,
	}
	// An account whose identities were moved to another (O-1). Present only
	// on those, so the console can say which account replaced it.
	if u.AliasOf != "" {
		v["alias_of"] = u.AliasOf
	}
	return v
}

// handleListRoles returns roles, read from the database rather than the Go
// catalog so a custom role (R-082) appears the moment it exists.
//
// By default the ones that can be granted across the installation, which is
// what giving someone an installation role needs. `scope=app` gives the roles
// granted on one app, and `scope=all` both — the list of every role there is,
// which the Groups and roles screen showed as two of Pando's five because it
// only ever asked for the first kind.
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}

	switch scope := r.URL.Query().Get("scope"); scope {
	case "", "install":
	case "app", "all":
		rows, err := s.Roles.List(r.Context())
		if err != nil {
			Error(w, r, err)
			return
		}
		out := make([]state.RoleRow, 0, len(rows))
		for _, row := range rows {
			if scope == "all" || row.Scope == scope {
				out = append(out, row)
			}
		}
		JSON(w, http.StatusOK, map[string]any{"roles": out})
		return
	default:
		Error(w, r, errs.Newf(errs.ValidInvalid, "%q is not a role scope.", scope).
			WithRemedy("Use install, app or all."))
		return
	}

	roles, err := s.Grants.InstallRoles(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(roles))
	for _, role := range roles {
		verbs := make([]string, 0, len(role.Verbs))
		for _, v := range role.Verbs {
			verbs = append(verbs, string(v))
		}
		out = append(out, map[string]any{
			"id": role.ID, "name": role.Name, "builtin": role.Builtin,
			"scope": "install", "verbs": verbs,
		})
	}
	JSON(w, http.StatusOK, map[string]any{"roles": out})
}

// handlePutUserRole gives an account an installation-wide role.
//
// The missing half of O-17: the verbs and the role existed, bootstrap wrote one
// grant, and nothing could write a second — so an install had exactly one
// administrator forever and no way to hand over.
//
// Deliberately its own route rather than a field on PATCH /users/{id}. Changing
// someone's status and changing their power are different acts with different
// verbs in every other part of this system, and folding them into one body is
// how a status update quietly becomes a promotion.
func (s *Server) handlePutUserRole(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok {
		return
	}

	var req struct {
		RoleID string `json:"role_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	if req.RoleID == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "This needs a role to grant.").
			WithRemedy("Choose one of the roles from GET /roles."))
		return
	}

	userID := chi.URLParam(r, "userID")
	user, found, err := s.Users.ByID(r.Context(), userID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no account with that ID."))
		return
	}

	grant, err := s.Grants.GrantInstall(r.Context(), "user", user.ID, req.RoleID, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "grant.create",
		TargetKind:    "grant",
		TargetID:      grant.ID,
		Detail: map[string]any{
			"scope": "install", "role": req.RoleID, "user": user.ID,
		},
	})
	JSON(w, http.StatusOK, grant)
}

// handleDeleteUserRole removes an account's installation-wide role.
//
// The store refuses to remove the last account that can manage accounts, and
// that refusal is transactional — see state.Grants.RevokeInstall. An install
// that cannot be administered has no recovery path inside the product.
func (s *Server) handleDeleteUserRole(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok {
		return
	}

	userID := chi.URLParam(r, "userID")
	if err := s.Grants.RevokeInstall(r.Context(), "user", userID); err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "grant.delete",
		TargetKind:    "user",
		TargetID:      userID,
		Detail:        map[string]any{"scope": "install"},
	})
	JSON(w, http.StatusNoContent, nil)
}

// handleGetPolicy returns the installation's host policy (R-274).
func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	if s.PolicyStore == nil {
		Error(w, r, errs.New(errs.Internal, "Host policy is not set up on this installation."))
		return
	}

	doc, err := s.PolicyStore.Load(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, doc)
}

// handlePutPolicy replaces the host policy.
//
// Read by install.view, written by install.policy.manage: seeing the rules you
// are working under is not the same privilege as changing them, which is the
// same split as app.secrets.read and app.secrets.write (R-083).
//
// O-10: saving does not touch running apps. A policy that a running app now
// violates leaves that app running and fails its **next** deploy at plan time,
// because stopping someone's app on a schedule, for a rule they did not write,
// is destruction of something they may have wanted. POST /policy:preview
// answers what this save would block, beforehand (design 05 §3).
func (s *Server) handlePutPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallPolicyManage)
	if !ok {
		return
	}
	if s.PolicyStore == nil {
		Error(w, r, errs.New(errs.Internal, "Host policy is not set up on this installation."))
		return
	}

	var doc corepolicy.Document
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The policy could not be read."))
		return
	}

	// A field set in the startup config cannot be changed here (R-271). Sending
	// it back as it is — which is what GET /policy hands out — is fine; the
	// store saves everything else and leaves that field's stored value alone.
	if f, changed := s.PolicyOverlay.Changes(doc); changed {
		Error(w, r, errs.Newf(errs.ValidInvalid,
			"%s is set in the startup configuration (%s), so it cannot be changed here.", f.Key, where(f.Source)).
			WithRemedy("Change it there and restart Pando, or remove it there to manage it here."))
		return
	}

	// One of the three rules, or unset: anything else would be saved and
	// enforce nothing (R-076).
	if err := doc.PublicSharing.Valid(); err != nil {
		Error(w, r, errs.Newf(errs.ValidInvalid,
			"public_sharing is %q, which is not one of Pando's rules for sharing with everyone.", string(doc.PublicSharing)).
			WithRemedy("Use allowed, passcode_only or none."))
		return
	}

	// Egress and approval settings: a mode or entry that does not parse would
	// be saved and enforce nothing (R-181, R-185).
	if err := doc.ValidateRules(); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "This policy has a setting Pando cannot use: "+err.Error()+"."))
		return
	}

	// A disabled verb must be a real verb. Policy can only deny (R-272), so a
	// typo here denies nothing and looks exactly like a rule that works —
	// the worst failure mode a security control has.
	for _, v := range doc.DisabledVerbs {
		if !authz.IsVerb(authz.Verb(v)) {
			Error(w, r, errs.Newf(errs.ValidInvalid,
				"%q is not a permission Pando has, so disabling it would have no effect.", v).
				WithRemedy("Use one of the verbs from GET /roles, for example app.exec."))
			return
		}
	}

	// Turning password sign-in off with no identity provider on would leave
	// nobody able to sign in (issue #51).
	if s.IDP != nil {
		before, err := s.PolicyStore.Load(r.Context())
		if err != nil {
			Error(w, r, err)
			return
		}
		if err := s.IDP.ValidatePolicy(r.Context(), before.DisablePasswordSignIn, doc.DisablePasswordSignIn); err != nil {
			Error(w, r, err)
			return
		}
	}

	if err := s.PolicyStore.Save(r.Context(), doc, p.ID); err != nil {
		Error(w, r, err)
		return
	}
	// What now applies, startup fields included.
	doc = s.PolicyOverlay.Apply(doc)

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "policy.update",
		TargetKind:    "policy",
		TargetID:      "host",
		Detail: map[string]any{"disabled_verbs": doc.DisabledVerbs,
			"disable_password_sign_in": doc.DisablePasswordSignIn, "disable_jit_provisioning": doc.DisableJITProvisioning},
	})
	JSON(w, http.StatusOK, doc)
}

// handlePreviewPolicy reports which apps a policy would block, without saving
// it (design 05 §3).
//
// The same body as PUT, so the console previews exactly what it is about to
// send rather than something assembled a second way. An empty list is a real
// answer and returns 200: "nothing breaks" is the thing an admin most wants to
// be told, and it should not be indistinguishable from an error.
func (s *Server) handlePreviewPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallPolicyManage); !ok {
		return
	}
	if s.Planner == nil {
		Error(w, r, errs.New(errs.Internal, "Pando cannot check this policy against the installation's apps."))
		return
	}

	var doc corepolicy.Document
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The policy could not be read."))
		return
	}

	// The same verb check PUT makes, and for the same reason (R-272): a typo
	// denies nothing and looks exactly like a rule that works. Catching it in
	// the preview means an admin sees it before they save rather than after.
	for _, v := range doc.DisabledVerbs {
		if !authz.IsVerb(authz.Verb(v)) {
			Error(w, r, errs.Newf(errs.ValidInvalid,
				"%q is not a permission Pando has, so disabling it would have no effect.", v).
				WithRemedy("Use one of the verbs from GET /roles, for example app.exec."))
			return
		}
	}

	if err := doc.ValidateRules(); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "This policy has a setting Pando cannot use: "+err.Error()+"."))
		return
	}

	// Previewed as it would apply: with the startup fields over it.
	doc = s.PolicyOverlay.Apply(doc)

	violations, err := s.Planner.PreviewPolicy(r.Context(), doc)
	if err != nil {
		Error(w, r, err)
		return
	}

	// Not audited. A preview reads and changes nothing, and an audit log with
	// an entry every keystroke of a form is a log nobody reads (R-229).
	JSON(w, http.StatusOK, map[string]any{"violations": violations})
}

// handleListAudit reads the audit log (R-227, R-229).
//
// Its own verb, separate from install.view. The log records what everyone did,
// including things they did inside apps they own, so reading it is a different
// level of trust from seeing how many CPUs the host has.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallAuditRead); !ok {
		return
	}
	if s.AuditLog == nil {
		Error(w, r, errs.New(errs.Internal, "The audit log is not readable on this installation."))
		return
	}

	// action may repeat: any of them matches.
	q := audit.Query{
		Actions:     r.URL.Query()["action"],
		AppID:       r.URL.Query().Get("app_id"),
		PrincipalID: r.URL.Query().Get("principal_id"),
		TargetKind:  r.URL.Query().Get("target_kind"),

		PrincipalKind: r.URL.Query().Get("principal_kind"),
		TargetID:      r.URL.Query().Get("target_id"),
		Involving:     r.URL.Query().Get("involving"),
	}
	// RFC 3339, like every time on the wire. A bound that will not parse is an
	// error rather than ignored: an investigation that silently searched all
	// of time would answer a different question than the one asked.
	for _, bound := range []struct {
		name string
		into *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		v := r.URL.Query().Get(bound.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			Error(w, r, errs.Newf(errs.ValidInvalid, "%s must be a time in RFC 3339 form, such as 2026-09-21T09:00:00Z.", bound.name))
			return
		}
		*bound.into = t
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			Error(w, r, errs.New(errs.ValidInvalid, "The limit must be a whole number of records."))
			return
		}
		q.Limit = n
	}
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			Error(w, r, errs.New(errs.ValidInvalid,
				"The cursor must be the id of a record from the previous page."))
			return
		}
		q.Cursor = n
	}

	events, err := s.AuditLog.List(r.Context(), q)
	if err != nil {
		Error(w, r, err)
		return
	}

	// The cursor for the next page, or empty when this was the last one. Named
	// rather than left for the client to derive: deriving it means knowing that
	// IDs descend, which is this endpoint's business and not the console's.
	//
	// The empty check is load-bearing. Without it, a query with no explicit
	// limit that matched nothing satisfied `len(events) == q.Limit` as 0 == 0
	// and then indexed events[-1] — so every audit filter that found no events
	// answered 500 instead of an empty page, which is the one answer a search
	// UI produces most often.
	//
	// The size comes from the reader rather than being worked out again here.
	// This used to apply only the default, which left the cap out: a request
	// for 1000 records got the 500 the reader allows, compared them against
	// 1000, decided the page was not full and sent no cursor — so paging
	// stopped dead on any limit above the cap.
	page := audit.PageSize(q.Limit)
	var next string
	if len(events) > 0 && len(events) == page {
		next = strconv.FormatInt(events[len(events)-1].ID, 10)
	}
	JSON(w, http.StatusOK, map[string]any{"events": events, "next_before": next})
}

// handleListAuditArchives lists the months retention has archived (R-347):
// each with its manifest — row count, first and last event, time range, size
// and SHA-256 digest — and where it is kept.
func (s *Server) handleListAuditArchives(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallAuditRead); !ok {
		return
	}
	archives := []audit.ArchiveRecord{}
	if s.AuditArchives != nil {
		var err error
		if archives, err = s.AuditArchives.List(r.Context()); err != nil {
			Error(w, r, err)
			return
		}
	}
	JSON(w, http.StatusOK, map[string]any{"archives": archives})
}

// handleGetAuditArchive downloads one archive: gzipped JSON lines, one audit
// event per line, as the live log held it (R-347). The digest is sent too, so
// a client can check what it received against the manifest.
//
// Not audited, like reading the log: the read is the verb's whole purpose, and
// an entry per download would be the log recording itself being read.
func (s *Server) handleGetAuditArchive(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallAuditRead); !ok {
		return
	}
	archiveID := chi.URLParam(r, "archiveID")
	if s.AuditArchives == nil || !id.Is(id.AuditArchive, archiveID) {
		Error(w, r, errs.Newf(errs.NotFound, "There is no audit archive %q.", archiveID).
			WithRemedy("GET /api/v1/audit/archives lists the archives this installation holds."))
		return
	}
	rec, body, err := s.AuditArchives.Open(r.Context(), archiveID)
	if err != nil {
		Error(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Length", strconv.FormatInt(rec.SizeBytes, 10))
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "audit-"+rec.Month+".jsonl.gz"))
	h.Set("X-Content-Type-Options", "nosniff")
	if sum, err := hex.DecodeString(rec.SHA256); err == nil {
		h.Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum)+":")
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		log.From(r.Context()).Warn("audit archive download ended early",
			zap.String("archive_id", rec.ID), zap.Error(err))
	}
}

// handleGetConfig reports the configuration Pando started with (R-271): every
// non-secret setting with its value and where it came from — an environment
// variable, the config file, or the default — and the host policy fields set
// there, which the console shows as fixed. Secrets are never listed (R-194).
//
// install.view, like reading policy: it is the installation's own settings,
// and nothing here is a credential.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	// Empty lists, never null: a typed nil slice in the map is not == nil, so
	// the check has to be on the slice before it goes in.
	fixed := s.PolicyOverlay.Fixed()
	if fixed == nil {
		fixed = []corepolicy.Fixed{}
	}
	out := map[string]any{
		"file":     "",
		"settings": []config.Setting{},
		"policy":   fixed,
	}
	// Adapters declared in the file, and the AI functions each handles, with
	// where each was declared. Credentials by name only: the file names a
	// variable or a path, never a value (R-190).
	declared := make([]map[string]any, 0)
	if s.Startup != nil {
		out["file"] = s.Startup.File
		if s.Startup.Settings != nil {
			out["settings"] = s.Startup.Settings
		}
		for _, d := range s.Startup.Adapters {
			fields := make([]string, 0, len(d.Credentials))
			for f := range d.Credentials {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			functions := d.Functions
			if functions == nil {
				functions = []config.FunctionDecl{}
			}
			declared = append(declared, map[string]any{
				"id": d.ID, "category": d.Category, "kind": d.Kind, "name": d.Name,
				"is_default": d.Default, "enabled": d.Enabled, "credentials": fields,
				"functions": functions, "source": d.Source,
			})
		}
	}
	out["adapters"] = declared
	JSON(w, http.StatusOK, out)
}

// where says in words where a startup value was set.
func where(src corepolicy.Source) string {
	switch src.Kind {
	case "env":
		return "environment variable " + src.Name
	case "file":
		return "config file " + src.Name + ", key " + src.Key
	}
	return "startup configuration"
}
