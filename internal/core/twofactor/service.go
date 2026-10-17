package twofactor

import (
	"context"
	"net/url"
	"strings"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/qrcode"
	"github.com/trypando/pando/internal/secret"
)

// Store is where second factors are kept (state.TwoFactor).
type Store interface {
	Status(ctx context.Context, userID string) (state.TwoFactorStatus, error)
	PutPending(ctx context.Context, userID string, v secret.Value) error
	Secrets(ctx context.Context, userID string) (current, pending secret.Value, err error)
	Enable(ctx context.Context, userID string, v secret.Value, lastStep int64, digests []string) error
	AcceptStep(ctx context.Context, userID string, step int64) (bool, error)
	UseRecoveryCode(ctx context.Context, userID, digest string) (bool, error)
	ReplaceRecoveryCodes(ctx context.Context, userID string, digests []string) error
	Disable(ctx context.Context, userID string, reoffer bool) (bool, error)
	MarkOffered(ctx context.Context, userID string) error
}

// Service is a local account's second factor (R-405). It decides nothing
// about who may change whose: the caller has already, and audits what it did.
type Service struct {
	Store Store

	// Policy is host policy as in force, startup overlay included.
	Policy interface {
		Load(ctx context.Context) (policy.Document, error)
	}
	Clock clock.Clock

	// Issuer names this installation in an authenticator app, beside the
	// account's username: "Pando (pando.example.com)".
	Issuer string
}

// Summary is an account's second factor as the account's holder sees it.
type Summary struct {
	Enabled bool `json:"enabled"`

	// Required says host policy requires a second factor on local accounts.
	Required bool `json:"required"`

	// SetupRequired is Required without one: the account can do nothing but
	// set it up.
	SetupRequired bool `json:"setup_required"`

	// Offer says to ask the holder to set one up: not required, not on, and
	// not asked before. Asked once.
	Offer bool `json:"offer"`

	RecoveryCodesRemaining int `json:"recovery_codes_remaining"`
}

// Required says whether host policy requires a second factor on every local
// account.
func (s *Service) Required(ctx context.Context) (bool, error) {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return false, err
	}
	return doc.RequireLocalTwoFactor, nil
}

// SetupPending says a session of this account may do nothing but set up its
// second factor: a local account without one, where policy requires it. Read
// on every request a session makes, so the account's own row decides first and
// policy is read only for the accounts it could hold back.
func (s *Service) SetupPending(ctx context.Context, user state.User) (bool, error) {
	if user.AdapterID != state.LocalAdapterID || user.TwoFactorEnabled {
		return false, nil
	}
	return s.Required(ctx)
}

// Summarize is an account's second factor for GET /me.
func (s *Service) Summarize(ctx context.Context, userID string) (Summary, error) {
	st, err := s.Store.Status(ctx, userID)
	if err != nil {
		return Summary{}, err
	}
	required, err := s.Required(ctx)
	if err != nil {
		return Summary{}, err
	}
	enabled := st.EnabledAt != nil
	return Summary{
		Enabled:                enabled,
		Required:               required,
		SetupRequired:          required && !enabled,
		Offer:                  !required && !enabled && st.OfferedAt == nil,
		RecoveryCodesRemaining: st.RecoveryCodesLeft,
	}, nil
}

// Enrollment is a new authenticator, shown to its holder once: the secret to
// type in, and the same as a key URI and a QR code holding it.
type Enrollment struct {
	Secret secret.Value `json:"-"`
	URI    secret.Value `json:"-"`
	QRSVG  string       `json:"-"`
}

// Begin makes a new authenticator secret for an account and keeps it pending
// until Confirm sees a code from it. Replaces any pending one. The one in use,
// if any, stays in use until then, so starting over never locks anyone out.
func (s *Service) Begin(ctx context.Context, userID, username string) (Enrollment, error) {
	raw, err := newSecret()
	if err != nil {
		return Enrollment{}, errs.Wrap(errs.Internal, "Could not make an authenticator key.", err)
	}
	if err := s.Store.PutPending(ctx, userID, secret.New(raw)); err != nil {
		return Enrollment{}, err
	}
	uri := otpauthURI(s.issuer(), username, raw)
	svg, err := qrcode.SVG(uri)
	if err != nil {
		return Enrollment{}, errs.Wrap(errs.Internal, "Could not draw the QR code.", err)
	}
	return Enrollment{Secret: secret.New(raw), URI: secret.New(uri), QRSVG: svg}, nil
}

// IssuerFor names an installation in authenticator apps: "Pando", with the
// host it is reached at when one is configured, so two installations on one
// phone are told apart.
func IssuerFor(external *url.URL) string {
	if external == nil || external.Hostname() == "" {
		return "Pando"
	}
	return "Pando (" + external.Hostname() + ")"
}

func (s *Service) issuer() string {
	if s.Issuer != "" {
		return s.Issuer
	}
	return "Pando"
}

// Confirm checks a code from the pending authenticator and, if it is right,
// puts the authenticator in use with a fresh set of recovery codes, which are
// returned to be shown once and never again.
func (s *Service) Confirm(ctx context.Context, userID, presented string) ([]string, error) {
	_, pending, err := s.Store.Secrets(ctx, userID)
	if err != nil {
		return nil, err
	}
	if pending.IsZero() {
		return nil, errs.New(errs.StateInvalid,
			"There is no authenticator being set up on this account, so there is nothing to confirm.").
			WithRemedy("Start setting up two-factor authentication again, scan the new QR code, then enter its code.")
	}
	stepUsed, ok := match(pending.Reveal(), strings.ReplaceAll(presented, " ", ""), s.Clock.Now(), 0)
	if !ok {
		return nil, errWrongCode()
	}
	codes, digests, err := freshCodes()
	if err != nil {
		return nil, err
	}
	if err := s.Store.Enable(ctx, userID, pending, stepUsed, digests); err != nil {
		return nil, err
	}
	return codes, nil
}

// Method is how a sign-in's second factor was given.
type Method string

const (
	MethodAuthenticator Method = "authenticator"
	MethodRecoveryCode  Method = "recovery_code"
)

// Verify checks a code given at sign-in: an authenticator code, good once, or
// an unused recovery code, which is then spent. A wrong one is AUTH_INVALID;
// counting it is the caller's (R-404's counts, R-405).
func (s *Service) Verify(ctx context.Context, userID, presented string) (Method, error) {
	presented = strings.TrimSpace(presented)
	if IsAuthenticatorCode(presented) {
		current, _, err := s.Store.Secrets(ctx, userID)
		if err != nil {
			return "", err
		}
		st, err := s.Store.Status(ctx, userID)
		if err != nil {
			return "", err
		}
		if current.IsZero() || st.EnabledAt == nil {
			return "", errWrongCode()
		}
		stepUsed, ok := match(current.Reveal(), strings.ReplaceAll(presented, " ", ""), s.Clock.Now(), st.LastStep)
		if !ok {
			return "", errWrongCode()
		}
		accepted, err := s.Store.AcceptStep(ctx, userID, stepUsed)
		if err != nil {
			return "", err
		}
		if !accepted {
			return "", errWrongCode()
		}
		return MethodAuthenticator, nil
	}
	if normalizeRecoveryCode(presented) == "" {
		return "", errWrongCode()
	}
	used, err := s.Store.UseRecoveryCode(ctx, userID, recoveryDigest(presented))
	if err != nil {
		return "", err
	}
	if !used {
		return "", errWrongCode()
	}
	return MethodRecoveryCode, nil
}

// NewRecoveryCodes replaces an account's recovery codes with a fresh set.
func (s *Service) NewRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	st, err := s.Store.Status(ctx, userID)
	if err != nil {
		return nil, err
	}
	if st.EnabledAt == nil {
		return nil, errs.New(errs.StateInvalid,
			"This account does not have two-factor authentication turned on, so it has no recovery codes to replace.").
			WithRemedy("Set up two-factor authentication first. It comes with recovery codes.")
	}
	codes, digests, err := freshCodes()
	if err != nil {
		return nil, err
	}
	if err := s.Store.ReplaceRecoveryCodes(ctx, userID, digests); err != nil {
		return nil, err
	}
	return codes, nil
}

// TurnOff removes an account's second factor at its holder's asking, which
// host policy may forbid.
func (s *Service) TurnOff(ctx context.Context, userID string) (bool, error) {
	required, err := s.Required(ctx)
	if err != nil {
		return false, err
	}
	if required {
		return false, errs.New(errs.PolicyTwoFactorRequired,
			"This installation requires two-factor authentication on local accounts, so it cannot be turned off.").
			WithRemedy("To move it to a new phone, set up the authenticator again from your settings instead. " +
				"It replaces the old one.")
	}
	return s.Store.Disable(ctx, userID, false)
}

// Reset removes an account's second factor at an administrator's asking, or
// from the host: someone who lost their authenticator and their recovery
// codes. They are asked to set it up again at the next sign-in, and must if
// policy requires it.
func (s *Service) Reset(ctx context.Context, userID string) (bool, error) {
	return s.Store.Disable(ctx, userID, true)
}

// Decline records that the holder chose not to set one up when asked, so they
// are not asked again. Refused where policy requires one.
func (s *Service) Decline(ctx context.Context, userID string) error {
	required, err := s.Required(ctx)
	if err != nil {
		return err
	}
	if required {
		return errs.New(errs.PolicyTwoFactorRequired,
			"This installation requires two-factor authentication on local accounts. Set it up to continue.")
	}
	return s.Store.MarkOffered(ctx, userID)
}

func freshCodes() (codes, digests []string, err error) {
	codes, err = newRecoveryCodes()
	if err != nil {
		return nil, nil, errs.Wrap(errs.Internal, "Could not make recovery codes.", err)
	}
	digests = make([]string, len(codes))
	for i, c := range codes {
		digests[i] = recoveryDigest(c)
	}
	return codes, digests, nil
}

func errWrongCode() error {
	return errs.New(errs.AuthInvalid, "That code is not right.").
		WithRemedy("Enter the 6-digit code your authenticator app shows now, or one of your recovery codes. " +
			"If codes keep failing, check that your phone's clock is set automatically.")
}
