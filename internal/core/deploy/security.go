package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

// The security score, in the deploy path (R-312, R-314, design 09 §4.1).
//
// Two things happen here and they are deliberately separate: the scan, which
// produces a fact about what was just built, and the decision, which is host
// policy's. A deploy that is refused was scanned; a deploy on an installation
// with no threshold is scanned too, because the number is worth having before
// anybody sets one.

func (r *Runner) scan(ctx context.Context, dep state.Deployment, appSpec *spec.AppSpec, image, sourceDir, commit string, sink io.Writer) error {
	if r.security == nil {
		return nil
	}
	if _, configured := r.security.Configured(); !configured {
		return nil
	}

	// Scanned once per source, not once per deploy (R-312). Detection scans
	// the source it read, a person can ask for a scan, and a new commit or a
	// new upload gets one here — but a deploy of a source already scanned by
	// this scanner uses that scan, attached to this revision. Redeploying an
	// unchanged source scanned it again every time, and a score that moved
	// with how often somebody pressed deploy would describe the button rather
	// than the app. The threshold below is still checked either way.
	if reused, found, err := r.security.Reuse(ctx, dep.AppID, dep.SpecID, commit); err == nil && found {
		fmt.Fprintf(sink, "=> Using the security scan of %s from %s (source unchanged)\n",
			describeSource(appSpec, commit), reused.RanAt.UTC().Format(time.RFC3339))
		return r.allowed(ctx, dep, reused.Findings)
	}

	fmt.Fprintln(sink, "=> Scanning for known vulnerabilities")

	scanned, err := r.security.Scan(ctx, api.ScanRequest{
		AppID:     dep.AppID,
		SpecID:    dep.SpecID,
		Commit:    commit,
		Image:     image,
		SourceDir: sourceDir,

		// A build pushed to the install's registry is not on the scanner's
		// host; it is fetched with the registry's credential (issue #72).
		PullAuth: r.builtImageAuth(ctx, image),
	}, audit.Event{
		PrincipalKind: audit.KindUser,
		PrincipalID:   dep.CreatedBy,
		OnBehalfOf:    dep.CreatedBy,
	})
	if err != nil {
		// R-318: a scanner that could not run does not block a deploy. The app
		// is not insecure because Pando could not look — it is unscanned, which
		// is recorded, shown, and left to the threshold check below, where an
		// app with an older passing scan still passes.
		fmt.Fprintf(sink, "   The scan did not run: %s\n", messageOf(err))
	} else if scanned.Score != nil {
		counts := security.Count(scanned.Findings)
		fmt.Fprintf(sink, "   Score %d — %d critical, %d high, %d medium, %d low\n",
			*scanned.Score, counts.Critical, counts.High, counts.Medium, counts.Low+counts.Unknown)
	}

	return r.allowed(ctx, dep, scanned.Findings)
}

// describeSource names a scanned source for the deploy log: an upload by what
// it is, since its digest means nothing to the person who sent it, and a
// commit or image digest by its first characters.
func describeSource(appSpec *spec.AppSpec, source string) string {
	if appSpec != nil && appSpec.Source.Type == spec.SourceUpload {
		return "the uploaded source"
	}
	return short(strings.TrimPrefix(source, "sha256:"))
}

// allowed is host policy's decision on the app's standing (R-314): the scan
// just taken, or the one this deploy reused.
func (r *Runner) allowed(ctx context.Context, dep state.Deployment, findings []api.Finding) error {
	standing, err := r.security.Allows(ctx, dep.AppID, dep.SpecID)
	if err != nil {
		return err
	}
	if standing.Deployable() {
		return nil
	}
	return security.Refusal(standing, security.Worst(findings, 3))
}
