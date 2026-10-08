package imageregistry_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	registryoci "github.com/trypando/pando/internal/adapter/imageregistry/oci"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type rows []state.AdapterConfig

func (r rows) List(context.Context) ([]state.AdapterConfig, error) { return r, nil }

type creds map[string]map[string]secret.Value

func (c creds) Resolve(_ context.Context, id string) (map[string]secret.Value, error) {
	return c[id], nil
}

func oci(kind string) api.ImageRegistryAdapter {
	if kind == registryoci.Kind {
		return registryoci.New()
	}
	return nil
}

func row(id, url string, isDefault bool) state.AdapterConfig {
	return state.AdapterConfig{ID: id, Category: string(api.CategoryImageRegistry), Kind: registryoci.Kind,
		Name: id, Enabled: true, IsDefault: isDefault, Config: json.RawMessage(`{"url":"` + url + `"}`)}
}

func declared(t *testing.T, url string) imageregistry.Declared {
	t.Helper()
	a := registryoci.New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"url":"`+url+`"}`)))
	return imageregistry.Declared{ID: "image_registry", Adapter: a, IsDefault: true}
}

// TestR252_WhichImageRegistryBuildsArePushedTo asserts how core chooses the
// install's registry among its image registry adapters (issue #153): none is
// none; one is that one; the default among several; a declared default over a
// stored one (R-271); and several with no default is a refusal that says so,
// never a silent pick.
func TestR252_WhichImageRegistryBuildsArePushedTo(t *testing.T) {
	ctx := context.Background()
	host := func(s *imageregistry.Service) string {
		r, err := s.Current(ctx)
		require.NoError(t, err)
		return r.Host()
	}

	none, err := (&imageregistry.Service{Configs: rows{}, New: oci}).Current(ctx)
	require.NoError(t, err)
	require.Nil(t, none)
	require.False(t, none.Configured())

	off := row("reg_off", "https://off.internal", false)
	off.Enabled = false
	require.Equal(t, "one.internal", host(&imageregistry.Service{Configs: rows{off, row("reg_one", "https://one.internal", false)}, New: oci}))
	require.Equal(t, "b.internal", host(&imageregistry.Service{Configs: rows{
		row("reg_a", "https://a.internal", false), row("reg_b", "https://b.internal", true)}, New: oci}))
	require.Equal(t, "env.internal", host(&imageregistry.Service{Configs: rows{row("reg_b", "https://b.internal", true)},
		New: oci, Declared: []imageregistry.Declared{declared(t, "https://env.internal")}}))

	_, err = (&imageregistry.Service{Configs: rows{
		row("reg_a", "https://a.internal", false), row("reg_b", "https://b.internal", false)}, New: oci}).Current(ctx)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "default")
}

// TestR105_AStoredRegistryThatCannotBeBuiltSaysWhy asserts that a stored
// registry the adapter refuses is an error naming it and the adapter's reason
// — never "no registry", which would send builds nowhere — and that its
// password is opened from the credential store, not the row.
func TestR105_AStoredRegistryThatCannotBeBuiltSaysWhy(t *testing.T) {
	ctx := context.Background()
	half := row("reg_half", "https://registry.internal", false)
	half.Config = json.RawMessage(`{"url":"https://registry.internal","username":"pando"}`)
	_, err := (&imageregistry.Service{Configs: rows{half}, New: oci}).Current(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, `"reg_half"`)
	require.Contains(t, errs.As(err).Message, "both a username and a password")

	s := &imageregistry.Service{Configs: rows{half}, New: oci,
		Credentials: creds{"reg_half": {"password": secret.New("pw")}}}
	r, err := s.Current(ctx)
	require.NoError(t, err)
	auth, err := r.Auth(ctx)
	require.NoError(t, err)
	require.Equal(t, "pw", auth.Password.Reveal())

	unknown := row("reg_x", "https://registry.internal", false)
	unknown.Kind = "nexus"
	_, err = (&imageregistry.Service{Configs: rows{unknown}, New: oci}).Current(ctx)
	require.Contains(t, errs.As(err).Message, "no image registry adapter of kind nexus")

	// Validate checks what would be saved, beside the stored credential.
	require.NoError(t, s.Validate(ctx, "reg_half", "oci", half.Config, nil))
	require.Error(t, s.Validate(ctx, "reg_half", "oci", half.Config, map[string]secret.Value{"password": {}}),
		"an empty credential removes the stored one, leaving half")
}
