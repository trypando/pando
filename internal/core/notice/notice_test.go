package notice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

func ptr[T any](v T) *T { return &v }

func rev(n int, body *spec.AppSpec) *state.Revision {
	return &state.Revision{ID: "spec_" + string(rune('0'+n)), Revision: n, Body: body}
}

func withEnv(env ...spec.EnvEntry) *spec.AppSpec {
	return &spec.AppSpec{Workloads: []spec.Workload{{Name: "web", Env: env}}}
}

func keys(ns []Notice) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Key)
	}
	return out
}

func find(t *testing.T, ns []Notice, key string) Notice {
	t.Helper()
	for _, n := range ns {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no notice %q in %v", key, keys(ns))
	return Notice{}
}

func TestNothingToSayIsAnEmptyList(t *testing.T) {
	got := Compute(Inputs{})
	require.NotNil(t, got)
	require.Empty(t, got)

	body := withEnv(spec.EnvEntry{Key: "A", Value: ptr("a")})
	got = Compute(Inputs{Newest: rev(1, body), Pinned: rev(1, body),
		LastBackup: &state.BackupAttempt{Outcome: state.AttemptTaken},
		Standing:   &security.Standing{Verdict: security.VerdictOK}})
	require.Empty(t, got)
}

func TestEveryKindInOverviewOrder(t *testing.T) {
	score := 40
	body := withEnv(spec.EnvEntry{Key: "A", Value: ptr("")})
	body.Warnings = []spec.Warning{
		{Code: "WARN_ONE", Message: "First."},
		{Code: "WARN_TWO", Message: "Gone.", Dismissed: true},
		{Code: "WARN_ONE", Message: "Second."},
	}
	got := Compute(Inputs{
		Newest:     rev(2, nil),
		Pinned:     rev(1, body),
		LastBackup: &state.BackupAttempt{Outcome: state.AttemptFailed, Message: "The backup failed.", Remedy: "Check the disk."},
		Standing:   &security.Standing{Verdict: security.VerdictInsecure, Score: &score, Threshold: 70},
	})
	require.Equal(t, []string{KeyUnshipped, KeySecurity, KeyBackup, KeyUnsetVars, "warning.WARN_ONE"}, keys(got))
	require.Equal(t, "The backup failed. Check the disk.", find(t, got, KeyBackup).Message)
	require.Equal(t, "First. Second.", find(t, got, "warning.WARN_ONE").Message)
	require.Equal(t, "A is declared with no value.", find(t, got, KeyUnsetVars).Message)
	require.Equal(t, "This app's security score is 40, below this installation's requirement of 70. "+
		"Fix the findings its security scan reports and deploy again.", find(t, got, KeySecurity).Message)
	for _, n := range got {
		require.Len(t, n.Fingerprint, 2*fingerprintBytes)
		require.False(t, n.Ignored)
	}
}

func TestUnsetVariablesSkipSecretsSlotsAndRepeats(t *testing.T) {
	body := &spec.AppSpec{Workloads: []spec.Workload{
		{Name: "web", Env: []spec.EnvEntry{
			{Key: "E1", Value: ptr("")}, {Key: "E2"}, {Key: "S", SecretRef: ptr("s")},
			{Key: "L", SlotRef: ptr("db")}, {Key: "V", Value: ptr("v")},
			{Key: "E3"}, {Key: "E4"}, {Key: "E5"}, {Key: "E6"},
		}},
		{Name: "worker", Env: []spec.EnvEntry{{Key: "E1"}}},
	}}
	n, ok := unsetVariables(body)
	require.True(t, ok)
	require.Equal(t, "6 variables are declared with no value: E1, E2, E3, E4, and 2 more.", n.Message)

	_, ok = unsetVariables(withEnv(spec.EnvEntry{Key: "S", SecretRef: ptr("s")}))
	require.False(t, ok)
	_, ok = unsetVariables(nil)
	require.False(t, ok)
}

func TestFingerprintChangesExactlyWhenTheNoticeDoes(t *testing.T) {
	a, _ := unsetVariables(withEnv(spec.EnvEntry{Key: "A"}, spec.EnvEntry{Key: "B"}))
	reordered, _ := unsetVariables(withEnv(spec.EnvEntry{Key: "B"}, spec.EnvEntry{Key: "A"}))
	more, _ := unsetVariables(withEnv(spec.EnvEntry{Key: "A"}, spec.EnvEntry{Key: "B"}, spec.EnvEntry{Key: "C"}))
	require.Equal(t, a.Fingerprint, reordered.Fingerprint, "the same names in another order say the same thing")
	require.NotEqual(t, a.Fingerprint, more.Fingerprint)

	u1, _ := unshipped(rev(2, nil), rev(1, nil))
	u2, _ := unshipped(rev(3, nil), rev(1, nil))
	require.NotEqual(t, u1.Fingerprint, u2.Fingerprint)

	at := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	b1, _ := backupNotTaken(&state.BackupAttempt{AttemptedAt: at, Outcome: state.AttemptFailed})
	b2, _ := backupNotTaken(&state.BackupAttempt{AttemptedAt: at.Add(24 * time.Hour), Outcome: state.AttemptFailed})
	require.NotEqual(t, b1.Fingerprint, b2.Fingerprint, "a later failed backup comes back")
	require.Equal(t, "This app's last scheduled backup, at 2026-10-01T03:00:00Z, was failed.", b1.Message)

	s1, s2 := 40, 41
	sec1, _ := insecure(&security.Standing{Verdict: security.VerdictInsecure, Score: &s1, Threshold: 70})
	sec2, _ := insecure(&security.Standing{Verdict: security.VerdictInsecure, Score: &s2, Threshold: 70})
	require.NotEqual(t, sec1.Fingerprint, sec2.Fingerprint)

	// Facts are separated, so moving a boundary is a different fingerprint.
	require.NotEqual(t, Fingerprint("k", "a", "bc"), Fingerprint("k", "ab", "c"))
}

func TestSecurityNoticeOnlyWhenInsecure(t *testing.T) {
	for _, v := range []security.Verdict{security.VerdictOK, security.VerdictInert, security.VerdictUnscanned} {
		_, ok := insecure(&security.Standing{Verdict: v, Threshold: 70})
		require.False(t, ok, v)
	}
	score := 10
	stop := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	n, ok := insecure(&security.Standing{Verdict: security.VerdictInsecure, Score: &score, Threshold: 70, StopAt: stop})
	require.True(t, ok)
	require.Contains(t, n.Message, "Pando will stop it at 2026-10-30T12:00:00Z unless its score reaches 70.")
}

func TestUnshippedNeedsBothRevisions(t *testing.T) {
	for _, tc := range []struct{ newest, pinned *state.Revision }{
		{nil, rev(1, nil)}, {rev(1, nil), nil}, {rev(1, nil), rev(1, nil)},
	} {
		_, ok := unshipped(tc.newest, tc.pinned)
		require.False(t, ok)
	}
}

// --- the service ------------------------------------------------------------

type fakeRevisions struct {
	revs []state.Revision
	err  error
}

func (f *fakeRevisions) ListRevisions(context.Context, string) ([]state.Revision, error) {
	return f.revs, f.err
}

func (f *fakeRevisions) RevisionByID(_ context.Context, id string) (state.Revision, bool, error) {
	for _, r := range f.revs {
		if r.ID == id {
			return r, true, nil
		}
	}
	return state.Revision{}, false, nil
}

type fakeBackups struct {
	attempts []state.BackupAttempt
	err      error
}

func (f *fakeBackups) Attempts(context.Context, string, int) ([]state.BackupAttempt, error) {
	return f.attempts, f.err
}

type fakeSecurity struct {
	report security.Report
	err    error
}

func (f *fakeSecurity) Report(context.Context, string, string) (security.Report, error) {
	return f.report, f.err
}

type fakeIgnores struct {
	rows map[string]map[string]string // user -> key -> fingerprint
	err  error
}

func (f *fakeIgnores) For(_ context.Context, userID, _ string) (map[string]string, error) {
	return f.rows[userID], f.err
}

func (f *fakeIgnores) Ignore(_ context.Context, userID, _, key, fp string) error {
	if f.err != nil {
		return f.err
	}
	if f.rows[userID] == nil {
		f.rows[userID] = map[string]string{}
	}
	f.rows[userID][key] = fp
	return nil
}

func (f *fakeIgnores) Restore(_ context.Context, userID, _, key string) error {
	delete(f.rows[userID], key)
	return f.err
}

func newService() (*Service, *fakeIgnores) {
	body := withEnv(spec.EnvEntry{Key: "A"})
	ignores := &fakeIgnores{rows: map[string]map[string]string{}}
	return &Service{
		Revisions: &fakeRevisions{revs: []state.Revision{*rev(2, nil), *rev(1, body)}},
		Backups:   &fakeBackups{},
		Security:  &fakeSecurity{},
		Ignores:   ignores,
	}, ignores
}

var app = state.App{ID: "app_1", PinnedSpecID: "spec_1"}

func TestServiceIgnoresPerPersonUntilTheNoticeChanges(t *testing.T) {
	ctx := context.Background()
	s, ignores := newService()

	got, err := s.List(ctx, "usr_a", app)
	require.NoError(t, err)
	require.Equal(t, []string{KeyUnshipped, KeyUnsetVars}, keys(got))

	n, err := s.Ignore(ctx, "usr_a", app, KeyUnsetVars)
	require.NoError(t, err)
	require.True(t, n.Ignored)

	got, _ = s.List(ctx, "usr_a", app)
	require.True(t, find(t, got, KeyUnsetVars).Ignored)
	got, _ = s.List(ctx, "usr_b", app)
	require.False(t, find(t, got, KeyUnsetVars).Ignored, "another person still sees it")
	got, _ = s.List(ctx, "", app)
	require.False(t, find(t, got, KeyUnsetVars).Ignored, "a caller that is not a person sees everything")

	// What it said has changed since.
	ignores.rows["usr_a"][KeyUnsetVars] = "stale"
	got, _ = s.List(ctx, "usr_a", app)
	require.False(t, find(t, got, KeyUnsetVars).Ignored)

	require.NoError(t, s.Restore(ctx, "usr_a", app, KeyUnsetVars))
	require.NotContains(t, ignores.rows["usr_a"], KeyUnsetVars)
}

func TestServiceRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := newService()

	_, err := s.Ignore(ctx, "", app, KeyUnsetVars)
	require.Equal(t, errs.AuthRequired, errs.CodeOf(err))
	require.Equal(t, errs.AuthRequired, errs.CodeOf(s.Restore(ctx, "", app, KeyUnsetVars)))

	_, err = s.Ignore(ctx, "usr_a", app, KeyBackup)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	long := string(make([]byte, 300))
	_, err = s.Ignore(ctx, "usr_a", app, long)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(s.Restore(ctx, "usr_a", app, long)))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(s.Restore(ctx, "usr_a", app, "")))
}

func TestServicePassesOnFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	for name, broken := range map[string]func(*Service){
		"revisions": func(s *Service) { s.Revisions = &fakeRevisions{err: boom} },
		"backups":   func(s *Service) { s.Backups = &fakeBackups{err: boom} },
		"security":  func(s *Service) { s.Security = &fakeSecurity{err: boom} },
		"ignores":   func(s *Service) { s.Ignores = &fakeIgnores{err: boom} },
	} {
		s, _ := newService()
		broken(s)
		_, err := s.List(ctx, "usr_a", app)
		require.ErrorIs(t, err, boom, name)
		_, err = s.Ignore(ctx, "usr_a", app, KeyUnsetVars)
		require.ErrorIs(t, err, boom, name)
	}

	s, _ := newService()
	s.Ignores = &fakeIgnores{rows: map[string]map[string]string{}, err: boom}
	require.ErrorIs(t, s.Restore(ctx, "usr_a", app, KeyUnsetVars), boom)
}

func TestServiceWithoutBackupsOrSecurity(t *testing.T) {
	s, _ := newService()
	s.Backups, s.Security = nil, nil
	s.Revisions = &fakeRevisions{revs: []state.Revision{*rev(1, nil)}}
	got, err := s.List(context.Background(), "usr_a", state.App{ID: "app_1", PinnedSpecID: "spec_missing"})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestServiceReadsTheLastBackupAndTheStanding(t *testing.T) {
	score := 1
	s, _ := newService()
	s.Backups = &fakeBackups{attempts: []state.BackupAttempt{{Outcome: state.AttemptSkipped, Message: "Skipped: no storage."}}}
	s.Security = &fakeSecurity{report: security.Report{Standing: security.Standing{
		Verdict: security.VerdictInsecure, Score: &score, Threshold: 50}}}
	got, err := s.List(context.Background(), "usr_a", app)
	require.NoError(t, err)
	require.Equal(t, []string{KeyUnshipped, KeySecurity, KeyBackup, KeyUnsetVars}, keys(got))
	require.Equal(t, "Skipped: no storage.", find(t, got, KeyBackup).Message)
}

func TestTruncateLongKeys(t *testing.T) {
	require.Equal(t, "short", truncate("short"))
	require.Len(t, []rune(truncate(string(make([]byte, 100)))), 65)
}

// TestR412_TheAppsListCountsOnlyTheNoticesYouHaveNotIgnored asserts R-412:
// the count beside each app in the list is that person's — what they
// ignored is not counted, and everyone else's count is unchanged.
func TestR412_TheAppsListCountsOnlyTheNoticesYouHaveNotIgnored(t *testing.T) {
	ctx := context.Background()
	s, _ := newService()
	other := state.App{ID: "app_2", PinnedSpecID: "spec_1"}

	got, err := s.Counts(ctx, "usr_a", []state.App{app, other})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"app_1": 2, "app_2": 2}, got)

	_, err = s.Ignore(ctx, "usr_a", app, KeyUnsetVars)
	require.NoError(t, err)
	got, err = s.Counts(ctx, "usr_a", []state.App{app})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"app_1": 1}, got)

	got, err = s.Counts(ctx, "usr_b", []state.App{app})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"app_1": 2}, got, "another person's count is their own")
	got, err = s.Counts(ctx, "", []state.App{app})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"app_1": 2}, got, "a caller that is not a person has ignored nothing")

	got, err = s.Counts(ctx, "usr_a", nil)
	require.NoError(t, err)
	require.Empty(t, got)

	s.Revisions = &fakeRevisions{err: errors.New("boom")}
	_, err = s.Counts(ctx, "usr_a", []state.App{app})
	require.Error(t, err)
}

type fakeStandbys struct {
	standing StandbyStanding
	err      error
}

func (f *fakeStandbys) Standby(context.Context, string) (StandbyStanding, error) {
	return f.standing, f.err
}

// TestR422_AnAppWithNoRoomForItsStandbyGetsANotice asserts that an app
// whose standby fits nowhere is told so on its Overview, in the runtime's
// words naming the largest room any host has — a notice, never a blocker —
// and that it comes back when the room changes.
func TestR422_AnAppWithNoRoomForItsStandbyGetsANotice(t *testing.T) {
	s, _ := newService()
	s.Standbys = &fakeStandbys{}
	got, err := s.List(context.Background(), "usr_a", app)
	require.NoError(t, err)
	require.NotContains(t, keys(got), KeyNoStandby, "an app with its standby has nothing to be told")

	unplaced := "This app runs without a standby: it needs 2.00 cores of CPU, 4.0 GB of memory and 0 MB of disk on a host other than b, kept free for when b is lost, and the most any host has is 1.00 cores of CPU, 2.0 GB of memory and 0 MB of disk, on c."
	s.Standbys = &fakeStandbys{standing: StandbyStanding{Unplaced: unplaced, Remedy: "Add a host."}}
	got, err = s.List(context.Background(), "usr_a", app)
	require.NoError(t, err)
	n := find(t, got, KeyNoStandby)
	require.Equal(t, unplaced+" Add a host.", n.Message)
	require.Equal(t, []string{KeyUnshipped, KeyNoStandby, KeyUnsetVars}, keys(got))

	again, _ := noStandby(&StandbyStanding{Unplaced: unplaced + " ", Remedy: "Add a host."})
	require.NotEqual(t, n.Fingerprint, again.Fingerprint, "what it says changed, so an ignore no longer hides it")

	boom := errors.New("boom")
	s.Standbys = &fakeStandbys{err: boom}
	_, err = s.List(context.Background(), "usr_a", app)
	require.ErrorIs(t, err, boom)
}
