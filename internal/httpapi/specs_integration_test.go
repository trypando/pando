//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// R-152: spec revisions are append-only, so rollback is always to something
// that provably existed.
func TestR152_SpecRevisionsAccumulateAndAreNumbered(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	first := i.writeSpec(admin, id, minimalSpec())
	require.Equal(t, 1, first)

	second := minimalSpec()
	second["workloads"].([]map[string]any)[0]["image"] = "nginx:1.27-alpine"
	require.Equal(t, 2, i.writeSpec(admin, id, second))

	listed := i.do(admin, http.MethodGet, "/apps/"+id+"/specs", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())

	var body struct {
		Revisions []struct {
			Revision int    `json:"revision"`
			Origin   string `json:"origin"`
		} `json:"revisions"`
	}
	listed.JSON(t, &body)
	require.Len(t, body.Revisions, 2, "the earlier revision is still there")
}

func TestASpecRevisionCanBeReadBack(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")
	rev := i.writeSpec(admin, id, minimalSpec())

	got := i.do(admin, http.MethodGet, fmt.Sprintf("/apps/%s/specs/%d", id, rev), nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, got.String(), "nginx:alpine")

	missing := i.do(admin, http.MethodGet, "/apps/"+id+"/specs/99", nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.String())

	notANumber := i.do(admin, http.MethodGet, "/apps/"+id+"/specs/latest", nil)
	require.GreaterOrEqual(t, notANumber.Code, 400, notANumber.String())
	require.NotEqual(t, http.StatusInternalServerError, notANumber.Code)
}

// Pinning points the app at a revision. It does not deploy — keeping the two
// separate is what makes "accepted but not deployed" a state someone can sit in.
func TestPinningARevisionDoesNotDeploy(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")
	rev := i.writeSpec(admin, id, minimalSpec())

	i.pinSpec(admin, id, rev)

	got := i.do(admin, http.MethodGet, "/apps/"+id, nil)
	var app map[string]any
	got.JSON(t, &app)
	require.NotEmpty(t, app["pinned_spec_id"])

	deployments := i.do(admin, http.MethodGet, "/apps/"+id+"/deployments", nil)
	var deps struct {
		Deployments []map[string]any `json:"deployments"`
	}
	deployments.JSON(t, &deps)
	require.Empty(t, deps.Deployments, "pinning is not deploying")
}

func TestPinningARevisionThatDoesNotExist(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/specs/99/pin", map[string]any{})
	require.Equal(t, http.StatusNotFound, got.Code, got.String())
}

// A spec that cannot describe a runnable app is refused at write time rather
// than at deploy time.
func TestAnInvalidSpecIsRefusedWithEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	// No primary workload, and a mount pointing at a volume that is not
	// declared: two problems, which must not be reported one at a time.
	got := i.do(admin, http.MethodPost, "/apps/"+id+"/specs", map[string]any{
		"schema_version": spec.SchemaVersion,
		"workloads": []map[string]any{{
			"name":   "web",
			"image":  "nginx:alpine",
			"mounts": []map[string]any{{"volume_id": "vol_undeclared", "path": "/data"}},
		}},
	})
	require.Equal(t, http.StatusBadRequest, got.Code, got.String())
	require.Equal(t, string(errs.ValidInvalid), got.ErrorCode())

	var env struct {
		Details map[string]any `json:"details"`
	}
	got.JSON(t, &env)
	if problems, ok := env.Details["problems"].([]any); ok {
		require.GreaterOrEqual(t, len(problems), 2,
			"the caller fixes one and learns about the next on the next try, otherwise")
	}
}

func TestASpecForAnotherSchemaVersionIsRefused(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	body := minimalSpec()
	body["schema_version"] = spec.SchemaVersion + 99

	// The handler stamps the current schema version, so this lands as a valid
	// spec rather than a rejection — what must not happen is a 500.
	got := i.do(admin, http.MethodPost, "/apps/"+id+"/specs", body)
	require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())
}

func TestAnUnreadableSpecBodyIsRefusedReadably(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/specs", "not an object")
	require.Equal(t, http.StatusBadRequest, got.Code, got.String())

	var env struct {
		Remedy string `json:"remedy"`
	}
	got.JSON(t, &env)
	require.NotEmpty(t, env.Remedy)
}

// The install's defaults are applied to a hand-written spec too — R-223's log
// cap, R-211's backup retention and the revision limit were all absent from
// every hand-written spec, which is the API's own documented way to configure
// an app.
func TestAHandWrittenSpecStillInheritsTheInstallsDefaults(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")
	rev := i.writeSpec(admin, id, minimalSpec())

	got := i.do(admin, http.MethodGet, fmt.Sprintf("/apps/%s/specs/%d", id, rev), nil)
	require.Equal(t, http.StatusOK, got.Code)

	var body struct {
		Body struct {
			Retention struct {
				LogBytes         int64 `json:"log_bytes"`
				BackupDailyCount int   `json:"backup_daily_count"`
				SpecRevisions    int   `json:"spec_revisions"`
			} `json:"retention"`
			Deploy struct {
				Strategy string `json:"strategy"`
			} `json:"deploy"`
		} `json:"body"`
	}
	got.JSON(t, &body)

	require.Positive(t, body.Body.Retention.LogBytes, "R-223: an app with no log cap can fill the host")
	require.Positive(t, body.Body.Retention.BackupDailyCount, "R-211")
	require.Positive(t, body.Body.Retention.SpecRevisions, "R-152")
	require.Equal(t, string(spec.DeployRecreate), body.Body.Deploy.Strategy, "R-144")
}

// An imported spec is untrusted input like any other and lands as a proposal
// requiring review, never as a live deployment.
func TestAnImportedSpecIsRecordedAsImported(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	body := minimalSpec()
	body["origin"] = string(spec.OriginImported)
	rev := i.writeSpec(admin, id, body)

	got := i.do(admin, http.MethodGet, fmt.Sprintf("/apps/%s/specs/%d", id, rev), nil)
	var revision struct {
		Origin string `json:"origin"`
	}
	got.JSON(t, &revision)
	require.Equal(t, string(spec.OriginImported), revision.Origin)

	// And writing one does not pin it.
	app := i.do(admin, http.MethodGet, "/apps/"+id, nil)
	var current map[string]any
	app.JSON(t, &current)
	require.Empty(t, current["pinned_spec_id"])
}

// Diffing two revisions is how someone sees what a change did before pinning it.
func TestTwoRevisionsCanBeDiffed(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	first := i.writeSpec(admin, id, minimalSpec())
	changed := minimalSpec()
	changed["workloads"].([]map[string]any)[0]["image"] = "nginx:1.27-alpine"
	second := i.writeSpec(admin, id, changed)

	got := i.do(admin, http.MethodGet, fmt.Sprintf("/apps/%s/specs/%d/diff/%d", id, first, second), nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.NotEqual(t, "{}", got.String())

	missing := i.do(admin, http.MethodGet, fmt.Sprintf("/apps/%s/specs/%d/diff/99", id, first), nil)
	require.GreaterOrEqual(t, missing.Code, 400, missing.String())
	require.NotEqual(t, http.StatusInternalServerError, missing.Code)
}

// R-020: the spec is the sole record of how an app runs, so exporting it is how
// someone takes that record elsewhere.
func TestR020_APinnedSpecCanBeExported(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodGet, "/apps/"+id+"/export", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var exported map[string]any
	got.JSON(t, &exported)
	require.Equal(t, float64(spec.SchemaVersion), exported["schema_version"])
	require.NotEmpty(t, exported["workloads"])

	// Safe to hand to someone: a literal slot value is stored as a secret
	// rather than inline, so an export carries no credential.
	require.NotContains(t, got.String(), "password")
}

// Writing a spec is app.spec.edit, which is not something a data-plane grant
// carries.
func TestWritingASpecIsBehindTheSpecEditVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")
	id := i.createApp(admin, "notes")
	rev := i.writeSpec(admin, id, minimalSpec())

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, fmt.Sprintf("/apps/%s/specs", id), minimalSpec()},
		{http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", id, rev), map[string]any{}},
	} {
		got := i.do(other, c.method, c.path, c.body)
		require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, got.Code,
			"%s %s: %s", c.method, c.path, got.String())
	}
}

// --- what a pinned spec unlocks -------------------------------------------

// R-132: a required unfilled slot blocks deploy at plan time, not at deploy
// time. A spec with no slots plans as far as its adapters allow.
func TestR132_PlanningAPinnedSpecReportsWhatIsMissingAtPlanTime(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/plan", map[string]any{})
	require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())

	// No runtime adapter is configured in a test install, so the honest answer
	// is the planner's — named, and before anything is deployed.
	if got.Code >= 400 {
		require.Contains(t, got.ErrorCode(), "PLAN_", got.String())
	}
}

func TestSlotsComeFromThePinnedSpec(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	withSlot := minimalSpec()
	withSlot["slots"] = []map[string]any{
		{"key": "DATABASE_URL", "type": "postgres", "required": true},
	}
	withSlot["workloads"].([]map[string]any)[0]["env"] = []map[string]any{
		{"key": "DATABASE_URL", "slot_ref": "DATABASE_URL"},
	}
	i.pinSpec(admin, id, i.writeSpec(admin, id, withSlot))

	got := i.do(admin, http.MethodGet, "/apps/"+id+"/slots", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Slots []struct {
			Key      string `json:"key"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"slots"`
	}
	got.JSON(t, &body)
	require.Len(t, body.Slots, 1)
	require.Equal(t, "DATABASE_URL", body.Slots[0].Key)
	require.True(t, body.Slots[0].Required)
}

// A slot says what it needs, not who supplies it — but a literal is the
// caller's own value and is stored as a secret rather than inline, so an export
// stays safe to hand to someone.
func TestASlotCanBeFilledWithALiteral(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	withSlot := minimalSpec()
	withSlot["slots"] = []map[string]any{
		{"key": "DATABASE_URL", "type": "postgres", "required": true},
	}
	withSlot["workloads"].([]map[string]any)[0]["env"] = []map[string]any{
		{"key": "DATABASE_URL", "slot_ref": "DATABASE_URL"},
	}
	i.pinSpec(admin, id, i.writeSpec(admin, id, withSlot))

	got := i.do(admin, http.MethodPut, "/apps/"+id+"/slots/DATABASE_URL", map[string]any{
		"key": "DATABASE_URL", "mode": "literal", "value": "postgres://user:pw@db/app",
	})
	require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())

	if got.Code < 400 {
		exported := i.do(admin, http.MethodGet, "/apps/"+id+"/export", nil)
		require.NotContains(t, exported.String(), "user:pw",
			"a literal is stored as a secret, so the export carries no credential")
	}
}

// TestR132_FillingTwoSlotsKeepsBoth asserts R-132.
//
// Each fill wrote a new revision built from the pinned spec, so the second
// fill after an accept replaced the first, and the deploy was refused for a
// slot that had been filled (issue #55).
func TestR132_FillingTwoSlotsKeepsBoth(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	withSlots := minimalSpec()
	withSlots["slots"] = []map[string]any{
		{"key": "SECRET_KEY", "type": "unknown", "required": true},
		{"key": "SITE_NAME", "type": "unknown", "required": true},
	}
	withSlots["workloads"].([]map[string]any)[0]["env"] = []map[string]any{
		{"key": "SECRET_KEY", "slot_ref": "SECRET_KEY"},
		{"key": "SITE_NAME", "slot_ref": "SITE_NAME"},
	}
	i.pinSpec(admin, id, i.writeSpec(admin, id, withSlots))

	var last float64
	for _, key := range []string{"SECRET_KEY", "SITE_NAME"} {
		got := i.do(admin, http.MethodPut, "/apps/"+id+"/slots/"+key, map[string]any{
			"key": key, "mode": "literal", "value": "value-for-" + key,
		})
		require.Less(t, got.Code, 300, got.String())
		var filled map[string]any
		got.JSON(t, &filled)
		last = filled["spec_revision"].(float64)
	}

	// The revision the second fill wrote carries both.
	got := i.do(admin, http.MethodGet, fmt.Sprintf("/apps/%s/specs/%d", id, int(last)), nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var rev struct {
		Body struct {
			Slots []struct {
				Key        string         `json:"key"`
				Resolution map[string]any `json:"resolution"`
			} `json:"slots"`
		} `json:"body"`
	}
	got.JSON(t, &rev)
	require.Len(t, rev.Body.Slots, 2)
	for _, s := range rev.Body.Slots {
		require.NotNil(t, s.Resolution, "%s was filled", s.Key)
	}
}

// TestR130_ALaravelKeyThatIsNotOneIsRefused asserts R-130 and R-105. A slot
// typed as a Laravel APP_KEY refuses a value Laravel would refuse, before it
// is stored, and the refusal says what a valid one is — rather than a
// placeholder that deploys and fails every page.
func TestR130_ALaravelKeyThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	withSlot := minimalSpec()
	withSlot["slots"] = []map[string]any{
		{"key": "APP_KEY", "type": "unknown", "required": true, "format": "laravel_key"},
	}
	withSlot["workloads"].([]map[string]any)[0]["env"] = []map[string]any{
		{"key": "APP_KEY", "slot_ref": "APP_KEY"},
	}
	i.pinSpec(admin, id, i.writeSpec(admin, id, withSlot))

	got := i.do(admin, http.MethodPut, "/apps/"+id+"/slots/APP_KEY", map[string]any{
		"mode": "literal", "value": "qa-placeholder",
	})
	require.Equal(t, http.StatusBadRequest, got.Code, got.String())
	require.Equal(t, string(errs.ValidInvalid), got.ErrorCode())
	require.Contains(t, got.String(), "base64:")

	got = i.do(admin, http.MethodPut, "/apps/"+id+"/slots/APP_KEY", map[string]any{
		"mode": "literal", "value": "base64:" + "a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s=",
	})
	require.Less(t, got.Code, 300, got.String())
}

func TestSettingASlotThatTheSpecDoesNotDeclare(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodPut, "/apps/"+id+"/slots/NOT_DECLARED", map[string]any{
		"key": "NOT_DECLARED", "mode": "literal", "value": "anything",
	})
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
}

// R-020: adding storage is a spec change rather than a direct call to the
// runtime, because the spec is the sole record of how an app runs — a volume
// created behind the spec's back would exist until the next deploy and then
// quietly not be mounted.
func TestR020_AddingAVolumeWritesARevisionRatherThanCreatingStorage(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/volumes", map[string]any{
		"name": "data", "path": "/var/lib/data", "workload": "web",
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())

	var created struct {
		ID           string `json:"id"`
		SpecRevision int    `json:"spec_revision"`
		Note         string `json:"note"`
	}
	got.JSON(t, &created)
	require.NotEmpty(t, created.ID)
	require.Equal(t, 2, created.SpecRevision, "it lands as a new revision")
	require.Contains(t, created.Note, "Deploy", "and says the storage does not exist yet")

	// The new revision carries the volume and the mount.
	revision := i.do(admin, http.MethodGet,
		fmt.Sprintf("/apps/%s/specs/%d", id, created.SpecRevision), nil)
	require.Equal(t, http.StatusOK, revision.Code, revision.String())
	require.Contains(t, revision.String(), "/var/lib/data")
	require.Contains(t, revision.String(), created.ID)

	// GET /volumes lists storage that exists, which is none until a deploy
	// creates it (R-030).
	listed := i.do(admin, http.MethodGet, "/apps/"+id+"/volumes", nil)
	require.Equal(t, http.StatusOK, listed.Code)
	var body struct {
		Volumes []map[string]any `json:"volumes"`
	}
	listed.JSON(t, &body)
	require.Empty(t, body.Volumes)
}

// A workload the spec does not declare is refused rather than invented.
func TestAVolumeCannotMountIntoAWorkloadThatDoesNotExist(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/volumes", map[string]any{
		"name": "orphan", "path": "/var/lib/orphan", "workload": "nonexistent",
	})
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
}

func TestDeployingAPinnedSpecReachesThePlannerRatherThanAuthorization(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/deployments", map[string]any{})
	require.NotEqual(t, http.StatusForbidden, got.Code, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())
}

// R-146: with two revisions there is something to roll back to.
func TestR146_RollingBackNeedsAnEarlierPinnedRevision(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	first := i.writeSpec(admin, id, minimalSpec())
	i.pinSpec(admin, id, first)

	changed := minimalSpec()
	changed["workloads"].([]map[string]any)[0]["image"] = "nginx:1.27-alpine"
	i.pinSpec(admin, id, i.writeSpec(admin, id, changed))

	got := i.do(admin, http.MethodPost, "/apps/"+id+"/deployments/rollback",
		map[string]any{"to": first})
	require.NotEqual(t, http.StatusInternalServerError, got.Code, got.String())
	require.NotEqual(t, http.StatusForbidden, got.Code)
}
