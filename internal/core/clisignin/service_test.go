package clisignin_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clisignin"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type fakeStore struct {
	rows map[string]state.CLISignIn
	err  error
}

func (f *fakeStore) Create(_ context.Context, s state.CLISignIn) error {
	if f.err != nil {
		return f.err
	}
	f.rows[s.CodeHash] = s
	return nil
}

func (f *fakeStore) Take(_ context.Context, hash string) (state.CLISignIn, bool, error) {
	if f.err != nil {
		return state.CLISignIn{}, false, f.err
	}
	s, ok := f.rows[hash]
	delete(f.rows, hash)
	return s, ok, nil
}

type fakeUsers map[string]state.User

func (f fakeUsers) ByID(_ context.Context, id string) (state.User, bool, error) {
	if id == "usr_broken" {
		return state.User{}, false, errors.New("database down")
	}
	u, ok := f[id]
	return u, ok, nil
}

type minted struct {
	kind, name, owner, createdBy string
	expires                      *time.Time
}

type fakeTokens struct {
	made []minted
	err  error
}

func (f *fakeTokens) Create(_ context.Context, kind, name, owner, createdBy string, expires *time.Time) (state.Issued, error) {
	if f.err != nil {
		return state.Issued{}, f.err
	}
	f.made = append(f.made, minted{kind, name, owner, createdBy, expires})
	return state.Issued{Token: state.Token{ID: "tok_1", Name: name}, Secret: secret.New("s3cret")}, nil
}

type fakePolicy struct {
	doc policy.Document
	err error
}

func (f fakePolicy) Load(context.Context) (policy.Document, error) { return f.doc, f.err }

const verifier = "a-verifier-that-is-at-least-forty-three-characters-long"

type rig struct {
	svc    *clisignin.Service
	store  *fakeStore
	tokens *fakeTokens
	clock  *clock.Fake
	users  fakeUsers
}

func newRig() *rig {
	r := &rig{
		store:  &fakeStore{rows: map[string]state.CLISignIn{}},
		tokens: &fakeTokens{},
		clock:  clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
		users: fakeUsers{
			"usr_ada":     {ID: "usr_ada", ExternalID: "ada", Status: "active"},
			"usr_new":     {ID: "usr_new", ExternalID: "new", Status: "active", MustChangePassword: true},
			"usr_gone":    {ID: "usr_gone", ExternalID: "gone", Status: "suspended"},
			"usr_pending": {ID: "usr_pending", ExternalID: "pending", Status: "active"},
		},
	}
	r.svc = &clisignin.Service{Store: r.store, Users: r.users, Tokens: r.tokens, Policy: fakePolicy{}, Clock: r.clock}
	return r
}

func request(port int) clisignin.Request {
	return clisignin.Request{
		CodeChallenge: clisignin.Challenge(verifier), CodeChallengeMethod: "S256",
		Name: "laptop CLI", Port: port, State: "state-0123456789abcdef",
	}
}

func code(t *testing.T, a clisignin.Approved) string {
	t.Helper()
	return a.Code.Reveal()
}

// TestR406_TheCLISignsInThroughTheBrowser asserts R-406: an approval made by a
// signed-in person is traded, with the verifier behind its challenge, for a
// delegated token acting as that person, and the browser is sent only to the
// CLI's loopback address.
func TestR406_TheCLISignsInThroughTheBrowser(t *testing.T) {
	t.Parallel()
	r := newRig()
	ctx := context.Background()

	approved, err := r.svc.Approve(ctx, "usr_ada", "usr_ada", request(49152))
	require.NoError(t, err)
	require.Equal(t, r.clock.Now().Add(clisignin.CodeLifetime), approved.ExpiresAt)

	to, err := url.Parse(approved.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "http", to.Scheme)
	require.Equal(t, "127.0.0.1:49152", to.Host, "only ever the loopback address")
	require.Equal(t, "/callback", to.Path)
	require.Equal(t, code(t, approved), to.Query().Get("code"))
	require.Equal(t, "state-0123456789abcdef", to.Query().Get("state"))

	for hash := range r.store.rows {
		require.NotContains(t, hash, code(t, approved), "only the code's digest is stored")
		require.Equal(t, clisignin.Digest(code(t, approved)), hash)
	}

	got, err := r.svc.Exchange(ctx, code(t, approved), verifier)
	require.NoError(t, err)
	require.Equal(t, "usr_ada", got.UserID)
	require.Equal(t, "ada", got.Username)
	require.Equal(t, "s3cret", got.Issued.Secret.Reveal())
	require.Equal(t, []minted{{state.TokenDelegated, "laptop CLI", "usr_ada", "usr_ada", nil}}, r.tokens.made,
		"a delegated token (R-058), never expiring when policy does not cap it")

	_, err = r.svc.Exchange(ctx, code(t, approved), verifier)
	requireCode(t, err, errs.AuthInvalid, "a code works once")
}

// TestR406_ACodeIsWorthNothingWithoutItsVerifier asserts R-406: the code that
// reaches the browser cannot be traded by anyone but the CLI that made the
// challenge, and trying it with the wrong verifier spends it.
func TestR406_ACodeIsWorthNothingWithoutItsVerifier(t *testing.T) {
	t.Parallel()
	r := newRig()
	ctx := context.Background()

	approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(0))
	require.NoError(t, err)
	require.Empty(t, approved.RedirectURL, "pasted, not redirected")

	refused, err := r.svc.Exchange(ctx, code(t, approved), strings.Repeat("x", 43))
	requireCode(t, err, errs.AuthInvalid, "wrong verifier")
	require.Equal(t, clisignin.Exchanged{UserID: "usr_ada", ApprovedBy: "ses_1", Name: "laptop CLI", Kind: clisignin.KindDelegated}, refused,
		"says whose approval was spent, for the audit log")
	refused, err = r.svc.Exchange(ctx, code(t, approved), verifier)
	requireCode(t, err, errs.AuthInvalid, "spent by the wrong verifier")
	require.Empty(t, refused.UserID, "a code that names nothing says nothing")
	require.Empty(t, r.tokens.made)

	_, err = r.svc.Exchange(ctx, code(t, approved), "short")
	requireCode(t, err, errs.AuthInvalid, "a verifier RFC 7636 would not allow")
	_, err = r.svc.Exchange(ctx, "", verifier)
	requireCode(t, err, errs.AuthInvalid, "no code")
}

func TestR406_ACodeExpires(t *testing.T) {
	t.Parallel()
	r := newRig()
	ctx := context.Background()

	approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(0))
	require.NoError(t, err)
	r.clock.Advance(clisignin.CodeLifetime)
	_, err = r.svc.Exchange(ctx, code(t, approved), verifier)
	requireCode(t, err, errs.AuthInvalid, "expired")
}

func TestR406_TheTokenKeepsPolicysLifetime(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.svc.Policy = fakePolicy{doc: policy.Document{MaxTokenLifetimeDays: 30}}
	ctx := context.Background()

	approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(0))
	require.NoError(t, err)
	_, err = r.svc.Exchange(ctx, code(t, approved), verifier)
	require.NoError(t, err)
	require.Len(t, r.tokens.made, 1)
	require.NotNil(t, r.tokens.made[0].expires)
	require.Equal(t, r.clock.Now().AddDate(0, 0, 30), *r.tokens.made[0].expires, "R-061")
}

func TestR406_OnlyAnActiveAccountGetsAToken(t *testing.T) {
	t.Parallel()
	r := newRig()
	ctx := context.Background()

	_, err := r.svc.Approve(ctx, "usr_gone", "ses_1", request(0))
	requireCode(t, err, errs.AuthInvalid, "suspended (R-049)")
	_, err = r.svc.Approve(ctx, "usr_nobody", "ses_1", request(0))
	requireCode(t, err, errs.AuthInvalid, "no such account")
	_, err = r.svc.Approve(ctx, "usr_new", "ses_1", request(0))
	requireCode(t, err, errs.PermDenied, "R-046: the generated password first")

	// Suspended between approving and collecting.
	approved, err := r.svc.Approve(ctx, "usr_pending", "ses_1", request(0))
	require.NoError(t, err)
	u := r.users["usr_pending"]
	u.Status = "suspended"
	r.users["usr_pending"] = u
	_, err = r.svc.Exchange(ctx, code(t, approved), verifier)
	requireCode(t, err, errs.AuthInvalid, "suspended since")
	require.Empty(t, r.tokens.made)
}

func TestR406_ARequestIsCheckedBeforeAnythingIsStored(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*clisignin.Request){
		"plain method":       func(q *clisignin.Request) { q.CodeChallengeMethod = "plain" },
		"short challenge":    func(q *clisignin.Request) { q.CodeChallenge = "abc" },
		"privileged port":    func(q *clisignin.Request) { q.Port = 80 },
		"port out of range":  func(q *clisignin.Request) { q.Port = 70000 },
		"no state":           func(q *clisignin.Request) { q.State = "" },
		"state with a quote": func(q *clisignin.Request) { q.State = `abcdefghijklmnop"` },
		"no name":            func(q *clisignin.Request) { q.Name = "  " },
		"name too long":      func(q *clisignin.Request) { q.Name = strings.Repeat("n", 201) },
		"control in name":    func(q *clisignin.Request) { q.Name = "laptop\nCLI" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig()
			q := request(49152)
			change(&q)
			_, err := r.svc.Approve(context.Background(), "usr_ada", "ses_1", q)
			requireCode(t, err, errs.ValidInvalid, name)
			require.Empty(t, r.store.rows)
		})
	}

	// Pasted: no port, so no state is needed.
	r := newRig()
	q := request(0)
	q.State = ""
	_, err := r.svc.Approve(context.Background(), "usr_ada", "ses_1", q)
	require.NoError(t, err)
}

func TestCLISignInFailuresPassThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errs.New(errs.Internal, "boom")

	r := newRig()
	r.store.err = boom
	_, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(0))
	require.ErrorIs(t, err, boom)
	_, err = r.svc.Exchange(ctx, "code", verifier)
	require.ErrorIs(t, err, boom)

	r = newRig()
	_, err = r.svc.Approve(ctx, "usr_broken", "ses_1", request(0))
	require.Error(t, err)

	for name, set := range map[string]func(*rig){
		"policy": func(r *rig) { r.svc.Policy = fakePolicy{err: boom} },
		"tokens": func(r *rig) { r.tokens.err = boom },
	} {
		r := newRig()
		approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(0))
		require.NoError(t, err)
		set(r)
		_, err = r.svc.Exchange(ctx, code(t, approved), verifier)
		require.ErrorIs(t, err, boom, name)
	}
}

func requireCode(t *testing.T, err error, want errs.Code, why string) {
	t.Helper()
	require.Error(t, err, why)
	e := errs.As(err)
	require.NotNil(t, e, why)
	require.Equal(t, want, e.Code, why)
	require.NotEmpty(t, e.Remedy, "%s: R-105 — says what to do", why)
}

// TestR060_TheCLICreatesAServiceTokenThroughTheBrowser asserts R-060 for
// `pando token create --service` (issue #396): an approval asking for a
// service token mints one that is its own principal, owned by nobody and
// recorded as made by the session that approved it.
func TestR060_TheCLICreatesAServiceTokenThroughTheBrowser(t *testing.T) {
	r := newRig()
	var asked []string
	r.svc.MayCreateServiceTokens = func(_ context.Context, userID string) error {
		asked = append(asked, userID)
		return nil
	}

	req := request(0)
	req.Name, req.Kind = "nightly agent", clisignin.KindService
	approved, err := r.svc.Approve(context.Background(), "usr_ada", "ses_ada", req)
	require.NoError(t, err)

	got, err := r.svc.Exchange(context.Background(), code(t, approved), verifier)
	require.NoError(t, err)
	require.Equal(t, clisignin.KindService, got.Kind)
	require.Equal(t, []minted{{kind: state.TokenAccount, name: "nightly agent", owner: "", createdBy: "ses_ada"}}, r.tokens.made)
	require.Equal(t, []string{"usr_ada", "usr_ada"}, asked, "checked when approved and again when collected")
}

func TestR060_AServiceTokenNeedsTheVerbWhenApprovedAndWhenCollected(t *testing.T) {
	denied := errs.New(errs.PermDenied, "no install.tokens.manage")
	req := request(0)
	req.Kind = clisignin.KindService

	t.Run("approving", func(t *testing.T) {
		r := newRig()
		r.svc.MayCreateServiceTokens = func(context.Context, string) error { return denied }
		_, err := r.svc.Approve(context.Background(), "usr_ada", "ses_ada", req)
		require.ErrorIs(t, err, denied)
		require.Empty(t, r.store.rows, "nothing stored")
	})

	t.Run("collecting", func(t *testing.T) {
		r := newRig()
		allowed := true
		r.svc.MayCreateServiceTokens = func(context.Context, string) error {
			if allowed {
				return nil
			}
			return denied
		}
		approved, err := r.svc.Approve(context.Background(), "usr_ada", "ses_ada", req)
		require.NoError(t, err)
		allowed = false

		got, err := r.svc.Exchange(context.Background(), code(t, approved), verifier)
		require.ErrorIs(t, err, denied)
		require.Equal(t, "usr_ada", got.UserID, "a refusal says whose approval it spent")
		require.Equal(t, clisignin.KindService, got.Kind)
		require.Empty(t, r.tokens.made)
	})

	t.Run("not wired", func(t *testing.T) {
		r := newRig()
		_, err := r.svc.Approve(context.Background(), "usr_ada", "ses_ada", req)
		require.Equal(t, errs.PermDenied, errs.As(err).Code)
	})
}

func TestR406_AnApprovalAsksForAKnownKindOfToken(t *testing.T) {
	r := newRig()
	req := request(0)
	req.Kind = "admin"
	_, err := r.svc.Approve(context.Background(), "usr_ada", "ses_ada", req)
	require.Equal(t, errs.ValidInvalid, errs.As(err).Code)

	req.Kind = clisignin.KindDelegated
	approved, err := r.svc.Approve(context.Background(), "usr_ada", "ses_ada", req)
	require.NoError(t, err)
	got, err := r.svc.Exchange(context.Background(), code(t, approved), verifier)
	require.NoError(t, err)
	require.Equal(t, clisignin.KindDelegated, got.Kind)
	require.Equal(t, "usr_ada", r.tokens.made[0].owner)
}
