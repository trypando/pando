package policy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

var (
	person = authz.Principal{Kind: authz.KindUser, ID: "usr_01HQ8"}
	agent  = authz.Principal{Kind: authz.KindToken, ID: "tok_01HQ8"}
)

func ctx() context.Context { return context.Background() }

// R-270: Pando ships permissive defaults.
func TestR270_TheDefaultDocumentIsPermissive(t *testing.T) {
	d := policy.Default()

	require.Empty(t, d.SourceAllowlist, "apps may be created from anywhere")
	require.Empty(t, d.DisabledVerbs, "nothing is turned off install-wide")
	require.NotNil(t, d.AllowAnonymousGrants)
	require.True(t, *d.AllowAnonymousGrants)
	require.Equal(t, spec.IsolationContainer, d.MinBuildIsolation)
	require.Equal(t, spec.IsolationContainer, d.MinRuntimeIsolation)
	require.Zero(t, d.MaxLogDiskBytes)
	require.Zero(t, d.MaxTokenLifetimeDays)
	require.False(t, d.RequireBackupBeforeDestroy)
}

// O-12's resolution: the exclusions are policy, not a list of tools the MCP
// server declines to expose. An agent holding a token can call the REST API
// directly, so an MCP-layer exclusion is a speed bump rather than a boundary.
func TestO12_AgentExclusionsShipAsPolicyAndApplyToEverySurface(t *testing.T) {
	e := policy.Static(policy.Default())

	for _, verb := range []authz.Verb{
		authz.AppExec, authz.AppSecretsRead, authz.AppGrantsManage,
		authz.InstallPolicyManage, authz.InstallUsersManage, authz.InstallBackupManage,
		// R-154: approval is a human sign-off.
		authz.InstallDeploysApprove, authz.AppDeployApprove,
	} {
		require.Error(t, e.Allows(ctx(), agent, verb, "app_01HQ8"), "%s", verb)
		require.NoError(t, e.Allows(ctx(), person, verb, "app_01HQ8"),
			"%s is denied to agents only, not to the person", verb)
	}
}

// A person told "this is turned off for the installation" when it is only
// turned off for their agent would go looking in the wrong place.
func TestTheAgentRuleProducesTheMoreSpecificMessage(t *testing.T) {
	e := policy.Static(policy.Document{
		AgentDisabledVerbs: []string{string(authz.AppExec)},
		DisabledVerbs:      []string{string(authz.AppExec)},
	})

	err := e.Allows(ctx(), agent, authz.AppExec, "app_01HQ8")
	require.Equal(t, errs.PolicyExecDisabled, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "Tokens and agents")
	require.Contains(t, errs.As(err).Remedy, "signed in")
	require.Equal(t, string(authz.AppExec), errs.As(err).Details["verb"])
}

// R-085: host policy may disable exec install-wide. It denies the owner too —
// policy is a floor evaluated before grants, not something a grant outranks.
func TestR085_ExecDisabledInstallWideDeniesEveryone(t *testing.T) {
	e := policy.Static(policy.Document{DisabledVerbs: []string{string(authz.AppExec)}})

	for _, p := range []authz.Principal{person, agent} {
		err := e.Allows(ctx(), p, authz.AppExec, "app_01HQ8")
		require.Equal(t, errs.PolicyExecDisabled, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "Running commands inside apps is turned off")
		require.NotEmpty(t, errs.As(err).Remedy)
	}
}

// exec has its own message because the console explains that one specifically:
// it is a deliberate posture, not a permissions mistake to escalate.
func TestAnyOtherDisabledVerbGetsTheGeneralMessage(t *testing.T) {
	e := policy.Static(policy.Document{DisabledVerbs: []string{string(authz.AppSecretsRead)}})

	err := e.Allows(ctx(), person, authz.AppSecretsRead, "app_01HQ8")
	require.Equal(t, errs.PolicyExecDisabled, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "This action is turned off")
	require.Equal(t, string(authz.AppSecretsRead), errs.As(err).Details["verb"])
}

// R-272: policy can only deny. Nothing here grants anything.
func TestR272_AVerbNoRuleMentionsIsUntouched(t *testing.T) {
	e := policy.Static(policy.Document{DisabledVerbs: []string{string(authz.AppExec)}})
	require.NoError(t, e.Allows(ctx(), person, authz.AppDeploy, "app_01HQ8"))
	require.NoError(t, policy.Static(policy.Document{}).Allows(ctx(), agent, authz.AppExec, "app_01HQ8"),
		"an empty document denies nothing, including to agents")
}

// R-274: policy applies to a running install, so the document is loaded per
// evaluation. A cached one would keep denying, or keep allowing, for however
// long the cache lived.
func TestR274_TheDocumentIsReloadedForEveryEvaluation(t *testing.T) {
	loads := 0
	disabled := false
	e := policy.New(func(context.Context) (policy.Document, error) {
		loads++
		if disabled {
			return policy.Document{DisabledVerbs: []string{string(authz.AppExec)}}, nil
		}
		return policy.Document{}, nil
	})

	require.NoError(t, e.Allows(ctx(), person, authz.AppExec, "app_01HQ8"))
	disabled = true
	require.Error(t, e.Allows(ctx(), person, authz.AppExec, "app_01HQ8"),
		"the next call sees the new document, with no cache to wait out")
	require.Equal(t, 2, loads)
}

func TestAPolicyThatCannotBeLoadedFailsClosedRatherThanSilently(t *testing.T) {
	boom := errors.New("the host_policy row is unreadable")
	e := policy.New(func(context.Context) (policy.Document, error) { return policy.Document{}, boom })

	require.ErrorIs(t, e.Allows(ctx(), person, authz.AppExec, "app_01HQ8"), boom)
	require.ErrorIs(t, e.AllowsSource(ctx(), gitSrc("https://github.com/ben/notes")), boom)
	require.ErrorIs(t, e.AllowsAnonymousGrant(ctx(), false), boom)

	_, _, err := e.IsolationFloors(ctx())
	require.ErrorIs(t, err, boom)

	_, err = e.Document(ctx())
	require.ErrorIs(t, err, boom)
}

// R-092: the allowlist is checked before any clone, so a blocked source
// produces zero disk writes.
func TestR092_TheSourceAllowlistAcceptsOnlyApprovedHosts(t *testing.T) {
	e := policy.Static(policy.Document{SourceAllowlist: []string{"github.com", ".corp.example"}})

	for _, allowed := range []string{
		"https://github.com/ben/notes.git",
		"https://GitHub.com/ben/notes",
		"git@github.com:ben/notes.git",
		"https://git.corp.example/team/app",
		"https://corp.example/team/app",
		"ssh://git@deep.git.corp.example/x",
	} {
		require.NoError(t, e.AllowsSource(ctx(), gitSrc(allowed)), allowed)
	}

	for _, blocked := range []string{
		"https://gitlab.com/ben/notes",
		"https://notgithub.com/ben/notes",
		"https://evil.example/x",
		"git@gitlab.com:ben/notes.git",
		// A suffix match must not be a substring match: this is not corp.example.
		"https://corp.example.evil.com/x",
	} {
		err := e.AllowsSource(ctx(), gitSrc(blocked))
		require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err), blocked)
	}
}

func TestABlockedSourceSaysWhichHostAndWhatIsAllowed(t *testing.T) {
	e := policy.Static(policy.Document{SourceAllowlist: []string{"github.com"}})

	err := e.AllowsSource(ctx(), gitSrc("https://gitlab.com/ben/notes"))
	require.Contains(t, errs.As(err).Message, "gitlab.com")
	require.Equal(t, "https://gitlab.com/ben/notes", errs.As(err).Details["source"])
	require.Equal(t, []string{"github.com"}, errs.As(err).Details["allowed"])
	require.NotEmpty(t, errs.As(err).Remedy)
}

// A source with no parseable host still has to be refused, and the message
// cannot name a host it does not have.
func TestASourceWithNoHostIsRefusedReadably(t *testing.T) {
	e := policy.Static(policy.Document{SourceAllowlist: []string{"github.com"}})

	err := e.AllowsSource(ctx(), gitSrc("/srv/local/repo"))
	require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "that address")
}

func TestAnEmptyAllowlistRestrictsNothing(t *testing.T) {
	e := policy.Static(policy.Document{})
	require.NoError(t, e.AllowsSource(ctx(), gitSrc("https://anywhere.example/x")))
	require.NoError(t, e.AllowsSource(ctx(), imageSrc("anyone/anything:latest")))
	require.NoError(t, e.AllowsSource(ctx(), spec.Source{Type: spec.SourceUpload}))

	// A source that names nothing yet is not the allowlist's business.
	require.NoError(t, policy.Static(policy.Document{SourceAllowlist: []string{"github.com"}}).
		AllowsSource(ctx(), spec.Source{}))
}

// R-092 names orgs and repos, not only forges: an entry with a path admits
// that namespace and nothing beside it.
func TestR092_AnAllowlistEntryWithAPathAdmitsOnlyThatNamespace(t *testing.T) {
	e := policy.Static(policy.Document{SourceAllowlist: []string{"github.com/acme", "https://gitlab.com/team/app.git"}})

	for _, allowed := range []string{
		"https://github.com/acme/notes",
		"https://github.com/ACME/notes.git",
		"git@github.com:acme/notes.git",
		"https://gitlab.com/team/app",
	} {
		require.NoError(t, e.AllowsSource(ctx(), gitSrc(allowed)), allowed)
	}
	for _, blocked := range []string{
		"https://github.com/other/notes",
		// A prefix is whole segments, not characters.
		"https://github.com/acme-evil/notes",
		"https://gitlab.com/team/app-two",
		"https://github.com",
	} {
		err := e.AllowsSource(ctx(), gitSrc(blocked))
		require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err), blocked)
	}
}

// TestR092_AnImageOutsideTheAllowlistIsRefused asserts R-092 for images: an
// image reference is read as a registry and a repository and checked like a
// git URL. It used to be passed the source's empty URL and admitted, so any
// image from any registry deployed on an install with an allowlist (issue #41).
func TestR092_AnImageOutsideTheAllowlistIsRefused(t *testing.T) {
	e := policy.Static(policy.Document{SourceAllowlist: []string{
		"ghcr.io/acme",
		"docker.io/library",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com",
	}})

	for _, allowed := range []string{
		"ghcr.io/acme/web:1.2",
		"ghcr.io/acme/tools/cli@sha256:" + strings.Repeat("a", 64),
		"nginx",
		"nginx:1.27",
		"docker.io/library/redis:7",
		"index.docker.io/library/redis",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/team/api:v3",
	} {
		require.NoError(t, e.AllowsSource(ctx(), imageSrc(allowed)), allowed)
	}

	for _, blocked := range []string{
		"ghcr.io/stranger/web:latest",
		"ghcr.io/acme-evil/web",
		"stranger/nginx",
		"quay.io/acme/web",
		"999999999999.dkr.ecr.us-east-1.amazonaws.com/team/api",
		"not a reference",
	} {
		err := e.AllowsSource(ctx(), imageSrc(blocked))
		require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err), blocked)
	}

	err := e.AllowsSource(ctx(), imageSrc("quay.io/acme/web:1"))
	require.Contains(t, errs.As(err).Message, "quay.io/acme/web")
	require.Equal(t, "quay.io/acme/web:1", errs.As(err).Details["source"])
}

// An upload has no host for an entry to name, so a non-empty allowlist admits
// one only when it says `upload`. Otherwise "only github.com/acme" would mean
// nothing to anyone holding a CLI token.
func TestR092_AnUploadIsRefusedUnlessTheAllowlistAdmitsUploads(t *testing.T) {
	up := spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"}

	err := policy.Static(policy.Document{SourceAllowlist: []string{"github.com"}}).AllowsSource(ctx(), up)
	require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "uploaded files")
	require.Contains(t, errs.As(err).Remedy, "`upload`")

	require.NoError(t, policy.Static(policy.Document{SourceAllowlist: []string{"github.com", "upload"}}).
		AllowsSource(ctx(), up))

	// And `upload` admits nothing else.
	err = policy.Static(policy.Document{SourceAllowlist: []string{"upload"}}).
		AllowsSource(ctx(), gitSrc("https://github.com/acme/notes"))
	require.Equal(t, errs.PolicySourceNotAllowed, errs.CodeOf(err))
}

func gitSrc(url string) spec.Source { return spec.Source{Type: spec.SourceGit, URL: url} }

func imageSrc(ref string) spec.Source { return spec.Source{Type: spec.SourceImage, Image: ref} }

// R-076: an app may be shared with everyone unless policy says otherwise.
func TestR076_AnonymousGrantsAreAllowedUnlessExplicitlyForbidden(t *testing.T) {
	require.NoError(t, policy.Static(policy.Default()).AllowsAnonymousGrant(ctx(), false))

	// Unset is not the same as false: a partially written document must not
	// start denying something nobody turned off.
	require.NoError(t, policy.Static(policy.Document{}).AllowsAnonymousGrant(ctx(), false))

	forbidden := false
	err := policy.Static(policy.Document{AllowAnonymousGrants: &forbidden}).AllowsAnonymousGrant(ctx(), false)
	require.Equal(t, errs.PolicyAnonymousGrantForbidden, errs.CodeOf(err))
	require.NotEmpty(t, errs.As(err).Remedy)

	allowed := true
	require.NoError(t, policy.Static(policy.Document{AllowAnonymousGrants: &allowed}).AllowsAnonymousGrant(ctx(), false))
}

// R-024 and R-114: two floors, because how isolated a build must be is a
// different question from how isolated the running app must be.
func TestR114_BuildAndRuntimeFloorsAreReportedSeparately(t *testing.T) {
	build, runtime, err := policy.Static(policy.Document{
		MinBuildIsolation:   spec.IsolationVM,
		MinRuntimeIsolation: spec.IsolationContainer,
	}).IsolationFloors(ctx())

	require.NoError(t, err)
	require.Equal(t, spec.IsolationVM, build)
	require.Equal(t, spec.IsolationContainer, runtime)
	require.NotEqual(t, build, runtime)
}

func TestDocumentReturnsWhatWasLoaded(t *testing.T) {
	want := policy.Document{MaxLogDiskBytes: 1 << 30, MaxTokenLifetimeDays: 90}
	got, err := policy.Static(want).Document(ctx())
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// The evaluator is the authz.Policy the authorizer calls. If that stops being
// true, the wiring breaks somewhere less obvious than here.
func TestTheEvaluatorIsTheAuthorizersPolicy(t *testing.T) {
	var _ authz.Policy = policy.Static(policy.Default())
}

// R-336: host policy may forbid AI screening install-wide. It ships allowed
// (R-270), an unreadable document denies — the direction that sends nothing
// anywhere — and a reason, not an error, is what comes back.
func TestR336_HostPolicyCanForbidAIScreening(t *testing.T) {
	allowed := policy.New(func(context.Context) (policy.Document, error) { return policy.Default(), nil })
	require.Empty(t, allowed.AllowsScreening(ctx()))

	off := policy.New(func(context.Context) (policy.Document, error) {
		return policy.Document{DisableAIScreening: true}, nil
	})
	require.Contains(t, off.AllowsScreening(ctx()), "administrator")

	broken := policy.New(func(context.Context) (policy.Document, error) {
		return policy.Document{}, errors.New("db down")
	})
	require.NotEmpty(t, broken.AllowsScreening(ctx()))
}

// TestR076_PublicSharingIsAllowedPasscodeOnlyOrNone asserts the three rules for
// sharing with everyone, and that the older boolean still means what it meant.
func TestR076_PublicSharingIsAllowedPasscodeOnlyOrNone(t *testing.T) {
	with := func(d policy.Document) *policy.Evaluator { return policy.Static(d) }

	allowed := with(policy.Document{PublicSharing: policy.PublicSharingAllowed})
	require.NoError(t, allowed.AllowsAnonymousGrant(ctx(), false))
	require.NoError(t, allowed.AllowsAnonymousGrant(ctx(), true))

	passcode := with(policy.Document{PublicSharing: policy.PublicSharingPasscodeOnly})
	require.Error(t, passcode.AllowsAnonymousGrant(ctx(), false), "plain public is refused")
	require.NoError(t, passcode.AllowsAnonymousGrant(ctx(), true))

	none := with(policy.Document{PublicSharing: policy.PublicSharingNone})
	require.Error(t, none.AllowsAnonymousGrant(ctx(), false))
	require.Error(t, none.AllowsAnonymousGrant(ctx(), true), "a passcode does not make it allowed")

	// The older boolean: false is none, and the new field wins over it.
	no := false
	require.Equal(t, policy.PublicSharingNone, policy.Document{AllowAnonymousGrants: &no}.PublicSharingMode())
	require.Equal(t, policy.PublicSharingPasscodeOnly,
		policy.Document{AllowAnonymousGrants: &no, PublicSharing: policy.PublicSharingPasscodeOnly}.PublicSharingMode())
	require.Equal(t, policy.PublicSharingAllowed, policy.Document{}.PublicSharingMode())

	// Set at startup, a value that is none of the three stops it.
	_, err := policy.NewOverlay([]policy.Setting{{Key: "public_sharing", Value: "sometimes"}})
	require.ErrorContains(t, err, "allowed, passcode_only or none")
	_, err = policy.NewOverlay([]policy.Setting{{Key: "public_sharing", Value: "passcode_only"}})
	require.NoError(t, err)
}

// TestR348_PolicyRefusesRetentionUnderTheFloor asserts R-348 where an
// administrator types it: fewer than three months, a mode that is not one,
// and a destination that nothing would be sent to are each refused, saying
// what to use instead. What the archiver reads is what was saved.
func TestR348_PolicyRefusesRetentionUnderTheFloor(t *testing.T) {
	for _, ok := range []policy.Document{
		{},
		{AuditRetentionMonths: 3},
		{AuditRetentionMonths: 24, AuditArchive: audit.ArchiveExport, AuditArchiveDestination: "bk_s3"},
		{AuditArchive: audit.ArchiveOff},
	} {
		require.NoError(t, ok.ValidateRules(), "%+v", ok)
	}

	err := policy.Document{AuditRetentionMonths: 1}.ValidateRules()
	require.ErrorContains(t, err, "at least 3 months")
	err = policy.Document{AuditRetentionMonths: -2}.ValidateRules()
	require.ErrorContains(t, err, "at least 3 months")
	err = policy.Document{AuditArchive: "shred"}.ValidateRules()
	require.ErrorContains(t, err, "keep, export or off")
	err = policy.Document{AuditArchive: audit.ArchiveKeep, AuditArchiveDestination: "bk_s3"}.ValidateRules()
	require.ErrorContains(t, err, "set audit_archive to export")

	got := policy.Document{AuditRetentionMonths: 6, AuditArchive: audit.ArchiveExport, AuditArchiveDestination: "bk_s3"}.AuditRetention()
	require.Equal(t, audit.Retention{Months: 6, Archive: audit.ArchiveExport, Destination: "bk_s3"}, got)
}

// TestR105_ANegativeDeployLimitSaysWhatToUse asserts R-105 for
// max_concurrent_deploys (issue #93): refused, with the values that work.
func TestR105_ANegativeDeployLimitSaysWhatToUse(t *testing.T) {
	err := policy.Document{MaxConcurrentDeploys: -1}.ValidateRules()
	require.Error(t, err)
	require.Contains(t, err.Error(), "max_concurrent_deploys is -1")
	require.Contains(t, err.Error(), "0 means one per CPU")

	require.NoError(t, policy.Document{MaxConcurrentDeploys: 0}.ValidateRules())
	require.NoError(t, policy.Document{MaxConcurrentDeploys: 8}.ValidateRules())
}
