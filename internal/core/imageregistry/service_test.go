package imageregistry

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// memStore is the stored registry without a database.
type memStore struct {
	stored   state.StoredRegistry
	have     bool
	password secret.Value
}

func (m *memStore) Load(context.Context) (state.StoredRegistry, bool, error) {
	return m.stored, m.have, nil
}
func (m *memStore) Save(_ context.Context, r state.StoredRegistry, by string) error {
	r.UpdatedBy = by
	m.stored, m.have = r, true
	return nil
}
func (m *memStore) Clear(context.Context) error {
	m.stored, m.have, m.password = state.StoredRegistry{}, false, secret.Value{}
	return nil
}
func (m *memStore) SetPassword(_ context.Context, v secret.Value) error { m.password = v; return nil }
func (m *memStore) Password(context.Context) (secret.Value, bool, error) {
	return m.password, !m.password.IsZero(), nil
}

func ptr[T any](v T) *T { return &v }

// TestR271_StartupRegistrySettingsWinAndAreShownFixed asserts R-271 for the
// install registry: a field set in the startup configuration wins over the
// stored one, is listed as fixed with where it was set, and cannot be changed
// through the API except to the value it already has. Fields the startup
// configuration leaves alone are the stored ones.
func TestR271_StartupRegistrySettingsWinAndAreShownFixed(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	s := &Service{
		Startup: Config{URL: "https://startup.internal:5000"},
		Fixed:   map[string]Source{FieldURL: {Kind: "env", Name: "PANDO_REGISTRY_URL"}},
		Store:   store,
	}

	v, err := s.Update(ctx, Change{Username: ptr("pando"), Password: ptr(secret.New("stored-pw")), Always: ptr(true)}, "usr_1")
	require.NoError(t, err)
	require.Equal(t, "https://startup.internal:5000", v.URL)
	require.Equal(t, "pando", v.Username)
	require.True(t, v.Always)
	require.True(t, v.PasswordSet)
	require.Equal(t, []Fixed{{Key: FieldURL, Value: "https://startup.internal:5000",
		Source: Source{Kind: "env", Name: "PANDO_REGISTRY_URL"}}}, v.Fixed)

	_, err = s.Update(ctx, Change{URL: ptr("https://other.internal")}, "usr_1")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_URL")
	_, err = s.Update(ctx, Change{URL: ptr("https://startup.internal:5000")}, "usr_1")
	require.NoError(t, err, "sending back the startup value is not a change")

	reg, err := s.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, "startup.internal:5000", reg.Host())
	auth, err := reg.Auth(ctx)
	require.NoError(t, err)
	require.Equal(t, "stored-pw", auth.Password.Reveal())

	// A password fixed at startup cannot be replaced from here.
	s.Fixed[FieldPassword] = Source{Kind: "env", Name: "PANDO_REGISTRY_PASSWORD"}
	s.Startup.Password = secret.New("startup-pw")
	_, err = s.Update(ctx, Change{Password: ptr(secret.New("x"))}, "usr_1")
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_PASSWORD")
	reg, err = s.Current(ctx)
	require.NoError(t, err)
	auth, err = reg.Auth(ctx)
	require.NoError(t, err)
	require.Equal(t, "startup-pw", auth.Password.Reveal())

	// Clearing leaves the startup fields in effect.
	v, err = s.Clear(ctx)
	require.NoError(t, err)
	require.True(t, v.Configured)
	require.Empty(t, v.Username)
}

// TestR194_TheStoredRegistryPasswordIsNeverShown asserts R-194: the view
// says whether a password is set and never holds it, whatever it is
// marshaled as, and a change that would leave a registry Pando cannot use is
// refused before anything is written.
func TestR194_TheStoredRegistryPasswordIsNeverShown(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	s := &Service{Fixed: map[string]Source{}, Store: store}

	v, err := s.Update(ctx, Change{URL: ptr("https://registry.internal"), Username: ptr("pando"),
		Password: ptr(secret.New("hunter2-registry"))}, "usr_1")
	require.NoError(t, err)
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "hunter2-registry")
	require.True(t, v.PasswordSet)

	_, err = s.Update(ctx, Change{URL: ptr("http://registry.internal")}, "usr_1")
	require.Contains(t, errs.As(err).Message, "PANDO_REGISTRY_INSECURE", "refused, and nothing written")
	require.Equal(t, "https://registry.internal", store.stored.URL)

	_, err = s.Update(ctx, Change{URL: ptr("https://pando:hunter2@registry.internal")}, "usr_1")
	require.Error(t, err, "a URL may not carry a credential (R-190)")
	require.NotContains(t, errs.As(err).Message, "hunter2")

	_, err = s.Update(ctx, Change{Password: ptr(secret.New(""))}, "usr_1")
	require.Contains(t, errs.As(err).Message, "needs both a username and a password")
	v, err = s.Update(ctx, Change{Username: ptr(""), Password: ptr(secret.New(""))}, "usr_1")
	require.NoError(t, err)
	require.False(t, v.PasswordSet, "an empty password removes it")

	none, err := (&Service{}).Describe(ctx)
	require.NoError(t, err)
	require.False(t, none.Configured)
	_, err = (&Service{}).Update(ctx, Change{URL: ptr("https://x")}, "usr_1")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
}
