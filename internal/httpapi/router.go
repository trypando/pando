package httpapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/address"
	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/assist"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/deploy"
	"github.com/trypando/pando/internal/core/edge"
	"github.com/trypando/pando/internal/core/idp"
	"github.com/trypando/pando/internal/core/planner"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// Health reports whether a dependency is reachable.
type Health interface {
	Ping(ctx context.Context) error
}

// Server holds what the HTTP surface needs.
//
// Handlers contain no business logic (R-261). Everything lives in a service
// layer under internal/core that both this package and internal/mcp call — a
// capability in one surface and not the other means logic leaked into a
// handler.
type Server struct {
	Logger *zap.Logger
	DB     Health

	Identity api.IdentityAdapter
	Users    *state.Users

	// IDP is external identity: providers, redirect sign-in, linking and
	// SCIM (issue #51). Nil leaves password sign-in the only way in.
	IDP      *idp.Service
	Sessions *state.Sessions
	Tokens   *state.Tokens
	Apps     *state.Apps
	Volumes  *state.Volumes
	Authz    *authz.Authorizer
	Auditor  *audit.Writer
	Authent  *Authenticator

	Registry *api.Registry
	Adapters *state.Adapters

	// Address resolves changes to where an app is reached (R-162, R-163).
	// Nil means the address cannot be changed after configuration.
	Address *address.Service

	// Edges reports whether each routing adapter's edge is running (R-174).
	// Nil means no adapter's edge is reported.
	Edges *edge.Service

	// AIFunctions is which AI adapter handles each AI function (R-259). Nil
	// means the endpoints say assignment is not set up.
	AIFunctions *assist.Assignments

	// Assist runs the administrative AI functions. Nil means the endpoints
	// say AI assistance is not set up.
	Assist *assist.Service

	// TeardownNow asks for a deleted app's bundle to be torn down now rather
	// than at the collector's next pass. Nil leaves it to the pass.
	TeardownNow func()

	// AdapterKinds are the kinds of adapter this build can run, with the
	// settings each takes (api.KindInfo), for GET /adapters/kinds.
	AdapterKinds []api.KindInfo

	// AdapterCredentials holds adapters' credentials encrypted (O-20). Written
	// by POST /adapters, never read back by any handler.
	AdapterCredentials *state.AdapterCredentials

	// Reconciles is the loop's bookkeeping. The lifecycle handlers touch it
	// for one reason: a person starting an app Pando gave up on is the human
	// intervention R-151 requires, and the failure count has to be cleared for
	// the loop to look at the app again.
	Reconciles  *state.Reconciles
	Planner     *planner.Planner
	Allocations *state.Allocations
	Deployments *state.Deployments

	// Security scores apps and answers where one stands (R-310). Nil on an
	// installation with no scanner, where the endpoints say so rather than
	// returning a zero.
	Security Security
	Deployer *deploy.Runner

	// Approvals starts every deploy, and asks for, records and acts on the
	// approval of those that need one (R-154 – R-159).
	Approvals *approval.Service

	Logs       *deploy.LogStore
	Secrets    *state.Secrets
	Detections *state.Detections

	// Clock times a detection wait and how long one has been running. Nil is
	// the system clock.
	Clock clock.Clock

	// Detector runs detection for an app. Nil on an install with no builder or
	// runtime configured, in which case the detection endpoints say so rather
	// than returning an empty proposal — R-106's shape: with nothing
	// configured, each step degrades to a question, not a dead end.
	Detector Detector

	// Sources keeps an uploaded source (R-262) and fetches an app's source for
	// a scan on request.
	Sources source.Sources

	// Defaults fills in what an author left out of a spec — the install's
	// adapters, its routing shape, and the retention caps R-211 and R-223 set.
	// Nil means a hand-written spec is taken exactly as written, which is how
	// it behaved before and is wrong: an app with no log cap is an app that can
	// fill the host.
	Defaults SpecDefaults

	// Policy is host policy. Nil until phase 3 configures it, in which case the
	// source allowlist check (R-092) is skipped rather than assumed to pass —
	// the call site says so explicitly.
	Policy SourcePolicy

	// ExternalURL is how a browser reaches this installation, when something
	// other than Pando terminates TLS. It decides one thing: whether the
	// session cookie is marked Secure (O-19). Nil falls back to the request,
	// which is right when Pando serves TLS itself and on a localhost install.
	ExternalURL *url.URL

	// Minter publishes the assertion signing keys at /.well-known/jwks.json.
	Minter     *assertion.Minter
	Grants     *state.Grants
	HostPolicy AnonymousPolicy

	// Verbs answers "what may this principal do install-wide" for GET /me.
	// Nil leaves the list empty, which denies nothing — every install-level
	// endpoint checks the verb itself — but it does hide the admin entry.
	Verbs InstallVerbs

	// PolicyStore reads and writes the host policy document (R-274). Distinct
	// from HostPolicy above, which *evaluates* it: one is the document, the
	// other is the decision, and an endpoint that edits the document has no
	// business asking the evaluator anything.
	PolicyStore PolicyDocument

	// PolicyOverlay is the host policy fields set in the startup config
	// (R-271). PolicyStore already reads through it; this is for refusing a
	// change to one of them and for saying where each was set. Nil means none.
	PolicyOverlay *corepolicy.Overlay

	// StartedAt is when this process started. An adapter configured after it
	// is saved but not running, since adapters are loaded at startup (R-253).
	StartedAt time.Time

	// Restart asks the server to shut down cleanly and start again, loading
	// the adapters and the configuration file afresh. It returns at once; the
	// restart follows the response. Nil means this process cannot restart
	// itself, and POST /restart says so.
	Restart func()

	// Startup is the configuration Pando started with, for GET /config: every
	// non-secret setting and where it came from. Nil in tests that do not set it.
	Startup *config.Config

	// AuditLog reads the append-only log (R-227). Nil on an install where the
	// endpoint should 500 rather than quietly return nothing — an empty audit
	// log and an unreadable one are very different answers.
	AuditLog AuditReader

	// AuditArchives lists and serves the months retention has archived
	// (R-347). Nil answers an empty list: an install with nothing archived
	// and one that cannot archive look the same from here, and both are true.
	AuditArchives AuditArchives

	// Backups records what has been backed up; Backup does the backing up
	// (R-212). Two fields because they are two concerns: the record has to
	// survive the thing it records, which is R-204's whole point.
	Backups *state.Backups
	Backup  *backup.Service

	// Groups and Roles back the identity endpoints (R-078, R-082).
	Groups *state.Groups
	Roles  *state.Roles

	// BundleSource supplies what goes into a backup. Held separately from
	// Backups because one records what was taken and the other reads what is
	// being taken — and the record has to outlive the thing (R-204).
	BundleSource *state.BundleSource

	// Idempotency deduplicates retried infrastructure-creating requests. Nil
	// disables replay rather than failing: the cost is a possible duplicate,
	// and refusing to deploy because a deduplication table is missing would be
	// a worse outcome than deploying twice.
	Idempotency *state.Idempotency

	// Console serves the embedded UI on Pando's own paths. Nil when the binary
	// was built without it, in which case those paths 404 like any other and
	// the API is unaffected — the API is the product (R-261), and the console
	// is one of its clients.
	Console http.Handler

	// AppHosts tells the console handler when a request belongs to an app
	// instead. Nil means the console answers on every hostname, which is
	// correct for a path-addressed install and wrong for a subdomain one.
	AppHosts AppHosts

	// AppProxy serves every request to every app (R-023). Mounted last, as the
	// catch-all, so Pando's own routes are reachable and everything else goes
	// through enforcement. There is no path that reaches an app without it.
	AppProxy http.Handler
}

// PolicyDocument reads and writes host policy.
type PolicyDocument interface {
	Load(ctx context.Context) (corepolicy.Document, error)
	Save(ctx context.Context, doc corepolicy.Document, updatedBy string) error
}

// AuditReader queries the audit log.
type AuditReader interface {
	List(ctx context.Context, q audit.Query) ([]audit.Record, error)
}

// AuditArchives lists and opens audit archives.
type AuditArchives interface {
	List(ctx context.Context) ([]audit.ArchiveRecord, error)
	Open(ctx context.Context, archiveID string) (audit.ArchiveRecord, io.ReadCloser, error)
}

// SpecDefaults supplies the install's defaults for a spec.
type SpecDefaults interface {
	Defaults(ctx context.Context) spec.Defaults
}

// AppHosts answers whether a hostname belongs to an app.
//
// Used only to decide whether the console or the proxy should handle a request
// (see consoleOrApp). It is not an authorization decision — the proxy still
// makes that — and a failure here falls back to the console, which is the safe
// direction: someone sees Pando instead of their app, rather than reaching an
// app without passing through the proxy.
type AppHosts interface {
	IsAppHostname(ctx context.Context, host string) (bool, error)
}

// AnonymousPolicy gates sharing an app with everyone (R-076).
type AnonymousPolicy interface {
	AllowsAnonymousGrant(ctx context.Context, withPasscode bool) error
	PublicSharing(ctx context.Context) (corepolicy.PublicSharing, error)
}

// SourcePolicy gates where apps may be created from (R-092).
//
// Evaluated before anything touches disk. There is nothing to clone yet in this
// phase, but the check belongs at creation and putting it here now means the
// ordering is already right when cloning arrives in phase 6.
type SourcePolicy interface {
	AllowsSource(ctx context.Context, url string) error
}

// audit records an event, logging rather than failing the request if the write
// does not land. An audit failure must not become a denial of service on the
// action being audited — but it must never pass silently either.
func (s *Server) audit(r *http.Request, e audit.Event) {
	if s.Auditor == nil {
		return
	}
	e.RequestID = RequestIDFrom(r.Context())
	if err := s.Auditor.Write(r.Context(), e); err != nil {
		log.From(r.Context()).Error("audit write failed", zap.String("action", e.Action), zap.Error(err))
	}
}

// Routes builds the router.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)
	r.Use(RequestID)
	r.Use(Logger(s.Logger))

	// The assertion verification keys (R-057). Unauthenticated by design: they
	// are public keys, and an app must be able to fetch them before it has any
	// credential of its own.
	r.Get("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		if s.Minter == nil {
			Error(w, r, errs.New(errs.Internal, "Assertion signing is not set up."))
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		JSON(w, http.StatusOK, s.Minter.JWKS())
	})

	// Liveness. Deliberately does not touch the database: a health check that
	// fails when Postgres blips causes an orchestrator to kill a process that
	// was about to recover on its own.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness. This one does check, because "ready to serve traffic" is
	// exactly the question it answers.
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.Ping(r.Context()); err != nil {
			JSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not ready",
				"reason": "state store unreachable",
			})
			return
		}
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(Authenticate(s.Authent))

		r.Post("/sessions", s.handleLogin)
		r.Delete("/sessions", s.handleLogout)

		// Signing in through an external identity provider (R-043). Public:
		// nobody is signed in yet. Browser navigations, answered with
		// redirects; design 06 §3.2 has the flow.
		r.Get("/auth/options", s.handleSignInOptions)
		r.Get("/auth/providers/{providerID}/start", s.handleSSOStart)
		r.Get("/auth/providers/{providerID}/callback", s.handleSSOCallback)
		r.Post("/auth/providers/{providerID}/callback", s.handleSSOCallback)
		r.Get("/auth/providers/{providerID}/metadata", s.handleSAMLMetadata)
		r.Get("/auth/complete", s.handleSSOComplete)
		r.Get("/auth/failures/{flowID}", s.handleSSOFailure)

		// SCIM 2.0 (R-048): a provider's own bearer token, not a Pando one.
		// The Authenticate middleware leaves this prefix alone.
		r.Route("/scim/v2", s.scimRoutes)

		// Identity providers: read with install.view, changed with
		// install.adapters.manage — identity is an adapter category (R-040).
		r.Route("/identity-providers", func(r chi.Router) {
			r.Get("/", s.handleListIdentityProviders)
			r.Post("/", s.handleCreateIdentityProvider)
			r.Get("/{providerID}", s.handleGetIdentityProvider)
			r.Patch("/{providerID}", s.handlePatchIdentityProvider)
			r.Delete("/{providerID}", s.handleDeleteIdentityProvider)
			r.Post("/{providerID}/check", s.handleCheckIdentityProvider)
			r.Post("/{providerID}/scim-token", s.handleRotateSCIMToken)
			r.Delete("/{providerID}/scim-token", s.handleDisableSCIM)
			r.Get("/{providerID}/test", s.handleTestIdentityProvider)
			r.Get("/{providerID}/tests/{flowID}", s.handleIdentityProviderTest)
		})

		// First-run setup (R-046): public, and refused once any account
		// exists.
		r.Get("/setup", s.handleGetSetup)
		r.Post("/setup", s.handlePostSetup)

		// A strong random password, for creating or resetting an account.
		r.Post("/passwords/generate", s.handleGeneratePassword)
		r.Get("/me", s.handleMe)

		// Changing your own password. Self only, no verb — see the handler.
		r.Post("/me/password", s.handleChangePassword)

		// The launcher (R-264). Data-plane scoped, deliberately a different
		// list from GET /apps.
		r.Get("/me/apps", s.handleMyApps)

		// Favorites (R-341): the caller's own, pinned to the top of that
		// list. Self only, like the password.
		r.Put("/me/favorites/{appID}", s.handleFavoriteApp)
		r.Delete("/me/favorites/{appID}", s.handleUnfavoriteApp)

		// Sections (R-342): the caller's own groupings in that list.
		r.Post("/me/sections", s.handleCreateSection)
		r.Patch("/me/sections/{sectionID}", s.handleRenameSection)
		r.Delete("/me/sections/{sectionID}", s.handleDeleteSection)
		r.Put("/me/sections/{sectionID}/apps/{appID}", s.handlePlaceApp)
		r.Delete("/me/sections/{sectionID}/apps/{appID}", s.handleUnplaceApp)

		// Accounts. Reading or changing your own needs nothing
		// administrative; doing either to someone else needs an
		// install-scoped verb (O-17). Before those verbs existed these were
		// gated only by being signed in, which meant any account could suspend
		// the administrator.
		r.Route("/users", func(r chi.Router) {
			r.Get("/", s.handleListUsers)
			r.Post("/", s.handleCreateUser)
			r.Get("/{userID}", s.handleGetUser)
			r.Patch("/{userID}", s.handlePatchUser)

			// Promotion and demotion, deliberately not a field on PATCH.
			// Changing someone's status and changing their power are different
			// acts, and folding them into one body is how a status update
			// quietly becomes a promotion.
			r.Put("/{userID}/role", s.handlePutUserRole)
			r.Delete("/{userID}/role", s.handleDeleteUserRole)

			// An administrator's reset of someone else's password (R-046).
			r.Post("/{userID}/password", s.handleResetPassword)

			// The external identities that sign in to an account, and linking
			// one — an alias, never a merge (O-1).
			r.Get("/{userID}/identities", s.handleListIdentities)
			r.Post("/{userID}/identities", s.handleLinkIdentity)
			r.Delete("/{userID}/identities", s.handleUnlinkIdentity)

			// The apps an account has something on, and what (R-081).
			r.Get("/{userID}/apps", s.handleUserApps)

			// Deletion fires R-282's destruction rules. A separate route from
			// PATCH status, because suspension is not deletion (R-049) and
			// neither should be reachable by mistyping the other.
			r.Delete("/{userID}", s.handleDeleteUser)
		})

		// Groups (R-078) and custom roles (R-082). Both decide what anyone can
		// do here, so both are administration.
		r.Route("/groups", func(r chi.Router) {
			r.Get("/", s.handleListGroups)
			r.Post("/", s.handleCreateGroup)
			r.Put("/{groupID}/members", s.handleSetGroupMembers)
			r.Put("/{groupID}/members/{userID}", s.handleAddGroupMember)
			r.Delete("/{groupID}/members/{userID}", s.handleRemoveGroupMember)

			// A group's installation role and its app grants: what everyone
			// in it holds (R-078).
			r.Put("/{groupID}/role", s.handlePutGroupRole)
			r.Delete("/{groupID}/role", s.handleDeleteGroupRole)
			r.Get("/{groupID}/apps", s.handleGroupApps)

			// A provider's group counting as a Pando group's members (R-078).
			r.Put("/{groupID}/links/{syncedGroupID}", s.handleLinkGroup)
			r.Delete("/{groupID}/links/{syncedGroupID}", s.handleUnlinkGroup)
			r.Delete("/{groupID}", s.handleDeleteGroup)
		})

		// The verb catalog, for composing a custom role.
		r.Get("/verbs", s.handleListVerbs)

		// The API's own description: every endpoint, the CLI, the MCP tools
		// and the error codes, built from this binary (R-261). Behind
		// authentication and no verb — the shape of the API is not a secret
		// from the people using it, and a product whose manual only
		// administrators can read has one client.
		r.Get("/reference", s.handleReference)

		// The roles that can be granted across the installation (R-082).
		r.Route("/roles", func(r chi.Router) {
			r.Get("/", s.handleListRoles)
			r.Post("/", s.handleCreateRole)
			r.Delete("/{roleID}", s.handleDeleteRole)
		})

		// Tokens: how the CLI and an agent authenticate (R-262). Self-service,
		// because a token holds nothing its owner does not — it is a second
		// credential for power already held, not new power.
		r.Route("/tokens", func(r chi.Router) {
			r.Get("/", s.handleListTokens)
			r.Post("/", s.handleCreateToken)

			// Account-level tokens (R-060): their own principal rather than a
			// second credential for somebody's, so behind a verb. Revoking one
			// goes through the same DELETE as any other token, which already
			// asks for install.users.manage when the token is not the
			// caller's.
			r.Get("/service", s.handleListServiceTokens)
			r.Post("/service", s.handleCreateServiceToken)

			r.Delete("/{tokenID}", s.handleRevokeToken)
		})

		// The install's own inventory, behind install.view. GET /adapters
		// returns live capabilities, not stored config, so the console can grey
		// out choices that would fail at plan time.
		r.Get("/adapters", s.handleListAdapters)
		r.Post("/adapters", s.handleCreateAdapter)
		r.Get("/adapters/kinds", s.handleAdapterKinds)

		// Which AI adapter handles each AI function, and on which model
		// (R-259). Read with install.view, changed with
		// install.adapters.manage, like the adapters themselves.
		r.Get("/ai/functions", s.handleListAIFunctions)
		r.Put("/ai/functions/{function}", s.handleAssignAIFunction)
		r.Delete("/ai/functions/{function}", s.handleUnassignAIFunction)

		// The AI functions themselves (R-343 … R-346). Each proposes and
		// none applies, and each is behind the verb its ordinary endpoint
		// needs: drafting a role is install.users.manage, like creating one.
		r.Post("/ai/access/draft", s.handleDraftAccess)
		r.Post("/ai/policy/draft", s.handleDraftPolicy)
		r.Post("/ai/audit/search", s.handleSearchAudit)
		r.Post("/ai/reference/answer", s.handleAnswerReference)
		r.Post("/restart", s.handleRestart)
		r.Get("/capacity", s.handleCapacity)

		// Host policy: read with install.view, written with
		// install.policy.manage. Seeing the rules you work under is not the
		// same privilege as changing them (R-274).
		r.Get("/policy", s.handleGetPolicy)
		r.Get("/config", s.handleGetConfig)
		r.Put("/policy", s.handlePutPolicy)

		// What this policy would block, before it is saved (design 05 §3).
		// Behind the write verb rather than install.view: the body is a policy
		// someone is composing, and answering "which apps does this break"
		// for anyone who can read policy hands them a probe for the whole
		// install's shape.
		r.Post("/policy/preview", s.handlePreviewPolicy)

		// The audit log (R-227). Its own verb: it records what everyone did,
		// including inside apps they own.
		r.Get("/audit", s.handleListAudit)

		// Months past retention, archived and removed from the live log
		// (R-347). Behind the same verb as the log itself: an archive is the
		// log, a month at a time.
		r.Get("/audit/archives", s.handleListAuditArchives)
		r.Get("/audit/archives/{archiveID}", s.handleGetAuditArchive)

		// Backup and disaster recovery (Sequence D). Verify is its own route
		// rather than a flag on restore, because a flag is a thing somebody
		// passes wrongly and the wrong value here replaces an installation.
		r.Route("/backups", func(r chi.Router) {
			r.Get("/", s.handleListBackups)
			r.Post("/", s.handleCreateBackup)
			r.Post("/{backupID}/verify", s.handleVerifyBackup)
			r.Post("/{backupID}/restore", s.handleRestoreBackup)
		})

		// Deploys waiting for approval, across every app the caller may view
		// (R-154): what an approver works through.
		r.Get("/approvals", s.handleListApprovals)

		r.Route("/apps", func(r chi.Router) {
			r.Get("/", s.handleListApps)
			r.Post("/", s.handleCreateApp)

			r.Route("/{appID}", func(r chi.Router) {
				r.Get("/", s.handleGetApp)
				r.Patch("/", s.handlePatchApp)
				r.Delete("/", s.handleDeleteApp)

				// The launcher tile's image (R-340). Reading it is the data
				// plane, like the tile itself; setting it is app.spec.edit.
				r.Get("/icon", s.handleGetAppIcon)
				r.Put("/icon", s.handleSetAppIcon)
				r.Delete("/icon", s.handleClearAppIcon)

				// Lifecycle. Start and stop set desired state and let the
				// reconciler converge (design 05), so "stopped" survives a
				// Pando restart. Restart is an act rather than a state and
				// goes through the runtime.
				r.Post("/start", s.handleStartApp)
				r.Post("/stop", s.handleStopApp)
				r.Post("/restart", s.handleRestartApp)

				// Slots and volumes: first-class objects in R-030 that had no
				// way to be reached.
				// The security score (R-310). Reading it is app.view;
				// asking for a new one is app.deploy, because a scan changes
				// what the next deploy will do.
				r.Get("/security", s.handleAppSecurity)
				r.Post("/security/scan", s.handleScanApp)

				r.Get("/slots", s.handleListSlots)
				r.Put("/slots/{key}", s.handleSetSlot)
				r.Get("/volumes", s.handleListVolumes)
				r.Post("/volumes", s.handleCreateVolume)

				// Restoring one app's data, in place (R-206). Gated on the app
				// rather than the install: it is an app operation, so an
				// owner can do it without install.backup.manage.
				r.Post("/restore", s.handleRestoreAppBackup)

				r.Get("/export", s.handleExportSpec)

				// `pando deploy ./` — a gzipped tar becomes the app's source
				// (design 04 §4). Gated by app.spec.edit, not app.deploy:
				// replacing the source changes what the app *is*.
				r.Post("/source", s.handleUploadSource)

				// Detection (design 04 §2.2). Re-detection is explicit
				// (R-022): nothing here runs on its own, because a spec that
				// changed under someone because a file moved in their
				// repository is a spec they did not write.
				r.Route("/detection", func(r chi.Router) {
					r.Get("/", s.handleGetDetection)
					r.Post("/rerun", s.handleRerunDetection)
					r.Get("/diff", s.handleDetectionDiff)
					r.Post("/answers", s.handleDetectionAnswers)
					r.Post("/revise", s.handleReviseDetection)
					r.Post("/accept", s.handleAcceptDetection)
				})

				// The dry run. Side-effect-free, so the console calls it on
				// every spec edit (design 04 §2.3).
				r.Post("/plan", s.handlePlan)

				// The app's egress rules, merged, and the installation's they
				// are edited against (R-182, R-188). Changed by writing a spec.
				r.Get("/egress", s.handleGetEgress)

				r.Get("/status", s.handleAppStatus)
				r.Get("/usage", s.handleAppUsage)
				r.Get("/logs", s.handleAppLogs)

				// A terminal inside a running workload (design 04 §2.4).
				//
				// The most privileged action in the system (R-086): a holder
				// can read the database directly and read injected environment
				// including secrets. The handler checks the verb, then host
				// policy (R-085), then writes the audit event, and only then
				// opens the session.
				r.Get("/exec", s.handleExec)

				r.Route("/deployments", func(r chi.Router) {
					r.Get("/", s.handleListDeployments)
					r.Post("/", s.handleDeploy)
					r.Post("/rollback", s.handleRollback)
					r.Get("/{depID}", s.handleGetDeployment)
					r.Get("/{depID}/logs", s.handleDeploymentLogs)

					// Deploy approval (R-154 – R-159). Behind app.view to
					// reach; who may decide is install.deploys.approve or
					// app.deploy.approve, which the service checks.
					r.Post("/{depID}/approve", s.handleApproveDeploy)
					r.Post("/{depID}/reject", s.handleRejectDeploy)
				})

				// Public with a passcode (R-075a): the passcode page's two calls,
				// made by a visitor with no account, so no verb — each handler
				// answers only for an app that asks for a passcode.
				r.Get("/passcode", s.handleGetPasscodeApp)
				r.Post("/passcode", s.handleEnterPasscode)

				// People and groups to share with, for whoever may share it.
				r.Get("/principals", s.handleSharePrincipals)

				r.Route("/grants", func(r chi.Router) {
					r.Get("/", s.handleListGrants)
					r.Post("/", s.handleCreateGrant)
					r.Patch("/{grantID}", s.handleSetGrantRole)
					r.Delete("/{grantID}", s.handleDeleteGrant)
				})

				r.Route("/secrets", func(r chi.Router) {
					r.Get("/", s.handleListSecrets)
					r.Put("/{key}", s.handlePutSecret)
					r.Delete("/{key}", s.handleDeleteSecret)

					// Its own verb (R-083): rotating a credential and reading
					// it are different levels of trust.
					r.Get("/{key}/value", s.handleReadSecretValue)
				})

				r.Get("/routing", s.handleGetRouting)
				r.Put("/routing", s.handleSetRouting)

				r.Route("/specs", func(r chi.Router) {
					r.Get("/", s.handleListSpecs)
					r.Post("/", s.handleCreateSpec)
					r.Get("/{rev}", s.handleGetSpec)
					r.Post("/{rev}/pin", s.handlePinSpec)
					r.Get("/{a}/diff/{b}", s.handleDiffSpecs)
				})
			})
		})
	})

	// The console, on Pando's own paths only.
	//
	// Never as the fallback: that belongs to the proxy, and a console mounted
	// there would shadow every app whose slug it did not recognize. The routes
	// listed here are the console's own, and each one is a path no app can
	// have because the slug would collide with a reserved name.
	if s.Console != nil {
		console := s.consoleOrApp()
		for _, route := range consoleRoutes {
			r.Handle(route, console)
		}
	}

	// Pando's own everything, on every hostname including an app's own.
	//
	// An unauthenticated visit to a subdomain app used to redirect to "/login"
	// on the app's own hostname, where consoleOrApp handed it straight back to
	// the proxy, which redirected to "/login" again: an infinite loop with the
	// query string growing on every hop. Subdomain routing was unusable for any
	// app that was not public, which is the mode Traefik makes the default
	// (R-172).
	//
	// A reserved prefix rather than "/login", because on an app's hostname
	// every path belongs to the app: taking "/login" would shadow the login
	// page of any app that has one, and R-171 says an app's own login is that
	// app working correctly. A slug cannot contain a dot — slugPattern allows
	// only [a-z0-9-] — so ".pando" is a path no app can ever claim.
	//
	// The whole router rather than the console alone, because a sign-in page
	// needs somewhere to post to. It re-enters with the prefix removed, which
	// terminates because the path is shorter every time. Nothing is exposed
	// that the front door does not already expose to the same caller, with the
	// same authentication and the same authorization.
	r.Handle(ReservedPrefix, http.RedirectHandler(ReservedPrefix+"/", http.StatusMovedPermanently))
	r.Handle(ReservedPrefix+"/*", http.StripPrefix(ReservedPrefix, http.HandlerFunc(
		func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()

			// A fresh routing context. Chi keeps its matching position in the
			// request context, so re-entering with the old one resumes from
			// where the match left off rather than matching the shortened path
			// from the start — every reserved path would land on whatever the
			// outer route had already chosen.
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chi.NewRouteContext())

			// And a note that this request came in reserved, which is what
			// tells consoleOrApp to serve the console even on a hostname that
			// belongs to an app. That is the whole point of the prefix: it is
			// Pando's everywhere, so the sign-in page exists everywhere.
			ctx = context.WithValue(ctx, reservedKey{}, true)

			r.ServeHTTP(w, req.WithContext(ctx))
		})))

	// Everything that is not one of Pando's own routes is a request to an app,
	// and goes through the proxy (R-023). Mounting it as the fallback rather
	// than on a prefix is what makes "there is no bypass" structural: a route
	// that does not exist above cannot reach an app any other way.
	if s.AppProxy != nil {
		r.NotFound(s.AppProxy.ServeHTTP)
	}

	return r
}

// consoleOrApp serves the console, unless the request is addressed to an app.
//
// The console owns "/" and a handful of other paths, which is right when Pando
// is reached at its own hostname. It is wrong the moment an app has a hostname
// of its own: a request to https://notes.example.com/ matches the console's "/"
// route and never reaches the proxy, so the app's owner gets Pando's console
// where their app should be — and every app in an install using subdomain
// addressing is unreachable at its root.
//
// Resolved by asking whether the Host names an app, rather than by removing "/"
// from the console's routes: the console does own "/" on Pando's own hostname,
// and in path mode that is the only hostname there is.
// reservedKey marks a request that arrived under ReservedPrefix.
type reservedKey struct{}

func (s *Server) consoleOrApp() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reserved paths are Pando's on every hostname, including one that
		// belongs to an app — otherwise there is nowhere to sign in to reach
		// that app (R-172).
		if reserved, _ := r.Context().Value(reservedKey{}).(bool); reserved {
			s.Console.ServeHTTP(w, r)
			return
		}

		if s.AppProxy != nil && s.AppHosts != nil {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if isApp, err := s.AppHosts.IsAppHostname(r.Context(), host); err == nil && isApp {
				s.AppProxy.ServeHTTP(w, r)
				return
			}
		}
		s.Console.ServeHTTP(w, r)
	})
}

// ReservedPrefix is the one path that is Pando's on every hostname.
//
// Its users are the sign-in page and the assets that page needs, reached from
// an app's hostname where every other path is the app's. Leading dot so it
// cannot collide with an app slug, which may not start with one.
const ReservedPrefix = "/.pando"

// LoginPath is where the proxy sends someone who needs to sign in.
const LoginPath = ReservedPrefix + "/login"

// ReservedOrApp serves Pando's reserved path, and everything else from app.
//
// For the listeners that are not the front door: a port-mode app has a socket
// of its own whose every path belongs to that app (design 03 §4.2), so the
// router — and with it the sign-in page — is not in front of it. Without this
// the R-172 redirect lands back on the proxy and loops, which is the bug R-172
// exists to fix, reintroduced one listener over.
func ReservedOrApp(pando, app http.Handler) http.Handler {
	if pando == nil {
		return app
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ReservedPrefix || strings.HasPrefix(r.URL.Path, ReservedPrefix+"/") {
			pando.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	})
}

// consoleRoutes are the paths the console owns.
//
// Enumerated rather than a prefix wildcard, so that adding a console route is a
// deliberate act that a reviewer sees. Every one of these is also a slug an app
// cannot have, which is what keeps the two namespaces from colliding.
//
// The console's HTML asks for its assets at "/.pando/assets/…", which reaches
// this list the same way everything reserved does: with the prefix stripped and
// re-entered, so "/assets/*" has to be a route here or the sign-in page loads
// without the script that draws it. That is how it broke once — the route was
// removed on the reasoning that the reserved mount served assets itself, which
// it had stopped doing.
var consoleRoutes = []string{
	"/",
	"/index.html",
	"/assets/*",
	"/login",
	"/admin",
	"/admin/*",
}
