package hosts_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type fakeStore struct {
	hosts   []state.Host
	created []state.JoinToken
}

func (f *fakeStore) CreateJoinToken(_ context.Context, roles []string, maxUses int, createdBy string, expiresAt time.Time) (state.IssuedJoinToken, error) {
	t := state.JoinToken{ID: "jt_01TEST", Roles: roles, MaxUses: maxUses, CreatedBy: createdBy, ExpiresAt: expiresAt}
	f.created = append(f.created, t)
	return state.IssuedJoinToken{Token: t, Secret: secret.New("jt_01TEST.c2VjcmV0")}, nil
}

func (f *fakeStore) JoinTokens(context.Context) ([]state.JoinToken, error) { return f.created, nil }

func (f *fakeStore) RevokeJoinToken(_ context.Context, id string) (state.JoinToken, error) {
	return state.JoinToken{ID: id}, nil
}

func (f *fakeStore) Hosts(context.Context) ([]state.Host, error) { return f.hosts, nil }

func (f *fakeStore) RemoveHost(_ context.Context, id string) (state.Host, error) {
	return state.Host{ID: id}, nil
}

func newService(t *testing.T, store *fakeStore) *hosts.Service {
	t.Helper()
	a, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	return &hosts.Service{Store: store, Authority: a, Address: "https://pando.example.com", Now: func() time.Time { return now }}
}

// TestR415_TheClusterAuthorityIsMadeOnceAndReadBack asserts R-415: two loads
// of one path give one authority, so the hash a token carries stays true.
func TestR415_TheClusterAuthorityIsMadeOnceAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "ca.pem")
	a, err := hosts.LoadOrCreateAuthority(path)
	require.NoError(t, err)
	b, err := hosts.LoadOrCreateAuthority(path)
	require.NoError(t, err)
	require.Equal(t, a.Hash(), b.Hash())
	require.Len(t, a.Hash(), 64)
	require.True(t, a.Cert.IsCA)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestAnUnreadableAuthorityIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte("not pem"), 0o600))
	_, err := hosts.LoadOrCreateAuthority(path)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Remedy, "control host")
}

// TestR415_AJoinTokenCarriesTheAddressAndTheAuthoritysHash asserts R-415: the
// joining host learns where to connect and which authority to expect from the
// token alone.
func TestR415_AJoinTokenCarriesTheAddressAndTheAuthoritysHash(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	minted, err := svc.Mint(context.Background(), hosts.MintRequest{CreatedBy: "usr_1"})
	require.NoError(t, err)

	d, err := hosts.Decode(minted.Join.Reveal())
	require.NoError(t, err)
	require.Equal(t, "https://pando.example.com", d.Address)
	require.Equal(t, svc.Authority.Hash(), d.AuthorityHash)
	require.Equal(t, "jt_01TEST.c2VjcmV0", d.Secret.Reveal())
	require.Equal(t, "[redacted]", minted.Join.String())
}

// TestR415_AJoinTokenDefaultsToOneUseAndADay asserts R-415's defaults.
func TestR415_AJoinTokenDefaultsToOneUseAndADay(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	minted, err := svc.Mint(context.Background(), hosts.MintRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, minted.Token.MaxUses)
	require.Equal(t, svc.Now().Add(24*time.Hour), minted.Token.ExpiresAt)
}

// TestR414_TheFirstThreeHostsTakeBothRoles asserts R-414's default: both roles
// until the cluster has three hosts, then the app role.
func TestR414_TheFirstThreeHostsTakeBothRoles(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	ctx := context.Background()

	roles, err := svc.DefaultRoles(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"control", "app"}, roles)

	store.hosts = []state.Host{{ID: "host_1"}}
	roles, err = svc.DefaultRoles(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"control", "app"}, roles)

	store.hosts = []state.Host{{ID: "host_1"}, {ID: "host_2"}}
	roles, err = svc.DefaultRoles(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"app"}, roles)
}

func TestMintRefusesWhatItCannotIssue(t *testing.T) {
	svc := newService(t, &fakeStore{})
	ctx := context.Background()
	for name, req := range map[string]hosts.MintRequest{
		"unknown role":  {Roles: []string{"scheduler"}},
		"too many uses": {MaxUses: 101},
		"negative uses": {MaxUses: -1},
		"too long":      {ExpiresIn: 31 * 24 * time.Hour},
		"too short":     {ExpiresIn: time.Second},
		"bad address":   {Address: "pando.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Mint(ctx, req)
			require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
		})
	}
}

func TestRolesAreSortedWithoutRepeats(t *testing.T) {
	roles, err := hosts.NormalizeRoles([]string{"control", " app", "control"})
	require.NoError(t, err)
	require.Equal(t, []string{"app", "control"}, roles)
	require.True(t, hosts.ControlRoleNeedsEveryVerb(roles))
	require.False(t, hosts.ControlRoleNeedsEveryVerb([]string{"app"}))
}

func TestDecodeRefusesWhatIsNotAJoinToken(t *testing.T) {
	for _, s := range []string{
		"", "pj1", "pj2.aaa.bbb.jt_1.s", "pj1.!!!." + strings.Repeat("a", 64) + ".jt_1.s",
		"pj1.aGk.short.jt_1.s", "pj1.aGk." + strings.Repeat("a", 64) + "..s",
	} {
		_, err := hosts.Decode(s)
		require.Error(t, err, s)
	}
}

func TestListRevokeAndRemovePassThrough(t *testing.T) {
	store := &fakeStore{hosts: []state.Host{{ID: "host_1"}}}
	svc := newService(t, store)
	ctx := context.Background()
	list, err := svc.Hosts(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	h, err := svc.Remove(ctx, "host_1")
	require.NoError(t, err)
	require.Equal(t, "host_1", h.ID)
	tok, err := svc.Revoke(ctx, "jt_1")
	require.NoError(t, err)
	require.Equal(t, "jt_1", tok.ID)
	_, err = svc.Mint(ctx, hosts.MintRequest{})
	require.NoError(t, err)
	toks, err := svc.Tokens(ctx)
	require.NoError(t, err)
	require.Len(t, toks, 1)
}
