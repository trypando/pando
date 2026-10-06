package oci

import (
	"context"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// CredentialStore keeps an app's sealed registry credential fields
// (state.NewRegistryCredentials).
type CredentialStore interface {
	Put(ctx context.Context, appID, field string, v secret.Value) error
	Delete(ctx context.Context, appID, field string) error
	Resolve(ctx context.Context, appID string) (map[string]secret.Value, error)
}

// Images reads an app's image with the app's own credential, and keeps that
// credential. It is what detection, the planner, the deployer and the API
// share, so all of them see the same registry with the same identity.
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

// CredentialSummary is what may be said about an app's credential without
// revealing it: which kind, and the parts of it that are names rather than
// secrets.
type CredentialSummary struct {
	Kind        CredentialKind `json:"kind"`
	Username    string         `json:"username,omitempty"`
	AccessKeyID string         `json:"access_key_id,omitempty"`
	Region      string         `json:"region,omitempty"`
}

// SaveCredential replaces the app's registry credential. Every field the
// previous one stored is removed first, so switching from a token to AWS keys
// leaves no token behind.
func (im *Images) SaveCredential(ctx context.Context, appID string, c Credential) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if im == nil || im.Credentials == nil {
		return errs.New(errs.StateInvalid, "Pando cannot store registry credentials on this installation.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	if err := im.RemoveCredential(ctx, appID); err != nil {
		return err
	}
	for field, v := range c.Fields() {
		if err := im.Credentials.Put(ctx, appID, field, v); err != nil {
			return err
		}
	}
	return nil
}

// RemoveCredential deletes the app's registry credential. Removing one that
// is not there is not an error.
func (im *Images) RemoveCredential(ctx context.Context, appID string) error {
	if im == nil || im.Credentials == nil {
		return nil
	}
	for _, field := range CredentialFields {
		if err := im.Credentials.Delete(ctx, appID, field); err != nil {
			return err
		}
	}
	return nil
}

// DescribeCredential says whether the app has a credential and of which kind.
// False when it has none.
func (im *Images) DescribeCredential(ctx context.Context, appID string) (CredentialSummary, bool, error) {
	if im == nil || im.Credentials == nil {
		return CredentialSummary{}, false, nil
	}
	fields, err := im.Credentials.Resolve(ctx, appID)
	if err != nil {
		return CredentialSummary{}, false, err
	}
	c, ok := CredentialFrom(fields)
	if !ok {
		return CredentialSummary{}, false, nil
	}
	return CredentialSummary{Kind: c.Kind, Username: c.Username, AccessKeyID: c.AccessKeyID, Region: c.Region}, true, nil
}

// Reference is what a spec's image source pulls: pinned when the revision
// recorded a digest (R-120's rule, applied to images), the tag otherwise.
func Reference(src spec.Source) string {
	if src.Type != spec.SourceImage {
		return ""
	}
	return Pin(src.Image, src.Digest)
}
