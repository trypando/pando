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

// Host roles (R-414).
const (
	HostRoleControl = "control"
	HostRoleApp     = "app"
)

// JoinToken is a stored join token, without its secret (R-415).
type JoinToken struct {
	ID        string     `json:"id"`
	Roles     []string   `json:"roles"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// IssuedJoinToken is a freshly minted join token. Secret is "<id>.<secret>"
// and is present exactly once, here: only its keyed digest is stored (R-063).
type IssuedJoinToken struct {
	Token  JoinToken    `json:"token"`
	Secret secret.Value `json:"secret"`
}

// Host is one machine in the install's cluster (R-414).
type Host struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Roles      []string   `json:"roles"`
	Address    string     `json:"address"`
	CertSHA256 string     `json:"cert_sha256"`
	Platform   string     `json:"platform,omitempty"`
	Version    string     `json:"version,omitempty"`
	JoinedWith string     `json:"joined_with,omitempty"`
	JoinedAt   time.Time  `json:"joined_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`

	// PingAddress and PingCertSHA256 are where a control host answers the
	// cluster's liveness checks, its cluster listener, and the fingerprint
	// of the certificate it answers with (R-421, migration 000091). Empty is
	// the agent's Address and CertSHA256, which an app host answers on.
	// Address and CertSHA256 stay the agent's: runtime calls go through it
	// (R-416), so a host with both roles keeps running apps.
	PingAddress    string `json:"-"`
	PingCertSHA256 string `json:"-"`
}

// Cluster stores the cluster's hosts and its join tokens.
type Cluster struct {
	db *DB

	// key is the install's API token key: join token digests are made with it,
	// as API tokens' are, so a dump of join_tokens cannot be used to join.
	key []byte
}

// NewCluster returns the cluster store. key must be tokenkey.Size bytes.
func NewCluster(db *DB, key []byte) *Cluster {
	if len(key) != tokenkey.Size {
		panic("state.NewCluster: the API token key must be tokenkey.Size bytes")
	}
	return &Cluster{db: db, key: bytes.Clone(key)}
}

const joinTokenPrefix = "hmac-sha256:"

func (c *Cluster) digest(s string) string {
	return joinTokenPrefix + hex.EncodeToString(keyedMAC(c.key, s))
}

const joinTokenColumns = `id, roles, max_uses, uses, created_by, created_at, expires_at, revoked_at`

func scanJoinToken(row pgx.Row) (JoinToken, error) {
	var t JoinToken
	err := row.Scan(&t.ID, &t.Roles, &t.MaxUses, &t.Uses, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt)
	return t, err
}

// CreateJoinToken mints a join token for roles, usable maxUses times until
// expiresAt.
func (c *Cluster) CreateJoinToken(ctx context.Context, roles []string, maxUses int, createdBy string, expiresAt time.Time) (IssuedJoinToken, error) {
	tokenID := id.New(id.JoinToken)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return IssuedJoinToken{}, errs.Wrap(errs.Internal, "Could not generate a join token.", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	row := c.db.QueryRow(ctx, `
		INSERT INTO join_tokens (id, hash, roles, max_uses, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+joinTokenColumns,
		tokenID, c.digest(plaintext), roles, maxUses, createdBy, expiresAt)
	t, err := scanJoinToken(row)
	if err != nil {
		return IssuedJoinToken{}, errs.Wrap(errs.Internal, "Could not create the join token.", err)
	}
	return IssuedJoinToken{Token: t, Secret: secret.New(tokenID + "." + plaintext)}, nil
}

// JoinTokens lists every join token, newest first.
func (c *Cluster) JoinTokens(ctx context.Context) ([]JoinToken, error) {
	rows, err := c.db.Query(ctx, `SELECT `+joinTokenColumns+` FROM join_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the join tokens.", err)
	}
	defer rows.Close()
	out := []JoinToken{}
	for rows.Next() {
		t, err := scanJoinToken(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the join tokens.", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeJoinToken revokes a join token. Revoking one already revoked is not
// an error; one that does not exist is.
func (c *Cluster) RevokeJoinToken(ctx context.Context, tokenID string) (JoinToken, error) {
	row := c.db.QueryRow(ctx, `
		UPDATE join_tokens SET revoked_at = coalesce(revoked_at, now())
		WHERE id = $1
		RETURNING `+joinTokenColumns, tokenID)
	t, err := scanJoinToken(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, errs.Newf(errs.NotFound, "There is no join token %s.", tokenID)
	}
	if err != nil {
		return t, errs.Wrap(errs.Internal, "Could not revoke the join token.", err)
	}
	return t, nil
}

// errJoinRefused is every failure to redeem a join token. Saying which —
// unknown, expired, used up, revoked — would tell whoever holds a guess which
// part of it was right.
var errJoinRefused = errs.New(errs.AuthTokenInvalid, "This join token is not valid. It may have expired, been used or been revoked.").
	WithRemedy("Create a new join token on a host already in the cluster and start the join again.")

// RedeemJoinToken spends one use of the presented "<id>.<secret>" token and
// returns it, or refuses. The use is taken in the same statement that checks
// the token is live, so two hosts joining at once with a single-use token
// cannot both succeed.
func (c *Cluster) RedeemJoinToken(ctx context.Context, presented secret.Value) (JoinToken, error) {
	return c.redeem(ctx, c.db, presented)
}

const msgCheckJoinToken = "Could not check the join token."

func (c *Cluster) redeem(ctx context.Context, q querier, presented secret.Value) (JoinToken, error) {
	tokenID, plaintext, ok := strings.Cut(presented.Reveal(), ".")
	if !ok || !strings.HasPrefix(tokenID, string(id.JoinToken)+"_") || plaintext == "" {
		return JoinToken{}, errJoinRefused
	}

	var stored string
	err := q.QueryRow(ctx, `SELECT hash FROM join_tokens WHERE id = $1`, tokenID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return JoinToken{}, errJoinRefused
	}
	if err != nil {
		return JoinToken{}, errs.Wrap(errs.Internal, msgCheckJoinToken, err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(c.digest(plaintext))) != 1 {
		return JoinToken{}, errJoinRefused
	}

	row := q.QueryRow(ctx, `
		UPDATE join_tokens SET uses = uses + 1
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now() AND uses < max_uses
		RETURNING `+joinTokenColumns, tokenID)
	t, err := scanJoinToken(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return JoinToken{}, errJoinRefused
	}
	if err != nil {
		return JoinToken{}, errs.Wrap(errs.Internal, msgCheckJoinToken, err)
	}
	return t, nil
}

// Join spends one use of the presented token and records the host admit
// returns for it, in one transaction (R-415). admit sees the token, so the
// host gets the token's roles and nothing the joining host asked for. When
// admit or recording the host fails, the use is given back: a join refused
// for a name already taken can be tried again with the same token.
func (c *Cluster) Join(ctx context.Context, presented secret.Value, admit func(JoinToken) (Host, error)) (JoinToken, Host, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return JoinToken{}, Host{}, errs.Wrap(errs.Internal, msgCheckJoinToken, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t, err := c.redeem(ctx, tx, presented)
	if err != nil {
		return JoinToken{}, Host{}, err
	}
	h, err := admit(t)
	if err != nil {
		return t, Host{}, err
	}
	h.Roles = t.Roles
	h.JoinedWith = t.ID
	if h, err = addHost(ctx, tx, h); err != nil {
		return t, Host{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return t, Host{}, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	return t, h, nil
}

// Fresh reports whether this install has ever had an account or an app. A
// Pando that has either cannot join a cluster (R-415): that would be merging
// two installs, and two audit logs with them. Deleted ones count.
func (c *Cluster) Fresh(ctx context.Context) (accounts, apps bool, err error) {
	err = c.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users), EXISTS (SELECT 1 FROM apps)`).Scan(&accounts, &apps)
	if err != nil {
		return false, false, errs.Wrap(errs.Internal, "Could not check whether this installation is new.", err)
	}
	return accounts, apps, nil
}

const msgRecordHost = "Could not record the host."

const hostColumns = `id, name, roles, address, cert_sha256, platform, version, coalesce(joined_with, ''), joined_at, last_seen_at,
	ping_address, ping_cert_sha256`

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	err := row.Scan(&h.ID, &h.Name, &h.Roles, &h.Address, &h.CertSHA256, &h.Platform, &h.Version, &h.JoinedWith, &h.JoinedAt, &h.LastSeenAt,
		&h.PingAddress, &h.PingCertSHA256)
	return h, err
}

// AddHost records a host that has joined.
func (c *Cluster) AddHost(ctx context.Context, h Host) (Host, error) {
	return addHost(ctx, c.db, h)
}

// firstHostLock serializes recording the install's first host, so replicas
// starting together on one database record one host between them.
const firstHostLock int64 = 0x70616e646f03

// RecordFirstHost records h as the install's own first host (R-414) unless
// one is recorded already: a host no join token brought in. It reports
// whether it recorded h. Replicas of one install share the database and are
// one host between them; a host that joined is never recorded here.
func (c *Cluster) RecordFirstHost(ctx context.Context, h Host) (Host, bool, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return Host{}, false, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, firstHostLock); err != nil {
		return Host{}, false, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	var recorded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cluster_hosts WHERE joined_with IS NULL)`).Scan(&recorded); err != nil {
		return Host{}, false, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	if recorded {
		return Host{}, false, nil
	}
	h.JoinedWith = ""
	if h, err = addHost(ctx, tx, h); err != nil {
		return Host{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Host{}, false, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	return h, true, nil
}

func addHost(ctx context.Context, q querier, h Host) (Host, error) {
	if h.ID == "" {
		h.ID = id.New(id.Host)
	}
	row := q.QueryRow(ctx, `
		INSERT INTO cluster_hosts (id, name, roles, address, cert_sha256, platform, version, joined_with)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+hostColumns,
		h.ID, h.Name, h.Roles, h.Address, h.CertSHA256, h.Platform, h.Version, nullable(h.JoinedWith))
	out, err := scanHost(row)
	if err != nil {
		if isUniqueViolation(err) {
			return out, errs.Newf(errs.StateInvalid, "A host named %q is already in the cluster.", h.Name).
				WithRemedy("Give the joining host another name, or remove the old host first.")
		}
		return out, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	return out, nil
}

// Hosts lists the cluster's hosts, oldest first.
func (c *Cluster) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := c.db.Query(ctx, `SELECT `+hostColumns+` FROM cluster_hosts ORDER BY joined_at, id`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the cluster's hosts.", err)
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the cluster's hosts.", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Host reads one host.
func (c *Cluster) Host(ctx context.Context, hostID string) (Host, error) {
	h, err := scanHost(c.db.QueryRow(ctx, `SELECT `+hostColumns+` FROM cluster_hosts WHERE id = $1`, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
	}
	if err != nil {
		return h, errs.Wrap(errs.Internal, "Could not read the host.", err)
	}
	return h, nil
}

// RemoveHost removes a host from the cluster.
func (c *Cluster) RemoveHost(ctx context.Context, hostID string) (Host, error) {
	h, err := scanHost(c.db.QueryRow(ctx, `DELETE FROM cluster_hosts WHERE id = $1 RETURNING `+hostColumns, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
	}
	if err != nil {
		return h, errs.Wrap(errs.Internal, "Could not remove the host.", err)
	}
	return h, nil
}

// TouchHost records that a host was heard from.
func (c *Cluster) TouchHost(ctx context.Context, hostID string) error {
	if _, err := c.db.Exec(ctx, `UPDATE cluster_hosts SET last_seen_at = now() WHERE id = $1`, hostID); err != nil {
		return errs.Wrap(errs.Internal, "Could not record that the host was heard from.", err)
	}
	return nil
}
