package forgekit

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TokenCredential is HTTPS basic authentication with a token as the password.
func TokenCredential(username string, token secret.Value, caBundle []byte) api.GitCredential {
	return api.GitCredential{Username: username, Password: token, CABundle: caBundle}
}

// SSHCredential is an SSH key, for the repository at repoURL. An HTTPS address
// is turned into the SSH one for the same path on sshHost, since a deploy key
// cannot be used over HTTPS; sshHost is the host's own when empty.
func SSHCredential(repo RepoURL, sshHost string, key, passphrase secret.Value, knownHosts string) api.GitCredential {
	cred := api.GitCredential{
		SSHUser:       "git",
		SSHPrivateKey: key,
		SSHPassphrase: passphrase,
		KnownHosts:    knownHosts,
	}
	if repo.SSH {
		if repo.User != "" {
			cred.SSHUser = repo.User
		}
		return cred
	}
	host := sshHost
	if host == "" {
		host = repo.Host
	}
	cred.URL = SCPURL(cred.SSHUser, host, "", repo.Path)
	return cred
}

// CheckSSHKey refuses a private key Pando cannot use, at configuration rather
// than at the first deploy. Nothing about the key is put in the error.
func CheckSSHKey(key, passphrase secret.Value) error {
	if key.IsZero() {
		return errs.New(errs.ValidInvalid, "An SSH connection needs a private key.").
			WithRemedy("Paste the private key the repository's deploy key was made from, beginning -----BEGIN OPENSSH PRIVATE KEY-----.")
	}
	var err error
	if passphrase.IsZero() {
		_, err = ssh.ParsePrivateKey([]byte(key.Reveal()))
	} else {
		_, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key.Reveal()), []byte(passphrase.Reveal()))
	}
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return errs.New(errs.ValidInvalid,
				"The SSH private key is protected by a passphrase, and none was given.").
				WithRemedy("Enter the key's passphrase, or use a key without one.")
		}
		return errs.New(errs.ValidInvalid,
			"The SSH private key could not be read. It may be incomplete, or the passphrase may be wrong.").
			WithRemedy("Paste the whole private key, including its BEGIN and END lines, and check the passphrase.")
	}
	return nil
}

// CheckKnownHosts refuses known_hosts text that pins nothing. An SSH
// connection without a pinned host key would trust whoever answers, which is
// how a network attacker reads the repository (api.GitCredential.KnownHosts).
func CheckKnownHosts(text string) error {
	rest := []byte(strings.TrimSpace(text))
	if len(rest) == 0 {
		return errs.New(errs.ValidInvalid,
			"An SSH connection needs the host's public key, so Pando can tell it is talking to the real host.").
			WithRemedy("Run ssh-keyscan <host> on a machine you trust and paste its output as the known hosts.")
	}
	found := 0
	for len(rest) > 0 {
		_, _, _, _, next, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			if found > 0 && strings.TrimSpace(string(rest)) == "" {
				break
			}
			return errs.New(errs.ValidInvalid, "The known hosts could not be read.").
				WithRemedy("Paste lines in the form ssh-keyscan prints, such as: github.com ssh-ed25519 AAAA…")
		}
		found++
		rest = next
	}
	return nil
}

// CABundle reads a PEM bundle setting and refuses one with no certificate in
// it. Empty is fine: the system's roots alone.
func CABundle(pem string) ([]byte, error) {
	pem = strings.TrimSpace(pem)
	if pem == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, errs.New(errs.ValidInvalid,
			"The certificate authority bundle has no certificate Pando can read.").
			WithRemedy("Paste one or more PEM certificates, each beginning -----BEGIN CERTIFICATE-----.")
	}
	return []byte(pem + "\n"), nil
}

// HTTPClient is a client for a forge's API that trusts caBundle beside the
// system's roots.
func HTTPClient(caBundle []byte) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(caBundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(caBundle)
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}
