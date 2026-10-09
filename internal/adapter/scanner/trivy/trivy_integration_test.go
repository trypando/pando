//go:build integration

package trivy_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/scanner/trivy"
)

func adapter(t *testing.T) *trivy.Adapter {
	t.Helper()
	a := trivy.New()
	require.NoError(t, a.Configure(context.Background(), nil))
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	return a
}

// TestR311_ScanningAnImageReportsWhatIsInIt asserts the half of the score that
// comes from what the app ships with.
//
// Against a real image and a real scanner, because the thing worth testing is
// the handoff: Pando saves the image and copies it into a container that has no
// socket, no app volume and no app network, and reads a report back.
func TestR311_ScanningAnImageReportsWhatIsInIt(t *testing.T) {
	ctx := context.Background()
	a := adapter(t)

	// An old base image, chosen because it reliably has findings. A scan that
	// returns nothing proves nothing about a scanner.
	const target = "alpine:3.17"
	pull(t, target)

	result, err := a.Scan(ctx, api.ScanRequest{AppID: "app_test", SpecID: "spec_test", Image: target})
	require.NoError(t, err)
	require.Contains(t, result.Scanner, "trivy")
	require.False(t, result.Ran.IsZero())
	require.NotEmpty(t, result.Findings, "an unpatched 3.17 base image has known vulnerabilities")

	for _, f := range result.Findings {
		require.NotEmpty(t, f.ID)
		require.Contains(t,
			[]api.Severity{api.SeverityCritical, api.SeverityHigh, api.SeverityMedium, api.SeverityLow, api.SeverityUnknown},
			f.Severity, "every finding carries Pando's own severity, never the scanner's string")
	}
}

// TestR311_ScanningSourceFindsWhatNeverReachesAnImage asserts the other half:
// a secret committed to a repository is not in the built image's package list
// and is exactly what somebody wants to know about.
func TestR311_ScanningSourceFindsWhatNeverReachesAnImage(t *testing.T) {
	ctx := context.Background()
	a := adapter(t)

	dir := t.TempDir()

	// Assembled rather than written out. The fixture has to look enough like a
	// live key for the scanner's rule to fire, and a string that looks like one
	// in a source file is a string GitHub's push protection refuses — which it
	// did, on this test, which is the rule working. Nothing here is a
	// credential: it is the shape of one.
	fake := "sk_" + "live_" + strings.Repeat("A", 10) + "bCdEfGhIjKlMnOpQrStU"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.py"),
		[]byte("STRIPE_KEY = \""+fake+"\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "huge"), make([]byte, 1<<20), 0o600))

	result, err := a.Scan(ctx, api.ScanRequest{AppID: "app_test", SourceDir: dir})
	require.NoError(t, err)

	var secrets int
	for _, f := range result.Findings {
		if f.Target != "" && f.ID != "" {
			secrets++
		}
	}
	require.NotZero(t, secrets, "a committed key is a finding")
}

// Nothing to scan is a refusal, not an empty pass. An app with no image and no
// checkout has not been found clean.
func TestNothingToScanIsRefused(t *testing.T) {
	a := adapter(t)
	_, err := a.Scan(context.Background(), api.ScanRequest{AppID: "app_test"})
	require.Error(t, err)
}

func pull(t *testing.T, ref string) {
	t.Helper()
	// Pulled through the same daemon the adapter reads from, so the test does
	// not depend on what happens to be cached.
	out, err := exec.Command("docker", "pull", ref).CombinedOutput()
	require.NoError(t, err, "pulling %s: %s", ref, out)
}
