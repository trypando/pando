package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// EdgeCertificates keeps the certificates Pando issues for the edge, its ACME
// account, and pending HTTP-01 challenges (migration 000052; R-169, R-174).
//
// The account key and each certificate with its key are sealed by the
// install's secrets adapter, as adapter credentials are (R-190): the rows hold
// ciphertext or an external reference, never PEM.
type EdgeCertificates struct {
	db         *DB
	adapter    api.SecretsAdapter
	adapterRef string
}

func NewEdgeCertificates(db *DB, adapter api.SecretsAdapter, adapterRef string) *EdgeCertificates {
	return &EdgeCertificates{db: db, adapter: adapter, adapterRef: adapterRef}
}

// Sealed under scopes no app ID can take (an app's is always app_…), so a
// certificate's ciphertext cannot be replayed as anything else.
func certRef(name string) api.SecretRef {
	return api.SecretRef{AppID: "edge-certificate:" + name, Key: "bundle"}
}
func accountRef(email string) api.SecretRef {
	return api.SecretRef{AppID: "acme-account:" + email, Key: "key"}
}

// EdgeCertificate is one certificate as stored.
type EdgeCertificate struct {
	Name     string
	Domains  []string
	CertPEM  []byte
	KeyPEM   secret.Value
	NotAfter time.Time // zero until the first issuance succeeds

	AttemptedAt time.Time
	LastError   string
}

// bundle is what is sealed: the chain and its key, together.
type bundle struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

func (e *EdgeCertificates) seal(ctx context.Context, ref api.SecretRef, v secret.Value) (api.StoredRef, error) {
	if e.adapter == nil {
		return api.StoredRef{}, errs.New(errs.StateInvalid,
			"Pando has no secrets adapter configured, so it cannot keep the edge's certificates.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	return e.adapter.Put(ctx, ref, v)
}

func (e *EdgeCertificates) open(ctx context.Context, ref api.SecretRef, ciphertext []byte, handle *string) (secret.Value, error) {
	if e.adapter == nil {
		return secret.Value{}, errs.New(errs.StateInvalid, "Pando has no secrets adapter configured, so it cannot read the edge's certificates.")
	}
	stored := api.StoredRef{AppID: ref.AppID, Key: ref.Key, Ciphertext: ciphertext}
	if handle != nil {
		stored.Handle = *handle
	}
	return e.adapter.Get(ctx, stored)
}

// Certificate reads one certificate. A row with only a failure recorded is
// returned with no PEM.
func (e *EdgeCertificates) Certificate(ctx context.Context, name string) (EdgeCertificate, bool, error) {
	var (
		c          = EdgeCertificate{Name: name}
		ciphertext []byte
		handle     *string
		notAfter   *time.Time
		attempted  *time.Time
		lastErr    *string
	)
	err := e.db.QueryRow(ctx, `
		SELECT domains, ciphertext, external_ref, not_after, attempted_at, last_error
		FROM edge_certificates WHERE name = $1`, name).
		Scan(&c.Domains, &ciphertext, &handle, &notAfter, &attempted, &lastErr)
	if errors.Is(err, pgx.ErrNoRows) {
		return EdgeCertificate{}, false, nil
	}
	if err != nil {
		return EdgeCertificate{}, false, errs.Wrap(errs.Internal, "Could not read the edge's certificate.", err)
	}
	if attempted != nil {
		c.AttemptedAt = *attempted
	}
	if lastErr != nil {
		c.LastError = *lastErr
	}
	if notAfter == nil {
		return c, true, nil
	}
	c.NotAfter = *notAfter
	v, err := e.open(ctx, certRef(name), ciphertext, handle)
	if err != nil {
		return EdgeCertificate{}, false, err
	}
	var b bundle
	if err := json.Unmarshal([]byte(v.Reveal()), &b); err != nil {
		return EdgeCertificate{}, false, errs.Wrap(errs.Internal, "The edge's stored certificate could not be read.", err)
	}
	c.CertPEM, c.KeyPEM = []byte(b.Cert), secret.New(b.Key)
	return c, true, nil
}

// PutCertificate stores a newly issued certificate, clearing any failure.
func (e *EdgeCertificates) PutCertificate(ctx context.Context, c EdgeCertificate) error {
	raw, err := json.Marshal(bundle{Cert: string(c.CertPEM), Key: c.KeyPEM.Reveal()})
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the edge's certificate.", err)
	}
	stored, err := e.seal(ctx, certRef(c.Name), secret.New(string(raw)))
	if err != nil {
		return err
	}
	_, err = e.db.Exec(ctx, `
		INSERT INTO edge_certificates (name, domains, adapter_ref, ciphertext, external_ref, not_after, issued_at, attempted_at, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now(), NULL)
		ON CONFLICT (name) DO UPDATE SET
			domains = EXCLUDED.domains, adapter_ref = EXCLUDED.adapter_ref,
			ciphertext = EXCLUDED.ciphertext, external_ref = EXCLUDED.external_ref,
			not_after = EXCLUDED.not_after, issued_at = now(), attempted_at = now(), last_error = NULL`,
		c.Name, c.Domains, e.adapterRef, stored.Ciphertext, nullable(stored.Handle), c.NotAfter)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the edge's certificate.", err)
	}
	return nil
}

// RecordFailure notes a failed issuance, keeping any certificate already held.
func (e *EdgeCertificates) RecordFailure(ctx context.Context, name string, domains []string, message string, at time.Time) error {
	_, err := e.db.Exec(ctx, `
		INSERT INTO edge_certificates (name, domains, attempted_at, last_error)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET attempted_at = EXCLUDED.attempted_at, last_error = EXCLUDED.last_error`,
		name, domains, at, message)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record a failed certificate order.", err)
	}
	return nil
}

// AcmeAccount is the ACME account the edge's certificates are ordered with.
type AcmeAccount struct {
	Email        string
	Directory    string
	KeyPEM       secret.Value
	Registration json.RawMessage
}

// Account reads the account for an email address at a CA.
func (e *EdgeCertificates) Account(ctx context.Context, email, directory string) (AcmeAccount, bool, error) {
	var (
		ciphertext []byte
		handle     *string
		reg        []byte
	)
	err := e.db.QueryRow(ctx, `
		SELECT ciphertext, external_ref, registration FROM edge_acme_accounts
		WHERE email = $1 AND directory = $2`, email, directory).Scan(&ciphertext, &handle, &reg)
	if errors.Is(err, pgx.ErrNoRows) {
		return AcmeAccount{}, false, nil
	}
	if err != nil {
		return AcmeAccount{}, false, errs.Wrap(errs.Internal, "Could not read the certificate account.", err)
	}
	key, err := e.open(ctx, accountRef(email), ciphertext, handle)
	if err != nil {
		return AcmeAccount{}, false, err
	}
	return AcmeAccount{Email: email, Directory: directory, KeyPEM: key, Registration: reg}, true, nil
}

// PutAccount stores the account key, sealed, and its registration.
func (e *EdgeCertificates) PutAccount(ctx context.Context, a AcmeAccount) error {
	stored, err := e.seal(ctx, accountRef(a.Email), a.KeyPEM)
	if err != nil {
		return err
	}
	var reg any
	if len(a.Registration) > 0 {
		reg = []byte(a.Registration)
	}
	_, err = e.db.Exec(ctx, `
		INSERT INTO edge_acme_accounts (email, directory, adapter_ref, ciphertext, external_ref, registration)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email, directory) DO UPDATE SET
			adapter_ref = EXCLUDED.adapter_ref, ciphertext = EXCLUDED.ciphertext,
			external_ref = EXCLUDED.external_ref, registration = EXCLUDED.registration, updated_at = now()`,
		a.Email, a.Directory, e.adapterRef, stored.Ciphertext, nullable(stored.Handle), reg)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the certificate account.", err)
	}
	return nil
}

// PutChallenge records a pending HTTP-01 challenge for any replica to answer.
func (e *EdgeCertificates) PutChallenge(ctx context.Context, token, keyAuthorization, domain string, expires time.Time) error {
	_, err := e.db.Exec(ctx, `
		INSERT INTO edge_acme_challenges (token, key_authorization, domain, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (token) DO UPDATE SET key_authorization = EXCLUDED.key_authorization,
			domain = EXCLUDED.domain, expires_at = EXCLUDED.expires_at`,
		token, keyAuthorization, domain, expires)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record a certificate challenge.", err)
	}
	return nil
}

// DeleteChallenge removes a challenge once the CA has checked it, and any
// that have expired.
func (e *EdgeCertificates) DeleteChallenge(ctx context.Context, token string) error {
	_, err := e.db.Exec(ctx, `DELETE FROM edge_acme_challenges WHERE token = $1 OR expires_at < now()`, token)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove a certificate challenge.", err)
	}
	return nil
}

// Challenge is the key authorization for a pending token, unexpired.
func (e *EdgeCertificates) Challenge(ctx context.Context, token string) (string, bool, error) {
	var ka string
	err := e.db.QueryRow(ctx, `
		SELECT key_authorization FROM edge_acme_challenges
		WHERE token = $1 AND expires_at > now()`, token).Scan(&ka)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errs.Wrap(errs.Internal, "Could not read a certificate challenge.", err)
	}
	return ka, true, nil
}
