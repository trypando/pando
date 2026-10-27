package hosts_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type fakeStore struct {
	hosts   []state.Host
	created []state.JoinToken

	// live is the join tokens a join can redeem, by their "<id>.<secret>",
	// spent as state.Cluster spends them: a refused admit gives the use back.
	live map[string]*state.JoinToken

	// revoked is the fingerprints of removed hosts' certificates.
	revoked map[string]bool
}

func (f *fakeStore) RenewHostCert(_ context.Context, hostID, presented, next string, nextPEM []byte) (state.Host, bool, error) {
	for i, h := range f.hosts {
		if h.ID != hostID || h.Pending || !h.HoldsCert(presented) || f.revoked[presented] {
			continue
		}
		if h.CertSHA256 != presented {
			return h, false, nil
		}
		f.hosts[i].PrevCertSHA256, f.hosts[i].CertSHA256, f.hosts[i].NextCertPEM = presented, next, nextPEM
		return f.hosts[i], true, nil
	}
	return state.Host{}, false, errs.New(errs.AuthInvalid, "This certificate is no longer this host's.")
}

func (f *fakeStore) ConfirmHostCert(_ context.Context, hostID, current string) (bool, error) {
	for i, h := range f.hosts {
		if h.ID == hostID && h.CertSHA256 == current && h.PrevCertSHA256 != "" {
			f.hosts[i].PrevCertSHA256, f.hosts[i].NextCertPEM = "", nil
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) CertRevoked(_ context.Context, sha string) (bool, error) {
	return f.revoked[sha], nil
}

func (f *fakeStore) HostByCert(_ context.Context, sha string) (state.Host, bool, error) {
	for _, h := range f.hosts {
		if h.HoldsCert(sha) {
			return h, true, nil
		}
	}
	return state.Host{}, false, nil
}

func (f *fakeStore) CheckJoinToken(_ context.Context, presented secret.Value) (state.JoinToken, error) {
	t, ok := f.live[presented.Reveal()]
	if !ok {
		return state.JoinToken{}, errs.New(errs.AuthTokenInvalid, "This join token is not valid.")
	}
	return *t, nil
}

// Join spends a use and records h pending, as state.Cluster.Join does: a
// pending join of the same name is forgotten first, its certificate revoked
// and its use given back.
func (f *fakeStore) Join(ctx context.Context, presented secret.Value, h state.Host) (state.JoinToken, state.Host, error) {
	t, err := f.CheckJoinToken(ctx, presented)
	if err != nil {
		return t, state.Host{}, err
	}
	f.forgetPending(h.Name)
	live := f.live[presented.Reveal()]
	if live.Uses >= live.MaxUses {
		return state.JoinToken{}, state.Host{}, errs.New(errs.AuthTokenInvalid, "This join token is not valid.")
	}
	for _, have := range f.hosts {
		if have.Name == h.Name {
			return *live, state.Host{}, errs.Newf(errs.StateInvalid, "A host named %q is already in the cluster.", h.Name)
		}
	}
	live.Uses++
	if h.ID == "" {
		h.ID = "host_01TEST"
	}
	h.Roles, h.JoinedWith, h.Pending = live.Roles, live.ID, true
	f.hosts = append(f.hosts, h)
	return *live, h, nil
}

func (f *fakeStore) forgetPending(name string) {
	kept := f.hosts[:0]
	for _, h := range f.hosts {
		if !h.Pending || h.Name != name {
			kept = append(kept, h)
			continue
		}
		f.revoke(h.CertSHA256)
		for _, t := range f.live {
			if t.ID == h.JoinedWith && t.Uses > 0 {
				t.Uses--
			}
		}
	}
	f.hosts = kept
}

func (f *fakeStore) revoke(sha string) {
	if f.revoked == nil {
		f.revoked = map[string]bool{}
	}
	if sha != "" {
		f.revoked[sha] = true
	}
}

func (f *fakeStore) ConfirmJoin(_ context.Context, hostID string) (bool, error) {
	for i, h := range f.hosts {
		if h.ID == hostID && h.Pending {
			f.hosts[i].Pending = false
			return true, nil
		}
	}
	return false, nil
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

func (f *fakeStore) Hosts(context.Context) ([]state.Host, error) {
	out := []state.Host{}
	for _, h := range f.hosts {
		if !h.Pending {
			out = append(out, h)
		}
	}
	return out, nil
}

func (f *fakeStore) RemoveHost(_ context.Context, id string) (state.Host, error) {
	for i, h := range f.hosts {
		if h.ID == id {
			if h.JoinedWith == "" {
				return h, state.ErrInstallHost
			}
			f.hosts = append(f.hosts[:i], f.hosts[i+1:]...)
			f.revoke(h.CertSHA256)
			f.revoke(h.PrevCertSHA256)
			return h, nil
		}
	}
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
	minted, err := svc.Mint(context.Background(), everyVerb{}, admin, hosts.MintRequest{})
	require.NoError(t, err)

	d, err := hosts.Decode(minted.Join.Reveal())
	require.NoError(t, err)
	require.Equal(t, "https://pando.example.com:7444", d.Address, "the cluster listener's default port")
	require.Equal(t, svc.Authority.Hash(), d.AuthorityHash)
	require.Equal(t, "jt_01TEST.c2VjcmV0", d.Secret.Reveal())
	require.Equal(t, "[redacted]", minted.Join.String())
}

// TestR415_AJoinTokenDefaultsToOneUseAndADay asserts R-415's defaults.
func TestR415_AJoinTokenDefaultsToOneUseAndADay(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	minted, err := svc.Mint(context.Background(), everyVerb{}, admin, hosts.MintRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, minted.Token.MaxUses)
	require.Equal(t, svc.Now().Add(24*time.Hour), minted.Token.ExpiresAt)
}

// TestR414_TheFirstThreeHostsTakeBothRoles asserts R-414's default: both roles
// until the cluster has three hosts, then the app role. The install's own
// machine is the first of them, recorded or not yet.
func TestR414_TheFirstThreeHostsTakeBothRoles(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	ctx := context.Background()

	roles, err := svc.DefaultRoles(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"control", "app"}, roles)

	store.hosts = []state.Host{{ID: "host_1"}, {ID: "host_2", JoinedWith: "jt_1"}}
	roles, err = svc.DefaultRoles(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"control", "app"}, roles)

	store.hosts = append(store.hosts, state.Host{ID: "host_3", JoinedWith: "jt_2"})
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
		"plain http":    {Address: "http://pando.example.com:7444"},
		"with a path":   {Address: "https://pando.example.com/x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Mint(ctx, everyVerb{}, admin, req)
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

// admin is who mints in these tests.
var admin = authz.Principal{ID: "usr_1"}

// everyVerb holds every install verb, or lacks one.
type everyVerb struct{ lacking bool }

func (e everyVerb) CheckHoldsEveryInstallVerb(_ context.Context, _ authz.Principal, act, because string) error {
	if e.lacking {
		return errs.Newf(errs.PermVerbRequired, "Pando refused to %s, because %s.", act, because)
	}
	return nil
}

// TestR080a_MintingAControlTokenNeedsEveryInstallVerb asserts R-080a and
// R-415 in the service layer, where every surface that mints a join token
// reaches it (R-261): a token for the control role, asked for or by default,
// is refused to a principal lacking an install verb, and to a caller that
// gives no way to check; one for the app role alone is not.
func TestR080a_MintingAControlTokenNeedsEveryInstallVerb(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	ctx := context.Background()

	for name, roles := range map[string][]string{"asked for": {"control"}, "by default": nil} {
		_, err := svc.Mint(ctx, everyVerb{lacking: true}, admin, hosts.MintRequest{Roles: roles})
		require.Equal(t, errs.PermVerbRequired, errs.CodeOf(err), name)
		require.Contains(t, errs.As(err).Message, "given every key this installation has", name)
		_, err = svc.Mint(ctx, nil, admin, hosts.MintRequest{Roles: roles})
		require.Error(t, err, name)
	}
	require.Empty(t, store.created, "nothing is minted")

	minted, err := svc.Mint(ctx, everyVerb{lacking: true}, admin, hosts.MintRequest{Roles: []string{"app"}})
	require.NoError(t, err)
	require.Equal(t, []string{"app"}, minted.Token.Roles)
	require.Equal(t, "usr_1", minted.Token.CreatedBy)

	minted, err = svc.Mint(ctx, everyVerb{}, admin, hosts.MintRequest{Roles: []string{"control", "app", "control"}})
	require.NoError(t, err)
	require.Equal(t, []string{"app", "control"}, minted.Token.Roles)
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
	store := &fakeStore{hosts: []state.Host{{ID: "host_1", JoinedWith: "jt_1"}}}
	svc := newService(t, store)
	ctx := context.Background()
	list, err := svc.Hosts(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	changes := 0
	svc.OnChange = func(context.Context) { changes++ }
	h, err := svc.Remove(ctx, "host_1")
	require.NoError(t, err)
	require.Equal(t, "host_1", h.Host.ID)
	require.Equal(t, 1, changes, "a removed host leaves the runtime's host list")
	tok, err := svc.Revoke(ctx, "jt_1")
	require.NoError(t, err)
	require.Equal(t, "jt_1", tok.ID)
	_, err = svc.Mint(ctx, everyVerb{}, admin, hosts.MintRequest{})
	require.NoError(t, err)
	toks, err := svc.Tokens(ctx)
	require.NoError(t, err)
	require.Len(t, toks, 1)
}
