package state

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// Token kinds (R-058, R-060).
const (
	TokenDelegated = "delegated"
	TokenAccount   = "account"
)

// Tokens stores API tokens.
type Tokens struct {
	db *DB

	// key is what token digests are made with (core/tokenkey). It is not in
	// the database, so a dump of the tokens table cannot test a guess.
	key []byte
}

// NewTokens returns the token store. key is the install's API token key
// (tokenkey.LoadOrCreate), and must be tokenkey.Size bytes: a store without
// one could neither issue a token nor check one, and finding that out at the
// first request is later than at start.
func NewTokens(db *DB, key []byte) *Tokens {
	if len(key) != tokenkey.Size {
		panic("state.NewTokens: the API token key must be tokenkey.Size bytes")
	}
	return &Tokens{db: db, key: bytes.Clone(key)}
}

// Token is a stored token, without its secret.
type Token struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	OwnerUserID string     `json:"owner_user_id,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// Issued is a freshly minted token. Secret is present exactly once, here
// (R-063) — it is hashed on the way into the database and cannot be recovered.
//
// Secret is a secret.Value, so serializing this struct redacts it. A handler
// returning the plaintext to its one legitimate caller must reveal it
// deliberately, which is the point: there is no path where it leaks by
// forgetting.
type Issued struct {
	Token  Token        `json:"token"`
	Secret secret.Value `json:"secret"`
}

// Create mints a token.
//
// A delegated token requires an owner and holds no grants of its own: it
// resolves through the owner on every request (R-058, R-059). An account token
// has no owner and is its own principal in grants (R-060). The database enforces
// both shapes, so a caller cannot produce a token that is neither.
func (t *Tokens) Create(ctx context.Context, kind, name, ownerUserID, createdBy string, expiresAt *time.Time) (Issued, error) {
	switch kind {
	case TokenDelegated:
		if ownerUserID == "" {
			return Issued{}, errs.New(errs.ValidInvalid, "A delegated token needs an owner.")
		}
	case TokenAccount:
		if ownerUserID != "" {
			return Issued{}, errs.New(errs.ValidInvalid, "An account token acts as itself and cannot have an owner.")
		}
	default:
		return Issued{}, errs.New(errs.ValidInvalid, "A token is either delegated or an account token.")
	}

	tokenID := id.New(id.Token)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Issued{}, errs.Wrap(errs.Internal, "Could not generate a token.", err)
	}
	plaintext := secret.New(base64.RawURLEncoding.EncodeToString(raw))

	digest := t.digest(plaintext)

	_, err := t.db.Exec(ctx, `
		INSERT INTO tokens (id, kind, name, hash, owner_user_id, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tokenID, kind, name, digest, nullable(ownerUserID), createdBy, expiresAt)
	if err != nil {
		return Issued{}, errs.Wrap(errs.Internal, "Could not create the token.", err)
	}

	return Issued{
		Token:  Token{ID: tokenID, Kind: kind, Name: name, OwnerUserID: ownerUserID, ExpiresAt: expiresAt},
		Secret: secret.New(tokenID + "." + plaintext.Reveal()),
	}, nil
}

// Authenticate resolves a presented token string.
//
// The string is "<id>.<secret>". Splitting on the ID lets the digest be looked
// up directly rather than compared against every row.
//
// Every failure returns the same error. Distinguishing "no such token" from
// "wrong secret" would let a caller enumerate valid token IDs.
func (t *Tokens) Authenticate(ctx context.Context, presented secret.Value) (Token, error) {
	tokenID, plaintext, found := strings.Cut(presented.Reveal(), ".")
	if !found || !id.Is(id.Token, tokenID) {
		return Token{}, errInvalidToken()
	}

	var (
		tok       Token
		digest    string
		owner     *string
		expiresAt *time.Time
		revokedAt *time.Time
		stale     bool
	)
	err := t.db.QueryRow(ctx, `
		SELECT id, kind, name, hash, owner_user_id, expires_at, revoked_at,
		       last_used_at IS NULL OR last_used_at < now() - make_interval(secs => $2)
		FROM tokens WHERE id = $1`, tokenID, lastUsedResolution.Seconds()).
		Scan(&tok.ID, &tok.Kind, &tok.Name, &digest, &owner, &expiresAt, &revokedAt, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, errInvalidToken()
	}
	if err != nil {
		return Token{}, errs.Wrap(errs.Internal, "Could not read the token.", err)
	}

	presentedSecret := secret.New(plaintext)
	ok, rehash := t.verify(presentedSecret, digest)
	if !ok {
		return Token{}, errInvalidToken()
	}

	// Step 3 of the evaluation order: token validity. Revocation and expiry are
	// checked here; the owner's status is step 4 and belongs to the authorizer,
	// which does it live on every request.
	if revokedAt != nil {
		return Token{}, errInvalidToken()
	}
	if expiresAt != nil && expiresAt.Before(time.Now().UTC()) {
		return Token{}, errInvalidToken()
	}

	if owner != nil {
		tok.OwnerUserID = *owner
	}
	tok.ExpiresAt = expiresAt

	// A token stored before tokens were HMAC-SHA-256 — argon2id, or the
	// unkeyed SHA-256 an earlier build for issue #93 wrote — is rewritten the
	// first time it is used, so its next use costs an HMAC and not an
	// argon2id, and the table stops holding a digest a dump alone could test
	// guesses against. Only if the row still holds the digest just verified:
	// a rotation racing this one wins. Best effort, like last use below.
	if rehash {
		_, _ = t.db.Exec(ctx, `UPDATE tokens SET hash = $2 WHERE id = $1 AND hash = $3`,
			tokenID, t.digest(presentedSecret), digest)
	}

	// R-062. Written when the stored time is older than lastUsedResolution,
	// not on every request: a token used in a loop otherwise writes its row
	// once per call. Best effort: a failure to record last use must not deny a
	// valid request.
	if stale {
		_, _ = t.db.Exec(ctx, `
			UPDATE tokens SET last_used_at = now()
			WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - make_interval(secs => $2))`,
			tokenID, lastUsedResolution.Seconds())
	}

	return tok, nil
}

// lastUsedResolution is how stale a token's last_used_at may be before a use
// writes it again (R-062). The column says when a token was last used to
// within this, which is what reviewing stale credentials needs.
const lastUsedResolution = time.Minute

// Prefixes of the digests the tokens table holds. A digest with neither is an
// argon2id one, from before issue #93.
const (
	// apiTokenPrefix is the current form: HMAC-SHA-256 under the token key.
	apiTokenPrefix = "hmac-sha256:"

	// legacySHA256Prefix is unkeyed SHA-256, which an earlier build for issue
	// #93 wrote. Still accepted, and rewritten on use.
	legacySHA256Prefix = "sha256:"
)

// digest is what the tokens table stores for a token's secret, so a leaked
// table hands nobody a working token (R-063).
//
// HMAC-SHA-256 under the install's token key (core/tokenkey), which is kept
// in a file and not in the database. Not argon2id, which passwords get: the
// secret is 256 bits Create generated, not anything a person chose, and
// argon2id cost 64 MiB and tens of milliseconds on every request a token
// made, which anyone who knew a token's ID could make Pando pay (issue #93).
// Keyed rather than plain SHA-256 so that a database dump alone — a copied
// Postgres backup, a read-only SQL injection — cannot be used to check a
// candidate secret against any row: that also takes the key file.
func (t *Tokens) digest(v secret.Value) string {
	return apiTokenPrefix + hex.EncodeToString(t.mac(v.Reveal()))
}

func (t *Tokens) mac(s string) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// verify reports whether v is the secret a stored digest was made from, and
// whether that digest is in an older form that should be rewritten. Constant
// time for an HMAC or SHA-256 digest; argon2id's own comparison is too.
func (t *Tokens) verify(v secret.Value, stored string) (ok, rehash bool) {
	if digest, isHMAC := strings.CutPrefix(stored, apiTokenPrefix); isHMAC {
		want := hex.EncodeToString(t.mac(v.Reveal()))
		return subtle.ConstantTimeCompare([]byte(want), []byte(digest)) == 1, false
	}
	if digest, isSHA := strings.CutPrefix(stored, legacySHA256Prefix); isSHA {
		want := tokenHash(v.Reveal())
		return subtle.ConstantTimeCompare([]byte(want), []byte(digest)) == 1, true
	}
	matched, err := hash.Verify(v, stored)
	if err != nil || !matched {
		return false, false
	}
	return true, true
}

// Active returns a token that is neither revoked nor expired, by ID alone.
//
// For authorizing work a token set going rather than a request it is making
// now — an account token's event subscription (R-368) — where there is no
// secret to present. Not a way to authenticate.
func (t *Tokens) Active(ctx context.Context, tokenID string) (Token, bool, error) {
	var tok Token
	var owner *string
	err := t.db.QueryRow(ctx, `
		SELECT id, kind, name, owner_user_id, expires_at
		FROM tokens
		WHERE id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`, tokenID).
		Scan(&tok.ID, &tok.Kind, &tok.Name, &owner, &tok.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, errs.Wrap(errs.Internal, "Could not read the token.", err)
	}
	if owner != nil {
		tok.OwnerUserID = *owner
	}
	return tok, true, nil
}

// Revoke revokes a token.
func (t *Tokens) Revoke(ctx context.Context, tokenID string) error {
	_, err := t.db.Exec(ctx,
		`UPDATE tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, tokenID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not revoke the token.", err)
	}
	return nil
}

func errInvalidToken() error {
	return errs.New(errs.AuthTokenInvalid, "That token is not valid.")
}

// ListForUser returns a user's own tokens, newest first.
//
// Delegated tokens only — an account token has no owner and belongs to whoever
// administers the install, not to a person (R-060). Revoked ones are included,
// because "this token was revoked" is what someone checking after an incident
// needs to see; hiding them would make the list agree with nothing.
func (t *Tokens) ListForUser(ctx context.Context, userID string) ([]Token, error) {
	rows, err := t.db.Query(ctx, `
		SELECT id, kind, name, coalesce(owner_user_id, ''), expires_at, last_used_at, revoked_at
		FROM tokens
		WHERE owner_user_id = $1
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read your tokens.", err)
	}
	defer rows.Close()

	out := make([]Token, 0)
	for rows.Next() {
		var tok Token
		if err := rows.Scan(&tok.ID, &tok.Kind, &tok.Name, &tok.OwnerUserID,
			&tok.ExpiresAt, &tok.LastUsedAt, &tok.RevokedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read your tokens.", err)
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// ListAccounts lists account-level tokens (R-060).
//
// Separate from ListForUser because these have no owner: an account token is
// its own principal, appears in grants under its own ID, and outlives whoever
// created it. "Every token in the install" is not a list anyone needs and is
// not offered — a delegated token belongs to the person who made it, and
// showing all of them to an administrator would make a screen out of other
// people's credentials.
func (t *Tokens) ListAccounts(ctx context.Context) ([]Token, error) {
	rows, err := t.db.Query(ctx, `
		SELECT id, kind, name, expires_at, last_used_at, revoked_at
		FROM tokens
		WHERE kind = $1 AND revoked_at IS NULL
		ORDER BY id`, TokenAccount)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list service tokens.", err)
	}
	defer rows.Close()

	out := make([]Token, 0)
	for rows.Next() {
		var tok Token
		if err := rows.Scan(&tok.ID, &tok.Kind, &tok.Name, &tok.ExpiresAt, &tok.LastUsedAt, &tok.RevokedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list service tokens.", err)
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// Owner returns a token's owner, for checking who may revoke it.
func (t *Tokens) Owner(ctx context.Context, tokenID string) (string, bool, error) {
	var owner *string
	err := t.db.QueryRow(ctx,
		`SELECT owner_user_id FROM tokens WHERE id = $1`, tokenID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errs.Wrap(errs.Internal, "Could not read the token.", err)
	}
	if owner == nil {
		return "", true, nil
	}
	return *owner, true, nil
}
