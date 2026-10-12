package security_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/security"
)

// TestR311_AScoreIsTheUnionOfTheImageAndTheSource asserts R-311 for a scan
// taken in two parts — the source while the image builds, the image once it
// exists: the score is of both. A finding only the source holds (a committed
// key) and one only the image holds (its base image's OS packages) both count,
// and one both report counts once, as a scanner given both at once reports it.
func TestR311_AScoreIsTheUnionOfTheImageAndTheSource(t *testing.T) {
	t.Parallel()
	early, late := time.Unix(1700000000, 0).UTC(), time.Unix(1700000100, 0).UTC()
	shared := api.Finding{ID: "CVE-2025-9232", Severity: api.SeverityHigh, Target: "alpine 3.21.3"}
	image := api.ScanResult{Scanner: "trivy 0.60", Ran: late, Findings: []api.Finding{
		{ID: "CVE-2025-9230", Severity: api.SeverityCritical, Target: "alpine 3.21.3"},
		shared,
	}}
	source := api.ScanResult{Scanner: "trivy 0.60", Ran: early, Findings: []api.Finding{
		{ID: "aws-access-key-id", Severity: api.SeverityCritical, Target: "js/application.js"},
		shared,
		// The same ID at another target is another finding.
		{ID: "CVE-2025-9232", Severity: api.SeverityHigh, Target: "package-lock.json"},
	}}

	got := security.Union(image, source)
	ids := make([]string, 0, len(got.Findings))
	for _, f := range got.Findings {
		ids = append(ids, f.ID+"@"+f.Target)
	}
	require.Equal(t, []string{
		"CVE-2025-9230@alpine 3.21.3",
		"CVE-2025-9232@alpine 3.21.3",
		"aws-access-key-id@js/application.js",
		"CVE-2025-9232@package-lock.json",
	}, ids)
	require.Equal(t, "trivy 0.60", got.Scanner)
	require.Equal(t, late, got.Ran, "when the last part finished")
	require.Equal(t, 100-2*25-2*10, security.Score(got.Findings))

	require.Empty(t, security.Union().Findings)
}
