package forgekit

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/secret"
)

func TestCheckKnownHosts(t *testing.T) {
	good := "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	if err := CheckKnownHosts(good); err != nil {
		t.Errorf("one line: %v", err)
	}
	if err := CheckKnownHosts(good + "\n# a comment\n\n" + strings.Replace(good, "github.com", "[git.example.com]:2222", 1) + "\n"); err != nil {
		t.Errorf("several lines with a comment: %v", err)
	}
	for _, bad := range []string{"", "   \n", "github.com not-a-key", "github.com ssh-ed25519 !!!!"} {
		if err := CheckKnownHosts(bad); err == nil {
			t.Errorf("CheckKnownHosts(%q) accepted it", bad)
		}
	}
}

func TestCheckSSHKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := ssh.MarshalPrivateKey(priv, "")
	locked, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("pass-s3cret"))
	plainPEM := secret.New(string(pem.EncodeToMemory(plain)))
	lockedPEM := secret.New(string(pem.EncodeToMemory(locked)))

	if err := CheckSSHKey(plainPEM, secret.Value{}); err != nil {
		t.Errorf("plain key: %v", err)
	}
	if err := CheckSSHKey(lockedPEM, secret.New("pass-s3cret")); err != nil {
		t.Errorf("locked key with its passphrase: %v", err)
	}
	for name, err := range map[string]error{
		"no key":           CheckSSHKey(secret.Value{}, secret.Value{}),
		"garbage":          CheckSSHKey(secret.New("key-s3cret"), secret.Value{}),
		"missing pass":     CheckSSHKey(lockedPEM, secret.Value{}),
		"wrong passphrase": CheckSSHKey(lockedPEM, secret.New("wrong-s3cret")),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "PRIVATE KEY-----\n") {
			t.Errorf("%s: error carries key material: %v", name, err)
		}
	}
}

func testCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestCABundle(t *testing.T) {
	if b, err := CABundle("  "); err != nil || b != nil {
		t.Errorf("empty: %v %v", b, err)
	}
	cert := testCertPEM(t)
	b, err := CABundle("\n" + cert + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "-----BEGIN CERTIFICATE-----") || !strings.HasSuffix(string(b), "\n") {
		t.Errorf("bundle = %q", b)
	}
	if _, err := CABundle("-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----"); err == nil {
		t.Error("a bundle with no certificate was accepted")
	}
	if c := HTTPClient(b); c.Transport == nil || c.Timeout == 0 {
		t.Error("HTTPClient returned no transport or timeout")
	}
}

func TestTokenAndSSHCredential(t *testing.T) {
	c := TokenCredential("oauth2", secret.New("tok"), []byte("ca"))
	if c.Username != "oauth2" || c.Password.Reveal() != "tok" || string(c.CABundle) != "ca" {
		t.Errorf("token credential = %+v", c)
	}

	https, _ := ParseRepoURL("https://gitlab.com/acme/api")
	s := SSHCredential(https, "", secret.New("k"), secret.Value{}, "kh")
	if s.URL != "git@gitlab.com:acme/api.git" || s.SSHUser != "git" || s.KnownHosts != "kh" {
		t.Errorf("ssh credential from https = %+v", s)
	}
	s = SSHCredential(https, "ssh.gitlab.com", secret.New("k"), secret.Value{}, "kh")
	if s.URL != "git@ssh.gitlab.com:acme/api.git" {
		t.Errorf("ssh credential on another host = %+v", s)
	}
	scp, _ := ParseRepoURL("deploy@gitlab.com:acme/api.git")
	s = SSHCredential(scp, "", secret.New("k"), secret.Value{}, "kh")
	if s.URL != "" || s.SSHUser != "deploy" {
		t.Errorf("ssh credential from ssh = %+v", s)
	}
}
