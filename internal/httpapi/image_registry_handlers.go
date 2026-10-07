package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/errs"
)

// ImageRegistry is the install's image registry (issue #72, PR 5), as
// *imageregistry.Service serves it: the startup configuration laid over what
// was stored here.
type ImageRegistry interface {
	Describe(ctx context.Context) (imageregistry.View, error)
	Update(ctx context.Context, ch imageregistry.Change, by string) (imageregistry.View, error)
	Clear(ctx context.Context) (imageregistry.View, error)
}

// handleGetImageRegistry reports the registry in effect: each setting, which
// are fixed by the startup configuration and where, and whether a password is
// set — never the password (R-194). install.view, like GET /config.
func (s *Server) handleGetImageRegistry(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	if s.ImageRegistry == nil {
		JSON(w, http.StatusOK, imageregistry.View{Kind: "basic", Layout: "per_app", Fixed: []imageregistry.Fixed{}})
		return
	}
	v, err := s.ImageRegistry.Describe(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, v)
}

// handlePutImageRegistry changes the stored registry. install.adapters.manage:
// the registry is infrastructure Pando's runtimes and builder use, as an
// adapter is. The password is sealed by the secrets adapter (R-190) and every
// replica uses it at its next push or pull.
func (s *Server) handlePutImageRegistry(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if s.ImageRegistry == nil {
		Error(w, r, errs.New(errs.StateInvalid, "Pando cannot store the install registry's settings on this installation."))
		return
	}
	var ch imageregistry.Change
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		// The decoder's own message is not passed on: it can quote the body,
		// and the body may hold the password.
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read as registry settings.").
			WithRemedy(`Send JSON such as {"url": "https://registry.internal:5000", "username": "pando", "password": "…"}. Every field is optional; one left out is unchanged.`))
		return
	}
	v, err := s.ImageRegistry.Update(r.Context(), ch, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	// Which fields changed, never their values: the URL and username are not
	// secret, but one audit shape for every field keeps the password out by
	// construction.
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "install.registry.update", TargetKind: "registry", TargetID: "install",
		Detail: map[string]any{"fields": ch.Changed()},
	})
	JSON(w, http.StatusOK, v)
}

// handleDeleteImageRegistry removes the stored registry and its password.
// Fields fixed at startup stay as they are.
func (s *Server) handleDeleteImageRegistry(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if s.ImageRegistry == nil {
		JSON(w, http.StatusNoContent, nil)
		return
	}
	if _, err := s.ImageRegistry.Clear(r.Context()); err != nil {
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "install.registry.clear", TargetKind: "registry", TargetID: "install",
	})
	JSON(w, http.StatusNoContent, nil)
}
