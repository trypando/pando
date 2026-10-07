package imageregistry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR271_EveryStartupRegistryFieldIsFixedAndSaysWhereItWasSet asserts R-271
// field by field: each field the startup configuration sets is in effect,
// listed as fixed with its value (never the password's) and where it was set,
// and refused when a change would move it.
func TestR271_EveryStartupRegistryFieldIsFixedAndSaysWhereItWasSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &memStore{stored: state.StoredRegistry{URL: "https://stored.internal", Username: "stored",
		Kind: "basic", Layout: "per_app"}, have: true, password: secret.New("stored-pw")}
	env := func(name string) Source { return Source{Kind: "env", Name: name} }
	s := &Service{
		Startup: Config{URL: "http://startup.internal:5000/pando", Username: "pando", Password: secret.New("startup-pw"),
			Kind: "basic", Layout: "single", Insecure: true, Always: true},
		Fixed: map[string]Source{
			FieldURL:      env("PANDO_REGISTRY_URL"),
			FieldUsername: env("PANDO_REGISTRY_USERNAME"),
			FieldPassword: env("PANDO_REGISTRY_PASSWORD"),
			FieldKind:     env("PANDO_REGISTRY_KIND"),
			FieldLayout:   {Kind: "file", Name: "/etc/pando/pando.yaml", Key: "registry.layout"},
			FieldInsecure: {Kind: "file", Name: "/etc/pando/pando.yaml", Key: "registry.insecure"},
			FieldAlways:   {Kind: "flag"},
		},
		Store: store,
	}

	v, err := s.Describe(ctx)
	require.NoError(t, err)
	require.True(t, v.Configured)
	require.True(t, v.PasswordSet)
	values := map[string]any{}
	for _, f := range v.Fixed {
		values[f.Key] = f.Value
	}
	require.Equal(t, map[string]any{
		FieldURL: "http://startup.internal:5000/pando", FieldUsername: "pando", FieldPassword: nil,
		FieldKind: "basic", FieldLayout: "single", FieldInsecure: true, FieldAlways: true,
	}, values, "every fixed field shows its value, except the password")

	reg, err := s.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, "startup.internal:5000", reg.Host())
	require.True(t, reg.Always())
	auth, err := reg.Auth(ctx)
	require.NoError(t, err)
	require.Equal(t, "startup-pw", auth.Password.Reveal(), "the startup password, not the stored one")
	planned, err := s.CurrentRegistry(ctx)
	require.NoError(t, err)
	require.NotNil(t, planned)

	_, err = s.Update(ctx, Change{Insecure: ptr(false)}, "usr_1")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "config file /etc/pando/pando.yaml, key registry.insecure")
	_, err = s.Update(ctx, Change{Always: ptr(false)}, "usr_1")
	require.Contains(t, errs.As(err).Message, "(startup configuration)")
	_, err = s.Update(ctx, Change{Username: ptr("someone")}, "usr_1")
	require.Contains(t, errs.As(err).Message, "environment variable PANDO_REGISTRY_USERNAME")

	_, err = s.Update(ctx, Change{Insecure: ptr(true), Always: ptr(true), Layout: ptr("single")}, "usr_1")
	require.NoError(t, err, "the values already in effect are not changes")
	require.Equal(t, []string{FieldLayout, FieldInsecure, FieldAlways},
		Change{Insecure: ptr(true), Always: ptr(true), Layout: ptr("single")}.Changed())
}

// failingStore is a stored registry whose every call fails with err, or only
// the calls named in only.
type failingStore struct {
	memStore
	err  error
	only map[string]bool
}

func (f *failingStore) fails(call string) bool { return f.only == nil || f.only[call] }

func (f *failingStore) Load(ctx context.Context) (state.StoredRegistry, bool, error) {
	if f.fails("load") {
		return state.StoredRegistry{}, false, f.err
	}
	return f.memStore.Load(ctx)
}
func (f *failingStore) Save(ctx context.Context, r state.StoredRegistry, by string) error {
	if f.fails("save") {
		return f.err
	}
	return f.memStore.Save(ctx, r, by)
}
func (f *failingStore) Clear(ctx context.Context) error {
	if f.fails("clear") {
		return f.err
	}
	return f.memStore.Clear(ctx)
}
func (f *failingStore) SetPassword(ctx context.Context, v secret.Value) error {
	if f.fails("set_password") {
		return f.err
	}
	return f.memStore.SetPassword(ctx, v)
}
func (f *failingStore) Password(ctx context.Context) (secret.Value, bool, error) {
	if f.fails("password") {
		return secret.Value{}, false, f.err
	}
	return f.memStore.Password(ctx)
}

// TestTheRegistrySettingsPassOnAStoreThatCannotBeRead asserts that every
// path through the service reports the store's error rather than answering as
// though no registry were configured — which would send a build somewhere it
// was never meant to go, or nowhere.
func TestTheRegistrySettingsPassOnAStoreThatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gone := errs.New(errs.Internal, "Could not read the install registry.")
	configured := memStore{stored: state.StoredRegistry{URL: "https://registry.internal"}, have: true}

	everything := &Service{Store: &failingStore{memStore: configured, err: gone}}
	_, err := everything.Current(ctx)
	require.ErrorIs(t, err, gone)
	_, err = everything.CurrentRegistry(ctx)
	require.ErrorIs(t, err, gone)
	_, err = everything.Describe(ctx)
	require.ErrorIs(t, err, gone)
	_, err = everything.Update(ctx, Change{Always: ptr(true)}, "usr_1")
	require.ErrorIs(t, err, gone)
	_, err = everything.Clear(ctx)
	require.ErrorIs(t, err, gone)

	for _, call := range []string{"password", "save", "set_password"} {
		s := &Service{Store: &failingStore{memStore: configured, err: gone, only: map[string]bool{call: true}}}
		_, err := s.Update(ctx, Change{Username: ptr("pando"), Password: ptr(secret.New("pw"))}, "usr_1")
		require.ErrorIs(t, err, gone, call)
	}

	var none *Service
	reg, err := none.Current(ctx)
	require.NoError(t, err)
	require.Nil(t, reg, "no service is no registry")
	_, err = (&Service{}).Clear(ctx)
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.False(t, errors.Is(err, gone))
}
