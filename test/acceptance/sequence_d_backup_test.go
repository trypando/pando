//go:build integration

package acceptance_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Sequence D — disaster recovery.
//
// The flow nobody tests until they need it, which is why it is one of the four.
// Phase 9's Done when: Sequence D passes, including rejection of a tampered
// bundle **with the target untouched**. Not mostly untouched.

const bundlePassphrase = "a-long-backup-passphrase-for-tests"

func takeBackup(t *testing.T, c *client) string {
	t.Helper()
	body, status := c.do(t, http.MethodPost, "/backups",
		fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase))
	require.Equal(t, http.StatusCreated, status, body)

	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &rec))
	return rec["id"].(string)
}

// TestR212_ADRBundleContainsWhatItPromises asserts the bundle is the whole
// install, not a database dump with a hopeful name.
//
// A bundle missing the secrets key restores an install holding every app's
// ciphertext and nothing that opens it — which looks like success right up
// until an app starts.
func TestR212_ADRBundleContainsWhatItPromises(t *testing.T) {
	admin := login(t)
	id := takeBackup(t, admin)

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/verify",
		fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase))
	require.Equal(t, http.StatusOK, status, body)

	var out struct {
		Verified bool `json:"verified"`
		Manifest struct {
			Entries []struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
			} `json:"entries"`
			Counts        map[string]int `json:"counts"`
			SchemaVersion int            `json:"schema_version"`
		} `json:"manifest"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.True(t, out.Verified)

	names := map[string]bool{}
	for _, e := range out.Manifest.Entries {
		names[e.Name] = true
		require.Len(t, e.SHA256, 64, "every entry carries a checksum, or verification proves nothing")
	}
	require.True(t, names["postgres.dump"], "the state store")
	require.True(t, names["secrets.key"], "R-212: without this a restore cannot read any app's secrets")
	require.True(t, names["token.key"], "R-212, R-063: without this a restore cannot check any API token")
	require.True(t, names["adapters.json"])
	require.True(t, names["policy.json"])

	// Counts, so an operator at a restore prompt can tell whether this is the
	// install they think it is. Checksums only prove the bytes arrived.
	require.Positive(t, out.Manifest.Counts["users"])
	require.Positive(t, out.Manifest.SchemaVersion)
}

// TestR214_AWrongPassphraseRevealsNothing asserts the accepted cost of R-213.
//
// It must also fail *as a passphrase problem*. Reporting it as a damaged bundle
// sends an operator to hunt for an older backup when the one in their hand is
// perfectly good — which, in a disaster, is the difference between a bad hour
// and a bad week.
func TestR214_AWrongPassphraseRevealsNothing(t *testing.T) {
	admin := login(t)
	id := takeBackup(t, admin)

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/verify",
		`{"passphrase":"not the passphrase"}`)
	require.Equal(t, http.StatusUnprocessableEntity, status, body)
	require.Contains(t, body, "BACKUP_DECRYPT_FAILED")

	// Nothing about the contents leaks — no entry names, no counts, no sizes.
	require.NotContains(t, body, "postgres.dump")
	require.NotContains(t, body, "secrets.key")
	require.NotContains(t, body, "manifest")
}

// TestR215_RestoreRefusesWithoutConfirmation asserts Sequence D step 3.
//
// Restoring replaces every app, account and secret. A caller who has not said
// so explicitly is a caller who meant to verify.
func TestR215_RestoreRefusesWithoutConfirmation(t *testing.T) {
	admin := login(t)
	id := takeBackup(t, admin)

	// A witness created after the backup. If the restore ran, it would be gone.
	witness := uniqueName("witness")
	admin.createUser(t, witness)

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/restore",
		fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase))
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "confirm")

	require.True(t, userExists(t, admin, witness),
		"an unconfirmed restore must not touch the installation")
}

// TestR215_AWrongPassphraseLeavesTheInstallUntouched is the first half of the
// Done when: rejected, with the target untouched.
func TestR215_AWrongPassphraseLeavesTheInstallUntouched(t *testing.T) {
	admin := login(t)
	id := takeBackup(t, admin)

	witness := uniqueName("witness")
	admin.createUser(t, witness)

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/restore",
		`{"passphrase":"not the passphrase","confirm":true}`)
	require.Equal(t, http.StatusUnprocessableEntity, status, body)
	require.Contains(t, body, "BACKUP_DECRYPT_FAILED")

	// Confirmed, and still nothing happened: decryption is step 1 and the
	// install is not touched until step 4.
	require.True(t, userExists(t, admin, witness),
		"a restore that failed to decrypt must leave the installation exactly as it was")
}

// TestR215_ATamperedBundleIsRejectedWithTheTargetUntouched is the assertion
// phase 9's Done when names explicitly.
//
// The bundle is damaged at the destination — the way a real one is, by the
// storage under it — and the restore must refuse at verification rather than
// discover the problem while applying.
func TestR215_ATamperedBundleIsRejectedWithTheTargetUntouched(t *testing.T) {
	admin := login(t)
	id := takeBackup(t, admin)

	witness := uniqueName("witness")
	admin.createUser(t, witness)

	corruptStoredBundle(t, id)

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/restore",
		fmt.Sprintf(`{"passphrase":%q,"confirm":true}`, bundlePassphrase))
	require.Equal(t, http.StatusUnprocessableEntity, status, body)

	require.True(t, userExists(t, admin, witness),
		"a tampered bundle must be rejected with the target untouched — not mostly untouched")

	// And verify says so too, so an operator can find this out before the
	// disaster rather than during it (R-216).
	body, status = admin.do(t, http.MethodPost, "/backups/"+id+"/verify",
		fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase))
	require.NotEqual(t, http.StatusOK, status, body)
}

// TestR212_RestoreReturnsTheInstallToItsBackedUpState is the happy path, and
// the one that matters: the bundle has to actually work.
func TestR212_RestoreReturnsTheInstallToItsBackedUpState(t *testing.T) {
	admin := login(t)

	// Present at backup time.
	before := uniqueName("before")
	admin.createUser(t, before)

	id := takeBackup(t, admin)

	// Created afterwards, so it must not survive.
	after := uniqueName("after")
	admin.createUser(t, after)
	require.True(t, userExists(t, admin, after))

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/restore",
		fmt.Sprintf(`{"passphrase":%q,"confirm":true}`, bundlePassphrase))
	require.Equal(t, http.StatusOK, status, body)

	require.True(t, userExists(t, admin, before), "an account present at backup time must come back")
	require.False(t, userExists(t, admin, after), "an account created after the backup must not survive it")

	// The bundle survives being used. It contains no record of itself — a
	// backup cannot — so without the handler putting the row back, restoring
	// once would make the bundle unlistable and unrestorable.
	body, status = admin.do(t, http.MethodPost, "/backups/"+id+"/restore",
		fmt.Sprintf(`{"passphrase":%q,"confirm":true}`, bundlePassphrase))
	require.Equal(t, http.StatusOK, status, body)
}

// TestR227_TheRestoreIsRecordedInTheInstallItProduced asserts the audit trail
// survives into the restored install.
//
// It has to land in the restored database rather than the one being replaced,
// or the only record of the most destructive action in the system is in the
// state that action threw away.
func TestR227_TheRestoreIsRecordedInTheInstallItProduced(t *testing.T) {
	admin := login(t)
	id := takeBackup(t, admin)

	body, status := admin.do(t, http.MethodPost, "/backups/"+id+"/restore",
		fmt.Sprintf(`{"passphrase":%q,"confirm":true}`, bundlePassphrase))
	require.Equal(t, http.StatusOK, status, body)

	log := admin.get(t, "/audit?action=backup.restore")
	events, ok := log["events"].([]any)
	require.True(t, ok, "%v", log)
	require.NotEmpty(t, events, "the restore must be recorded in the installation it produced")
}

// TestR080_BackupsRequireTheirOwnVerb asserts install.backup.manage gates all
// four routes.
//
// Its own verb rather than folded into policy management: a restore replaces
// the whole install, and handing that to everyone who can edit a source
// allowlist is not the same trust.
func TestR080_BackupsRequireTheirOwnVerb(t *testing.T) {
	admin := login(t)
	outsider := admin.asUser(t, admin.createUser(t, uniqueName("kim")))
	id := takeBackup(t, admin)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/backups", ""},
		{http.MethodPost, "/backups", fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase)},
		{http.MethodPost, "/backups/" + id + "/verify", fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase)},
		{http.MethodPost, "/backups/" + id + "/restore", fmt.Sprintf(`{"passphrase":%q,"confirm":true}`, bundlePassphrase)},
	} {
		body, status := outsider.do(t, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, status, "%s %s: %s", tc.method, tc.path, body)
		require.Contains(t, body, "install.backup.manage")
	}
}

// TestR213_AShortPassphraseIsRefused — the only rule, and it is length.
//
// No composition requirements: they produce shorter, more guessable secrets and
// a note on a monitor. This one guards every secret in the installation and
// there is no recovery path, so the floor is higher than a password's.
func TestR213_AShortPassphraseIsRefused(t *testing.T) {
	admin := login(t)

	body, status := admin.do(t, http.MethodPost, "/backups", `{"passphrase":"short"}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "16 characters")
	require.Contains(t, body, "never stores", "the message has to say the passphrase is unrecoverable")
}

// --- helpers ---------------------------------------------------------------

func userExists(t *testing.T, c *client, username string) bool {
	t.Helper()
	body, status := c.do(t, http.MethodGet, "/users", "")
	require.Equal(t, http.StatusOK, status, body)

	var out struct {
		Users []struct {
			ExternalID string `json:"external_id"`
		} `json:"users"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	for _, u := range out.Users {
		if u.ExternalID == username {
			return true
		}
	}
	return false
}

// corruptStoredBundle flips bytes in the middle of a bundle at its destination.
//
// In the middle rather than at the end, so this is not the truncation case in
// the unit tests: a bundle whose length is right and whose contents are not is
// what silent storage corruption actually looks like.
func corruptStoredBundle(t *testing.T, id string) {
	t.Helper()

	// The bundle lives in the pando container's local destination.
	out, err := composeExec("pando", "sh", "-c",
		fmt.Sprintf("f=/var/lib/pando/backups/%s; "+
			"size=$(wc -c < $f); "+
			"dd if=/dev/urandom of=$f bs=1 seek=$((size/2)) count=64 conv=notrunc 2>/dev/null && echo ok", id))
	require.NoError(t, err, "could not damage the stored bundle: %s", out)
	require.Contains(t, out, "ok", out)
}

func composeExec(service string, args ...string) (string, error) {
	full := append([]string{"compose", "exec", "-T", service}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestR204_DeletingAnAppKeepsAFinalBackup asserts the promise R-204 makes and
// nothing kept: on delete, Pando asks whether to keep a final copy, and the
// copy it keeps is kept until explicitly discarded.
//
// Until now the answer to backup=true was "not available yet" — an excuse left
// over from phase 3 that was still there in phase 10, pointing at runtime
// adapters that had long since shipped. So the only way to delete an app with
// storage was to discard it.
func TestR204_DeletingAnAppKeepsAFinalBackup(t *testing.T) {
	admin := login(t)
	app := deployedAppWithStorage(t, admin, "ondelete-"+stamp())

	before := len(listBackups(t, admin))

	body, status := admin.do(t, http.MethodDelete, "/apps/"+app+"?backup=true", "")
	require.Equal(t, http.StatusNoContent, status, body)

	after := listBackups(t, admin)
	require.Len(t, after, before+1, "deleting an app with storage must leave a backup behind")

	var kept map[string]any
	for _, b := range after {
		if b["app_id"] == app {
			kept = b
		}
	}
	require.NotNil(t, kept, "the backup must name the app it came from")
	require.Equal(t, "on_delete", kept["kind"])

	// R-204: kept until explicitly discarded, never aged out. The schema
	// refuses to give an on_delete row an expiry, and this is the assertion
	// that would notice if somebody set one anyway.
	require.Nil(t, kept["retain_until"],
		"a final backup is kept until discarded, not aged out")

	// And the record outlives the app, which is the whole reason backups.app_id
	// is ON DELETE SET NULL rather than CASCADE.
	_, status = admin.do(t, http.MethodGet, "/apps/"+app, "")
	require.Equal(t, http.StatusNotFound, status, "the app is gone")
}

func listBackups(t *testing.T, c *client) []map[string]any {
	t.Helper()
	out := c.get(t, "/backups")
	raw, _ := out["backups"].([]any)

	list := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		if m, ok := b.(map[string]any); ok {
			list = append(list, m)
		}
	}
	return list
}

// deployedAppWithStorage deploys an app that has a volume, which is what makes
// R-204 apply at all — an app with no storage has nothing to keep.
func deployedAppWithStorage(t *testing.T, c *client, name string) string {
	t.Helper()

	app := c.createApp(t, name)
	c.putSpec(t, app, fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:1.27-alpine"},
		"build": {"strategy": "prebuilt"},
		"volumes": [{"id": "vol_data", "name": "data", "declared": "user"}],
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"image": "nginx:1.27-alpine",
			"mounts": [{"volume_id": "vol_data", "path": "/data"}],
			"ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": %d},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, 9400+time.Now().Second()%50))
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 0)
	final := c.awaitDeployment(t, app, dep["id"].(string), 10*time.Minute)
	require.Equal(t, "succeeded", final["status"], c.deploymentLogs(t, app, dep["id"].(string)))
	return app
}

// TestR030_AnAppsStorageIsRecordedWhenItDeploys asserts that Pando knows about
// the storage it created.
//
// It did not, for ten phases. The spec declared volumes, the planner planned
// them and the runtime created them — and the `volumes` table stayed empty. So
// R-204's ON DELETE RESTRICT guarded no rows, deleting an app never offered to
// keep its data because it counted none, and **a DR bundle contained the
// database and no app data at all**. Everything downstream reads this table.
func TestR030_AnAppsStorageIsRecordedWhenItDeploys(t *testing.T) {
	admin := login(t)
	app := deployedAppWithStorage(t, admin, "volrec-"+stamp())

	// Through the API rather than the database, so this asserts what a client
	// can actually see.
	out := admin.get(t, "/apps/"+app)
	require.Equal(t, "running", out["state"])

	// The DR bundle is the thing that was quietly empty, so that is what this
	// checks: a full backup taken now must contain this app's volume.
	body, status := admin.do(t, http.MethodPost, "/backups",
		fmt.Sprintf(`{"passphrase":%q}`, bundlePassphrase))
	require.Equal(t, http.StatusCreated, status, body)

	var rec struct {
		Manifest struct {
			Entries []struct {
				Name string `json:"name"`
			} `json:"entries"`
			Counts map[string]int `json:"counts"`
		} `json:"manifest"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rec))

	require.Positive(t, rec.Manifest.Counts["volumes"],
		"a DR bundle must contain the app volumes it claims to (R-212)")

	var hasVolume bool
	for _, e := range rec.Manifest.Entries {
		if strings.HasPrefix(e.Name, "volumes/") {
			hasVolume = true
		}
	}
	require.True(t, hasVolume, "and the volume's data must actually be in the bundle")
}

// TestR206_AnAppIsRestoredFromItsOwnBackup asserts R-206.
//
// The half of backup that is actually reached for. R-210 says the scope of a
// per-app backup is "recover from a recent mistake", and a copy nobody can put
// back is not a recovery path — it is a file. This restores one app's data in
// place, without touching anything else on the install.
func TestR206_AnAppIsRestoredFromItsOwnBackup(t *testing.T) {
	admin := login(t)
	app := deployedAppWithStorage(t, admin, "restore-app-"+stamp())

	// Something to lose, then a copy of it.
	writeInto(t, app, "/data/keep.txt", "before")

	body, status := admin.do(t, http.MethodPost, "/backups",
		fmt.Sprintf(`{"kind":"rolling","app_id":%q}`, app))
	require.Equal(t, http.StatusCreated, status, body)

	var made map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &made))
	backupID := made["id"].(string)

	// The mistake.
	writeInto(t, app, "/data/keep.txt", "after the mistake")

	// Restoring replaces data, so it is confirmed. A restore that happens
	// because a request arrived is a restore that happens by accident.
	body, status = admin.do(t, http.MethodPost, "/apps/"+app+"/restore",
		fmt.Sprintf(`{"backup_id":%q}`, backupID))
	require.GreaterOrEqual(t, status, 400, "restoring without confirming must be refused: %s", body)

	body, status = admin.do(t, http.MethodPost, "/apps/"+app+"/restore",
		fmt.Sprintf(`{"backup_id":%q,"confirm":true}`, backupID))
	require.Equal(t, http.StatusOK, status, body)

	require.Equal(t, "before", strings.TrimSpace(readFrom(t, app, "/data/keep.txt")),
		"the app's data was not put back")
}

// A backup restores only to the app it came from.
//
// The alternative is one mistyped identifier overwriting a different app's
// database with this one's, which is the most destructive single request the
// API could accept.
func TestR206_ABackupRestoresOnlyToItsOwnApp(t *testing.T) {
	admin := login(t)

	source := deployedAppWithStorage(t, admin, "restore-src-"+stamp())
	other := deployedAppWithStorage(t, admin, "restore-other-"+stamp())

	body, status := admin.do(t, http.MethodPost, "/backups",
		fmt.Sprintf(`{"kind":"rolling","app_id":%q}`, source))
	require.Equal(t, http.StatusCreated, status, body)

	var made map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &made))

	body, status = admin.do(t, http.MethodPost, "/apps/"+other+"/restore",
		fmt.Sprintf(`{"backup_id":%q,"confirm":true}`, made["id"].(string)))
	require.GreaterOrEqual(t, status, 400,
		"a backup must not restore into a different app: %s", body)
}

// TestR216_ABackupIsVerifiableBeforeItIsNeeded asserts R-216.
//
// A backup nobody has checked is a backup nobody knows they have. Verification
// touches nothing, which is the property that makes it a separate call rather
// than a flag on restore — a flag is a thing somebody passes wrongly, and the
// wrong value here overwrites an install.
func TestR216_ABackupIsVerifiableBeforeItIsNeeded(t *testing.T) {
	admin := login(t)
	app := deployedAppWithStorage(t, admin, "verify-"+stamp())
	writeInto(t, app, "/data/keep.txt", "intact")

	body, status := admin.do(t, http.MethodPost, "/backups",
		fmt.Sprintf(`{"kind":"rolling","app_id":%q}`, app))
	require.Equal(t, http.StatusCreated, status, body)

	var made map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &made))

	backupID := made["id"].(string)

	body, status = admin.do(t, http.MethodPost, "/backups/"+backupID+"/verify", `{}`)
	require.Equal(t, http.StatusOK, status, body)

	// And the app is untouched by having been checked.
	require.Equal(t, "intact", strings.TrimSpace(readFrom(t, app, "/data/keep.txt")))

	// An app's backup is not an installation. The install-wide restore route
	// refuses it by name rather than failing to decrypt it, which would answer
	// "that passphrase is wrong" about a bundle that never had one — on the
	// most destructive route in the API.
	body, status = admin.do(t, http.MethodPost, "/backups/"+backupID+"/restore",
		`{"passphrase":"whatever-sixteen-plus","confirm":true}`)
	require.GreaterOrEqual(t, status, 400, body)
	require.NotContains(t, body, "BACKUP_DECRYPT_FAILED",
		"the refusal should name the mismatch, not the passphrase: %s", body)
}

// TestR217_TheBackupDestinationIsAnAdapter asserts R-217.
//
// Backup is the eighth adapter category, not a hard-coded directory. The
// evidence is that it is configured like every other category and reports its
// own retention ownership — which is what decides whether Pando prunes or the
// store does.
func TestR217_TheBackupDestinationIsAnAdapter(t *testing.T) {
	admin := login(t)

	raw := admin.get(t, "/adapters")
	list, _ := raw["adapters"].([]any)
	require.NotEmpty(t, list)

	found := false
	for _, a := range list {
		row, ok := a.(map[string]any)
		if ok && row["category"] == "backup" {
			found = true
		}
	}
	require.True(t, found, "the backup destination is configured as an adapter like any other")
}

// TestR211_AnAppsRetentionIsCarriedOnItsBackups asserts R-211.
//
// "Seven retained" has to mean seven. Retention is recorded on the row at the
// moment the copy is taken, because a count evaluated later against a policy
// that has since changed prunes a different set than the one anybody chose.
func TestR211_AnAppsRetentionIsCarriedOnItsBackups(t *testing.T) {
	admin := login(t)
	app := deployedAppWithStorage(t, admin, "rolling-"+stamp())

	body, status := admin.do(t, http.MethodPost, "/backups",
		fmt.Sprintf(`{"kind":"rolling","app_id":%q,"retain_days":7}`, app))
	require.Equal(t, http.StatusCreated, status, body)

	var made map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &made))
	require.Equal(t, "rolling", made["kind"])
	require.NotNil(t, made["retain_until"],
		"a rolling backup ages out; that is what distinguishes it from the copy kept at delete")
}

// writeInto puts a file inside a running app, through Docker.
//
// A test probe rather than a second client: the assertion is about bytes on a
// volume, and routing it through /exec would be testing the websocket.
func writeInto(t *testing.T, appID, path, content string) {
	t.Helper()
	inApp(t, appID, "web", "sh", "-c", fmt.Sprintf("printf %%s %q > %s", content, path))
}

func readFrom(t *testing.T, appID, path string) string {
	t.Helper()
	return inApp(t, appID, "web", "cat", path)
}
