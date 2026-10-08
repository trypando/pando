package generic

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/adapter/api"
)

func TestInfoValidates(t *testing.T) {
	if err := Info().Validate(); err != nil {
		t.Fatal(err)
	}
}

func configure(t *testing.T, cfg map[string]any) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := New()
	return a, a.Configure(context.Background(), raw)
}

func testKey(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), "git.example.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func TestTokenConnectionCoversItsHostAndScope(t *testing.T) {
	a, err := configure(t, map[string]any{
		"method": "token", "host": "https://git.example.com", "scope": "acme",
		"credentials": map[string]string{"token": "s3cret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]int{
		"https://git.example.com/acme/api.git":  2,
		"https://GIT.example.com/ACME/api":      2,
		"git@git.example.com:acme/api.git":      2,
		"https://git.example.com/other/api.git": 0,
		"https://git.example.com/acme":          0,
		"https://github.com/acme/api.git":       0,
		"not an address":                        0,
	} {
		if got := a.Covers(url); got != want {
			t.Errorf("Covers(%q) = %d, want %d", url, got, want)
		}
	}

	cred, err := a.GitCredential(context.Background(), "https://git.example.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "git" || cred.Password.Reveal() != "s3cret" || cred.URL != "" {
		t.Fatalf("credential = %+v", cred)
	}
	if strings.Contains(cred.Password.String(), "s3cret") {
		t.Fatal("the token rendered in a string")
	}
	if _, err := a.GitCredential(context.Background(), "git@git.example.com:acme/api.git"); err == nil {
		t.Fatal("a token connection handed out a credential for an SSH address")
	}
}

func TestSSHConnectionNeedsAKeyAndAPinnedHost(t *testing.T) {
	key, known := testKey(t)

	if _, err := configure(t, map[string]any{
		"method": "ssh", "host": "git.example.com",
		"credentials": map[string]string{"ssh_private_key": key},
	}); err == nil {
		t.Fatal("an SSH connection was configured without known hosts")
	}
	if _, err := configure(t, map[string]any{
		"method": "ssh", "host": "git.example.com", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": "not a key"},
	}); err == nil {
		t.Fatal("an SSH connection was configured with a key that is not one")
	}

	a, err := configure(t, map[string]any{
		"method": "ssh", "host": "git.example.com", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": key},
	})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := a.GitCredential(context.Background(), "https://git.example.com/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "git@git.example.com:acme/api.git" || cred.SSHUser != "git" || cred.KnownHosts != known {
		t.Fatalf("credential = %+v", cred)
	}
	if a.SourceCapabilities().ListRepositories {
		t.Fatal("a plain git connection claims it can list repositories")
	}
	if _, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err == nil {
		t.Fatal("listing repositories on a plain git connection succeeded")
	}
}

func TestUnknownMethodIsRefused(t *testing.T) {
	if _, err := configure(t, map[string]any{"method": "app", "host": "git.example.com"}); err == nil {
		t.Fatal("a git connection accepted the app method")
	}
	if _, err := configure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": "x"}}); err == nil {
		t.Fatal("a git connection was configured with no host")
	}
}
