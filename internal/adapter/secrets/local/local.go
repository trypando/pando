package local

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "local"

// KeySize is the AES-256 key length.
const KeySize = 32

// DefaultKeyPath is where the encryption key lives unless configured
// otherwise. Exported because the DR bundle has to find it (R-212).
const DefaultKeyPath = "/var/lib/pando/secrets.key"

// Adapter encrypts secrets at rest with a key on disk (R-190).
//
// The threat this defends against is a leaked database dump or backup, not a
// compromised host: an attacker who can read the key file can read the secrets.
// R-042's posture applies here too — this must be secure, and it is not claimed
// to be the strongest option. An install with real requirements configures an
// external secrets adapter, and the interface exists so that drops in without
// touching core.
type Adapter struct {
	aead cipher.AEAD
	path string
}

// Config is the adapter's configuration.
type Config struct {
	// KeyPath is where the encryption key lives. It is generated on first use
	// with 0600 permissions.
	KeyPath string `json:"key_path"`
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategorySecrets }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	cfg := Config{KeyPath: DefaultKeyPath}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The local secrets configuration could not be read.", err)
		}
	}
	a.path = cfg.KeyPath

	key, err := loadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not set up secret encryption.", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not set up secret encryption.", err)
	}
	a.aead = aead
	return nil
}

func (a *Adapter) HealthCheck(context.Context) error {
	if a.aead == nil {
		return errs.New(errs.AdapterUnavailable, "Secret storage has not been set up.")
	}
	return nil
}

// Put encrypts a value and returns a reference holding the ciphertext.
//
// The app ID and key are bound into the ciphertext as additional authenticated
// data, so a ciphertext lifted from one app's row cannot be decrypted as
// another's. Without that, a database-level swap of two rows would silently
// hand one app another's credential.
func (a *Adapter) Put(_ context.Context, ref api.SecretRef, v secret.Value) (api.StoredRef, error) {
	if a.aead == nil {
		return api.StoredRef{}, errs.New(errs.AdapterUnavailable, "Secret storage has not been set up.")
	}

	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return api.StoredRef{}, errs.Wrap(errs.Internal, "Could not store the secret.", err)
	}

	sealed := a.aead.Seal(nonce, nonce, []byte(v.Reveal()), aad(ref))
	return api.StoredRef{AppID: ref.AppID, Key: ref.Key, Ciphertext: sealed}, nil
}

// Get decrypts a stored value.
func (a *Adapter) Get(_ context.Context, ref api.StoredRef) (secret.Value, error) {
	if a.aead == nil {
		return secret.Value{}, errs.New(errs.AdapterUnavailable, "Secret storage has not been set up.")
	}

	nonceSize := a.aead.NonceSize()
	if len(ref.Ciphertext) < nonceSize {
		return secret.Value{}, errs.New(errs.Internal, "The stored secret is unreadable.")
	}

	plaintext, err := a.aead.Open(nil, ref.Ciphertext[:nonceSize], ref.Ciphertext[nonceSize:],
		aad(api.SecretRef{AppID: ref.AppID, Key: ref.Key}))
	if err != nil {
		// Deliberately vague: a decryption failure means the key changed, the
		// data was tampered with, or the row was moved between apps. None of
		// those is safe to distinguish for a caller.
		return secret.Value{}, errs.Wrap(errs.Internal,
			"This secret could not be read. It may have been stored with a different encryption key.", err)
	}
	return secret.New(string(plaintext)), nil
}

// Delete is a no-op for this adapter: the ciphertext lives in Pando's own
// database row, and core removes it. An external adapter would delete remotely.
func (a *Adapter) Delete(context.Context, api.StoredRef) error { return nil }

var _ api.SecretsAdapter = (*Adapter)(nil)

// aad binds a ciphertext to the app and key it was stored under.
func aad(ref api.SecretRef) []byte {
	return []byte(ref.AppID + "\x00" + ref.Key)
}

// loadOrCreateKey reads the encryption key, generating it on first use.
func loadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != KeySize {
			return nil, errs.Newf(errs.Internal,
				"The secret encryption key at %s is the wrong size.", path).
				WithRemedy("Restore the original key from a backup. Without it, existing secrets cannot be read.")
		}
		return key, nil

	case errors.Is(err, os.ErrNotExist):
		key = make([]byte, KeySize)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not generate a secret encryption key.", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Could not create %s.", filepath.Dir(path)), err)
		}
		// 0600: the key is the thing that makes the ciphertext worth anything.
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Could not write the key to %s.", path), err)
		}
		return key, nil

	default:
		return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Could not read the key at %s.", path), err)
	}
}

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategorySecrets,
		Kind:        Kind,
		Name:        "Local",
		Description: "Keeps secrets encrypted in Pando’s own database.",
		IDPrefix:    "sek_",
		Fields: []api.Field{
			{Key: "key_path", Label: "Key file", Type: "string", Help: "The file holding the encryption key.", Default: DefaultKeyPath, Advanced: true},
		},
	}
}
