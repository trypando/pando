package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// SessionRevoker ends sessions by account. It needs no key: ending a session
// reads no secret, so the host commands that reset a credential can end its
// sessions without the API token key (cmd/pando admin).
type SessionRevoker struct{ db *DB }

// NewSessionRevoker returns a store that can only end sessions.
func NewSessionRevoker(db *DB) *SessionRevoker { return &SessionRevoker{db: db} }

// RevokeAllForUser ends every session a user holds. Used when an adapter pushes
// a revocation (R-048), which is what makes immediate revocation possible.
func (s *SessionRevoker) RevokeAllForUser(ctx context.Context, userID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not end the account's sessions.", err)
	}
	return nil
}

// Sessions are server-side rows (design 02 §2.7). The cookie carries
// ses_<id>.<secret>: the ID is public — it is what the audit log, logs and the
// API name a session by — and the secret is what signs a browser in. The
// table holds only the secret's HMAC under the API token key, as tokens do
// (R-063), so neither the audit log nor a copy of the table hands anyone a
// working cookie.
type Sessions struct {
	*SessionRevoker

	// key is the API token key (core/tokenkey), kept outside the database.
	key []byte
}

// NewSessions returns the session store. key is the install's API token key
// and must be tokenkey.Size bytes, for the same reason as NewTokens.
func NewSessions(db *DB, key []byte) *Sessions {
	if len(key) != tokenkey.Size {
		panic("state.NewSessions: the API token key must be tokenkey.Size bytes")
	}
	return &Sessions{SessionRevoker: NewSessionRevoker(db), key: bytes.Clone(key)}
}

// Session is an active login. ID is its public name; it is not a credential.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	AdapterID string    `json:"adapter_id"`
	ExpiresAt time.Time `json:"expires_at"`

	// IssuedAt is when its holder signed in. A session younger than a few
	// minutes changes its own second factor without the password again
	// (R-405): its holder has just typed it.
	IssuedAt time.Time `json:"issued_at"`
}

// Started is a session just created. Cookie is the value of the session
// cookie, present here and nowhere else: only its digest is stored (R-063).
// It is a secret.Value, so it is redacted wherever this struct is logged or
// serialized (R-194).
type Started struct {
	Session Session      `json:"session"`
	Cookie  secret.Value `json:"cookie"`
}

// sessionSecretBytes is the size of a session's secret: 256 bits.
const sessionSecretBytes = 32

// Create issues a session.
func (s *Sessions) Create(ctx context.Context, userID, adapterID string, lifetime time.Duration, userAgent, ip string) (Started, error) {
	raw := make([]byte, sessionSecretBytes)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error (Go 1.24+)
	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now().UTC()
	sess := Session{
		ID:        id.New(id.Session),
		UserID:    userID,
		AdapterID: adapterID,
		ExpiresAt: now.Add(lifetime),
		IssuedAt:  now,
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO sessions (id, secret_hash, user_id, adapter_id, expires_at, user_agent, ip, issued_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		sess.ID, s.digest(plaintext), userID, adapterID, sess.ExpiresAt, nullable(userAgent), nullableInet(ip), now)
	if err != nil {
		return Started{}, errs.Wrap(errs.Internal, "Could not start a session.", err)
	}
	return Started{Session: sess, Cookie: secret.New(sess.ID + "." + plaintext)}, nil
}

func (s *Sessions) digest(plaintext string) string {
	return apiTokenPrefix + hex.EncodeToString(keyedMAC(s.key, plaintext))
}

// splitCookie parses a session cookie into its public ID and its secret. ok
// is false for anything not shaped like one, a bare session ID included.
func splitCookie(cookie string) (sessionID, plaintext string, ok bool) {
	sessionID, plaintext, found := strings.Cut(cookie, ".")
	if !found || plaintext == "" || !id.Is(id.Session, sessionID) {
		return "", "", false
	}
	return sessionID, plaintext, true
}

// matches reports, in constant time, whether plaintext is the secret a
// stored digest was made from.
func (s *Sessions) matches(plaintext, stored string) bool {
	want := s.digest(plaintext)
	return subtle.ConstantTimeCompare([]byte(want), []byte(stored)) == 1
}

// Active returns the session a cookie carries, if its secret is right and the
// session is unrevoked and unexpired.
//
// The API checks this on every request. The proxy reads it with the account
// and groups in one query (ActiveWithUser) and keeps the answer until a NOTIFY
// says something changed, never more than 30 seconds — a bound that is in
// design 06 §3.1's revocation window, which is a stated number rather than an
// accident (issue #93).
func (s *Sessions) Active(ctx context.Context, cookie string) (Session, bool, error) {
	sessionID, plaintext, ok := splitCookie(cookie)
	if !ok {
		return Session{}, false, nil
	}
	var (
		sess   Session
		stored string
	)
	err := s.db.QueryRow(ctx, `
		SELECT id, secret_hash, user_id, adapter_id, expires_at, issued_at
		FROM sessions
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()`,
		sessionID).Scan(&sess.ID, &stored, &sess.UserID, &sess.AdapterID, &sess.ExpiresAt, &sess.IssuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, errs.Wrap(errs.Internal, "Could not read the session.", err)
	}
	if !s.matches(plaintext, stored) {
		return Session{}, false, nil
	}
	return sess, true, nil
}

// Revoke ends one session, by its public ID. A caller holding only a cookie
// resolves it with Active first, so that a cookie naming someone else's
// session with the wrong secret ends nothing.
func (s *Sessions) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not end the session.", err)
	}
	return nil
}

// RevokeOthersForUser ends every session a user holds except one, named by
// its public ID.
//
// For a password change: the sessions someone did not know about should end,
// and the one they are typing in should not — being signed out by your own
// password change teaches people that changing it is a risky thing to do.
func (s *Sessions) RevokeOthersForUser(ctx context.Context, userID, keepSessionID string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, userID, keepSessionID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not end the account's other sessions.", err)
	}
	return nil
}
