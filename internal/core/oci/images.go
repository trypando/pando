package oci

import (
	"context"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

// CredentialStore reads an app's sealed registry credential fields
// (state.NewRegistryCredentials).
type CredentialStore interface {
	Resolve(ctx context.Context, appID string) (map[string]secret.Value, error)
}

// Images reads an app's image with the app's own credential. It is what
// detection, the planner and the deployer share, so all three see the same
// registry with the same identity.
type Images struct {
	Inspector   Inspector
	Resolver    Resolver
	Credentials CredentialStore
}

// Auth resolves the app's credential for reference. Nil, with no error, when
// the app has none and the pull is anonymous.
func (im *Images) Auth(ctx context.Context, appID, reference string) (*Auth, error) {
	if im == nil || im.Credentials == nil || appID == "" {
		return nil, nil
	}
	fields, err := im.Credentials.Resolve(ctx, appID)
	if err != nil {
		return nil, err
	}
	c, ok := CredentialFrom(fields)
	if !ok {
		return nil, nil
	}
	return im.Resolver.Resolve(ctx, c, reference)
}

// Inspect reads reference as the app would pull it.
func (im *Images) Inspect(ctx context.Context, appID, reference string, want Platform) (Inspection, error) {
	auth, err := im.Auth(ctx, appID, reference)
	if err != nil {
		return Inspection{}, err
	}
	return im.Inspector.Inspect(ctx, reference, auth, want)
}

// Reference is what a spec's image source pulls: pinned when the revision
// recorded a digest (R-120's rule, applied to images), the tag otherwise.
func Reference(src spec.Source) string {
	if src.Type != spec.SourceImage {
		return ""
	}
	return Pin(src.Image, src.Digest)
}
