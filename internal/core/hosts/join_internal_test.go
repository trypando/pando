package hosts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func testAuthority(t *testing.T) *Authority {
	t.Helper()
	a, err := LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	return a
}

func TestWhatTheClusterIssuedIsChecked(t *testing.T) {
	a := testAuthority(t)
	other := testAuthority(t)
	key, csr, err := newHostKey("edge-2")
	require.NoError(t, err)
	cert, _, err := a.IssueHost(csr, "edge-2", []string{"app"})
	require.NoError(t, err)
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	require.NoError(t, checkIssued(JoinResult{Certificate: cert, Authority: a.CertPEM()}, key, a.Hash()))
	for what, c := range map[string]struct {
		res  JoinResult
		key  *ecdsa.PrivateKey
		hash string
	}{
		"not PEM":         {JoinResult{Certificate: []byte("x"), Authority: a.CertPEM()}, key, a.Hash()},
		"bad authority":   {JoinResult{Certificate: cert, Authority: []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")}, key, a.Hash()},
		"bad certificate": {JoinResult{Certificate: []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), Authority: a.CertPEM()}, key, a.Hash()},
		"other authority": {JoinResult{Certificate: cert, Authority: a.CertPEM()}, key, other.Hash()},
		"another key":     {JoinResult{Certificate: cert, Authority: a.CertPEM()}, otherKey, a.Hash()},
	} {
		require.Error(t, checkIssued(c.res, c.key, c.hash), what)
	}
	require.ErrorIs(t, verifyPinned(nil, a.Hash()), errAuthorityMismatch)
}

func TestARefusalIsPassedOnAsTheClusterSaidIt(t *testing.T) {
	err := refusal("https://c:7443", http.StatusConflict, []byte(`{"code":"STATE_INVALID","message":"No.","remedy":"Do this."}`))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Equal(t, "Do this.", errs.As(err).Remedy)

	err = refusal("https://c:7443", http.StatusBadGateway, []byte("<html>"))
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "HTTP status 502")
}

func TestKeysThatCannotBeReadAreRefused(t *testing.T) {
	_, err := WireJoinResult{Keys: &WireKeys{TokenKey: "!!"}}.result()
	require.Error(t, err)
	_, err = WireJoinResult{Keys: &WireKeys{TokenKey: "a2V5", SecretsKey: "!!"}}.result()
	require.Error(t, err)
	require.Equal(t, "", b64(secret.Value{}))
	require.Equal(t, "of an unknown version", orUnknown(""))
}

func TestAControlHostWithoutItsKeysCannotHandThemOver(t *testing.T) {
	s := &Service{Authority: testAuthority(t)}
	_, err := s.controlKeys(context.Background())
	require.Contains(t, errs.As(err).Message, "database address")

	s.Keys = KeySource{DatabaseURL: secret.New("postgres://x"), TokenKeyPath: filepath.Join(t.TempDir(), "missing")}
	_, err = s.controlKeys(context.Background())
	require.Contains(t, errs.As(err).Message, "API token key")

	dir := t.TempDir()
	s.Keys.TokenKeyPath = filepath.Join(dir, "token.key")
	require.NoError(t, os.WriteFile(s.Keys.TokenKeyPath, []byte("k"), 0o600))
	s.Keys.SecretsKeyPath = func(context.Context) string { return filepath.Join(dir, "missing") }
	_, err = s.controlKeys(context.Background())
	require.Contains(t, errs.As(err).Message, "secrets key")

	s.Keys.SecretsKeyPath = func(context.Context) string { return "" }
	k, err := s.controlKeys(context.Background())
	require.NoError(t, err)
	require.True(t, k.SecretsKey.IsZero(), "an external secrets adapter has no key to hand over")
}

func TestAJoinThatCannotBeRecordedSaysWhere(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	f := JoinFiles{Dir: filepath.Join(blocked, "cluster")}
	require.Contains(t, errs.As(f.write(Joined{}, JoinResult{}, nil)).Message, "could not create")

	f = JoinFiles{Dir: filepath.Join(dir, "cluster"), TokenKeyPath: filepath.Join(blocked, "token.key")}
	err := f.write(Joined{}, JoinResult{Keys: &ControlKeys{}}, nil)
	require.Contains(t, errs.As(err).Message, "token.key")

	f = JoinFiles{Dir: filepath.Join(dir, "unreadable")}
	require.NoError(t, os.MkdirAll(filepath.Join(f.Dir, JoinedFile), 0o700))
	_, _, err = f.Read()
	require.Error(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, JoinedFile), []byte("{"), 0o600))
	_, _, err = JoinFiles{Dir: dir}.Read()
	require.Contains(t, errs.As(err).Remedy, "join it again")
}

func TestAJoinerUsesTheClientItIsGiven(t *testing.T) {
	used := false
	j := &Joiner{HTTP: func(*tls.Config) *http.Client { used = true; return http.DefaultClient }}
	_ = j.client(PinnedTLS("x"))
	require.True(t, used)
	require.False(t, (&Joiner{}).now().IsZero())
	require.False(t, (&Service{}).now().IsZero())
}
