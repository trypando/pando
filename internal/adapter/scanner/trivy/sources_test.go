package trivy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

func configured(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	a := New()
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

func TestSourceFlagsAreEmptyByDefault(t *testing.T) {
	require.Empty(t, configured(t, Config{}).sourceFlags(),
		"an unset setting passes no flag, so Trivy's own default applies")
}

func TestSourceFlagsPassEachSetting(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"db repository list", Config{DBRepository: " registry.example.com/trivy-db:2 , ghcr.io/aquasecurity/trivy-db:2,, "},
			[]string{"--db-repository", "registry.example.com/trivy-db:2,ghcr.io/aquasecurity/trivy-db:2"}},
		{"java db repository", Config{JavaDBRepository: "registry.example.com/trivy-java-db:1"},
			[]string{"--java-db-repository", "registry.example.com/trivy-java-db:1"}},
		{"checks bundle repository", Config{ChecksBundleRepository: "registry.example.com/trivy-checks:1"},
			[]string{"--checks-bundle-repository", "registry.example.com/trivy-checks:1"}},
		{"offline scan", Config{OfflineScan: true}, []string{"--offline-scan"}},
		{"blank list", Config{DBRepository: " , "}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, configured(t, c.cfg).sourceFlags())
		})
	}
}

func TestConfigureRefusesARepositoryWithAScheme(t *testing.T) {
	err := New().Configure(context.Background(), json.RawMessage(`{"db_repository":"https://registry.example.com/trivy-db:2"}`))
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	require.Equal(t, errs.ValidInvalid, e.Code)
	require.Contains(t, e.Message, "vulnerability database repository")
	require.NotEmpty(t, e.Remedy)
}

func TestConfigureRefusesAChecksBundleList(t *testing.T) {
	err := New().Configure(context.Background(), json.RawMessage(`{"checks_bundle_repository":"a.example.com/c:1,b.example.com/c:1"}`))
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	require.Equal(t, errs.ValidInvalid, e.Code)
}

// Stderr as Trivy 0.74 prints it when the vulnerability database cannot be
// fetched (pkg/commands/artifact/run.go wrapping pkg/db/db.go and
// pkg/oci/artifact.go).
const dbDownloadStderr = `2026-10-18T12:00:00Z	FATAL	Fatal error	run error: init error: DB error: failed to download vulnerability DB: OCI artifact error: failed to download vulnerability DB: failed to download artifact from any source: 2 errors occurred:
	* failed to download artifact from mirror.gcr.io/aquasec/trivy-db:2: OCI repository error: Get "https://mirror.gcr.io/v2/": dial tcp: lookup mirror.gcr.io: no such host
	* failed to download artifact from ghcr.io/aquasecurity/trivy-db:2: OCI repository error: Get "https://ghcr.io/v2/": dial tcp: lookup ghcr.io: no such host`

func TestR105_DatabaseDownloadFailureNamesTheSetting(t *testing.T) {
	err := configured(t, Config{}).downloadFailure(1, dbDownloadStderr)
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	require.Equal(t, errs.AdapterFailed, e.Code)
	require.Contains(t, e.Message, "could not download its vulnerability database")
	require.Contains(t, e.Message, trivyDBRepository, "the message says where Trivy tried")
	require.Contains(t, e.Remedy, `"Vulnerability database repository" (db_repository)`)
}

func TestR105_DatabaseDownloadFailureNamesTheConfiguredMirror(t *testing.T) {
	a := configured(t, Config{JavaDBRepository: "registry.example.com/trivy-java-db:1"})
	err := a.downloadFailure(1, "FATAL\tFatal error\trun error: Java DB error: failed to download Java DB: OCI artifact error")
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	require.Contains(t, e.Message, "its Java database")
	require.Contains(t, e.Message, "registry.example.com/trivy-java-db:1")
	require.NotContains(t, e.Message, "ghcr.io")
	require.Contains(t, e.Remedy, "java_db_repository")
}

func TestR105_ChecksBundleDownloadFailureNamesTheSetting(t *testing.T) {
	err := configured(t, Config{}).downloadFailure(1, "failed to download checks bundle: OCI artifact error")
	require.Error(t, err)
	require.Contains(t, err.Error(), "misconfiguration checks")
}

func TestOtherScannerFailuresAreNotMappedToADownload(t *testing.T) {
	require.NoError(t, configured(t, Config{}).downloadFailure(1, "FATAL\tFatal error\tscan error: unable to open /scan/image.tar"))
}

func TestInfoListsTheSourceSettingsAsAdvanced(t *testing.T) {
	keys := map[string]string{}
	for _, f := range Info().Fields {
		require.True(t, f.Advanced, f.Key)
		require.NotEmpty(t, f.Default, f.Key)
		keys[f.Key] = f.Default
	}
	require.Equal(t, trivyDBRepository, keys["db_repository"])
	require.Equal(t, trivyJavaDBRepository, keys["java_db_repository"])
	require.Equal(t, trivyChecksRepository, keys["checks_bundle_repository"])
	require.Contains(t, keys, "offline_scan")
}
