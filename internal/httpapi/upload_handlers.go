package httpapi

import (
	"errors"
	"net/http"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// maxUploadBytes bounds an uploaded source archive.
//
// Generous for source and mean for anything else. A repository that does not
// fit in 256 MiB compressed is a repository with build artifacts committed to
// it, and the right answer there is a message saying so rather than a disk
// filling up quietly.
const maxUploadBytes = 256 << 20

// handleUploadSource accepts a gzipped tar as an app's source (design 04 §4).
//
// This is what makes `pando deploy ./` work, which design 04 marks [D] for a
// specific reason: an agent that just generated an app cannot commit and push,
// but it can run a command. Without it, R-262's agent workflow begins by asking
// a person to make a repository.
//
// Gated by app.spec.edit rather than app.deploy. Replacing an app's source is
// changing what the app *is* — a deploy runs what the spec says, and this
// changes what it will say.
func (s *Server) handleUploadSource(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}

	// The allowlist again, before a byte is written (R-092): an install that
	// does not admit uploads refuses this one even for an app that was created
	// before the rule was.
	if s.Policy != nil {
		if err := s.Policy.AllowsSource(r.Context(), spec.Source{Type: spec.SourceUpload, UploadID: app.ID}); err != nil {
			Error(w, r, err)
			return
		}
	}

	// The limit is enforced on the reader, not on Content-Length. A client that
	// lies about the length would otherwise stream until the disk gave out.
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	defer func() { _ = body.Close() }()

	path, err := s.Sources.StoreUpload(app.ID, body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			Error(w, r, errs.Newf(errs.ValidInvalid,
				"That directory is larger than %d MB compressed, which is more than Pando accepts as an upload.",
				maxUploadBytes>>20).
				WithRemedy("Check for build output or a .git directory being included, or deploy from a repository instead."))
			return
		}
		Error(w, r, err)
		return
	}

	// The app's source becomes the upload. Recorded on the app rather than
	// inferred at deploy time, because R-020 makes the spec the sole record of
	// how an app runs — an upload that was not written down is a deploy nobody
	// can explain afterwards.
	if err := s.Apps.SetSource(r.Context(), app.ID, spec.Source{
		Type:     spec.SourceUpload,
		UploadID: app.ID,
	}); err != nil {
		Error(w, r, err)
		return
	}

	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.source.upload", AppID: app.ID, TargetKind: "app", TargetID: app.ID,
	})

	JSON(w, http.StatusOK, map[string]any{
		"app_id": app.ID,
		"stored": path != "",
	})
}
