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
	// PrevCertSHA256 is the certificate the host held before its latest
	// renewal, still its own until it presents the new one (R-421).
	PrevCertSHA256 string `json:"-"`
	// Lost is that the host is marked lost (R-421, lost_at).
	Lost bool `json:"-"`

	// PingAddress and PingCertSHA256 are where a control host answers the
	// cluster's liveness checks, its cluster listener, and the fingerprint
	// of the certificate it answers with (R-421, migration 000091). Empty is
	// the agent's Address and CertSHA256, which an app host answers on.
	// Address, CertSHA256 and PrevCertSHA256 stay the agent's: runtime calls
	// go through it (R-416, Pins), so a host with both roles keeps running
	// apps.
	PingAddress    string `json:"-"`
	PingCertSHA256 string `json:"-"`

	// Pending is a host whose join is recorded and that has not yet
	// presented the certificate it was issued (migration 000093): it is not
	// listed (Hosts) and takes no part in the cluster until it does
	// (ConfirmJoin), and its row is removed after PendingJoinLifetime or when
	// its name joins again (R-415).
	Pending bool `json:"-"`
	// NextCertPEM is the certificate the host's latest renewal answered
	// with, until the host presents it (R-421): a renewal presenting the
	// certificate before it, for the same key, is answered with it again.
	NextCertPEM []byte `json:"-"`
}

// Pins is every certificate fingerprint that is the host's agent's now,
// comma separated, newest first: what a connection to its agent accepts
// (hostagent.PinnedClientTLS).
func (h Host) Pins() string {
	if h.PrevCertSHA256 == "" || h.PrevCertSHA256 == h.CertSHA256 {
		return h.CertSHA256
	}
	return h.CertSHA256 + "," + h.PrevCertSHA256
}

// HoldsCert reports whether the certificate with fingerprint sha is the
// host's: the one it joined or last renewed with, the one before it, or the
// one a control host answers pings with.
func (h Host) HoldsCert(sha string) bool {
	return sha != "" && (sha == h.CertSHA256 || sha == h.PrevCertSHA256 || sha == h.PingCertSHA256)
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

// JoinTokens lists every join token, newest first, with the uses of joins
// pending past their lifetime given back first (forgetPending).
func (c *Cluster) JoinTokens(ctx context.Context) ([]JoinToken, error) {
	if err := forgetPendingJoins(ctx, c.db, ""); err != nil {
		return nil, err
	}
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
	tokenID, err := c.verify(ctx, q, presented)
	if err != nil {
		return JoinToken{}, err
	}
	return spend(ctx, q, tokenID)
}

// verify checks the presented "<id>.<secret>" token's secret against its
// stored digest, and returns its ID. It reads nothing else and locks nothing.
func (c *Cluster) verify(ctx context.Context, q querier, presented secret.Value) (string, error) {
	tokenID, plaintext, ok := strings.Cut(presented.Reveal(), ".")
	if !ok || !strings.HasPrefix(tokenID, string(id.JoinToken)+"_") || plaintext == "" {
		return "", errJoinRefused
	}
	var stored string
	err := q.QueryRow(ctx, `SELECT hash FROM join_tokens WHERE id = $1`, tokenID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errJoinRefused
	}
	if err != nil {
		return "", errs.Wrap(errs.Internal, msgCheckJoinToken, err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(c.digest(plaintext))) != 1 {
		return "", errJoinRefused
	}
	return tokenID, nil
}

// spend takes one use of a live token, in the statement that checks it is
// live, so two hosts joining at once with a single-use token cannot both
// succeed.
func spend(ctx context.Context, q querier, tokenID string) (JoinToken, error) {
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

// CheckJoinToken checks the presented token without spending a use or
// locking anything (R-415): its secret, and that it is neither revoked nor
// expired. Whether a use is left is checked when Join spends one, after the
// uses of pending joins past their lifetime are given back. A join makes
// every slow check between the two, holding no connection meanwhile.
func (c *Cluster) CheckJoinToken(ctx context.Context, presented secret.Value) (JoinToken, error) {
	tokenID, err := c.verify(ctx, c.db, presented)
	if err != nil {
		return JoinToken{}, err
	}
	t, err := scanJoinToken(c.db.QueryRow(ctx, `
		SELECT `+joinTokenColumns+` FROM join_tokens
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()`, tokenID))
	if errors.Is(err, pgx.ErrNoRows) {
		return JoinToken{}, errJoinRefused
	}
	if err != nil {
		return JoinToken{}, errs.Wrap(errs.Internal, msgCheckJoinToken, err)
	}
	return t, nil
}

// PendingJoinLifetime is how long a host's join stays pending before it is
// forgotten (R-415 [P]): the joining host confirms it as soon as it has
// written what it was given (hosts.Joiner), so a join still pending this long
// failed on the joining side.
const PendingJoinLifetime = 10 * time.Minute

// forgetPending removes the pending joins of the host named $1 and every one
// older than $2 seconds (R-415): their certificates are revoked, so a joining
// host that was slow cannot confirm one later, and the uses of their join
// tokens are given back.
const forgetPending = `
	WITH gone AS (
	    DELETE FROM cluster_hosts
	    WHERE confirmed_at IS NULL AND (name = $1 OR joined_at < now() - make_interval(secs => $2))
	    RETURNING id, joined_with, cert_sha256
	), revoked AS (
	    INSERT INTO cluster_revoked_certs (cert_sha256, host_id)
	    SELECT cert_sha256, id FROM gone
	    ON CONFLICT (cert_sha256) DO NOTHING
	)
	UPDATE join_tokens t SET uses = greatest(t.uses - g.n, 0)
	FROM (SELECT joined_with, count(*)::int AS n FROM gone WHERE joined_with IS NOT NULL GROUP BY joined_with) g
	WHERE t.id = g.joined_with`

func forgetPendingJoins(ctx context.Context, q Execer, name string) error {
	if _, err := q.Exec(ctx, forgetPending, name, PendingJoinLifetime.Seconds()); err != nil {
		return errs.Wrap(errs.Internal, "Could not forget the joins that were never confirmed.", err)
	}
	return nil
}

// Join spends one use of the presented token and records h, pending, with
// the token's roles, in one short transaction (R-415): whatever is slow — the
// cluster's readiness, the runtime's checks, issuing the host's certificates
// — is done before it (CheckJoinToken), so the token's row is locked only
// for the spend and the insert. A pending join of the same name, or one past
// PendingJoinLifetime, is forgotten first and its use given back, so a join
// that failed on the joining side can be tried again under its name. When
// recording the host fails, the use is given back too. The host is listed
// once it confirms the join (ConfirmJoin).
func (c *Cluster) Join(ctx context.Context, presented secret.Value, h Host) (JoinToken, Host, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return JoinToken{}, Host{}, errs.Wrap(errs.Internal, msgCheckJoinToken, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tokenID, err := c.verify(ctx, tx, presented)
	if err != nil {
		return JoinToken{}, Host{}, err
	}
	if err := forgetPendingJoins(ctx, tx, h.Name); err != nil {
		return JoinToken{}, Host{}, err
	}
	t, err := spend(ctx, tx, tokenID)
	if err != nil {
		return JoinToken{}, Host{}, err
	}
	h.Roles = t.Roles
	h.JoinedWith = t.ID
	if h, err = addHost(ctx, tx, h, true); err != nil {
		return t, Host{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return t, Host{}, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	return t, h, nil
}

// ConfirmJoin records that the host whose join is pending presented the
// certificate its join issued (R-415): from here it is listed and takes part
// in the cluster. It reports whether that changed anything.
func (c *Cluster) ConfirmJoin(ctx context.Context, hostID string) (bool, error) {
	tag, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts SET confirmed_at = now(), updated_at = now()
		WHERE id = $1 AND confirmed_at IS NULL`, hostID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record that the host finished joining.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// HostByCert is the host, pending or not, holding the certificate with
// fingerprint sha: the one it joined or last renewed with, the one before it,
// or the one a control host answers pings with. Whether sha is revoked is the
// caller's to check (CertRevoked).
func (c *Cluster) HostByCert(ctx context.Context, sha string) (Host, bool, error) {
	if sha == "" {
		return Host{}, false, nil
	}
	h, err := scanHost(c.db.QueryRow(ctx, `
		SELECT `+hostColumns+` FROM cluster_hosts
		WHERE cert_sha256 = $1 OR prev_cert_sha256 = $1 OR ping_cert_sha256 = $1
		ORDER BY confirmed_at IS NULL LIMIT 1`, sha))
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, false, nil
	}
	if err != nil {
		return Host{}, false, errs.Wrap(errs.Internal, "Could not read which host holds the certificate.", err)
	}
	return h, true, nil
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

const hostColumns = `id, name, roles, address, cert_sha256, platform, version, coalesce(joined_with, ''), joined_at, last_seen_at, lost_at IS NOT NULL, coalesce(prev_cert_sha256, ''),
	ping_address, ping_cert_sha256, confirmed_at IS NULL, coalesce(next_cert_pem, '')`

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	var next string
	err := row.Scan(&h.ID, &h.Name, &h.Roles, &h.Address, &h.CertSHA256, &h.Platform, &h.Version, &h.JoinedWith, &h.JoinedAt, &h.LastSeenAt, &h.Lost, &h.PrevCertSHA256,
		&h.PingAddress, &h.PingCertSHA256, &h.Pending, &next)
	if next != "" {
		h.NextCertPEM = []byte(next)
	}
	return h, err
}

// AddHost records a host that has joined, confirmed.
func (c *Cluster) AddHost(ctx context.Context, h Host) (Host, error) {
	return addHost(ctx, c.db, h, false)
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
	if h, err = addHost(ctx, tx, h, false); err != nil {
		return Host{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Host{}, false, errs.Wrap(errs.Internal, msgRecordHost, err)
	}
	return h, true, nil
}

// addHost records h, pending its confirmation (ConfirmJoin) or confirmed.
func addHost(ctx context.Context, q querier, h Host, pending bool) (Host, error) {
	if h.ID == "" {
		h.ID = id.New(id.Host)
	}
	row := q.QueryRow(ctx, `
		INSERT INTO cluster_hosts (id, name, roles, address, cert_sha256, platform, version, joined_with, confirmed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $9 THEN NULL ELSE now() END)
		RETURNING `+hostColumns,
		h.ID, h.Name, h.Roles, h.Address, h.CertSHA256, h.Platform, h.Version, nullable(h.JoinedWith), pending)
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

// Hosts lists the cluster's hosts, oldest first: every one but those whose
// join is pending (ConfirmJoin).
func (c *Cluster) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := c.db.Query(ctx, `SELECT `+hostColumns+` FROM cluster_hosts WHERE confirmed_at IS NOT NULL ORDER BY joined_at, id`)
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

// Host reads one host whose join is not pending.
func (c *Cluster) Host(ctx context.Context, hostID string) (Host, error) {
	h, err := scanHost(c.db.QueryRow(ctx, `SELECT `+hostColumns+` FROM cluster_hosts WHERE id = $1 AND confirmed_at IS NOT NULL`, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
	}
	if err != nil {
		return h, errs.Wrap(errs.Internal, "Could not read the host.", err)
	}
	return h, nil
}

// ErrInstallHost refuses removing the install's own first host (R-414): it is
// recorded again at its next start under a new ID, which would leave its apps'
// homes and its history naming a host that no longer exists.
var ErrInstallHost = errs.New(errs.StateInvalid, "This is the installation's own first host, which cannot be removed from the cluster. Every installation is a cluster of at least this host.").
	WithRemedy("Remove the other hosts instead. To retire this machine, move the installation to another machine from a backup.")

const msgRemoveHost = "Could not remove the host."

// forgetStandbyHost clears a removed host, by name, from every app's standby
// row (R-419), so nothing ships to it, promotes from it, removes a copy from
// it or holds an app for it. Every CASE reads the row as it was:
//
//   - a standby on it is gone: the next standby pass places another;
//   - an app homed on it has no home: its runtime no longer has the host, so
//     the reconciler finds it absent and restores it (R-148);
//   - a promotion onto it is over, and the app's home is the host it was
//     leaving, as when a promotion is refused (standby.putBack);
//   - a copy left on it is gone with it; a promotion away from it goes on.
const forgetStandbyHost = `
	UPDATE app_standbys SET
	    home_host = CASE WHEN home_host = $1 AND promoting_at IS NOT NULL THEN stale_host
	                     WHEN home_host = $1 THEN '' ELSE home_host END,
	    stale_host = CASE WHEN stale_host = $1 OR (home_host = $1 AND promoting_at IS NOT NULL) THEN '' ELSE stale_host END,
	    promoted_at = CASE WHEN home_host = $1 AND promoting_at IS NOT NULL THEN NULL ELSE promoted_at END,
	    promoting_at = CASE WHEN home_host = $1 THEN NULL ELSE promoting_at END,
	    host = CASE WHEN host = $1 THEN '' ELSE host END,
	    spec_id = CASE WHEN host = $1 THEN NULL ELSE spec_id END,
	    copies = CASE WHEN host = $1 THEN 0 ELSE copies END,
	    shipped_at = CASE WHEN host = $1 THEN NULL ELSE shipped_at END,
	    updated_at = now()
	WHERE $1 IN (host, home_host, stale_host)`

// RemoveHost removes a host from the cluster, and revokes every certificate
// it held, in one transaction, so that no host, listener or runtime takes
// them again (R-415). The install's own first host is refused
// (ErrInstallHost).
func (c *Cluster) RemoveHost(ctx context.Context, hostID string) (Host, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return Host{}, errs.Wrap(errs.Internal, msgRemoveHost, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	h, err := scanHost(tx.QueryRow(ctx, `SELECT `+hostColumns+` FROM cluster_hosts WHERE id = $1 FOR UPDATE`, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", hostID)
	}
	if err != nil {
		return h, errs.Wrap(errs.Internal, msgRemoveHost, err)
	}
	if h.JoinedWith == "" {
		return h, ErrInstallHost
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cluster_hosts WHERE id = $1`, hostID); err != nil {
		return h, errs.Wrap(errs.Internal, msgRemoveHost, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cluster_revoked_certs (cert_sha256, host_id)
		SELECT sha, $1 FROM unnest($2::text[]) AS sha WHERE sha <> ''
		ON CONFLICT (cert_sha256) DO NOTHING`, hostID, []string{h.CertSHA256, h.PrevCertSHA256, h.PingCertSHA256}); err != nil {
		return h, errs.Wrap(errs.Internal, msgRemoveHost, err)
	}
	if _, err := tx.Exec(ctx, forgetStandbyHost, h.Name); err != nil {
		return h, errs.Wrap(errs.Internal, msgRemoveHost, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return h, errs.Wrap(errs.Internal, msgRemoveHost, err)
	}
	return h, nil
}

// CertRevoked reports whether the certificate with fingerprint sha belonged
// to a host removed from the cluster (R-415).
func (c *Cluster) CertRevoked(ctx context.Context, sha string) (bool, error) {
	var revoked bool
	if err := c.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cluster_revoked_certs WHERE cert_sha256 = $1)`, sha).Scan(&revoked); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not check whether the certificate belongs to a removed host.", err)
	}
	return revoked, nil
}

// errNotHostsCert refuses a renewal presented with a certificate that is no
// longer the host's.
var errNotHostsCert = errs.New(errs.AuthInvalid, "This certificate is no longer this host's, so it cannot be renewed with.").
	WithRemedy("Remove the host from the cluster and join it again with a new join token.")

// RenewHostCert records next, whose PEM is nextPEM, as the certificate of
// the host that presented its current one (R-421). It is a compare-and-set on
// presented: only the first renewal of a certificate is recorded, and keeps
// presented as the one before until the host presents next
// (ConfirmHostCert). A renewal that finds presented no longer current — one
// that overlapped another, or was tried again after its answer was lost — is
// told so (false) with the row as it is now, whose NextCertPEM is what the
// first was answered with, so the pins always hold the certificate the host
// ends up using.
func (c *Cluster) RenewHostCert(ctx context.Context, hostID, presented, next string, nextPEM []byte) (Host, bool, error) {
	h, err := scanHost(c.db.QueryRow(ctx, `
		UPDATE cluster_hosts SET prev_cert_sha256 = $2, cert_sha256 = $3, next_cert_pem = $4, updated_at = now()
		WHERE id = $1 AND cert_sha256 = $2 AND confirmed_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM cluster_revoked_certs WHERE cert_sha256 = $2)
		RETURNING `+hostColumns, hostID, presented, next, string(nextPEM)))
	if err == nil {
		return h, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return h, false, errs.Wrap(errs.Internal, "Could not record the host's renewed certificate.", err)
	}
	h, found, err := c.HostByCert(ctx, presented)
	if err != nil {
		return h, false, err
	}
	if !found || h.ID != hostID || h.Pending {
		return h, false, errNotHostsCert
	}
	return h, false, nil
}

// ConfirmHostCert records that the host presented current, its newest
// certificate: the one before it is no longer the host's. It reports whether
// that changed anything.
func (c *Cluster) ConfirmHostCert(ctx context.Context, hostID, current string) (bool, error) {
	tag, err := c.db.Exec(ctx, `
		UPDATE cluster_hosts SET prev_cert_sha256 = NULL, next_cert_pem = NULL, updated_at = now()
		WHERE id = $1 AND cert_sha256 = $2 AND prev_cert_sha256 IS NOT NULL`, hostID, current)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record that the host uses its renewed certificate.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClusterJoinAddrKey is the key refused joins from a client address are
// counted under, as wrong passwords are (R-404 applied to R-415).
func ClusterJoinAddrKey(addr string) string { return "cluster-join|addr|" + addr }

// TouchHost records that a host was heard from.
func (c *Cluster) TouchHost(ctx context.Context, hostID string) error {
	if _, err := c.db.Exec(ctx, `UPDATE cluster_hosts SET last_seen_at = now() WHERE id = $1`, hostID); err != nil {
		return errs.Wrap(errs.Internal, "Could not record that the host was heard from.", err)
	}
	return nil
}
