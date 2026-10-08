// Command pando is both the CLI and the server entrypoint (design 00 §2).
//
// Adapter registration happens here, in main, from compiled-in packages (R-253).
// Everything ships as one binary: server, CLI, and the embedded console.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	aianthropic "github.com/trypando/pando/internal/adapter/ai/anthropic"
	ailocal "github.com/trypando/pando/internal/adapter/ai/local"
	aiopenai "github.com/trypando/pando/internal/adapter/ai/openai"
	adapterapi "github.com/trypando/pando/internal/adapter/api"
	backuplocal "github.com/trypando/pando/internal/adapter/backup/local"
	buildkitadapter "github.com/trypando/pando/internal/adapter/builder/buildkit"
	"github.com/trypando/pando/internal/adapter/identity/local"
	oidcidentity "github.com/trypando/pando/internal/adapter/identity/oidc"
	samlidentity "github.com/trypando/pando/internal/adapter/identity/saml"
	registryecr "github.com/trypando/pando/internal/adapter/imageregistry/ecr"
	registryoci "github.com/trypando/pando/internal/adapter/imageregistry/oci"
	"github.com/trypando/pando/internal/adapter/notify/chat"
	notifyconsole "github.com/trypando/pando/internal/adapter/notify/console"
	notifyntfy "github.com/trypando/pando/internal/adapter/notify/ntfy"
	notifysmtp "github.com/trypando/pando/internal/adapter/notify/smtp"
	"github.com/trypando/pando/internal/adapter/routing/cloudflare"
	"github.com/trypando/pando/internal/adapter/routing/loopback"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	dockerruntime "github.com/trypando/pando/internal/adapter/runtime/docker"
	kubernetesruntime "github.com/trypando/pando/internal/adapter/runtime/kubernetes"
	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	trivyscanner "github.com/trypando/pando/internal/adapter/scanner/trivy"
	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	sourceazure "github.com/trypando/pando/internal/adapter/source/azuredevops"
	sourcebitbucket "github.com/trypando/pando/internal/adapter/source/bitbucket"
	sourcegeneric "github.com/trypando/pando/internal/adapter/source/generic"
	sourcegitea "github.com/trypando/pando/internal/adapter/source/gitea"
	sourcegithub "github.com/trypando/pando/internal/adapter/source/github"
	sourcegitlab "github.com/trypando/pando/internal/adapter/source/gitlab"
	"github.com/trypando/pando/internal/cli"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/console"
	"github.com/trypando/pando/internal/core/address"
	"github.com/trypando/pando/internal/core/approval"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/assist"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/capacity"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/cluster"
	"github.com/trypando/pando/internal/core/deploy"
	"github.com/trypando/pando/internal/core/detection"
	"github.com/trypando/pando/internal/core/edge"
	"github.com/trypando/pando/internal/core/edgecert"
	"github.com/trypando/pando/internal/core/idp"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/logstream"
	"github.com/trypando/pando/internal/core/observe"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/planner"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/retention"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/subscription"
	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/detect/registryprobe"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/proxy"
	"github.com/trypando/pando/internal/reference"
	"github.com/trypando/pando/internal/secret"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		// Cobra has already printed the error. The code is 1 unless the
		// command asked for another: `pando app detection --wait` exits 2 when
		// a person has questions to answer.
		os.Exit(cli.ExitCodeOf(err))
	}
	if restartRequested.Load() {
		reexec()
	}
}

// restartRequested is set when serve returned because someone asked for a
// restart (POST /restart) rather than because it was stopped.
var restartRequested atomic.Bool

// reexec starts Pando again in this process: the same binary, arguments and
// environment, and the same PID, so a container's PID 1 stays PID 1 and no
// supervisor or restart policy is needed. Only after serve has returned, so the
// database pool is closed and the listener released first.
//
// The environment is the one the process started with, as with docker compose
// restart; the configuration file is read afresh.
func reexec() {
	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, os.Args, os.Environ()) //nolint:gosec // G702: this binary, with the arguments and environment it was started with; nothing from a request reaches it.
	}
	// Exec returns only on failure. Exit non-zero so that a supervisor, if
	// there is one, starts Pando instead.
	fmt.Fprintf(os.Stderr, "pando could not restart itself: %v\n", err)
	os.Exit(1)
}

func rootCmd() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:           "pando",
		Short:         "Host your apps without setting up a deployment pipeline",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "path to a config file")

	root.AddCommand(serveCmd(&configPath), migrateCmd(&configPath), adminCmd(&configPath), versionCmd())

	// Hidden: started by the Docker runtime in front of a restricted app, from
	// Pando's own image, never by a person (R-187).
	root.AddCommand(egressGatewayCmd())
	root.AddCommand(upgradeHelperCmd())
	// The forwarding agent on each host of a multi-host Docker install (O-45),
	// started by that runtime; and the authority it and Pando trust.
	root.AddCommand(hostAgentCmd())
	root.AddCommand(cli.SelfUpdateCmd())

	// The client half (design 04 §4). In the same binary because Pando ships as
	// one, and a client of the API like any other (R-261) — internal/cli
	// imports no core package, so a command that needed something the API
	// cannot do would not compile rather than quietly growing a shortcut.
	cli.Version = buildVersion
	root.AddCommand(cli.Commands()...)
	return root
}

func serveCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the Pando server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), *configPath)
		},
	}
}

func migrateCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations and exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := setup(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()

			ctx := log.Into(cmd.Context(), logger)
			return state.Migrate(ctx, cfg.Database.URL)
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			out := cmd.OutOrStdout()
			if buildVersion == "dev" {
				fmt.Fprintln(out, "pando (development build)")
				return
			}
			fmt.Fprintf(out, "pando %s\n", buildVersion)
			if buildCommit != "" {
				fmt.Fprintf(out, "commit %s\n", buildCommit)
			}
			if buildDate != "" {
				fmt.Fprintf(out, "built %s\n", buildDate)
			}
		},
	}
}

// buildPlanner returns the configured builder if it can make a build plan.
//
// A capability discovered by type assertion, which R-254 forbids for anything
// the planner decides on — and this is not that. Whether a build plan can be
// shown before it runs changes nothing about whether the app can be built: the
// builder plans at build time regardless. It changes only whether detection has
// something to show, which is why a missing planner degrades to the behavior
// every install had before and not to a plan-time refusal.
func buildPlanner(registry *adapterapi.Registry) detect.BuildPlanner {
	ref, ok := registry.Default(adapterapi.CategoryBuilder)
	if !ok {
		return nil
	}
	builder, ok := registry.Builder(ref)
	if !ok {
		return nil
	}
	planner, ok := builder.(detect.BuildPlanner)
	if !ok {
		return nil
	}
	return planner
}

func setup(configPath string) (*config.Config, *zap.Logger, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	logger, err := log.New(cfg.Log.Level, cfg.Log.Development)
	if err != nil {
		return nil, nil, err
	}
	return cfg, logger, nil
}

func serve(ctx context.Context, configPath string) error {
	cfg, logger, err := setup(configPath)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	ctx = log.Into(ctx, logger)

	// Interrupts cancel the context, which unwinds the connect retry as well as
	// the server. Ctrl-C during a sixty-second wait for Postgres should exit,
	// not be ignored until the wait expires.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Adapters saved after this are not running until a restart (R-253); the
	// adapters list compares against it. restartCh is how POST /restart asks.
	startedAt := time.Now().UTC()
	restartCh := make(chan struct{}, 1)

	// Connect runs the whole bootstrap: wait for Postgres, migrate as the owner,
	// provision the restricted application role, apply grants, and verify that
	// the audit log cannot be rewritten. It refuses to return a usable database
	// if that last check fails (R-027).
	db, err := state.Connect(ctx, state.ConnectOptions{
		OwnerURL:       cfg.Database.URL,
		ConnectTimeout: cfg.Database.ConnectTimeout,
		MaxConns:       cfg.Database.MaxConns,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	auditor := audit.New(db.Pool)

	// A background job is as visible as a person (design 06 §1).
	if err := auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "server.start",
	}); err != nil {
		return err
	}

	users := state.NewUsers(db)
	sessions := state.NewSessions(db)
	// API tokens are stored as HMAC-SHA-256 under a key kept beside the
	// process, not in the database (R-063). Every replica must hold the same
	// one, and one that does not stops here, before it rejects every token
	// the others issued (issue #72).
	tokenKey, err := tokenkey.LoadOrCreate(cfg.Server.TokenKeyPath)
	if err != nil {
		return err
	}
	tokens := state.NewTokens(db, tokenKey)
	if err := tokens.VerifyKey(ctx); err != nil {
		return err
	}
	apps := state.NewApps(db)
	volumes := state.NewVolumes(db)
	adapters := state.NewAdapters(db)
	allocations := state.NewAllocations(db)
	authzStore := state.NewAuthzStore(db)
	grants := state.NewGrants(db)

	// Host policy is loaded per evaluation, not cached: R-274 says policy
	// applies to a running install, and a cached document would keep allowing
	// or keep denying for as long as the cache lived.
	//
	// Any field set in the startup config (a `policy:` section, or
	// PANDO_POLICY_<FIELD>) is laid over the stored document by the store
	// itself, so every reader — evaluator, handlers, the security pass — sees
	// it, and a save cannot write it into the stored one (R-271). A startup
	// policy that is not a policy, or will not read as one, stops startup.
	policyOverlay, err := startupPolicy(cfg)
	if err != nil {
		return err
	}
	policyStore := policyOverlay.Wrap(state.NewPolicy(db))
	hostPolicy := corepolicy.New(policyStore.Load)

	// R-046: a fresh installation waits for the first person to open the
	// console and set up the administrator there — unless the operator supplied
	// PANDO_ADMIN_PASSWORD, in which case the account is made now. No password
	// is ever printed.
	first, err := bootstrap.Run(ctx, users, grants, db, auditor,
		secret.New(cfg.Bootstrap.AdminPassword))
	if err != nil {
		return err
	}
	switch {
	case first.Created:
		// No password field. The operator supplied it and already has it;
		// printing it would copy a credential into a log for nobody's benefit
		// (R-194).
		logger.Warn("first run: created an administrator account",
			zap.String("username", bootstrap.AdminUsername),
			zap.String("note", "using the password from PANDO_ADMIN_PASSWORD; it must still be changed on first login"))

	case first.Unclaimed:
		// Said loudly: until somebody does this, whoever reaches the console
		// first becomes the administrator.
		logger.Warn("this installation is not set up yet",
			zap.String("next", "open the console and set up the administrator account"),
			zap.String("note", "the first person to reach the sign-in page sets it up; do this before exposing Pando to anyone else"))

	case cfg.Bootstrap.AdminPassword != "":
		// Said out loud, because the alternative is an operator who set it,
		// cannot sign in with it, and has no reason to suspect it was never
		// read.
		logger.Info("PANDO_ADMIN_PASSWORD was set and ignored",
			zap.String("reason", "this installation already has accounts"),
			zap.String("remedy", "run `pando admin reset-password` to set one"))
	}

	// Adapter registration happens here, in main, from compiled-in packages
	// (R-253). There is no plugin protocol and none is planned.
	identity := local.New(users)

	notifications := state.NewNotifications(db)

	registry, adapterCredentials, err := registerAdapters(ctx, db, adapters, notifications, cfg.Adapters, logger)
	if err != nil {
		return err
	}

	// Secrets go through the configured secrets adapter. Core never encrypts —
	// it stores what the adapter hands back (R-190, R-191).
	secretsRef, _ := registry.Default(adapterapi.CategorySecrets)
	secretsAdapter, _ := registry.Secrets(secretsRef)
	secrets := state.NewSecrets(db, secretsAdapter, secretsRef)

	// Every replica must hold the same secrets key, which by design is not in
	// the database (R-190). One that does not stops here, before it can write
	// a secret no other replica can read (issue #72).
	if err := secrets.VerifyKey(ctx); err != nil {
		return err
	}

	// External identity (issue #51). Unlike other adapter categories, a
	// provider is built from its stored row when first used and rebuilt when
	// it changes, so connecting one does not need a restart. The kinds are
	// compiled in, like everything else (R-253).
	identityService := &idp.Service{
		Providers:   state.NewIdentityProviders(db),
		Credentials: state.NewIdentityCredentials(db, secretsAdapter, secretsRef),
		Identities:  state.NewIdentities(db),
		Users:       users,
		Sessions:    sessions,
		Groups:      state.NewGroups(db),
		Flows:       state.NewSSOFlows(db),
		SCIMUsers:   state.NewSCIMUsers(db),
		SCIMGroups:  state.NewSCIMGroups(db),
		Policy:      policyStore,
		Local:       identity,
		Kinds: map[string]idp.Kind{
			oidcidentity.Kind: {New: func() adapterapi.IdentityAdapter { return oidcidentity.New() }, Info: oidcidentity.Info()},
			samlidentity.Kind: {New: func() adapterapi.IdentityAdapter { return samlidentity.New() }, Info: samlidentity.Info()},
		},
		Audit: httpapi.AuditFunc(auditor),
		Clock: clock.System{},
	}

	// Backup and disaster recovery (Sequence D). The service does the work; the
	// store records what it produced, and the record outlives the thing it
	// records (R-204).
	backups := state.NewBackups(db)
	bundleSource := state.NewBundleSource(db)
	backupService := &backup.Service{
		Registry:    registry,
		DatabaseURL: secret.New(cfg.Database.URL),

		// The local secrets adapter's key. Without it in the bundle a restored
		// install holds every app's ciphertext and nothing that opens it
		// (R-212) — which looks like a successful restore until an app starts.
		// Read from the adapter's own configuration rather than duplicated into
		// server config, so there is one place that decides where the key lives.
		SecretsKeyPath: secretsKeyPath(ctx, adapters, logger),

		// The API token key, for the same reason: without it a restored
		// install holds every token's digest and cannot check any of them.
		TokenKeyPath: cfg.Server.TokenKeyPath,

		// Uploaded source is the only record of what an uploaded app is built
		// from, and the registry's images are not in the bundle (O-37).
		UploadDir: source.DefaultUploadDir,

		State:         bundleSource,
		Version:       buildVersion,
		SchemaVersion: db.SchemaVersion(),
		WorkDir:       cfg.Server.WorkDir,
	}

	// pg_restore recreates every table and function with the owner's default
	// privileges, which hand the application role UPDATE and DELETE on each
	// month of the audit log until the next start re-grants. Re-granting
	// straight after the restore closes that window (R-027, R-348).
	backupService.Regrant = func(ctx context.Context) error { return state.Regrant(ctx, cfg.Database.URL) }

	// Audit retention (R-347, R-348). Archives are kept under Pando's own
	// directory, or exported to a backup destination, as host policy says.
	auditStores, err := auditArchiveStores(ctx, cfg.Server.AuditArchiveDir, registry)
	if err != nil {
		return err
	}

	deployments := state.NewDeployments(db)
	logStore := deploy.NewLogStore()
	// An image app's image, read from its registry with the app's own
	// credential (issue #41): detection pins and reads it, the planner checks
	// its platform, the deployer pulls it. One value so all three agree.
	registryCredentials := state.NewRegistryCredentials(db, secretsAdapter, secretsRef)
	images := &oci.Images{Credentials: registryCredentials}
	if cfg.Apps.DockerCredentials {
		// The server's own `docker login`, for apps with no credential of
		// their own. An operator's choice: it lends every app what that login
		// can read.
		images.Docker = oci.DockerLogin()
		logger.Info("private images may be pulled with the Docker login on this server (apps.docker_credentials)")
	}

	// The install's image registry (issue #72, PR 5; an adapter since issue
	// #153): where builds go for a runtime that pulls. None on a single host,
	// which imports (O-34). A stored one is built from its row on every push,
	// pull and plan, so a credential rotated on one replica reaches every
	// replica without a restart; one declared at startup (PANDO_REGISTRY_* or
	// the config file) was built with the other adapters, above.
	buildRegistry := &imageregistry.Service{
		Configs:     adapters,
		Credentials: adapterCredentials,
		New:         newImageRegistryAdapter,
		Declared:    declaredImageRegistries(registry),
	}
	if current, err := buildRegistry.Current(ctx); err != nil {
		logger.Warn("the install's image registry cannot be used; builds that need it will be refused",
			zap.Error(err))
	} else if current.Configured() {
		logger.Info("built images may be pushed to the install's image registry", zap.String("adapter", current.ID()),
			zap.String("registry", current.Host()), zap.Bool("always", current.Always()))
	}

	appPlanner := planner.New(registry, hostPolicy, allocations).WithInventory(apps).WithImages(images).
		WithInstallRegistry(buildRegistry)
	capacityReadings := &capacity.Snapshots{Registry: registry, Allocations: allocations, Logger: logger}

	// Every route points here (R-023). The proxy is phase 5; until it exists
	// this is the address routing adapters are told to use, and it is already
	// Pando's own rather than any workload's.
	proxyUpstream := cfg.Server.ProxyUpstream
	if proxyUpstream == "" {
		proxyUpstream = "http://pando:8080"
	}
	reconciles := state.NewReconciles(db)
	// The security score (R-310). One service, three readers: the deploy path
	// scores what it built, the API serves the number, and the GC places every
	// app against the threshold.
	scans := state.NewScans(db)
	securityService := &security.Service{
		Scans:       scans,
		Deployments: deployments,
		Registry:    registry,
		Policy:      hostPolicy,
		Auditor:     auditor,
		Logger:      logger,
	}

	// Where an app's source comes from, and where an uploaded one is kept
	// (R-262). One value for the deploy path, detection, the API and the GC.
	sources := source.Sources{UploadDir: source.DefaultUploadDir, WorkDir: cfg.Server.WorkDir}

	// The install's source connections (R-091, issue #127): what a private
	// repository is cloned with. Sources read through it for a credential,
	// and it probes through Sources when an app is added.
	sourceConnections := &sourceconn.Service{
		Configs:        adapters,
		Credentials:    adapterCredentials,
		Authorizations: sourceAuthorizationsFor(db, registry),
		New:            newSourceAdapter,
		Declared:       declaredSourceAdapters(registry),
		Policy:         hostPolicy,
		Audit:          auditor,
		Clock:          clock.System{},
	}
	sources.Credentials = sourceConnections
	sourceConnections.Prober = sources

	deployer := deploy.NewRunner(registry, appPlanner, apps, deployments, secrets, reconciles, logStore, volumes, proxyUpstream).
		WithServices(state.NewServices(db), secrets).
		WithSecurity(securityService).
		WithSources(sources).
		WithImages(images).
		WithBuildRegistry(buildRegistry)

	// The deploy queue (issue #72, O-32): a deploy is queued in Postgres and
	// run by whichever replica has room, at most work.deploys at once here.
	// Served once the loops start, below.
	deployQueue := &deploy.Queue{
		Runner:      deployer,
		Deployments: deployments,
		Revisions:   apps,
		Limit:       cfg.Work.Deploys,
		Logger:      logger,
	}

	// Detection (Sequence A). Every detector bids; the runtime supplies the
	// trial run (R-097), and a registry probe would supply R-094's top tier.
	// Both are optional here, and missing either degrades to a question rather
	// than to a failure.
	// The install's defaults: its adapters, its routing shape, and the
	// retention caps R-211 and R-223 set. Shared between detection and the
	// spec endpoint, because a hand-written spec needs them just as much — and
	// used to get none of them.
	installDefaults := detection.NewInstallation(registry, cfg.Server.BaseDomain)
	// R-240: the limits every new app inherits are the host's to set.
	installDefaults.Fallback.Resources = cfg.Apps.Resources(installDefaults.Fallback.Resources)

	detections := state.NewDetections(db)
	detector := &detection.Runner{
		// The score of what was proposed, before anybody decides whether to
		// deploy it (R-312).
		Scanner:    sourceScanner{service: securityService, logger: logger},
		Apps:       apps,
		Detections: detections,
		Policy:     hostPolicy,
		Sources:    sources,

		// R-104: everything a repository cannot say about itself is
		// configuration, not a question. Without this a detected spec describes
		// the app and says nothing about where it runs, which is a spec the
		// planner refuses.
		Install: installDefaults,

		Ports:          state.NewPorts(db),
		PortRangeStart: cfg.Server.PortRangeStart,
		PortRangeEnd:   cfg.Server.PortRangeEnd,
		Job: &detect.Job{
			Auction: detect.NewAuction(
				detect.DockerfileDetector{},
				detect.ComposeDetector{},
				detect.StaticDetector{},
				// The planner comes from the builder: core asks "how would you
				// build this" and stores the answer without interpreting it
				// (R-251). Nil on an install with no builder configured, where
				// detection still recognizes the language and asks its
				// questions — it just cannot show the plan.
				detect.BuildpackDetector{Planner: buildPlanner(registry)},
				detect.MonorepoDetector{},
			),
			Runtime: runtimeForTrial(registry),
			Images:  images,

			// R-094 tier 1. ghcr.io only by default — a Docker Hub username has
			// no relationship to the GitHub owner of the same name, so a match
			// there is not evidence that the image belongs to the project
			// (docs/design/notes-registry-tier-namespaces.md).
			Registry: registryprobe.New(),
		},
	}

	// The detection queue, as the deploy queue: queued in Postgres, run where
	// there is room, at most work.detections at once here.
	detectionQueue := &detection.Queue{
		Detections: detections,
		Detect:     detector.RunQueued,
		Limit:      cfg.Work.Detections,
		Logger:     logger,
	}

	// Which AI adapter handles each AI function (R-259): the stored
	// assignments with the config file's laid over them. Loaded into the
	// registry now and after every change, so a call asks the registry.
	aiFunctions := &assist.Assignments{
		Store:    state.NewAIAssignments(db),
		Registry: registry,
		Declared: declaredAssignments(cfg.Adapters),
		Name:     adapterNames(ctx, adapters, cfg.Adapters),
	}
	if err := aiFunctions.Load(ctx); err != nil {
		return err
	}

	// Screening (R-330, design 10). Optional in the strong sense: an install
	// with no AI function assigned is not a degraded install, because
	// everything the auction produced is in the proposal either way (R-335).
	detector.Screeners = detection.RegistryScreeners{Registry: registry}
	detector.ScreenPolicy = hostPolicy
	detector.Auditor = detectionAuditor{auditor}
	for _, a := range registry.AIAssignments() {
		logger.Info("AI function assigned", zap.String("function", string(a.Function)),
			zap.String("adapter", a.AdapterRef), zap.String("model", a.Model))
	}

	// Assertions are what an app can actually trust about a caller (R-051).
	// The signing key is generated per process and never leaves it; its public
	// half is published to the other replicas, and every replica's JWKS
	// carries every live replica's key, so an assertion verifies whichever
	// replica signed it (issue #72).
	minter, err := assertion.NewMinter(cfg.Server.Issuer, clock.System{})
	if err != nil {
		return err
	}
	replicas := state.NewReplicas(db)
	hostname, _ := os.Hostname()
	member := &cluster.Member{
		Store:        replicas,
		ID:           db.Replica(),
		Hostname:     hostname,
		AdvertiseURL: cfg.Server.Advertise(hostname),
		Version:      buildVersion,
		Minter:       minter,
		Logger:       logger,
	}
	minter.WithPeers(member.PeerKeys)
	if err := member.Join(ctx); err != nil {
		return err
	}
	defer member.Leave(context.WithoutCancel(ctx))
	logger = logger.With(zap.String("replica_id", member.ID))
	ctx = log.Into(ctx, logger)

	// Validated in config.Load, so this cannot fail here; the second read is
	// how the parsed value reaches the handlers.
	externalURL, err := cfg.Server.External()
	if err != nil {
		return err
	}
	identityService.ExternalURL = externalURL
	if externalURL == nil {
		logger.Info("no external URL configured; session cookies are marked Secure only when Pando itself serves TLS",
			zap.String("setting", "PANDO_SERVER_EXTERNAL_URL"))
	} else if externalURL.Scheme != "https" {
		logger.Warn("external URL is http, so session cookies are not marked Secure",
			zap.String("external_url", externalURL.String()))
	}

	authenticator := &httpapi.Authenticator{
		Sessions: sessions,
		Tokens:   tokens,
		Users:    users,
		Groups:   authzStore,
	}
	authorizer := authz.New(authzStore, hostPolicy, auditDenials{auditor})

	// Pando's own notifications go to the people they name, on the channels
	// that reach people, as each person's preferences allow (R-373). Event
	// subscriptions are managed by the service and sent by the dispatcher
	// started with the other loops below (issue #50).
	notifyRouter := subscription.Router{
		Registry:    registry,
		Preferences: state.NewNotificationPreferences(db),
		Users:       users,
		Logger:      logger,
	}
	subscriptions := &subscription.Service{
		Subscriptions: state.NewSubscriptions(db),
		Deliveries:    state.NewDeliveries(db),
		Events:        state.NewEvents(db),
		Keys:          state.NewSubscriptionSecrets(db, secretsAdapter, secretsRef),
		ExternalURL:   cfg.Server.ExternalURL,
		Prefs:         state.NewNotificationPreferences(db),
		Authz:         authorizer,
		Policy:        policyStore,
		Apps:          apps,
		Registry:      registry,
		Audit:         httpapi.AuditFunc(auditor),
		Clock:         clock.System{},
		Logger:        logger,
	}

	// Every deploy starts through here, and the ones that need somebody's
	// approval wait here for it (R-154 – R-159).
	approvals := &approval.Service{
		Deployments: deployments,
		Apps:        apps,
		Authz:       authorizer,
		Policy:      policyStore,
		Planner:     appPlanner,
		Capacity:    allocations,
		Deployer:    deployQueue,
		Audit:       httpapi.AuditFunc(auditor),
		Notifier:    notifyRouter,
		Approvers:   authzStore,
		Clock:       clock.System{},
		Logger:      logger,
	}

	// Auto-deploy is a separate job on its own clock (R-141). It never modifies
	// a running app — it creates a revision and deploys it through the same
	// service a person's deploy goes through. Off unless an app's pinned spec
	// asks for it, so its poll is usually a query returning nothing. A webhook
	// (R-142) asks the same job to check one app sooner.
	autoDeployJob, autoDeploy := wireAutoDeploy(autoDeployWiring{db: db, apps: apps, deployments: deployments, sources: sources,
		deployer: approvals, auditor: auditor, policy: policyStore, authz: authorizer, secrets: secretsAdapter,
		secretsRef: secretsRef, concurrency: cfg.Work.AutoDeploy, logger: logger})

	// One resolver, used by the proxy to route and by the router to tell an
	// app's hostname from Pando's own.
	appResolver := proxy.NewStateResolver(apps)

	// The single enforcement point for every request to every app (R-023).

	appProxy := &proxy.Proxy{
		Resolver:      appResolver,
		Authenticator: authenticator,
		Authz:         authorizer,
		Minter:        minter,
		Upstreams:     proxy.NewRuntimeUpstreams(registry),
		Auditor:       auditor,
		// Whether a visit by someone not signed in is recorded as app.use
		// (R-227); read when one would be, like any other policy.
		UsePolicy:   hostPolicy,
		ExternalURL: externalURL,
		Metrics:     proxy.NewCounters(),
		Logger:      logger,
		LoginPath:   httpapi.LoginPath,
		Mode:        cfg.Server.RoutingMode,
	}

	// A delete asks for its teardown now rather than at the next GC pass.
	teardownNow := make(chan struct{}, 1)

	// Built once and used twice: as the front door, and as what a port-mode
	// app's own listener falls back to for Pando's reserved path (R-172).
	// What routing adapters need running in front of Pando — a Traefik on
	// :80 and :443, a cloudflared — run through the runtime adapter (R-174).
	// Certificates the edge cannot issue itself — on Kubernetes, where it is
	// several replicas — are issued by the leader in its edge pass and kept
	// sealed (R-169, R-190). Any replica answers the HTTP-01 challenge.
	edgeCerts := state.NewEdgeCertificates(db, secretsAdapter, secretsRef)
	edges := &edge.Service{
		Registry:      registry,
		ProxyUpstream: proxyUpstream,
		Logger:        logger,
		Clock:         clock.System{},
	}
	if secretsAdapter != nil {
		// The CA is a setting (O-49): Let's Encrypt by default, or an
		// organization's own ACME server, trusted through its CA file.
		acmeClient, err := edgecert.ClientTrusting(cfg.ACME.CAFile)
		if err != nil {
			return err
		}
		edges.Certificates = &edgecert.Issuer{
			Store: edgeCerts, ACME: edgecert.Lego{HTTPClient: acmeClient}, Clock: clock.System{}, Logger: logger,
			Directory: cfg.ACME.DirectoryURL,
		}
	}

	// Whether a newer Pando is released (R-349). Started with the other loops
	// below; host policy can turn it off, and then it sends nothing.
	updates := &update.Checker{
		Source: update.NewGitHub(buildVersion),
		Settings: func(ctx context.Context) (update.Settings, error) {
			doc, err := policyStore.Load(ctx)
			return update.SettingsFrom(doc), err
		},
		Current: buildVersion,
		Install: update.DetectInstall(),
		Clock:   clock.System{},
		Logger:  logger,
	}

	// Upgrading in place (R-355 – R-362).
	upgrades := newUpgradeService(upgradeDeps{
		cfg: cfg, updates: updates, registry: registry,
		policy:  func(ctx context.Context) (corepolicy.Document, error) { return policyStore.Load(ctx) },
		backups: backups, backup: backupService, authzStore: authzStore, auditor: auditor,
		notify: notifyRouter, logger: logger,
	})
	upgrades.Replicas = func(ctx context.Context) (int, error) {
		live, err := replicas.Live(ctx)
		return len(live), err
	}

	apiHandler := (&httpapi.Server{
		Updates:  updates,
		Upgrades: upgrades,
		Version:  buildVersion,
		Edges:    edges,
		// Changing where a configured app is reached (R-162, R-163).
		Address: &address.Service{
			Registry:       registry,
			Ports:          state.NewPorts(db),
			Taken:          apps,
			PortRangeStart: cfg.Server.PortRangeStart,
			PortRangeEnd:   cfg.Server.PortRangeEnd,
			BaseDomain:     cfg.Server.BaseDomain,
		},
		TeardownNow: func() {
			select {
			case teardownNow <- struct{}{}:
			default:
			}
		},
		Logger:   logger,
		Security: securityService,
		Sources:  sources,
		Images:   images,

		SourceConnections: sourceConnections,

		ImageRegistries: buildRegistry,
		DB:              db,
		Identity:        identity,
		IDP:             identityService,
		Users:           users,
		Sessions:        sessions,
		Tokens:          tokens,
		Apps:            apps,
		Volumes:         volumes,
		Auditor:         auditor,
		Policy:          hostPolicy,

		Registry:    registry,
		Adapters:    adapters,
		AIFunctions: aiFunctions,
		// One answer per app per moment for the console's status and usage
		// polls, rather than one Docker call per open tab (issue #72).
		Observations: observe.New(),
		// One live log stream per app part for every viewer of it (O-51).
		LogStreams: logstream.New(logstream.WithLogger(logger)),
		Assist: &assist.Service{
			Registry: registry,
			Users:    users,
			Apps:     apps,
			Roles:    state.NewRoles(db),
			Groups:   state.NewGroups(db),
			Verbs:    authzStore,
			Policy:   policyStore,
			Overlay:  policyOverlay,
			Audit:    audit.NewReader(db.Pool),

			// Built once: the reference is the binary's, and does not change
			// while it runs.
			Reference: sync.OnceValue(func() string { return reference.Markdown(httpapi.Reference()) }),
		},
		AdapterKinds: adapterKinds(),
		StartedAt:    startedAt,
		Restart: func() {
			// Every replica, not only the one the load balancer chose: a
			// restart is how saved adapters start running (R-253), and
			// replicas left on the old configuration would drift apart.
			// The others notice at their next heartbeat (issue #72).
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := replicas.RequestRestart(ctx); err != nil {
				logger.Warn("could not ask the other replicas to restart", zap.Error(err))
			}
			select {
			case restartCh <- struct{}{}:
			default: // one is already on its way
			}
		},

		AdapterCredentials: adapterCredentials,

		Allocations: allocations,
		// GET /capacity's runtime readings, taken in the background while
		// somebody is looking rather than on every view (issue #72).
		Capacity:    capacityReadings,
		Planner:     appPlanner,
		Deployments: deployments,
		Reconciles:  reconciles,
		Deployer:    deployer,
		Approvals:   approvals,
		AutoDeploy:  autoDeploy,

		Subscriptions: subscriptions,
		Inbox:         &subscription.Inbox{Store: notifications},
		Notifier:      notifyRouter,
		Logs:          logStore,
		LogOwner: cluster.LogOwner{
			Self: member.ID, Deployments: deployments, Replicas: replicas,
		}.Where,
		PasscodeFailures: state.NewPasscodeFailures(db),
		Secrets:          secrets,
		Detections:       detections,
		Detector:         detector,
		DetectionQueue:   detectionQueue,
		Console:          consoleHandler(logger),

		// Policy is evaluated before grants, so it is wired into the
		// authorizer rather than checked alongside it (R-272).
		Authz:    authorizer,
		Authent:  authenticator,
		Minter:   minter,
		AppProxy: appProxy,

		// Decides whether the session cookie is marked Secure (O-19).
		ExternalURL: externalURL,

		// So the console does not answer on an app's own hostname. Without
		// this the console's "/" route shadows every subdomain app's root.
		AppHosts:       appResolver,
		ACMEChallenges: edgecert.Challenges{Store: edgeCerts},
		Grants:         grants,
		HostPolicy:     hostPolicy,
		Verbs:          authzStore,
		Defaults:       installDefaults,

		// The policy *document* and the policy *evaluator* are different
		// things and both are wired: one endpoint edits the document, every
		// authorization check consults the evaluator, and the evaluator
		// reads the document per evaluation rather than caching it (R-274).
		PolicyStore:   policyStore,
		PolicyOverlay: policyOverlay,
		Startup:       cfg,
		AuditLog:      audit.NewReader(db.Pool),
		AuditArchives: &audit.Archives{Pool: db.Pool, Stores: auditStores},

		Groups:       state.NewGroups(db),
		Roles:        state.NewRoles(db),
		Backups:      backups,
		Backup:       backupService,
		BundleSource: bundleSource,

		// Retried deploys replay rather than repeat (R-262). An agent
		// retries on a timeout, and a deploy that clones regularly outlasts
		// a client's patience.
		Idempotency: state.NewIdempotency(db),
	}).Routes()

	srv := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: apiHandler,

		// A client that opens a connection and dribbles header bytes holds a
		// goroutine open indefinitely without this. Matches what the proxy's
		// per-app listeners already do (internal/proxy/ports.go).
		//
		// ReadHeaderTimeout and IdleTimeout only. A ReadTimeout or WriteTimeout
		// would cut the responses this API exists to stream — the deploy log's
		// SSE feed, `pando logs --follow`, and the exec websocket — at a fixed
		// wall-clock deadline, which is the wrong tool for a slow client.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// The reconciler. Apps that kept running while Pando was away are converged
	// to, not restarted for tidiness — an app that was running and is still
	// running needs nothing done to it (design 05 §2.1.1).
	backoffSchedule, err := cfg.Reconciler.BackoffSchedule()
	if err != nil {
		return err
	}
	warnIfRetriesAreFast(logger, backoffSchedule)

	loop := &reconciler.Reconciler{
		Apps:          apps,
		Reconciles:    reconciles,
		Secrets:       secrets,
		Volumes:       volumes,
		Services:      deployer,
		Environments:  deployer,
		Registry:      registryAdapters{registry},
		Auditor:       reconcilerAuditor{auditor},
		Logger:        logger,
		Clock:         clock.System{},
		ProxyUpstream: proxyUpstream,

		// Replicas share the apps rather than each visiting every one.
		MinRevisit: reconciler.DefaultMinRevisit,

		// Every runtime's events are followed, on every replica, so an app
		// is visited when something happens to it and a settled one only on
		// the slow sweep (O-52). A runtime without events keeps the fast
		// cadence.
		Runtimes: func() []string { return registry.ByCategory(adapterapi.CategoryRuntime) },

		// The owner hears that their app failed (design 05 §4). Unset until
		// issue #50, which is to say nobody heard.
		Notifier: notifyRouter,

		// A workload restored from a build in the install registry pulls with
		// its credential (issue #72, PR 5).
		BuiltImageAuth: builtImageAuth(buildRegistry),

		// Unset in production: the zero values mean R-149 and R-150's defaults.
		Backoff:          backoffSchedule,
		FailureThreshold: cfg.Reconciler.FailureThreshold,
		FailureWindow:    cfg.Reconciler.FailureWindow,
	}
	// Work a stopped process had claimed will never finish there, so it goes
	// back in the queue for a replica that is running — or, started too many
	// times, is recorded as interrupted (O-32). Only a stopped replica's:
	// another replica's live work is left alone, and queued work nobody has
	// claimed is waiting, not lost (issue #72). Here at startup, and then by
	// the leader's sweeper, since a lost pod is not followed by a restart of
	// itself.
	if n, err := detections.RecoverRunning(ctx); err != nil {
		logger.Warn("could not recover interrupted detections", zap.Error(err))
	} else if n > 0 {
		logger.Info("recovered detections interrupted by a stopped replica", zap.Int64("count", n))
	}
	if n, err := deployments.RecoverInFlight(ctx); err != nil {
		logger.Warn("could not recover interrupted deploys", zap.Error(err))
	} else if n > 0 {
		logger.Info("recovered deploys interrupted by a stopped replica", zap.Int64("count", n))
	}

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()

	// The reconciler runs on every replica: it claims apps under a lease
	// with SKIP LOCKED (state.Lease), so replicas share the apps between them
	// rather than fighting over them.
	go loop.Run(loopCtx)

	// The deploy and detection queues run on every replica: each claims what
	// it has room for (issue #72, O-32).
	go deployQueue.Serve(loopCtx)
	go detectionQueue.Serve(loopCtx)

	// What must happen once per install rather than once per process runs on
	// the leader alone (issue #72): two replicas each running the GC tore
	// down, backed up and pruned twice, and two running auto-deploy deployed
	// one commit twice. jobs collects them; the leader starts them below.
	var jobs []cluster.Job
	job := func(name string, run func(context.Context)) {
		jobs = append(jobs, cluster.Job{Name: name, Run: run})
	}
	job("sweep", (&cluster.Sweeper{
		// Silent replicas are recorded as stopped first, so one that was
		// only paused finds out at its next heartbeat and restarts rather
		// than carrying on with work another replica has claimed again.
		Abandon: []func(context.Context) (int64, error){
			replicas.StopSilent, deployments.RecoverInFlight, detections.RecoverRunning,
		},
		Prune: func(ctx context.Context) error {
			_, err := replicas.Prune(ctx, 24*time.Hour)
			if err == nil {
				err = state.NewPasscodeFailures(db).Prune(ctx, time.Hour)
			}
			return err
		},
		Logger: logger,
	}).Run)

	// Once a minute: an edge somebody removed by hand comes back, and one no
	// adapter asks for any more goes. Docker restarts one that crashed.
	job("edges", func(ctx context.Context) { edges.Run(ctx, time.Minute) })

	// Auto-deploy's polls (R-141, R-142).
	job("auto-deploy", autoDeployJob.Run)

	// Audit retention, daily (R-347). As the archiver role, which is the only
	// one that can remove a month, and only one archived and old enough.
	job("audit-retention", (&audit.Archiver{
		Pool:   db.Archiver(),
		Stores: auditStores,
		Retention: func(ctx context.Context) (audit.Retention, error) {
			doc, err := policyStore.Load(ctx)
			return doc.AuditRetention(), err
		},
		Clock:  clock.System{},
		Logger: logger,
	}).Run)

	// Deploy requests nobody answered in time expire (R-156), once a minute.
	// Approving or rejecting one also expires it on the spot, so the minute
	// is how long the list can show one that has already run out, not how
	// long one can be approved late.
	job("approval-expiry", func(ctx context.Context) { approvals.RunExpiry(ctx, time.Minute) })

	// The update check on every replica: what it learns is kept in memory,
	// and each replica answers the Updates screen from its own. The upgrade
	// loop records outcomes and notifies once, so it leads.
	go updates.Run(loopCtx)
	job("upgrades", upgrades.Run)

	// Event subscriptions (issue #50): route the outbox and send what is due,
	// every two seconds while it is quiet and continuously while it is not;
	// and check every adapter's health every five minutes, recording a change
	// as an event.
	dispatcher := &subscription.Dispatcher{
		Events:        subscriptions.Events,
		Subscriptions: subscriptions.Subscriptions,
		Deliveries:    subscriptions.Deliveries,
		Keys:          subscriptions.Keys,
		Tokens:        tokens,
		ExternalURL:   cfg.Server.ExternalURL,
		Deployments:   deployments,
		Holders:       authzStore,
		TokenOwners:   tokens,
		Authz:         authorizer,
		Policy:        policyStore,
		Apps:          apps,
		Users:         users,
		Groups:        authzStore,
		Registry:      registry,
		Notifier:      notifyRouter,
		Audit:         httpapi.AuditFunc(auditor),
		Clock:         clock.System{},
		Logger:        logger,
	}
	go dispatcher.Run(loopCtx)
	go capacityReadings.Run(loopCtx)

	// Retention, hourly, for the tables that otherwise only grow — the event
	// outbox among them, whose pruning used to run on every replica (issue
	// #72, R-224).
	job("retention", (&retention.Job{
		Store:  state.NewRetention(db),
		Outbox: subscriptions.Events,
		Settings: retention.Settings{
			DeploymentsPerApp: cfg.Retention.DeploymentsPerApp,
			Scans:             cfg.Retention.Scans,
			Sessions:          cfg.Retention.Sessions,
			IdempotencyKeys:   cfg.Retention.IdempotencyKeys,
			SSOFlows:          cfg.Retention.SSOFlows,
			Events:            cfg.Retention.Events,
			DeletedApps:       cfg.Retention.DeletedApps,
		},
		Clock:  clock.System{},
		Logger: logger,
	}).Run)
	// Delivery claims with SKIP LOCKED, so it runs on every replica; a health
	// change is one event per install, so the watch leads.
	job("adapter-health", func(ctx context.Context) { dispatcher.WatchAdapters(ctx, 5*time.Minute) })

	// Port-mode apps answer at the root of their own port (design 03 §4.2).
	//
	// The allocation was already being made and shown to people; nothing
	// listened on it, so every laptop install advertised an address that
	// refused the connection and the apps were reachable only under the path
	// prefix — where an app that writes "/assets/app.js" into its own HTML
	// comes up blank, and Pando will not rewrite the page to hide that
	// (R-167, R-028). Same proxy, same enforcement, one extra way in (R-023).
	go (&proxy.PortListeners{
		Ports: state.NewPorts(db),
		// Not the bare proxy: a port listener is a front door of its own, and
		// somebody arriving at it unauthenticated has to have somewhere to
		// sign in (R-172). Everything else on that socket is the app's.
		Handler: httpapi.ReservedOrApp(apiHandler, appProxy),
		Logger:  logger,
	}).Run(loopCtx)

	// Hourly garbage collection, on a much slower clock because nothing here is
	// urgent and all of it is destructive.
	// Reclaim the networks of apps deleted since the last run.
	//
	// At startup and only at startup. A bundle network still held by the
	// *previous* Pando container has a dead endpoint on it, so Docker removes
	// it with nothing to disconnect — whereas doing this while serving would
	// mean detaching the running container, and on Docker Desktop that drops
	// its published ports. Measured, not assumed.
	//
	// Only this install's networks. Two installs on one Docker host label their
	// networks alike, and each used to rejoin the other's apps and reclaim the
	// other's empty networks (issue #55). An app this install ever created,
	// deleted ones included, is in its own database.
	owns := func(bundleID string) bool {
		known, err := apps.Known(ctx, bundleID)
		return err == nil && known
	}
	if ref, ok := registry.Default(adapterapi.CategoryRuntime); ok {
		rt, _ := registry.Runtime(ref)
		if reclaimer, ok := rt.(interface {
			ReclaimNetworks(context.Context, func(string) bool) (int, error)
		}); ok {
			// Not an app with a deploy under way. Another replica's deploy
			// makes the app's network before its containers, so for a moment
			// it is empty and container-less and looks like a dead app's
			// (issue #72). A deleted app has nothing in flight.
			reclaimable := func(bundleID string) bool {
				if !owns(bundleID) {
					return false
				}
				busy, err := deployments.InFlight(ctx, bundleID)
				return err == nil && !busy
			}
			if n, err := reclaimer.ReclaimNetworks(ctx, reclaimable); err != nil {
				logger.Warn("could not reclaim app networks", zap.Error(err))
			} else if n > 0 {
				logger.Info("reclaimed app networks left by deleted apps", zap.Int("count", n))
			}
		}

		// Then rejoin what is left, which is the other half of the same
		// problem: this process is running in a *new* container, and the
		// networks of every already-running app were joined by the old one.
		// Nothing redeploys those apps, so without this they stay up and
		// unreachable — a Pando upgrade would 502 every app on the install
		// until each was deployed again by hand. After reclaim, never before:
		// reclaim recognizes a dead app's network by its being empty.
		if rejoiner, ok := rt.(interface {
			RejoinNetworks(context.Context, func(string) bool) (int, error)
		}); ok {
			if n, err := rejoiner.RejoinNetworks(ctx, owns); err != nil {
				logger.Warn("could not rejoin app networks", zap.Error(err))
			} else if n > 0 {
				logger.Info("rejoined the networks of running apps", zap.Int("count", n))
			}

			// And keep rejoining, on every replica. An app deployed by
			// another replica gets a network only that replica joined, so
			// this one's proxy could not reach it until its next restart
			// (issue #72). Fifteen seconds is how long a new app may answer
			// 502 here; joining is idempotent, so a pass with nothing new
			// changes nothing.
			go func() {
				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-loopCtx.Done():
						return
					case <-ticker.C:
					}
					if n, err := rejoiner.RejoinNetworks(loopCtx, owns); err != nil {
						logger.Warn("could not rejoin app networks", zap.Error(err))
					} else if n > 0 {
						logger.Debug("joined app networks", zap.Int("count", n))
					}
				}
			}()
		}
	}

	// The GC also tears down the bundles of deleted apps, which nothing used to
	// do: Destroy was never called, so every deleted app left containers and a
	// private network behind. Registry and Auditor are what make that possible
	// — and the teardown is audited, because destruction is destruction whoever
	// does it.
	gc := &reconciler.GC{
		Apps:     apps,
		Logger:   logger,
		Registry: registryAdapters{registry},
		Auditor:  reconcilerAuditor{auditor},
		Interval: cfg.Reconciler.GCInterval,

		Clock: clock.System{},

		// A deleted app's build cache and uploaded source (R-224, issue #55).
		BuildCaches:   buildCaches{registry},
		DiscardUpload: sources.DiscardUpload,

		// And its registry credential (issue #41).
		DiscardCredential: images.RemoveCredential,

		// And its builds in the install registry (issue #72, R-224).
		RegistryImages: registryImages(buildRegistry, apps),

		// Storage of a deleted app is reclaimed only once a backup holds it.
		Backups: backups,

		// The security pass (R-315, R-316): mark, warn, and stop when the
		// grace has run out. Inert until an administrator sets a threshold.
		Security:      securityService,
		SecurityState: scans,
		PolicyStore:   hostPolicy,
		Desired:       apps,
		Notifier:      securityNotifier{notifyRouter},
	}
	job("gc", gc.Run)

	// A delete tears its app down at once, on the replica that took the
	// delete, whichever replica leads (issue #72). Teardown is idempotent, so
	// this and the leader's own pass meeting on one app is harmless; and the
	// leader's pass is what catches a delete whose replica stopped first.
	go func() {
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-teardownNow:
				gc.TearDownDeleted(loopCtx)
			}
		}
	}()

	// R-211's rolling backups, which had a column, a default and an expiry
	// query and nothing that ever took one — and then took them in series
	// inside the hourly GC. A job of its own now, asking for what is due and
	// taking work.backups at a time (issue #72).
	job("backups", (&reconciler.RollingBackups{
		Apps:         apps,
		Backups:      backups,
		Backup:       backupService,
		BundleSource: bundleSource,
		Auditor:      reconcilerAuditor{auditor},
		Logger:       logger,
		Clock:        clock.System{},
		Concurrency:  cfg.Work.Backups,
	}).Run)

	// Whichever replica holds the leader lock runs the jobs above; with one
	// replica, that is this one, a moment after it starts.
	go (&cluster.Leader{Store: replicas, Jobs: jobs, Logger: logger}).Run(loopCtx)

	// Heartbeat. It ends early when this replica should restart: a restart
	// was asked for on any replica, or the install took this one for dead.
	memberCh := make(chan string, 1)
	go func() {
		if reason := member.Run(loopCtx); reason != "" {
			memberCh <- reason
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", zap.String("addr", cfg.Server.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	case <-restartCh:
		logger.Info("restarting")
		restartRequested.Store(true)
	case reason := <-memberCh:
		logger.Info("restarting", zap.String("reason", reason))
		restartRequested.Store(true)
	}
	// Leaders resign and jobs stop before the server does, so the next
	// leader can start while this one drains its requests.
	stopLoop()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if err != nil && restartRequested.Load() {
		// A request still open at the deadline, such as a streamed log, is
		// cut off either way; it is no reason not to come back.
		logger.Warn("requests were still open at restart", zap.Error(err))
		return nil
	}
	return err
}

// detectionAuditor adapts the audit writer to what detection needs.
//
// A screening is an action by Pando, not by the person who created the app:
// detection runs in the background after app creation, and the principal that
// read the repository is the install. KindSystem is what that is (R-337).
type detectionAuditor struct{ w *audit.Writer }

func (a detectionAuditor) Write(ctx context.Context, e detection.AuditEvent) error {
	return a.w.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        e.Action,
		AppID:         e.AppID,
		TargetKind:    "app",
		TargetID:      e.AppID,
		Detail:        e.Detail,
	})
}

// registerAdapters configures the compiled-in adapters from adapter_configs,
// seeding the defaults on a fresh install.
//
// An adapter that fails to configure is logged and skipped rather than
// preventing startup: one broken adapter should not take the whole install
// offline, and the planner already refuses to plan against an adapter it cannot
// reach (R-254).
func registerAdapters(ctx context.Context, db *state.DB, store *state.Adapters, notifications *state.Notifications, declared []config.AdapterDecl, logger *zap.Logger) (*adapterapi.Registry, *state.AdapterCredentials, error) {
	if err := seedDefaultAdapters(ctx, store); err != nil {
		return nil, nil, err
	}

	stored, err := store.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	// One list, stored and declared together. A declaration overrides a
	// stored adapter with its ID, a stored AI adapter of its provider (one
	// per provider, R-259), and a stored default in its category (R-271):
	// the file is what the operator wrote most recently and most
	// deliberately, and the stored row applies again once the declaration is
	// removed.
	type entry struct {
		c    state.AdapterConfig
		decl *config.AdapterDecl
	}
	declaredIDs := map[string]bool{}
	declaredAIKinds := map[string]bool{}
	declaredDefaults := map[string]bool{}
	var entries []entry
	for i := range declared {
		d := &declared[i]
		declaredIDs[d.ID] = true
		if d.Category == string(adapterapi.CategoryAI) && d.Enabled {
			declaredAIKinds[d.Kind] = true
		}
		if d.Default && d.Enabled {
			declaredDefaults[d.Category] = true
		}
		entries = append(entries, entry{
			c: state.AdapterConfig{ID: d.ID, Category: d.Category, Kind: d.Kind, Name: d.Name,
				IsDefault: d.Default, Enabled: d.Enabled},
			decl: d,
		})
	}
	for _, c := range stored {
		switch {
		case declaredIDs[c.ID]:
			logger.Info("stored adapter is overridden by the config file", zap.String("id", c.ID))
			continue
		case c.Category == string(adapterapi.CategoryAI) && declaredAIKinds[c.Kind]:
			logger.Info("stored AI adapter is overridden by one of its provider in the config file",
				zap.String("id", c.ID), zap.String("kind", c.Kind))
			continue
		}
		if declaredDefaults[c.Category] {
			c.IsDefault = false
		}
		entries = append(entries, entry{c: c})
	}

	// Secrets adapters first. Every other adapter's credentials are sealed by
	// one (O-20), so it has to be configured before they can be opened. A
	// secrets adapter's own configuration never carries credentials — there is
	// nothing to open them with — and the create handler refuses them.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].c.Category == string(adapterapi.CategorySecrets) &&
			entries[j].c.Category != string(adapterapi.CategorySecrets)
	})

	registry := adapterapi.NewRegistry()
	var credentials *state.AdapterCredentials
	for _, e := range entries {
		c := e.c
		if !c.Enabled {
			continue
		}
		// A source connection in the table is built from its row each time
		// it is used (core/sourceconn), so connecting one needs no restart
		// and a token it refreshes is read back. Only one the configuration
		// file declares is built here, from where the file says.
		if c.Category == string(adapterapi.CategorySource) && e.decl == nil {
			continue
		}
		// The same for an image registry (issue #153): a stored one is built
		// from its row on every push, pull and plan (core/imageregistry), so
		// a password rotated on one replica reaches all of them.
		if c.Category == string(adapterapi.CategoryImageRegistry) && e.decl == nil {
			continue
		}

		if c.Category != string(adapterapi.CategorySecrets) && credentials == nil {
			credentials = adapterCredentialsFor(db, registry)
		}

		adapter := newAdapter(c.Category, c.Kind, notifications)
		if adapter == nil {
			logger.Warn("skipping adapter of unknown kind",
				zap.String("id", c.ID), zap.String("category", c.Category), zap.String("kind", c.Kind))
			continue
		}

		var raw json.RawMessage
		var creds map[string]secret.Value
		if e.decl != nil {
			// Read from where the file says, at startup, and held in memory
			// only (R-190). A variable that is unset or a file that is missing
			// is a failure to configure, like a bad key: logged and skipped.
			if raw, err = json.Marshal(e.decl.Config); err != nil || e.decl.Config == nil {
				raw = json.RawMessage(`{}`)
			}
			if creds, err = declaredCredentials(e.decl.Credentials); err != nil {
				logger.Error("declared adapter's credentials could not be read, so the adapter was skipped",
					zap.String("id", c.ID), zap.String("reason", err.Error()))
				continue
			}
		} else {
			raw = c.Config
			if c.Category != string(adapterapi.CategorySecrets) {
				// Decrypted here and handed over in memory only (O-20). Nothing
				// on this path is logged: a failure is reported by adapter ID
				// alone.
				if creds, err = credentials.Resolve(ctx, c.ID); err != nil {
					logger.Error("adapter credentials could not be opened, so the adapter was skipped",
						zap.String("id", c.ID))
					continue
				}
			}
		}
		if raw, err = withCredentials(raw, creds); err != nil {
			logger.Error("adapter could not be configured and was skipped", zap.String("id", c.ID))
			continue
		}

		if err := adapter.Configure(ctx, raw); err != nil {
			logger.Error("adapter could not be configured and was skipped",
				zap.String("id", c.ID), zap.Error(err))
			continue
		}
		if err := registry.Register(c.ID, adapter); err != nil {
			return nil, nil, err
		}
		if c.IsDefault {
			if err := registry.SetDefault(adapterapi.Category(c.Category), c.ID); err != nil {
				return nil, nil, err
			}
		}
		logger.Info("adapter registered", zap.String("id", c.ID), zap.String("kind", c.Kind),
			zap.Bool("declared", e.decl != nil))
	}
	if credentials == nil {
		credentials = adapterCredentialsFor(db, registry)
	}

	if err := declaredServicesOverlap(registry, declared); err != nil {
		return nil, nil, err
	}
	return registry, credentials, nil
}

// declaredAssignments are the AI functions the config file assigns.
func declaredAssignments(decls []config.AdapterDecl) []assist.Declared {
	var out []assist.Declared
	for _, d := range decls {
		if !d.Enabled {
			continue
		}
		for _, f := range d.Functions {
			out = append(out, assist.Declared{
				Function: adapterapi.AIFunction(f.Function), Adapter: d.ID, Model: f.Model,
				Source: assist.Source{Kind: f.Source.Kind, Name: f.Source.Name, Key: f.Source.Key},
			})
		}
	}
	return out
}

// adapterNames names adapters for refusals, from the file and the database.
// Read once: a name is for a sentence, and a stale one costs nothing.
func adapterNames(ctx context.Context, store *state.Adapters, decls []config.AdapterDecl) func(string) string {
	names := map[string]string{}
	if stored, err := store.List(ctx); err == nil {
		for _, c := range stored {
			names[c.ID] = c.Name
		}
	}
	for _, d := range decls {
		names[d.ID] = d.Name
	}
	return func(ref string) string { return names[ref] }
}

// newAdapter is an unconfigured adapter of a category and kind this build can
// run, or nil.
func newAdapter(category, kind string, notifications *state.Notifications) adapterapi.Adapter {
	if category == string(adapterapi.CategorySource) {
		// Only a source connection the configuration file declares is built
		// at startup (registerAdapters); a nil interface must stay nil here.
		if a := newSourceAdapter(kind); a != nil {
			return a
		}
		return nil
	}
	if category == string(adapterapi.CategoryImageRegistry) {
		// Likewise only a declared image registry; a stored one is built
		// from its row each time it is used (core/imageregistry).
		if a := newImageRegistryAdapter(kind); a != nil {
			return a
		}
		return nil
	}
	switch {
	case category == string(adapterapi.CategoryRuntime) && kind == dockerruntime.Kind:
		return dockerruntime.New()
	case category == string(adapterapi.CategoryRuntime) && kind == kubernetesruntime.Kind:
		// Not seeded: it needs the cluster's address ranges, and Pando running
		// inside the cluster (deploy/kubernetes).
		return kubernetesruntime.New()
	case category == string(adapterapi.CategoryRuntime) && kind == multidocker.Kind:
		return multidocker.New()
	case category == string(adapterapi.CategoryRouting) && kind == loopback.Kind:
		return loopback.New()
	case category == string(adapterapi.CategorySecrets) && kind == secretslocal.Kind:
		return secretslocal.New()
	case category == string(adapterapi.CategoryBuilder) && kind == buildkitadapter.Kind:
		return buildkitadapter.New()
	case category == string(adapterapi.CategoryBackup) && kind == backuplocal.Kind:
		return backuplocal.New()
	case category == string(adapterapi.CategoryServices) && kind == servicesdocker.Kind:
		return servicesdocker.New()
	case category == string(adapterapi.CategoryRouting) && kind == traefik.Kind:
		return traefik.New()
	case category == string(adapterapi.CategoryRouting) && kind == cloudflare.Kind:
		return cloudflare.New()
	case category == string(adapterapi.CategoryScanner) && kind == trivyscanner.Kind:
		return trivyscanner.New()
	case category == string(adapterapi.CategoryAI) && kind == aianthropic.Kind:
		// Not seeded (design 10 §7): there is no AI adapter that works
		// without a credential, and seeding one would put a permanently
		// unhealthy adapter in every install's console. An install that
		// wants AI configures one itself.
		return aianthropic.New()
	case category == string(adapterapi.CategoryAI) && kind == aiopenai.Kind:
		return aiopenai.New()
	case category == string(adapterapi.CategoryAI) && kind == ailocal.Kind:
		// A model on the install's own hardware: nothing is sent to a
		// provider. Not seeded either — it needs a server and a model named.
		return ailocal.New()
	case category == string(adapterapi.CategoryNotify) && kind == notifyconsole.Kind:
		// The sink is supplied by core. The adapter stores nothing itself,
		// which is R-027 — an adapter never touches state.
		return notifyconsole.New(notifications)
	case category == string(adapterapi.CategoryNotify) && kind == notifysmtp.Kind:
		return notifysmtp.New()
	case category == string(adapterapi.CategoryNotify) && kind == notifyntfy.Kind:
		return notifyntfy.New()
	case category == string(adapterapi.CategoryNotify) && kind == chat.Slack.Kind:
		return chat.New(chat.Slack)
	case category == string(adapterapi.CategoryNotify) && kind == chat.Teams.Kind:
		return chat.New(chat.Teams)
	case category == string(adapterapi.CategoryNotify) && kind == chat.Discord.Kind:
		return chat.New(chat.Discord)
	}
	return nil
}

// declaredCredentials reads a declared adapter's credentials from the
// environment variables and files the config file names (R-190).
func declaredCredentials(refs map[string]config.CredentialRef) (map[string]secret.Value, error) {
	out := make(map[string]secret.Value, len(refs))
	for field, ref := range refs {
		switch {
		case ref.Env != "":
			v := os.Getenv(ref.Env)
			if v == "" {
				return nil, fmt.Errorf("credential %s names the environment variable %s, which is not set", field, ref.Env)
			}
			out[field] = secret.New(v)
		case ref.File != "":
			body, err := os.ReadFile(ref.File)
			if err != nil {
				return nil, fmt.Errorf("credential %s names the file %s, which could not be read", field, ref.File)
			}
			out[field] = secret.New(strings.TrimSpace(string(body)))
		}
	}
	return out, nil
}

// declaredServicesOverlap refuses two declared services adapters that fill the
// same kind of slot (R-271). ServicesFor would otherwise pick one by default
// or by name, and the file would not say which.
func declaredServicesOverlap(registry *adapterapi.Registry, declared []config.AdapterDecl) error {
	claimed := map[spec.SlotType]config.AdapterDecl{}
	for _, d := range declared {
		if d.Category != string(adapterapi.CategoryServices) || !d.Enabled {
			continue
		}
		sa, ok := registry.Services(d.ID)
		if !ok {
			continue
		}
		for _, t := range sa.Supports() {
			if other, dup := claimed[t]; dup {
				return fmt.Errorf("the config file %s declares two services adapters that both provide %s, at %s and %s. "+
					"Each kind of service has one adapter: remove one of them", d.Source.Name, t, other.Source.Key, d.Source.Key)
			}
			claimed[t] = d
		}
	}
	return nil
}

// adapterCredentialsFor is the credential store, sealed by the install's
// secrets adapter: the default one, or the only one.
func adapterCredentialsFor(db *state.DB, registry *adapterapi.Registry) *state.AdapterCredentials {
	ref, ok := registry.Default(adapterapi.CategorySecrets)
	if !ok {
		if refs := registry.ByCategory(adapterapi.CategorySecrets); len(refs) > 0 {
			ref = refs[0]
		}
	}
	sa, _ := registry.Secrets(ref)
	return state.NewAdapterCredentials(db, sa, ref)
}

// withCredentials adds decrypted credentials to an adapter's configuration as
// a `credentials` object — the one key the database refuses in the stored
// config, so an adapter reading it knows it came from encrypted storage.
func withCredentials(raw json.RawMessage, creds map[string]secret.Value) (json.RawMessage, error) {
	if len(creds) == 0 {
		return raw, nil
	}
	cfg := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	plain := make(map[string]string, len(creds))
	for field, v := range creds {
		plain[field] = v.Reveal()
	}
	body, err := json.Marshal(plain)
	if err != nil {
		return nil, err
	}
	cfg["credentials"] = body
	return json.Marshal(cfg)
}

// seedDefaultAdapters gives a fresh install a working set: Docker to run
// things, loopback to reach them, local storage for secrets. R-002 — setup cost
// is paid once, and this is part of not charging it twice.
func seedDefaultAdapters(ctx context.Context, store *state.Adapters) error {
	existing, err := store.List(ctx)
	if err != nil {
		return err
	}

	// Seeded per category, not once per install.
	//
	// The original rule was "if any adapter exists, do nothing", which is right
	// on the first run and wrong on every upgrade: an adapter category added in
	// a later version would never be seeded on an install that already had
	// others, so the feature would ship and silently not exist. That is exactly
	// what happened to notifications.
	//
	// The tradeoff is that deleting the *last* adapter in a category brings the
	// built-in default back on the next start. That is the better failure: a
	// category with nothing in it does nothing, and an install with no
	// notification adapter is not a considered posture — it is a gap.
	filled := map[string]bool{}
	for _, c := range existing {
		filled[c.Category] = true
	}

	for _, c := range []state.AdapterConfig{
		{ID: "rt_docker", Category: string(adapterapi.CategoryRuntime), Kind: dockerruntime.Kind,
			Name: "Docker", IsDefault: true, Enabled: true},
		{ID: "rte_loopback", Category: string(adapterapi.CategoryRouting), Kind: loopback.Kind,
			Name: "Localhost", IsDefault: true, Enabled: true},
		{ID: "sek_local", Category: string(adapterapi.CategorySecrets), Kind: secretslocal.Kind,
			Name: "Local storage", IsDefault: true, Enabled: true},
		{ID: "bld_buildkit", Category: string(adapterapi.CategoryBuilder), Kind: buildkitadapter.Kind,
			Name: "BuildKit", IsDefault: true, Enabled: true},

		// A local destination on a fresh install, so the DR path works out of
		// the box. R-217 is explicit that a bundle beside the install it backs
		// up does not survive the disk failing — this is the default that makes
		// backups testable, not the one an operator should keep.
		{ID: "bkp_local", Category: string(adapterapi.CategoryBackup), Kind: backuplocal.Kind,
			Name: "Local disk", IsDefault: true, Enabled: true},

		// Provisioned slots, the hobbyist default in R-131. A Postgres, MySQL
		// or Redis stood up inside the app's own bundle, reachable from
		// nowhere else (R-134).
		{ID: "svcs_docker", Category: string(adapterapi.CategoryServices), Kind: servicesdocker.Kind,
			Name: "Inside the app", IsDefault: true, Enabled: true},

		// Console-only notifications (R-231). Nothing is sent anywhere; a
		// message waits in Pando for the next time the recipient looks.
		{ID: "ntf_console", Category: string(adapterapi.CategoryNotify), Kind: notifyconsole.Kind,
			Name: "In the console", IsDefault: true, Enabled: true},

		// The security score (R-310). Seeded on, because a score nobody has is
		// a score nobody acts on — and seeded *permissive*: scanning happens,
		// the number is shown, and nothing is enforced until an administrator
		// sets a threshold (R-270, R-314).
		{ID: "scn_trivy", Category: string(adapterapi.CategoryScanner), Kind: trivyscanner.Kind,
			Name: "Trivy", IsDefault: true, Enabled: true},
	} {
		if filled[c.Category] {
			continue
		}
		if err := store.Upsert(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// secretsKeyPath reads where the local secrets adapter keeps its key.
//
// From adapter_configs rather than server config: the adapter owns that choice,
// and a second copy in another file is a second copy to get wrong. An install
// using an external secrets adapter has no local key, and the empty string is
// the correct answer there — the bundle then carries no key because there is
// none to carry.
func secretsKeyPath(ctx context.Context, store *state.Adapters, logger *zap.Logger) string {
	configured, err := store.List(ctx)
	if err != nil {
		logger.Warn("could not read adapter configuration for backups", zap.Error(err))
		return ""
	}
	for _, c := range configured {
		if c.Category != string(adapterapi.CategorySecrets) || c.Kind != secretslocal.Kind {
			continue
		}
		var cfg struct {
			KeyPath string `json:"key_path"`
		}
		if len(c.Config) > 0 {
			_ = json.Unmarshal(c.Config, &cfg)
		}
		if cfg.KeyPath != "" {
			return cfg.KeyPath
		}
		return secretslocal.DefaultKeyPath
	}
	return ""
}

// buildVersion is what the manifest records. Stamped by the release build
// (.goreleaser.yaml); "dev" in every other build, which is honest rather than a
// version number nobody set.
//
// Variables rather than constants because -ldflags -X cannot write to a const.
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildDate    = ""
)

// warnIfRetriesAreFast says so when the retry schedule is configured faster than
// R-149's default.
//
// Not refused, because a floor would make the schedule untestable end to end
// and that is the whole reason it is configurable. But an install retrying a
// broken app every couple of seconds forever is a real way to melt a host, and
// nobody should be able to do that without being told — especially since the
// setting exists for tests and is exactly the kind of thing that gets copied
// out of a test compose file into a real one.
func warnIfRetriesAreFast(logger *zap.Logger, schedule []time.Duration) {
	if len(schedule) == 0 {
		return
	}
	cap := schedule[len(schedule)-1]
	if cap >= reconciler.MinProductionCap {
		return
	}
	logger.Warn("retry backoff is configured faster than the shipped default",
		zap.Duration("cap", cap),
		zap.Duration("default_cap", reconciler.DefaultBackoff[len(reconciler.DefaultBackoff)-1]),
		zap.String("note", "this is a testing setting (R-149). An app that cannot start will be retried this often, forever."))
}

// auditDenials writes an audit event for every authorization denial, and for
// every change allowed through an install-wide grant.
//
// Every denial, not only successes: a denial pattern is the signal that matters
// for detecting misuse, and it is the thing most commonly left out.
type auditDenials struct{ writer *audit.Writer }

func (a auditDenials) Denied(ctx context.Context, p authz.Principal, appID string, verb authz.Verb, code errs.Code) {
	_ = a.writer.Write(ctx, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "authz.denied",
		AppID:         appID,
		Detail:        map[string]any{"verb": string(verb), "code": string(code)},
	})
}

// ThroughInstall records an app verb allowed by an install-wide grant rather
// than one on the app (issue #81): the audit log says the access came from
// the installation, through which verb and which grant.
func (a auditDenials) ThroughInstall(ctx context.Context, p authz.Principal, appID string, verb, through authz.Verb, grant authz.Grant) {
	_ = a.writer.Write(ctx, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "authz.install_wide",
		AppID:         appID,
		TargetKind:    "grant",
		TargetID:      grant.ID,
		Detail: map[string]any{
			"verb":    string(verb),
			"through": string(through),
			"role":    grant.RoleID,
			"holder":  grant.PrincipalKind + ":" + grant.PrincipalID,
		},
	})
}

// runtimeForTrial returns the configured runtime, for the trial run (R-097).
//
// Nil when none is configured, which detection treats as "no trial happened" —
// deferred questions become real ones and the proposal is still produced. An
// install with no runtime cannot deploy anything anyway; refusing to detect
// would just make the failure arrive earlier and less clearly.
func runtimeForTrial(registry *adapterapi.Registry) detect.TrialRunner {
	// The default runtime, because at detection time there is no spec yet to
	// name one. Nothing is guessed: Default is what the install was configured
	// with, and without one there is no trial.
	ref, ok := registry.Default(adapterapi.CategoryRuntime)
	if !ok {
		return nil
	}
	rt, ok := registry.Runtime(ref)
	if !ok {
		return nil
	}
	return rt
}

// registryAdapters adapts the adapter registry to what the reconciler needs.
//
// Two methods rather than the whole registry, so the reconciler cannot reach
// for anything else — it resolves the adapters an app's spec names and has no
// business with the rest.
type registryAdapters struct{ r *adapterapi.Registry }

func (a registryAdapters) Runtime(ref string) (adapterapi.RuntimeAdapter, bool) {
	return a.r.Runtime(ref)
}

func (a registryAdapters) Routing(ref string) (adapterapi.RoutingAdapter, bool) {
	return a.r.Routing(ref)
}

// builtImageAuth is the reconciler's view of the install registry: its
// credential for an image Pando pushed there, read when it is needed.
func builtImageAuth(reg imageregistry.Provider) func(context.Context, string) *adapterapi.RegistryAuth {
	return func(ctx context.Context, ref string) *adapterapi.RegistryAuth {
		current, err := reg.Current(ctx)
		if err != nil || !current.Owns(ref) {
			return nil
		}
		auth, err := current.Auth(ctx)
		if err != nil {
			return nil
		}
		return auth
	}
}

// deletedAppImages deletes a deleted app's builds from the install registry:
// the app-wide image's repository, and one per workload any of its revisions
// built separately.
type deletedAppImages struct {
	reg  imageregistry.Provider
	apps *state.Apps
}

func registryImages(reg imageregistry.Provider, apps *state.Apps) reconciler.RegistryImages {
	return deletedAppImages{reg: reg, apps: apps}
}

func (d deletedAppImages) DeleteApp(ctx context.Context, appID string) error {
	current, err := d.reg.Current(ctx)
	if err != nil || !current.Configured() {
		// No registry, nothing in it. One that cannot be read is retried at
		// the next teardown pass.
		return err
	}
	revisions, err := d.apps.ListRevisions(ctx, appID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var workloads []string
	for _, rev := range revisions {
		// ListRevisions reads no bodies; the workloads are in the body.
		full, found, err := d.apps.RevisionByID(ctx, rev.ID)
		if err != nil {
			return err
		}
		if !found || full.Body == nil {
			continue
		}
		for _, w := range full.Body.Workloads {
			if w.Build != nil && !seen[w.Name] {
				seen[w.Name] = true
				workloads = append(workloads, w.Name)
			}
		}
	}
	_, err = current.DeleteApp(ctx, appID, workloads)
	return err
}

// buildCaches resolves the builder that built a deleted app. A builder that is
// no longer configured holds nothing Pando can reach, so there is nothing to
// forget.
type buildCaches struct{ r *adapterapi.Registry }

func (b buildCaches) Forget(ctx context.Context, builderRef, appID string) error {
	builder, ok := b.r.Builder(builderRef)
	if !ok {
		return nil
	}
	return builder.Forget(ctx, appID)
}

// reconcilerAuditor writes the reconciler's events to the audit log.
//
// The reconciler acts with no principal: nobody asked for a drift correction,
// which is the point of it. Recorded as a system action so that "who restarted
// this" has an answer, and the answer is Pando.
type reconcilerAuditor struct{ w *audit.Writer }

func (a reconcilerAuditor) Write(ctx context.Context, e reconciler.AuditEvent) error {
	return a.w.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "reconciler",
		Action:        e.Action,
		AppID:         e.AppID,
		TargetKind:    "app",
		TargetID:      e.AppID,
		Detail:        e.Detail,
	})
}

// refResolver reads a remote's refs without cloning it, with the app's source
// connection when it has one.
type refResolver struct{ sources source.Sources }

func (r refResolver) Resolve(ctx context.Context, src spec.Source, ad spec.AutoDeploy) (source.Tracked, error) {
	return r.sources.ResolveTracked(ctx, src, ad)
}

// newSourceAdapter is an unconfigured source adapter of a kind this build
// can run, or nil (R-091).
func newSourceAdapter(kind string) adapterapi.SourceAdapter {
	switch kind {
	case sourcegeneric.Kind:
		return sourcegeneric.New()
	case sourcegithub.Kind:
		return sourcegithub.New()
	case sourcegitlab.Kind:
		return sourcegitlab.New()
	case sourcegitea.Kind:
		return sourcegitea.New()
	case sourceazure.Kind:
		return sourceazure.New()
	case sourcebitbucket.Kind:
		return sourcebitbucket.New()
	}
	return nil
}

// newImageRegistryAdapter is an unconfigured image registry adapter of a
// kind, or nil (R-252, issue #153).
func newImageRegistryAdapter(kind string) adapterapi.ImageRegistryAdapter {
	switch kind {
	case registryoci.Kind:
		return registryoci.New()
	case registryecr.Kind:
		return registryecr.New()
	}
	return nil
}

// declaredImageRegistries are the image registries the startup configuration
// declares, built at startup with the other adapters.
func declaredImageRegistries(registry *adapterapi.Registry) []imageregistry.Declared {
	def, _ := registry.Default(adapterapi.CategoryImageRegistry)
	var out []imageregistry.Declared
	for _, ref := range registry.ByCategory(adapterapi.CategoryImageRegistry) {
		if a, ok := registry.ImageRegistry(ref); ok {
			out = append(out, imageregistry.Declared{ID: ref, Adapter: a, IsDefault: ref == def})
		}
	}
	return out
}

// declaredSourceAdapters are the source connections the configuration file
// declares, built at startup.
func declaredSourceAdapters(registry *adapterapi.Registry) map[string]adapterapi.SourceAdapter {
	out := map[string]adapterapi.SourceAdapter{}
	for _, ref := range registry.ByCategory(adapterapi.CategorySource) {
		if a, ok := registry.Get(ref); ok {
			if sa, ok := a.(adapterapi.SourceAdapter); ok {
				out[ref] = sa
			}
		}
	}
	return out
}

// sourceAuthorizationsFor keeps OAuth authorizations of source connections
// while they are in progress, sealed like the connections' credentials.
func sourceAuthorizationsFor(db *state.DB, registry *adapterapi.Registry) *state.AdapterCredentials {
	ref, ok := registry.Default(adapterapi.CategorySecrets)
	if !ok {
		if refs := registry.ByCategory(adapterapi.CategorySecrets); len(refs) > 0 {
			ref = refs[0]
		}
	}
	sa, _ := registry.Secrets(ref)
	return state.NewSourceAuthorizations(db, sa, ref)
}

// consoleHandler returns the embedded console, or nil when the binary was built
// without it.
//
// A developer running `go run ./cmd/pando` without `make console` gets an API
// and no UI, which is the honest outcome — better than a panic at startup or a
// blank page that looks like a broken console rather than an absent one.
func consoleHandler(logger *zap.Logger) http.Handler {
	handler, ok := console.Handler()
	if !ok {
		logger.Warn("console assets are not built into this binary; the UI will 404",
			zap.String("remedy", "run `make console`"))
		return nil
	}
	return handler
}

// securityNotifier tells an app's owner that it is below the installation's
// security requirement, and what happens next (R-315).
//
// A shim rather than the notify adapter directly: the reconciler's pass knows a
// user ID and two strings, and nothing about recipients, retention or
// notification kinds. `policy_violation` is the kind, because that is what this
// is — the app did not fail and the deploy did not fail.
//
// It goes through the router like every other notification of Pando's own, so
// it reaches the owner on every channel that reaches people, as their
// preferences allow, rather than only the console (R-373).
type securityNotifier struct{ router subscription.Router }

func (n securityNotifier) Notify(ctx context.Context, userID, appID, subject, body string) {
	_ = n.router.Notify(ctx, adapterapi.Notification{
		Kind:       adapterapi.NotifyPolicyViolation,
		AppID:      appID,
		Recipients: []adapterapi.Recipient{{UserID: userID}},
		Subject:    subject,
		Body:       body,
	})
}

// sourceScanner scores a checkout during detection (R-312).
//
// A shim rather than the security service directly: detection knows an app ID
// and a directory, and nothing about scan requests, audit events or who is
// asking — which here is nobody. Detection runs in the background after an app
// is created, so the principal is the system, recorded as such.
type sourceScanner struct {
	service *security.Service
	logger  *zap.Logger
}

func (s sourceScanner) ScanSource(ctx context.Context, appID, dir, commit string) {
	if s.service == nil {
		return
	}
	if _, configured := s.service.Configured(); !configured {
		return
	}

	// No spec ID: there is no revision yet, and there may never be one — this
	// is a scan of what was proposed, which is exactly the thing somebody is
	// deciding about.
	// The commit, so a deploy of this same source uses this scan rather than
	// scanning it again.
	if _, err := s.service.Scan(ctx, adapterapi.ScanRequest{AppID: appID, SourceDir: dir, Commit: commit},
		audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "detection"}); err != nil {
		// Never fatal. The proposal is what this run is producing, and a
		// scanner that could not read a checkout is recorded as a failed scan
		// on the app already.
		s.logger.Info("could not scan an app's source during detection",
			zap.String("app_id", appID), zap.Error(err))
	}
}

// startupPolicy turns the policy fields set in the startup config into the
// overlay, checking each one — including that a disabled verb is a verb Pando
// has, the same check PUT /policy makes, since a typo there denies nothing and
// looks exactly like a rule that works.
func startupPolicy(cfg *config.Config) (*corepolicy.Overlay, error) {
	settings := make([]corepolicy.Setting, 0, len(cfg.Policy))
	for _, p := range cfg.Policy {
		settings = append(settings, corepolicy.Setting{
			Key: p.Key, Value: p.Value,
			Source: corepolicy.Source{Kind: p.Source.Kind, Name: p.Source.Name, Key: p.Source.Key},
		})
	}
	overlay, err := corepolicy.NewOverlay(settings)
	if err != nil {
		return nil, err
	}
	fixed := overlay.Apply(corepolicy.Document{})
	if err := fixed.ValidateRules(); err != nil {
		return nil, fmt.Errorf("the startup policy has a setting Pando cannot use: %w", err)
	}
	for _, verbs := range [][]string{fixed.DisabledVerbs, fixed.AgentDisabledVerbs} {
		for _, v := range verbs {
			if !authz.IsVerb(authz.Verb(v)) {
				return nil, fmt.Errorf("the startup policy disables %q, which is not a permission Pando has", v)
			}
		}
	}
	return overlay, nil
}

// adapterKinds is every kind of adapter this build can run, for the console's
// and the CLI's "add an adapter" forms. Kept beside the switch that constructs
// them, which is the other list of the same kinds: a kind added there and not
// here would be runnable but not configurable from anywhere but the API.
func adapterKinds() []adapterapi.KindInfo {
	return []adapterapi.KindInfo{
		aianthropic.Info(),
		aiopenai.Info(),
		ailocal.Info(),
		dockerruntime.Info(),
		kubernetesruntime.Info(),
		multidocker.Info(),
		loopback.Info(),
		traefik.Info(),
		cloudflare.Info(),
		buildkitadapter.Info(),
		trivyscanner.Info(),
		secretslocal.Info(),
		backuplocal.Info(),
		servicesdocker.Info(),
		notifyconsole.Info(),
		notifysmtp.Info(),
		chat.Slack.Info(),
		chat.Teams.Info(),
		chat.Discord.Info(),
		notifyntfy.Info(),
		sourcegeneric.Info(),
		sourcegithub.Info(),
		sourcegitlab.Info(),
		sourcegitea.Info(),
		sourceazure.Info(),
		sourcebitbucket.Info(),
		registryoci.Info(),
		registryecr.Info(),
	}
}

// auditArchiveStores is where audit archives go (R-347): a directory of
// Pando's own, written the way the local backup destination writes a bundle —
// to a temporary name, renamed when whole — or a configured backup
// destination, resolved by reference like a backup's.
func auditArchiveStores(ctx context.Context, dir string, registry *adapterapi.Registry) (audit.Stores, error) {
	kept := backuplocal.New()
	cfgJSON, err := json.Marshal(map[string]string{"path": dir})
	if err != nil {
		return audit.Stores{}, err
	}
	if err := kept.Configure(ctx, cfgJSON); err != nil {
		return audit.Stores{}, err
	}
	return audit.Stores{
		Kept: kept,
		Export: func(ref string) (audit.Store, string, error) {
			if ref == "" {
				var ok bool
				if ref, ok = registry.Default(adapterapi.CategoryBackup); !ok {
					return nil, "", errs.New(errs.Internal,
						"Host policy exports audit archives to the default backup destination, and this installation has none.").
						WithRemedy("Configure a backup destination, name one in audit_archive_destination, or set audit_archive to keep.")
				}
			}
			dest, ok := registry.Backup(ref)
			if !ok {
				return nil, "", errs.Newf(errs.Internal,
					"Host policy exports audit archives to the backup destination %q, which is not running.", ref).
					WithRemedy("Configure that backup destination, or change audit_archive_destination in host policy.")
			}
			return dest, ref, nil
		},
	}, nil
}
