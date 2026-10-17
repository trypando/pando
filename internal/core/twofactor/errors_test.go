package twofactor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

var errStoreDown = errors.New("the database is away")

// brokenStore fails the one method it is told to, and is a fakeStore
// otherwise.
type brokenStore struct {
	*fakeStore
	fail string
}

func (b *brokenStore) err(name string) error {
	if b.fail == name {
		return errStoreDown
	}
	return nil
}

func (b *brokenStore) Status(ctx context.Context, id string) (state.TwoFactorStatus, error) {
	if err := b.err("Status"); err != nil {
		return state.TwoFactorStatus{}, err
	}
	return b.fakeStore.Status(ctx, id)
}

func (b *brokenStore) PutPending(ctx context.Context, id string, v secret.Value) error {
	if err := b.err("PutPending"); err != nil {
		return err
	}
	return b.fakeStore.PutPending(ctx, id, v)
}

func (b *brokenStore) Secrets(ctx context.Context, id string) (secret.Value, secret.Value, error) {
	if err := b.err("Secrets"); err != nil {
		return secret.Value{}, secret.Value{}, err
	}
	return b.fakeStore.Secrets(ctx, id)
}

func (b *brokenStore) Enable(ctx context.Context, id string, v secret.Value, last int64, d []string) error {
	if err := b.err("Enable"); err != nil {
		return err
	}
	return b.fakeStore.Enable(ctx, id, v, last, d)
}

func (b *brokenStore) AcceptStep(ctx context.Context, id string, s int64) (bool, error) {
	if err := b.err("AcceptStep"); err != nil {
		return false, err
	}
	return b.fakeStore.AcceptStep(ctx, id, s)
}

func (b *brokenStore) UseRecoveryCode(ctx context.Context, id, d string) (bool, error) {
	if err := b.err("UseRecoveryCode"); err != nil {
		return false, err
	}
	return b.fakeStore.UseRecoveryCode(ctx, id, d)
}

func (b *brokenStore) ReplaceRecoveryCodes(ctx context.Context, id string, d []string) error {
	if err := b.err("ReplaceRecoveryCodes"); err != nil {
		return err
	}
	return b.fakeStore.ReplaceRecoveryCodes(ctx, id, d)
}

type brokenPolicy struct{}

func (brokenPolicy) Load(context.Context) (policy.Document, error) {
	return policy.Document{}, errStoreDown
}

// TestAStoreThatFailsIsAnErrorNeverAPass asserts that every store failure
// reaches the caller as an error: a second factor that could not be checked
// is not a right one.
func TestAStoreThatFailsIsAnErrorNeverAPass(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"Status", "PutPending", "Secrets", "Enable", "AcceptStep", "UseRecoveryCode", "ReplaceRecoveryCodes"} {
		t.Run(name, func(t *testing.T) {
			store := &brokenStore{fakeStore: &fakeStore{}}
			clk := clock.NewFake(time.Unix(1_800_000_000, 0).UTC())
			pol := policyOf(policy.Document{})
			svc := &Service{Store: store, Policy: &pol, Clock: clk}

			// Set up while nothing fails, so each call below reaches the step
			// that does.
			codes := enroll(ctx, t, svc, store.fakeStore, clk)
			clk.Advance(period)
			now := codeNow(t, store.current, clk.Now())
			store.fail = name

			var errs []error
			_, err := svc.Verify(ctx, "usr_1", now)
			errs = append(errs, err)
			_, err = svc.Verify(ctx, "usr_1", codes[0])
			errs = append(errs, err)
			_, err = svc.Summarize(ctx, "usr_1")
			errs = append(errs, err)
			_, err = svc.NewRecoveryCodes(ctx, "usr_1")
			errs = append(errs, err)
			_, err = svc.Begin(ctx, "usr_1", "ben")
			errs = append(errs, err)
			confirming := now
			if !store.pending.IsZero() {
				confirming = codeNow(t, store.pending, clk.Now())
			}
			_, err = svc.Confirm(ctx, "usr_1", confirming)
			errs = append(errs, err)

			var failed bool
			for _, e := range errs {
				if errors.Is(e, errStoreDown) {
					failed = true
				}
			}
			if !failed {
				t.Errorf("no call reported %s failing: %v", name, errs)
			}
		})
	}
}

func TestAPolicyThatCannotBeReadIsAnError(t *testing.T) {
	ctx := context.Background()
	svc := &Service{Store: &fakeStore{}, Policy: brokenPolicy{}, Clock: clock.System{}}
	if _, err := svc.Required(ctx); !errors.Is(err, errStoreDown) {
		t.Errorf("Required: %v", err)
	}
	if _, err := svc.SetupPending(ctx, state.User{AdapterID: state.LocalAdapterID}); !errors.Is(err, errStoreDown) {
		t.Errorf("SetupPending: %v", err)
	}
	if _, err := svc.Summarize(ctx, "usr_1"); !errors.Is(err, errStoreDown) {
		t.Errorf("Summarize: %v", err)
	}
	if _, err := svc.TurnOff(ctx, "usr_1"); !errors.Is(err, errStoreDown) {
		t.Errorf("TurnOff: %v", err)
	}
	if err := svc.Decline(ctx, "usr_1"); !errors.Is(err, errStoreDown) {
		t.Errorf("Decline: %v", err)
	}
}
