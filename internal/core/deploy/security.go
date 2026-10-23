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

// scanTarget is what a deploy's scan reads: the image the app runs, by digest,
// with the credential to pull it, and the checkout it came from, named by its
// commit or digest (R-311).
type scanTarget struct {
	Image     string
	Pull      *api.RegistryAuth
	SourceDir string
	Commit    string
}

func (r *Runner) scan(ctx context.Context, dep state.Deployment, appSpec *spec.AppSpec, target scanTarget, sink io.Writer) error {
	image, commit := target.Image, target.Commit
	if r.security == nil {
		return nil
	}
	if _, configured := r.security.Configured(); !configured {
		return nil
	}

	// Scanned once per source and image, not once per deploy (R-312). A deploy
	// of a source and image already scanned together by this scanner uses that
	// scan, attached to this revision: a score that moved with how often
	// somebody pressed deploy would describe the button rather than the app.
	//
	// Both, not the source alone (R-311). Detection scans the source before
	// anything is built, and that scan covers no image — reusing it for the
	// first deploy left the image that runs unscanned, and an app on a base
	// image with critical findings showed a clean score. The image is named by
	// its digest, so a rebuild that produced a different image is scanned. The
	// threshold below is checked either way.
	if reused, found, err := r.security.Reuse(ctx, dep.AppID, dep.SpecID, commit, image); err == nil && found {
		unchanged := "source unchanged"
		if image != "" {
			unchanged = "source and image unchanged"
		}
		fmt.Fprintf(sink, "=> Using the security scan of %s from %s (%s)\n",
			describeSource(appSpec, commit), reused.RanAt.UTC().Format(time.RFC3339), unchanged)
		return r.allowed(ctx, dep, reused.Findings)
	}

	fmt.Fprintln(sink, "=> Scanning for known vulnerabilities")

	scanned, err := r.security.Scan(ctx, api.ScanRequest{
		AppID:     dep.AppID,
		SpecID:    dep.SpecID,
		Commit:    commit,
		Image:     image,
		SourceDir: target.SourceDir,

		// A published image is fetched with the app's registry credential,
		// and a build pushed to the install's registry with the registry's
		// (issue #72): neither is necessarily on the scanner's host.
		PullAuth: firstAuth(target.Pull, r.builtImageAuth(ctx, appSpec, image)),
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

func firstAuth(auths ...*api.RegistryAuth) *api.RegistryAuth {
	for _, a := range auths {
		if a != nil {
			return a
		}
	}
	return nil
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
