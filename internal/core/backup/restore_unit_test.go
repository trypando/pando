package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The restore path of a DR bundle, without Postgres: a bundle built with the
// package's own writer, encrypted, stored on the local destination, and
// restored through Restore. The database step is exercised in its own test.

// volumesOnly is a state source that knows the install's volumes and nothing
// else a restore reads.
type volumesOnly struct {
	StateSource
	refs []VolumeRef
}

func (v volumesOnly) VolumesToSnapshot(context.Context) ([]VolumeRef, error) { return v.refs, nil }

type bundleEntry struct{ name, body string }

// storeBundle writes a DR bundle with the given entries to the destination.
func storeBundle(t *testing.T, s *Service, name string, schema uint, entries ...bundleEntry) {
	t.Helper()
	var plain bytes.Buffer
	w := NewWriter(&plain, "dr_bundle", "test", schema)
	for _, e := range entries {
		require.NoError(t, w.Add(e.name, int64(len(e.body)), strings.NewReader(e.body)))
	}
	_, err := w.Finish()
	require.NoError(t, err)

	dest, _, err := s.destination("")
	require.NoError(t, err)
	out, err := dest.Writer(context.Background(), name)
	require.NoError(t, err)
	require.NoError(t, Encrypt(out, &plain, secret.New("correct horse battery staple")))
	require.NoError(t, out.Close())
}

func restoring(t *testing.T) (*Service, *volumeRuntime) {
	t.Helper()
	s, rt := appBackups(t)
	s.State = volumesOnly{refs: []VolumeRef{{VolumeID: "vol_a", AdapterRef: "rt_docker", Handle: "pando-app1-vol_a"}}}
	return s, rt
}

var passphrase = secret.New("correct horse battery staple")

// TestR212_ARestorePutsBackTheKeyAndTheData asserts what a restore applies
// without a database in the bundle: the secrets key, each volume into the
// runtime the install says holds it, and the readable copies left alone.
func TestR212_ARestorePutsBackTheKeyAndTheData(t *testing.T) {
	s, rt := restoring(t)
	storeBundle(t, s, "dr_1", 1,
		bundleEntry{SecretsKey, "restored-key"},
		bundleEntry{AdaptersName, "[]"},
		bundleEntry{PolicyName, "{}"},
		bundleEntry{VolumesPrefix + "vol_a.tar", "uploads from the backup"},
	)

	got, err := s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1",
		Passphrase: passphrase, Confirm: true})
	require.NoError(t, err)
	require.Equal(t, 1, got.VolumesApplied)
	require.Equal(t, "uploads from the backup", string(rt.volumes["pando-app1-vol_a"]))

	key, err := os.ReadFile(s.SecretsKeyPath)
	require.NoError(t, err)
	require.Equal(t, "restored-key", string(key))
	info, err := os.Stat(s.SecretsKeyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the key to every secret is not world-readable")
}

// TestR215_ARestoreThatFailsAnEarlyStepTouchesNothing asserts R-215 and R-214.
func TestR215_ARestoreThatFailsAnEarlyStepTouchesNothing(t *testing.T) {
	s, rt := restoring(t)
	storeBundle(t, s, "dr_1", 1, bundleEntry{VolumesPrefix + "vol_a.tar", "from the backup"})
	before := string(rt.volumes["pando-app1-vol_a"])

	_, err := s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1", Passphrase: passphrase})
	require.ErrorContains(t, err, "replaces everything", "not confirmed")

	_, err = s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1",
		Passphrase: secret.New("the wrong one, long enough"), Confirm: true})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, errs.BackupDecryptFailed, e.Code)
	require.Contains(t, e.Remedy, "does not keep backup passphrases")

	storeBundle(t, s, "dr_new", 99, bundleEntry{VolumesPrefix + "vol_a.tar", "x"})
	_, err = s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_new",
		Passphrase: passphrase, Confirm: true})
	require.ErrorContains(t, err, "newer version of Pando")

	_, err = s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_gone", ObjectName: "dr_1", Passphrase: passphrase, Confirm: true})
	require.Error(t, err)
	_, err = s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_missing", Passphrase: passphrase, Confirm: true})
	require.Error(t, err)

	require.Equal(t, before, string(rt.volumes["pando-app1-vol_a"]))
}

func TestDataForStorageTheDatabaseDoesNotKnowIsReported(t *testing.T) {
	s, _ := restoring(t)
	storeBundle(t, s, "dr_1", 1, bundleEntry{VolumesPrefix + "vol_unknown.tar", "orphan"})
	_, err := s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1",
		Passphrase: passphrase, Confirm: true})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "vol_unknown")
	require.Contains(t, e.Remedy, "placed by hand")

	s.State = volumesOnly{refs: []VolumeRef{{VolumeID: "vol_unknown", AdapterRef: "rt_gone"}}}
	_, err = s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1",
		Passphrase: passphrase, Confirm: true})
	require.ErrorContains(t, err, "not configured")
}

func TestAKeyIsNotWrittenWhereThereIsNoLocalKey(t *testing.T) {
	s, _ := restoring(t)
	s.SecretsKeyPath = ""
	require.NoError(t, s.restoreSecretsKey(strings.NewReader("k")), "an external secrets adapter keeps its own")

	s.SecretsKeyPath = filepath.Join(t.TempDir(), "no-such-dir", "secrets.key")
	require.ErrorContains(t, s.restoreSecretsKey(strings.NewReader("k")), "could not write")
}

// TestADatabaseThatCannotBeRestoredSaysTheInstallIsNotReplaced covers the
// database step failing — no reachable database, or no pg_restore at all.
func TestADatabaseThatCannotBeRestoredSaysTheInstallIsNotReplaced(t *testing.T) {
	s, _ := restoring(t)
	s.DatabaseURL = secret.New("postgres://pando:pw@127.0.0.1:1/pando?sslmode=disable")
	storeBundle(t, s, "dr_1", 1, bundleEntry{PostgresName, "not a dump"})
	_, err := s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1",
		Passphrase: passphrase, Confirm: true})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Remedy, "has not been fully replaced")
	require.NotContains(t, e.Message+e.Remedy, "pw@", "the password never reaches a message (R-194)")

	s.DatabaseURL = secret.New("::not a url")
	require.Error(t, s.restoreDatabase(context.Background(), strings.NewReader("")))
}

// TestR212_TwoAppsWithAVolumeOfTheSameNameRestoreToTheirOwnStorage asserts
// that a whole-installation bundle keeps apart two apps that each keep a
// volume called "data" (issue #87).
//
// A volume ID is unique only within its app. Named by volume alone, both
// apps' data went into the bundle as volumes/data.tar, and a restore put both
// into whichever app the database listed first.
func TestR212_TwoAppsWithAVolumeOfTheSameNameRestoreToTheirOwnStorage(t *testing.T) {
	s, rt := restoring(t)
	first := VolumeRef{AppID: "app_1", VolumeID: "data", AdapterRef: "rt_docker", Handle: "pando-app_1-data"}
	second := VolumeRef{AppID: "app_2", VolumeID: "data", AdapterRef: "rt_docker", Handle: "pando-app_2-data"}
	s.State = volumesOnly{refs: []VolumeRef{first, second}}

	require.NotEqual(t, installVolumeEntry(first), installVolumeEntry(second),
		"two apps' volumes are two entries in the bundle")

	storeBundle(t, s, "dr_1", 1,
		bundleEntry{installVolumeEntry(second), "the second app's data"},
		bundleEntry{installVolumeEntry(first), "the first app's data"},
	)
	got, err := s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_1",
		Passphrase: passphrase, Confirm: true})
	require.NoError(t, err)
	require.Equal(t, 2, got.VolumesApplied)
	require.Equal(t, "the first app's data", string(rt.volumes["pando-app_1-data"]))
	require.Equal(t, "the second app's data", string(rt.volumes["pando-app_2-data"]))
}

// A bundle taken before entries were named by app still restores: its entry
// names the volume alone, and is matched on that as it always was.
func TestAVolumeEntryWithoutAnAppStillRestores(t *testing.T) {
	appID, volumeID := parseVolumeEntry(VolumesPrefix + "vol_a.tar")
	require.Empty(t, appID)
	require.Equal(t, "vol_a", volumeID)

	appID, volumeID = parseVolumeEntry(installVolumeEntry(VolumeRef{AppID: "app_1", VolumeID: "vol_a"}))
	require.Equal(t, "app_1", appID)
	require.Equal(t, "vol_a", volumeID)
}

// TestR212_TheTokenKeySurvivesABundleRoundTrip asserts R-212 for the API token
// key (R-063): it goes into a DR bundle beside the secrets key and a restore
// puts it back, 0600. Without it a restored install holds every token's
// HMAC-SHA-256 digest and can check none of them.
func TestR212_TheTokenKeySurvivesABundleRoundTrip(t *testing.T) {
	s, _ := restoring(t)
	s.TokenKeyPath = filepath.Join(t.TempDir(), "token.key")
	tokenKey := []byte("0123456789abcdef0123456789abcdef")
	require.NoError(t, os.WriteFile(s.TokenKeyPath, tokenKey, 0o600))
	secretsKey, err := os.ReadFile(s.SecretsKeyPath)
	require.NoError(t, err)

	var plain bytes.Buffer
	w := NewWriter(&plain, "dr_bundle", "test", 1)
	require.NoError(t, s.addKeys(w))
	manifest, err := w.Finish()
	require.NoError(t, err)
	var names []string
	for _, e := range manifest.Entries {
		names = append(names, e.Name)
	}
	require.ElementsMatch(t, []string{SecretsKey, TokenKey}, names)

	dest, _, err := s.destination("")
	require.NoError(t, err)
	out, err := dest.Writer(context.Background(), "dr_keys")
	require.NoError(t, err)
	require.NoError(t, Encrypt(out, &plain, passphrase))
	require.NoError(t, out.Close())

	// A new machine: neither key is there.
	require.NoError(t, os.Remove(s.TokenKeyPath))
	require.NoError(t, os.Remove(s.SecretsKeyPath))

	_, err = s.Restore(context.Background(), RestoreRequest{AdapterRef: "bk_local", ObjectName: "dr_keys",
		Passphrase: passphrase, Confirm: true})
	require.NoError(t, err)

	got, err := os.ReadFile(s.TokenKeyPath)
	require.NoError(t, err)
	require.Equal(t, tokenKey, got)
	info, err := os.Stat(s.TokenKeyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	got, err = os.ReadFile(s.SecretsKeyPath)
	require.NoError(t, err)
	require.Equal(t, secretsKey, got)
}

func TestABundleWithoutTheTokenKeyItShouldHaveIsNotMade(t *testing.T) {
	s, _ := restoring(t)
	s.TokenKeyPath = filepath.Join(t.TempDir(), "missing.key")
	var plain bytes.Buffer
	require.Error(t, s.addKeys(NewWriter(&plain, "dr_bundle", "test", 1)),
		"every install has a token key, so a missing one is not left out quietly")
}
