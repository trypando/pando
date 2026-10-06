package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// RegistryCredentials keeps the credential an image app's image is pulled
// with (oci.Images).
type RegistryCredentials interface {
	SaveCredential(ctx context.Context, appID string, c oci.Credential) error
	RemoveCredential(ctx context.Context, appID string) error
	DescribeCredential(ctx context.Context, appID string) (oci.CredentialSummary, bool, error)
}

// registryCredentialRef is what an image source records in its
// credential_ref when the app has a registry credential, so the spec says the
// pull is authenticated (R-020) without saying with what.
const registryCredentialRef = "registry"

// handleGetRegistryCredential says whether the app has a registry credential.
// Names only — the username or access key ID — never the password or secret
// key, which nothing reads back (R-194).
func (s *Server) handleGetRegistryCredential(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	if s.Images == nil {
		JSON(w, http.StatusOK, map[string]any{"set": false})
		return
	}
	summary, set, err := s.Images.DescribeCredential(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !set {
		JSON(w, http.StatusOK, map[string]any{"set": false})
		return
	}
	JSON(w, http.StatusOK, map[string]any{"set": true, "credential": summary})
}

// handlePutRegistryCredential sets the credential the app's image is pulled
// with (issue #41). app.secrets.write, as for any credential the app holds.
//
// App-owned, as O-3 decided for source credentials: it keeps working after
// the person who supplied it leaves. Who supplied it is the audit event's
// principal, which is where §21's offboarding looks for it.
func (s *Server) handlePutRegistryCredential(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSecretsWrite)
	if !ok {
		return
	}
	var c oci.Credential
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read as a registry credential.").
			WithRemedy(`Send {"kind": "basic", "username": "…", "password": "…"} or {"kind": "ecr", "access_key_id": "…", "secret_access_key": "…"}.`))
		return
	}
	if !s.saveRegistryCredential(w, r, app.ID, app.Source, c) {
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

// saveRegistryCredential stores c for an app, records on the app's source that
// its pulls are authenticated, and audits who supplied it.
func (s *Server) saveRegistryCredential(w http.ResponseWriter, r *http.Request, appID string, src spec.Source, c oci.Credential) bool {
	if s.Images == nil {
		Error(w, r, errs.New(errs.StateInvalid, "Pando cannot store registry credentials on this installation.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again."))
		return false
	}
	if src.Type != spec.SourceImage {
		Error(w, r, errs.New(errs.ValidInvalid,
			"A registry credential is for an app that runs a prebuilt image, and this app is built from source.").
			WithRemedy("Create the app from an image to use a registry credential."))
		return false
	}
	if err := s.Images.SaveCredential(r.Context(), appID, c); err != nil {
		Error(w, r, err)
		return false
	}
	if src.CredentialRef != registryCredentialRef {
		src.CredentialRef = registryCredentialRef
		if err := s.Apps.SetSource(r.Context(), appID, src); err != nil {
			Error(w, r, err)
			return false
		}
	}

	// The kind is audited; the credential is not, and cannot be — its secret
	// parts are secret.Value everywhere they travel.
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.registry_credential.write", AppID: appID, TargetKind: "app", TargetID: appID,
		Detail: map[string]any{"kind": string(c.Kind)},
	})
	return true
}

func (s *Server) handleDeleteRegistryCredential(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSecretsWrite)
	if !ok {
		return
	}
	if s.Images != nil {
		if err := s.Images.RemoveCredential(r.Context(), app.ID); err != nil {
			Error(w, r, err)
			return
		}
	}
	if app.Source.CredentialRef != "" {
		src := app.Source
		src.CredentialRef = ""
		if err := s.Apps.SetSource(r.Context(), app.ID, src); err != nil {
			Error(w, r, err)
			return
		}
	}
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.registry_credential.delete", AppID: app.ID, TargetKind: "app", TargetID: app.ID,
	})
	JSON(w, http.StatusNoContent, nil)
}
