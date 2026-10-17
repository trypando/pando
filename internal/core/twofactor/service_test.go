package twofactor

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// fakeStore keeps one account's second factor in memory, as state.TwoFactor
// would in its tables.
type fakeStore struct {
	enabled   bool
	offered   bool
	lastStep  int64
	current   secret.Value
	pending   secret.Value
	codes     map[string]bool // digest -> used
	disabled  int
	reoffered bool
}

func (f *fakeStore) Status(context.Context, string) (state.TwoFactorStatus, error) {
	st := state.TwoFactorStatus{LastStep: f.lastStep}
	now := time.Now()
	if f.enabled {
		st.EnabledAt = &now
	}
	if f.offered {
		st.OfferedAt = &now
	}
	for _, used := range f.codes {
		if !used {
			st.RecoveryCodesLeft++
		}
	}
	return st, nil
}

func (f *fakeStore) PutPending(_ context.Context, _ string, v secret.Value) error {
	f.pending = v
	return nil
}

func (f *fakeStore) Secrets(context.Context, string) (secret.Value, secret.Value, error) {
	return f.current, f.pending, nil
}

func (f *fakeStore) Enable(ctx context.Context, _ string, v secret.Value, last int64, digests []string) error {
	f.enabled, f.offered, f.current, f.pending, f.lastStep = true, true, v, secret.Value{}, last
	return f.ReplaceRecoveryCodes(ctx, "", digests)
}

func (f *fakeStore) AcceptStep(_ context.Context, _ string, s int64) (bool, error) {
	if !f.enabled || s <= f.lastStep {
		return false, nil
	}
	f.lastStep = s
	return true, nil
}

func (f *fakeStore) UseRecoveryCode(_ context.Context, _ string, digest string) (bool, error) {
	used, ok := f.codes[digest]
	if !ok || used {
		return false, nil
	}
	f.codes[digest] = true
	return true, nil
}

func (f *fakeStore) ReplaceRecoveryCodes(_ context.Context, _ string, digests []string) error {
	f.codes = map[string]bool{}
	for _, d := range digests {
		f.codes[d] = false
	}
	return nil
}

func (f *fakeStore) Disable(_ context.Context, _ string, reoffer bool) (bool, error) {
	had := f.enabled
	f.enabled, f.current, f.pending, f.codes, f.lastStep = false, secret.Value{}, secret.Value{}, nil, 0
	f.disabled++
	if reoffer {
		f.offered, f.reoffered = false, true
	}
	return had, nil
}

func (f *fakeStore) MarkOffered(context.Context, string) error {
	f.offered = true
	return nil
}

type policyOf policy.Document

func (p *policyOf) Load(context.Context) (policy.Document, error) { return policy.Document(*p), nil }

func newService(required bool) (*Service, *fakeStore, *clock.Fake) {
	store := &fakeStore{}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0).UTC())
	pol := policyOf(policy.Document{RequireLocalTwoFactor: required})
	return &Service{Store: store, Policy: &pol, Clock: clk, Issuer: "Pando (test)"}, store, clk
}

// codeNow is what the authenticator app shows for secret at the clock's time.
func codeNow(t *testing.T, s secret.Value, now time.Time) string {
	t.Helper()
	key, err := b32.DecodeString(s.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	return code(key, step(now))
}

func enroll(t *testing.T, svc *Service, store *fakeStore, clk *clock.Fake) []string {
	t.Helper()
	e, err := svc.Begin(context.Background(), "usr_1", "ben")
	if err != nil {
		t.Fatal(err)
	}
	codes, err := svc.Confirm(context.Background(), "usr_1", codeNow(t, e.Secret, clk.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if !store.enabled {
		t.Fatal("confirming did not turn it on")
	}
	return codes
}

// TestR405_SetUpWithAQRCodeAndAConfirmingCode asserts R-405: setting up
// shows a key and a QR code of it, and turns on only with a right code from
// it, giving ten recovery codes.
func TestR405_SetUpWithAQRCodeAndAConfirmingCode(t *testing.T) {
	svc, store, clk := newService(false)
	ctx := context.Background()
	e, err := svc.Begin(ctx, "usr_1", "ben")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(e.URI.Reveal())
	if err != nil || u.Query().Get("secret") != e.Secret.Reveal() || u.Query().Get("issuer") != "Pando (test)" {
		t.Fatalf("uri = %s (%v)", e.URI.Reveal(), err)
	}
	if !strings.HasPrefix(e.QRSVG, "<svg") {
		t.Errorf("qr = %.40s", e.QRSVG)
	}

	if _, err := svc.Confirm(ctx, "usr_1", "000000"); errs.CodeOf(err) != errs.AuthInvalid {
		// One chance in a million this is the real code; the fixed clock
		// makes it deterministic, and it is not.
		t.Fatalf("a wrong code: %v", err)
	}
	if store.enabled {
		t.Fatal("a wrong code turned it on")
	}

	codes, err := svc.Confirm(ctx, "usr_1", codeNow(t, e.Secret, clk.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("%d recovery codes, want %d", len(codes), RecoveryCodeCount)
	}
	shape := regexp.MustCompile(`^[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}$`)
	for _, c := range codes {
		if !shape.MatchString(c) {
			t.Errorf("recovery code %q is not xxxx-xxxx-xxxx-xxxx", c)
		}
	}

	// The code that confirmed it does not also sign in.
	if _, err := svc.Verify(ctx, "usr_1", codeNow(t, e.Secret, clk.Now())); errs.CodeOf(err) != errs.AuthInvalid {
		t.Errorf("the confirming code signed in: %v", err)
	}
}

func TestConfirmWithNothingPending(t *testing.T) {
	svc, _, _ := newService(false)
	if _, err := svc.Confirm(context.Background(), "usr_1", "123456"); errs.CodeOf(err) != errs.StateInvalid {
		t.Errorf("err = %v, want STATE_INVALID", err)
	}
}

// TestR405_SignInTakesACodeOnceOrARecoveryCodeOnce asserts R-405.
func TestR405_SignInTakesACodeOnceOrARecoveryCodeOnce(t *testing.T) {
	svc, store, clk := newService(false)
	ctx := context.Background()
	codes := enroll(t, svc, store, clk)

	clk.Advance(period)
	now := codeNow(t, store.current, clk.Now())
	if m, err := svc.Verify(ctx, "usr_1", now); err != nil || m != MethodAuthenticator {
		t.Fatalf("Verify = %v, %v", m, err)
	}
	if _, err := svc.Verify(ctx, "usr_1", now); errs.CodeOf(err) != errs.AuthInvalid {
		t.Errorf("the same code twice: %v", err)
	}

	typed := strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))
	if m, err := svc.Verify(ctx, "usr_1", typed); err != nil || m != MethodRecoveryCode {
		t.Fatalf("recovery code as typed by hand: %v, %v", m, err)
	}
	if _, err := svc.Verify(ctx, "usr_1", codes[0]); errs.CodeOf(err) != errs.AuthInvalid {
		t.Errorf("a recovery code twice: %v", err)
	}
	for _, bad := range []string{"", "nope", "aaaa-aaaa-aaaa-aaaa"} {
		if _, err := svc.Verify(ctx, "usr_1", bad); errs.CodeOf(err) != errs.AuthInvalid {
			t.Errorf("%q: %v", bad, err)
		}
	}
	sum, _ := svc.Summarize(ctx, "usr_1")
	if sum.RecoveryCodesRemaining != RecoveryCodeCount-1 {
		t.Errorf("remaining = %d", sum.RecoveryCodesRemaining)
	}
}

func TestVerifyWithoutAnAuthenticator(t *testing.T) {
	svc, _, _ := newService(false)
	if _, err := svc.Verify(context.Background(), "usr_1", "123456"); errs.CodeOf(err) != errs.AuthInvalid {
		t.Errorf("err = %v", err)
	}
}

func TestNewRecoveryCodesReplaceTheOld(t *testing.T) {
	svc, store, clk := newService(false)
	ctx := context.Background()
	if _, err := svc.NewRecoveryCodes(ctx, "usr_1"); errs.CodeOf(err) != errs.StateInvalid {
		t.Errorf("without two-factor: %v", err)
	}
	old := enroll(t, svc, store, clk)
	fresh, err := svc.NewRecoveryCodes(ctx, "usr_1")
	if err != nil || len(fresh) != RecoveryCodeCount {
		t.Fatalf("%v, %v", fresh, err)
	}
	if _, err := svc.Verify(ctx, "usr_1", old[0]); errs.CodeOf(err) != errs.AuthInvalid {
		t.Error("an old recovery code still worked")
	}
	if _, err := svc.Verify(ctx, "usr_1", fresh[0]); err != nil {
		t.Errorf("a new recovery code: %v", err)
	}
}

// TestR405_PolicyMakesItRequired asserts R-405: under the policy an account
// without it must set it up, cannot decline, and cannot turn it off.
func TestR405_PolicyMakesItRequired(t *testing.T) {
	svc, store, clk := newService(true)
	ctx := context.Background()
	local := state.User{ID: "usr_1", AdapterID: state.LocalAdapterID}
	if pending, _ := svc.SetupPending(ctx, local); !pending {
		t.Error("a local account without it is not held back")
	}
	if pending, _ := svc.SetupPending(ctx, state.User{ID: "usr_2", AdapterID: "idp_x"}); pending {
		t.Error("a provider's account is held back: its provider asks for the second factor")
	}
	sum, _ := svc.Summarize(ctx, "usr_1")
	if !sum.Required || !sum.SetupRequired || sum.Offer {
		t.Errorf("summary = %+v", sum)
	}
	if err := svc.Decline(ctx, "usr_1"); errs.CodeOf(err) != errs.PolicyTwoFactorRequired {
		t.Errorf("Decline: %v", err)
	}

	enroll(t, svc, store, clk)
	local.TwoFactorEnabled = true
	if pending, _ := svc.SetupPending(ctx, local); pending {
		t.Error("held back after setting it up")
	}
	if _, err := svc.TurnOff(ctx, "usr_1"); errs.CodeOf(err) != errs.PolicyTwoFactorRequired {
		t.Errorf("TurnOff: %v", err)
	}
	if store.disabled != 0 {
		t.Error("turned off anyway")
	}
}

// TestR405_AskedOnce asserts R-405: without the policy, the holder is offered
// it until they set it up or say not now, and an administrator's reset asks
// again.
func TestR405_AskedOnce(t *testing.T) {
	svc, store, clk := newService(false)
	ctx := context.Background()
	if sum, _ := svc.Summarize(ctx, "usr_1"); !sum.Offer || sum.Required || sum.SetupRequired {
		t.Errorf("first: %+v", sum)
	}
	if err := svc.Decline(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if sum, _ := svc.Summarize(ctx, "usr_1"); sum.Offer {
		t.Error("offered again after not now")
	}

	enroll(t, svc, store, clk)
	if had, err := svc.TurnOff(ctx, "usr_1"); err != nil || !had {
		t.Fatalf("TurnOff = %v, %v", had, err)
	}
	if sum, _ := svc.Summarize(ctx, "usr_1"); sum.Enabled || sum.Offer {
		t.Errorf("after turning it off: %+v", sum)
	}

	enroll(t, svc, store, clk)
	if had, err := svc.Reset(ctx, "usr_1"); err != nil || !had || !store.reoffered {
		t.Fatalf("Reset = %v, %v", had, err)
	}
	if sum, _ := svc.Summarize(ctx, "usr_1"); sum.Enabled || !sum.Offer {
		t.Errorf("after an administrator's reset: %+v", sum)
	}
}

func TestIssuerFor(t *testing.T) {
	if got := IssuerFor(nil); got != "Pando" {
		t.Errorf("nil: %q", got)
	}
	u, _ := url.Parse("https://pando.example.com:8443/")
	if got := IssuerFor(u); got != "Pando (pando.example.com)" {
		t.Errorf("got %q", got)
	}
	if got := (&Service{}).issuer(); got != "Pando" {
		t.Errorf("default issuer %q", got)
	}
}
