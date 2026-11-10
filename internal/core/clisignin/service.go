// Package clisignin signs the CLI in through the browser (R-406, design 06
// §3.5).
//
// The shape is OAuth 2.0's authorization code grant for a native app (RFC
// 8252) with PKCE (RFC 7636), with Pando as its own authorization server and
// the console as its consent page:
//
//  1. `pando login` makes a verifier and sends only its SHA-256, the
//     challenge, to the console in the address it opens.
//  2. The person, signed in to the console however their account signs in —
//     password and second factor, or an identity provider — approves it.
//     Approve stores the challenge against their account and answers with a
//     one-time code, which the browser carries to the CLI's loopback address,
//     or which the person pastes into the CLI.
//  3. The CLI trades the code and the verifier for a delegated token
//     (Exchange).
//
// `pando token create` uses the same approval to mint a token it prints rather
// than stores (issue #396): the CLI only ever holds a token, and a token
// cannot mint a token, so a signed-in person approves each one. With
// KindService the token is a service token (R-060) rather than a delegated
// one, and the person approving must hold install.tokens.manage both when they
// approve and when the CLI collects it.
//
// The code alone is worth nothing: a code that reaches the wrong hands — a
// link sent to someone, a page that read the loopback redirect — cannot be
// traded without the verifier, which never left the process that started the
// sign-in. That is this flow's version of R-043's rule that a sign-in finishes
// only where it started.
package clisignin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// CodeLifetime is how long an approval waits for the CLI [P]: long enough to
// copy a code from one window and paste it into a terminal, short enough that
// one left on a screen is soon worth nothing.
const CodeLifetime = 5 * time.Minute

// Store keeps approvals (state.CLISignIns).
type Store interface {
	Create(ctx context.Context, s state.CLISignIn) error
	Take(ctx context.Context, codeHash string) (state.CLISignIn, bool, error)
}

// AccountReader reads accounts (state.Users).
type AccountReader interface {
	ByID(ctx context.Context, userID string) (state.User, bool, error)
}

// TokenMinter mints tokens (state.Tokens).
type TokenMinter interface {
	Create(ctx context.Context, kind, name, ownerUserID, createdBy string, expiresAt *time.Time) (state.Issued, error)
}

// Service approves CLI sign-ins and trades them for tokens. The caller has
// established that a signed-in person, not a token, approved, and audits what
// happened.
type Service struct {
	Store  Store
	Users  AccountReader
	Tokens TokenMinter
	Policy interface {
		Load(ctx context.Context) (policy.Document, error)
	}
	Clock clock.Clock

	// MayCreateServiceTokens refuses an account that may not create service
	// tokens (install.tokens.manage, R-060). Nil refuses every service token.
	MayCreateServiceTokens func(ctx context.Context, userID string) error
}

// The kinds of token an approval makes.
const (
	// KindDelegated acts as the person who approved it (R-058). The default.
	KindDelegated = "delegated"

	// KindService is its own principal, holding only what is shared with it
	// (R-060).
	KindService = "service"
)

// Request is what the CLI put in the address it opened, as the console sends
// it on.
type Request struct {
	// CodeChallenge is base64url(SHA-256(verifier)), unpadded.
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`

	// Name is the token's name: the machine's, "laptop CLI".
	Name string `json:"name"`

	// Port is the loopback port the CLI is listening on, or 0 when it waits
	// for the code to be pasted.
	Port int `json:"port"`

	// State is echoed to the CLI's loopback address, so it can tell its own
	// sign-in's answer from any other request that reaches the port.
	State string `json:"state"`

	// Kind is KindDelegated, or empty for it, or KindService.
	Kind string `json:"kind,omitempty"`
}

// Approved is an approval's one-time code and where the browser takes it.
type Approved struct {
	Code secret.Value

	// RedirectURL is the CLI's loopback address with the code on it, or
	// empty when the person pastes the code instead.
	RedirectURL string

	ExpiresAt time.Time
}

// Exchanged is a token made for an approved sign-in.
type Exchanged struct {
	Issued state.Issued

	// UserID and Username are the account the token acts as (R-058).
	UserID   string
	Username string

	// ApprovedBy is the session that approved it.
	ApprovedBy string
	Name       string

	// Kind is KindDelegated or KindService.
	Kind string
}

var (
	// challengeShape is an unpadded base64url SHA-256: 43 characters.
	challengeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

	// verifierShape is RFC 7636 §4.1's: 43 to 128 unreserved characters.
	verifierShape = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

	// stateShape keeps what is echoed into the redirect plain.
	stateShape = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,128}$`)
)

func invalid(message string) error {
	return errs.New(errs.ValidInvalid, message).
		WithRemedy("Run pando login again, and open the address it prints.")
}

// validate checks a request and returns its token name, trimmed.
func validate(req Request) (string, error) {
	if req.Kind != "" && req.Kind != KindDelegated && req.Kind != KindService {
		return "", invalid(fmt.Sprintf("This CLI sign-in asks for a %q token. Pando makes delegated and service tokens.", req.Kind))
	}
	if req.CodeChallengeMethod != "S256" {
		return "", invalid("This CLI sign-in asks for a code challenge method other than S256, the only one Pando accepts.")
	}
	if !challengeShape.MatchString(req.CodeChallenge) {
		return "", invalid("This CLI sign-in's code challenge is not an unpadded base64url SHA-256 digest.")
	}
	if req.Port != 0 && (req.Port < 1024 || req.Port > 65535) {
		return "", invalid(fmt.Sprintf("This CLI sign-in names port %d. A CLI listens on a port from 1024 to 65535, or on none when its code is pasted.", req.Port))
	}
	if req.Port != 0 && !stateShape.MatchString(req.State) {
		return "", invalid("This CLI sign-in has no valid state: 16 to 128 letters, digits, or . _ ~ -.")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 200 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", invalid("This CLI sign-in's token name must be 1 to 200 characters with no control characters.")
	}
	return name, nil
}

// Digest is what is stored of a code: its SHA-256, hex.
func Digest(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// Challenge is the S256 code challenge for a verifier (RFC 7636 §4.2).
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Approve records that userID, signed in as approvedBy, lets the CLI that made
// req sign in as them.
func (s *Service) Approve(ctx context.Context, userID, approvedBy string, req Request) (Approved, error) {
	name, err := validate(req)
	if err != nil {
		return Approved{}, err
	}
	if err := s.usable(ctx, userID); err != nil {
		return Approved{}, err
	}
	kind := state.TokenDelegated
	if req.Kind == KindService {
		if err := s.mayCreateServiceTokens(ctx, userID); err != nil {
			return Approved{}, err
		}
		kind = state.TokenAccount
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Approved{}, errs.Wrap(errs.Internal, "Could not make a sign-in code.", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	expires := s.Clock.Now().Add(CodeLifetime)

	if err := s.Store.Create(ctx, state.CLISignIn{
		CodeHash: Digest(code), UserID: userID, ApprovedBy: approvedBy,
		CodeChallenge: req.CodeChallenge, TokenName: name, TokenKind: kind, ExpiresAt: expires,
	}); err != nil {
		return Approved{}, err
	}

	out := Approved{Code: secret.New(code), ExpiresAt: expires}
	if req.Port != 0 {
		// Only ever the loopback address, built here from a number: the
		// browser is never sent anywhere the request spelled out (RFC 8252
		// §7.3). 127.0.0.1 rather than localhost, which a hosts file or a
		// resolver can point elsewhere (§8.3).
		q := url.Values{"code": {code}, "state": {req.State}}
		out.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback?%s", req.Port, q.Encode())
	}
	return out, nil
}

// spent is the one answer to every code that cannot be traded, so trying one
// says nothing about why.
func spent() error {
	return errs.New(errs.AuthInvalid,
		"This sign-in code is not valid. It may have expired, been used already, or been approved for a different pando login.").
		WithRemedy("Run pando login again. A code works once, within 5 minutes of being approved, and only in the pando login that opened the browser.")
}

// Exchange trades a code and the verifier behind its challenge for the token
// it was approved for. The approval is gone whatever the outcome. When a code
// that named an approval is refused, the Exchanged returned with the error
// says whose it was (UserID, ApprovedBy, Name); for any other refusal it is
// empty.
func (s *Service) Exchange(ctx context.Context, code, verifier string) (Exchanged, error) {
	if code == "" || !verifierShape.MatchString(verifier) {
		return Exchanged{}, spent()
	}
	approval, found, err := s.Store.Take(ctx, Digest(code))
	if err != nil {
		return Exchanged{}, err
	}
	if !found {
		return Exchanged{}, spent()
	}

	// From here a refusal spent a real approval, and says whose: the caller
	// audits that, and only that. A code that matched nothing is refused
	// without a trace, so nobody can grow the audit log by posting guesses
	// to a public endpoint; every approval took a signed-in person to make.
	kind := KindDelegated
	if approval.TokenKind == state.TokenAccount {
		kind = KindService
	}
	refused := Exchanged{UserID: approval.UserID, ApprovedBy: approval.ApprovedBy, Name: approval.TokenName, Kind: kind}
	if !s.Clock.Now().Before(approval.ExpiresAt) ||
		subtle.ConstantTimeCompare([]byte(Challenge(verifier)), []byte(approval.CodeChallenge)) != 1 {
		return refused, spent()
	}

	// Approved minutes ago, but the account may have been suspended since.
	user, err := s.user(ctx, approval.UserID)
	if err != nil {
		return refused, err
	}

	// A service token belongs to nobody (R-060), so the person who approved
	// it is not its owner; and they may have lost the verb since.
	owner, tokenKind := user.ID, state.TokenDelegated
	if kind == KindService {
		if err := s.mayCreateServiceTokens(ctx, user.ID); err != nil {
			return refused, err
		}
		owner, tokenKind = "", state.TokenAccount
	}

	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return Exchanged{}, err
	}
	issued, err := s.Tokens.Create(ctx, tokenKind, approval.TokenName, owner,
		approval.ApprovedBy, doc.TokenExpiry(s.Clock.Now()))
	if err != nil {
		return Exchanged{}, err
	}
	return Exchanged{
		Issued: issued, UserID: user.ID, Username: user.ExternalID,
		ApprovedBy: approval.ApprovedBy, Name: approval.TokenName, Kind: kind,
	}, nil
}

func (s *Service) mayCreateServiceTokens(ctx context.Context, userID string) error {
	if s.MayCreateServiceTokens == nil {
		return errs.New(errs.PermDenied, "This installation does not create service tokens from the CLI.").
			WithRemedy("Create the service token in the console, under API and tools.")
	}
	return s.MayCreateServiceTokens(ctx, userID)
}

func (s *Service) usable(ctx context.Context, userID string) error {
	user, err := s.user(ctx, userID)
	if err != nil {
		return err
	}
	// R-046: the generated password is changed before anything else, and a
	// token made around it would outlive the change.
	if user.MustChangePassword {
		return errs.New(errs.PermDenied, "This account must change its password before it can sign in the CLI.").
			WithRemedy("Choose a new password in the console, then run pando login again.")
	}
	return nil
}

func (s *Service) user(ctx context.Context, userID string) (state.User, error) {
	user, found, err := s.Users.ByID(ctx, userID)
	if err != nil {
		return state.User{}, err
	}
	if !found || user.Status != "active" {
		return state.User{}, errs.New(errs.AuthInvalid, "This account is not active, so it cannot sign in the CLI.").
			WithRemedy("Ask an administrator of this Pando installation to reactivate it.")
	}
	return user, nil
}
