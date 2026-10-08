package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
)

// auditedRoutes is every state-changing route that writes to the audit log,
// with the actions it writes (R-391). A route that writes on some branches —
// a denial, a failure, a deploy that a webhook starts — is listed with the
// actions of its main outcomes. The actions are written in the handler, or in
// the core service it calls (idp, subscription, autodeploy, approval,
// upgrade, security, detection, bootstrap, reconciler).
var auditedRoutes = map[string][]string{
	// Sessions and setup.
	"POST /api/v1/sessions":                             {"session.create", "session.denied"},
	"DELETE /api/v1/sessions":                           {"session.revoke"},
	"POST /api/v1/auth/providers/{providerID}/callback": {"session.create", "session.denied", "identity_provider.test", "user.create", "user.identity.link"},
	"POST /api/v1/setup":                                {"user.create", "grant.create", "session.create"},
	"POST /api/v1/me/password":                          {"user.password.change", "user.password.denied"},

	// SCIM, audited by the idp service as the provider's token.
	"POST /api/v1/scim/v2/Users":         {"user.create", "user.scim.adopt"},
	"PUT /api/v1/scim/v2/Users/{id}":     {"user.scim.update", "user.suspend", "user.activate"},
	"PATCH /api/v1/scim/v2/Users/{id}":   {"user.scim.update", "user.suspend", "user.activate"},
	"DELETE /api/v1/scim/v2/Users/{id}":  {"user.scim.delete"},
	"POST /api/v1/scim/v2/Groups":        {"group.create"},
	"PUT /api/v1/scim/v2/Groups/{id}":    {"group.members.set"},
	"PATCH /api/v1/scim/v2/Groups/{id}":  {"group.rename", "group.members.set", "group.members.change"},
	"DELETE /api/v1/scim/v2/Groups/{id}": {"group.delete"},

	// Identity providers.
	"POST /api/v1/identity-providers":                           {"identity_provider.create"},
	"PATCH /api/v1/identity-providers/{providerID}":             {"identity_provider.update"},
	"DELETE /api/v1/identity-providers/{providerID}":            {"identity_provider.delete"},
	"POST /api/v1/identity-providers/{providerID}/scim-token":   {"identity_provider.scim.enable", "identity_provider.scim.rotate"},
	"DELETE /api/v1/identity-providers/{providerID}/scim-token": {"identity_provider.scim.disable"},

	// The launcher. Favorites and sections are per person, and audited anyway.
	"PUT /api/v1/me/favorites/{appID}":                    {"app.favorite"},
	"DELETE /api/v1/me/favorites/{appID}":                 {"app.unfavorite"},
	"POST /api/v1/me/sections":                            {"launcher.section.create"},
	"PATCH /api/v1/me/sections/{sectionID}":               {"launcher.section.rename"},
	"DELETE /api/v1/me/sections/{sectionID}":              {"launcher.section.delete"},
	"PUT /api/v1/me/sections/{sectionID}/apps/{appID}":    {"launcher.section.place"},
	"DELETE /api/v1/me/sections/{sectionID}/apps/{appID}": {"launcher.section.unplace"},

	// Users.
	"POST /api/v1/users":                       {"user.create"},
	"PATCH /api/v1/users/{userID}":             {"user.update"},
	"PUT /api/v1/users/{userID}/role":          {"grant.create"},
	"DELETE /api/v1/users/{userID}/role":       {"grant.delete"},
	"POST /api/v1/users/{userID}/password":     {"user.password.reset"},
	"POST /api/v1/users/{userID}/identities":   {"user.identity.link"},
	"DELETE /api/v1/users/{userID}/identities": {"user.identity.unlink"},
	"DELETE /api/v1/users/{userID}":            {"user.delete"},

	// Groups.
	"POST /api/v1/groups":                                   {"group.create"},
	"PUT /api/v1/groups/{groupID}/members":                  {"group.members.set"},
	"PUT /api/v1/groups/{groupID}/members/{userID}":         {"group.member.add"},
	"DELETE /api/v1/groups/{groupID}/members/{userID}":      {"group.member.remove"},
	"PUT /api/v1/groups/{groupID}/role":                     {"grant.create"},
	"DELETE /api/v1/groups/{groupID}/role":                  {"grant.delete"},
	"PUT /api/v1/groups/{groupID}/links/{syncedGroupID}":    {"group.link"},
	"DELETE /api/v1/groups/{groupID}/links/{syncedGroupID}": {"group.unlink"},
	"DELETE /api/v1/groups/{groupID}":                       {"group.delete"},

	// Roles and tokens.
	"POST /api/v1/roles":              {"role.create"},
	"DELETE /api/v1/roles/{roleID}":   {"role.delete"},
	"POST /api/v1/tokens":             {"token.create"},
	"POST /api/v1/tokens/service":     {"token.create"},
	"DELETE /api/v1/tokens/{tokenID}": {"token.revoke"},

	// Adapters, sources and AI.
	"POST /api/v1/adapters":                          {"adapter.configure"},
	"DELETE /api/v1/sources/{sourceID}":              {"source.connection.delete"},
	"POST /api/v1/sources/{sourceID}/authorize":      {"source.connection.authorize"},
	"POST /api/v1/sources/{sourceID}/authorize/poll": {"source.connection.authorized"},
	"PUT /api/v1/ai/functions/{function}":            {"ai.function.assign"},
	"DELETE /api/v1/ai/functions/{function}":         {"ai.function.unassign"},
	// What left the host for a provider, never its content (R-337).
	"POST /api/v1/ai/access/draft":     {"ai.draft_access"},
	"POST /api/v1/ai/policy/draft":     {"ai.draft_policy"},
	"POST /api/v1/ai/audit/search":     {"ai.search_audit"},
	"POST /api/v1/ai/reference/answer": {"ai.answer_reference"},

	// The installation.
	"POST /api/v1/restart":                    {"install.restart"},
	"PUT /api/v1/policy":                      {"policy.update"},
	"POST /api/v1/upgrade":                    {"upgrade.start", "upgrade.refused", "upgrade.backup_skipped", "upgrade.failed"},
	"POST /api/v1/backups":                    {"backup.create", "backup.failed"},
	"POST /api/v1/backups/{backupID}/verify":  {"backup.verify", "backup.verify.failed"},
	"POST /api/v1/backups/{backupID}/restore": {"backup.restore.start", "backup.restore", "backup.restore.refused"},

	// Subscriptions.
	"POST /api/v1/subscriptions":                                                    {"subscription.create"},
	"PATCH /api/v1/subscriptions/{subscriptionID}":                                  {"subscription.update"},
	"DELETE /api/v1/subscriptions/{subscriptionID}":                                 {"subscription.delete"},
	"POST /api/v1/subscriptions/{subscriptionID}/signing-key":                       {"subscription.key.rotate"},
	"POST /api/v1/subscriptions/{subscriptionID}/test":                              {"subscription.test"},
	"POST /api/v1/subscriptions/{subscriptionID}/deliveries/{deliveryID}/redeliver": {"subscription.redeliver"},

	// Apps.
	"POST /api/v1/apps":                                      {"app.create", "app.create.denied"},
	"PATCH /api/v1/apps/{appID}":                             {"app.update"},
	"DELETE /api/v1/apps/{appID}":                            {"app.delete", "app.delete.backup_failed"},
	"PUT /api/v1/apps/{appID}/icon":                          {"app.icon.set"},
	"DELETE /api/v1/apps/{appID}/icon":                       {"app.icon.clear"},
	"POST /api/v1/apps/{appID}/start":                        {"app.start"},
	"POST /api/v1/apps/{appID}/stop":                         {"app.stop"},
	"POST /api/v1/apps/{appID}/restart":                      {"app.restart"},
	"POST /api/v1/apps/{appID}/security/scan":                {"app.scan", "app.scan.failed"},
	"PUT /api/v1/apps/{appID}/slots/{key}":                   {"app.slot.set"},
	"POST /api/v1/apps/{appID}/volumes":                      {"app.volume.add"},
	"POST /api/v1/apps/{appID}/restore":                      {"app.restore.start", "app.restore"},
	"POST /api/v1/apps/{appID}/source":                       {"app.source.upload"},
	"POST /api/v1/apps/{appID}/detection/rerun":              {"detection.rerun"},
	"POST /api/v1/apps/{appID}/detection/revise":             {"detection.revise"},
	"POST /api/v1/apps/{appID}/detection/answers":            {"detection.answer"},
	"POST /api/v1/apps/{appID}/detection/accept":             {"spec.pin"},
	"POST /api/v1/apps/{appID}/deployments":                  {"app.deploy", "deploy.request", "spec.create"},
	"POST /api/v1/apps/{appID}/deployments/rollback":         {"app.deploy", "deploy.request"},
	"POST /api/v1/apps/{appID}/deployments/{depID}/approve":  {"deploy.approve"},
	"POST /api/v1/apps/{appID}/deployments/{depID}/reject":   {"deploy.reject"},
	"POST /api/v1/apps/{appID}/passcode":                     {"app.passcode.unlock", "app.passcode.denied"},
	"POST /api/v1/apps/{appID}/grants":                       {"grant.create"},
	"PATCH /api/v1/apps/{appID}/grants/{grantID}":            {"grant.update"},
	"DELETE /api/v1/apps/{appID}/grants/{grantID}":           {"grant.delete"},
	"PUT /api/v1/apps/{appID}/secrets/{key}":                 {"secret.write"},
	"DELETE /api/v1/apps/{appID}/secrets/{key}":              {"secret.delete"},
	"PUT /api/v1/apps/{appID}/registry-credential":           {"app.registry_credential.write"},
	"DELETE /api/v1/apps/{appID}/registry-credential":        {"app.registry_credential.delete"},
	"PUT /api/v1/apps/{appID}/routing":                       {"routing.change"},
	"PUT /api/v1/apps/{appID}/auto-deploy":                   {"app.auto_deploy.configure"},
	"POST /api/v1/apps/{appID}/auto-deploy/webhook-secret":   {"app.auto_deploy.webhook_secret"},
	"DELETE /api/v1/apps/{appID}/auto-deploy/webhook-secret": {"app.auto_deploy.webhook_secret"},
	// A verified delivery starts a check in the background; the deploy it
	// leads to is written by the reconciler as the system (R-228).
	"POST /api/v1/apps/{appID}/auto-deploy/webhook": {"app.auto_deploy"},
	"POST /api/v1/apps/{appID}/specs":               {"spec.create"},
	"POST /api/v1/apps/{appID}/specs/{rev}/pin":     {"spec.pin"},
}

// auditExempt is every state-changing route that writes nothing to the audit
// log, and why (R-391). A reason prefixed "GAP: " is a route that should be
// audited and is not yet.
var auditExempt = map[string]string{
	"POST /api/v1/apps/{appID}/plan": "A read expressed as POST: it plans the pinned revision and returns the " +
		"plan, creating nothing. Auditing it would audit every keystroke of the spec editor.",
	"POST /api/v1/policy/preview": "A read expressed as POST: it evaluates a proposed policy against the apps " +
		"and changes nothing. Saving the policy is PUT /policy, which writes policy.update.",
	"POST /api/v1/passwords/generate": "Returns a random string and stores nothing. The password it suggests is " +
		"audited when it is set, by POST /users or POST /users/{userID}/password.",
	"POST /api/v1/identity-providers/{providerID}/check": "A health probe: it asks the provider whether it " +
		"answers and stores nothing.",
	"POST /api/v1/me/notifications/read": "Marks the caller's own notifications read. A per-person reading " +
		"mark that grants, reveals and changes nothing anyone else sees.",
	"POST /api/v1/me/notifications/{notificationID}/read": "Marks one of the caller's own notifications read. " +
		"A per-person reading mark that grants, reveals and changes nothing anyone else sees.",
	"PUT /api/v1/notification-preferences": "The caller's own choice of which notifications reach them on which " +
		"channel. A per-person preference that grants and reveals nothing.",
}

// maxAuditExempt bounds auditExempt, so an exemption is a decision somebody
// makes in review rather than a line that slips in.
const maxAuditExempt = 7

func servedMutatingRoutes(t *testing.T) map[string]bool {
	t.Helper()
	handler := (&Server{Logger: zap.NewNop()}).Routes()
	routes, ok := handler.(chi.Routes)
	require.True(t, ok, "the router must be walkable for this check to mean anything")

	served := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return nil
		}
		served[method+" "+normalizeRoutePath(route)] = true
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, served, "walking the router found no state-changing route, so this test proves nothing")
	return served
}

// TestR391_EveryMutatingRouteAudits asserts R-391: every state-changing API
// route writes an audit event or is exempt by name, with a reason. chi knows
// every route this binary serves, so a new POST, PUT, PATCH or DELETE that is
// in neither table fails here, and so does an entry for a route that is gone.
func TestR391_EveryMutatingRouteAudits(t *testing.T) {
	served := servedMutatingRoutes(t)

	known := map[string]bool{}
	for _, a := range audit.Actions {
		known[a] = true
	}

	for key, actions := range auditedRoutes {
		require.True(t, served[key],
			"%s is in auditedRoutes and is not a served state-changing route. Remove it.", key)
		_, exempt := auditExempt[key]
		require.False(t, exempt, "%s is in both auditedRoutes and auditExempt. It is one or the other.", key)
		require.NotEmpty(t, actions, "%s is in auditedRoutes with no actions. Name what it writes.", key)
		for _, a := range actions {
			require.True(t, known[a], "%s names %q, which is not in audit.Actions.", key, a)
		}
	}
	for key, reason := range auditExempt {
		require.True(t, served[key],
			"%s is in auditExempt and is not a served state-changing route. Remove it.", key)
		require.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(reason, "GAP:")),
			"%s is exempt with no reason. Say why it writes nothing to the audit log.", key)
		if strings.HasPrefix(reason, "GAP: ") {
			t.Logf("%s writes no audit event yet: %s", key, reason)
		}
	}
	for key := range served {
		_, audited := auditedRoutes[key]
		_, exempt := auditExempt[key]
		require.True(t, audited || exempt,
			"%s changes state and is in neither auditedRoutes nor auditExempt. Write an audit event "+
				"in its handler or service and list the action in auditedRoutes, or exempt it by name "+
				"with a reason in auditExempt (R-391).", key)
	}
}

// TestR391_ExemptionsAreFew asserts the exemptions stay few, so adding one is
// deliberate: raising maxAuditExempt is a change a reviewer sees.
func TestR391_ExemptionsAreFew(t *testing.T) {
	require.LessOrEqual(t, len(auditExempt), maxAuditExempt,
		"auditExempt has grown past %d. An exemption is the exception: audit the route instead, or "+
			"raise maxAuditExempt in the same change and say why.", maxAuditExempt)
}
