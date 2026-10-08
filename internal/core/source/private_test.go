package source_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The token every test here reads its private repository with, and searches
// for afterwards in everything that should not hold it.
const token = "pando-test-token-7f3kq9"

// storeLoader serves one repository whatever path is asked for.
type storeLoader struct{ s storer.Storer }

func (l storeLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

func uploadPackServer(t *testing.T, dir string) transport.Transport {
	t.Helper()
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return gitserver.NewServer(storeLoader{r.Storer})
}

// privateHTTP serves the repository at dir over git's smart HTTP protocol,
// only to a request that authenticates with authorized.
func privateHTTP(t *testing.T, dir string, authorized func(*http.Request) bool) string {
	t.Helper()
	srv := uploadPackServer(t, dir)
	ep, err := transport.NewEndpoint("/repo.git")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/acme/api.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		sess, err := srv.NewUploadPackSession(ep, nil)
		require.NoError(t, err)
		ar, err := sess.AdvertisedReferencesContext(r.Context())
		require.NoError(t, err)
		ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		require.NoError(t, ar.Encode(w))
	})
	mux.HandleFunc("/acme/api.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		sess, err := srv.NewUploadPackSession(ep, nil)
		require.NoError(t, err)
		if _, err := sess.AdvertisedReferencesContext(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req := packp.NewUploadPackRequest()
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := sess.UploadPack(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_ = resp.Encode(w)
	})
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return hs.URL + "/acme/api.git"
}

// connection reads every repository its spec names with one credential.
type connection struct {
	access *source.Access
	asked  []source.Purpose
}

func (c *connection) Access(_ context.Context, src spec.Source, purpose source.Purpose) (*source.Access, error) {
	if src.CredentialRef == "" {
		return nil, nil
	}
	c.asked = append(c.asked, purpose)
	return c.access, nil
}

// requireNowhereIn fails if the token is in any file under dir, the clone's
// own git metadata included: the checkout is what the builder is given
// (R-112).
func requireNowhereIn(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		require.False(t, bytes.Contains(b, []byte(token)), "the credential was written to %s", path)
		return nil
	}))
}

// TestR091_APrivateRepositoryIsClonedWithItsConnectionsToken asserts R-091
// over HTTPS, and R-112: the credential is used by Pando's own git client and
// is in nothing the clone leaves on disk, nor in any error.
func TestR091_APrivateRepositoryIsClonedWithItsConnectionsToken(t *testing.T) {
	dir, shas := repo(t, map[string]string{"main.go": "package main"})
	url := privateHTTP(t, dir, func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return ok && u == "x-access-token" && p == token
	})
	src := spec.Source{Type: spec.SourceGit, URL: url, Commit: shas[0]}

	// No connection: anonymous, refused, and said so.
	_, err := source.Sources{}.Fetch(ctx(), src)
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), "no source connection on this installation covers it")

	// A connection whose token is wrong: named, and the token is not.
	bad := &connection{access: &source.Access{Connection: "GitHub (acme)", Credential: api.GitCredential{
		Username: "x-access-token", Password: secret.New(token + "-revoked"),
	}}}
	src.CredentialRef = "src_github_acme"
	_, err = source.Sources{Credentials: bad}.Fetch(ctx(), src)
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), `"GitHub (acme)"`)
	require.NotContains(t, err.Error(), token)

	good := &connection{access: &source.Access{Connection: "GitHub (acme)", Credential: api.GitCredential{
		Username: "x-access-token", Password: secret.New(token),
	}}}
	sources := source.Sources{Credentials: good, WorkDir: t.TempDir()}
	co, err := sources.Fetch(ctx(), src)
	require.NoError(t, err)
	defer co.Close()
	require.Equal(t, shas[0], co.Commit)
	require.FileExists(t, filepath.Join(co.Dir, "main.go"))
	requireNowhereIn(t, co.Dir)
	require.Equal(t, []source.Purpose{source.PurposeClone}, good.asked, "a clone asks for the credential once, as a clone")

	// Polling for new commits reads through the same connection, as a check.
	head, err := sources.ResolveRef(ctx(), spec.Source{Type: spec.SourceGit, URL: url, Ref: "master", CredentialRef: "src_github_acme"})
	require.NoError(t, err)
	require.Equal(t, shas[0], head)
	require.Equal(t, source.PurposeCheck, good.asked[len(good.asked)-1])
}

// TestR091_ABearerTokenIsSentAsAnAuthorizationHeader asserts R-091 for a host
// that takes an OAuth token as a bearer token (Azure DevOps with Entra ID).
func TestR091_ABearerTokenIsSentAsAnAuthorizationHeader(t *testing.T) {
	dir, shas := repo(t, map[string]string{"index.html": "hi"})
	url := privateHTTP(t, dir, func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+token
	})
	conn := &connection{access: &source.Access{Connection: "Azure DevOps", Credential: api.GitCredential{
		BearerToken: secret.New(token),
	}}}
	co, err := source.Sources{Credentials: conn}.Fetch(ctx(), spec.Source{
		Type: spec.SourceGit, URL: url, Commit: shas[0], CredentialRef: "src_azure",
	})
	require.NoError(t, err)
	defer co.Close()
	requireNowhereIn(t, co.Dir)
}

// TestR091_ProbeSaysWhyARepositoryCannotBeRead asserts the check an app's
// creation makes (R-105): private and uncovered is refused with a reason, and
// readable is not.
func TestR091_ProbeSaysWhyARepositoryCannotBeRead(t *testing.T) {
	dir, _ := repo(t, map[string]string{"a": "b"})
	url := privateHTTP(t, dir, func(r *http.Request) bool {
		_, p, ok := r.BasicAuth()
		return ok && p == token
	})

	err := source.Sources{}.Probe(ctx(), spec.Source{Type: spec.SourceGit, URL: url})
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))
	e := errs.As(err)
	require.NotEmpty(t, e.Remedy, "the refusal says what to do (R-105)")

	conn := &connection{access: &source.Access{Connection: "git.example.com", Credential: api.GitCredential{Password: secret.New(token)}}}
	require.NoError(t, source.Sources{Credentials: conn}.Probe(ctx(),
		spec.Source{Type: spec.SourceGit, URL: url, CredentialRef: "src_git"}))
	require.Equal(t, []source.Purpose{source.PurposeCheck}, conn.asked, "a probe is a check, not a clone")

	// An address that does not answer is left for detection to report; the
	// address may be right.
	require.NoError(t, source.Sources{}.Probe(ctx(), spec.Source{Type: spec.SourceGit, URL: "http://127.0.0.1:1/x.git"}))
}

// --- SSH -------------------------------------------------------------------

// privateSSH serves the repository at dir over SSH, only to a client holding
// the private half of allowed. It returns the address and the server's key as
// a known_hosts line.
func privateSSH(t *testing.T, dir string, allowed ssh.PublicKey) (string, string) {
	t.Helper()
	srv := uploadPackServer(t, dir)
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostKey, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), allowed.Marshal()) {
				return nil, nil
			}
			return nil, errs.New(errs.AuthInvalid, "unknown key")
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSH(c, cfg, srv)
		}
	}()
	addr := ln.Addr().String()
	known := knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostKey.PublicKey())
	return "ssh://git@" + addr + "/acme/api.git", known
}

func serveSSH(c net.Conn, cfg *ssh.ServerConfig, srv transport.Transport) {
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				n := binary.BigEndian.Uint32(req.Payload[:4])
				if !strings.HasPrefix(string(req.Payload[4:4+n]), "git-upload-pack") {
					_ = req.Reply(false, nil)
					return
				}
				_ = req.Reply(true, nil)
				ep, _ := transport.NewEndpoint("/repo.git")
				sess, err := srv.NewUploadPackSession(ep, nil)
				if err == nil {
					var ar *packp.AdvRefs
					if ar, err = sess.AdvertisedReferencesContext(context.Background()); err == nil {
						_ = ar.Encode(ch)
						up := packp.NewUploadPackRequest()
						// A client listing refs sends a flush and goes; one
						// fetching sends what it wants.
						if up.Decode(ch) == nil {
							if resp, err := sess.UploadPack(context.Background(), up); err == nil {
								_ = resp.Encode(ch)
							}
						}
					}
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}

func deployKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return sshPub, string(pem.EncodeToMemory(block))
}

// TestR091_APrivateRepositoryIsClonedWithADeployKey asserts R-091 over SSH,
// with the host's key pinned: a host that presents another key is not read
// from.
func TestR091_APrivateRepositoryIsClonedWithADeployKey(t *testing.T) {
	dir, shas := repo(t, map[string]string{"app.py": "print(1)"})
	pub, key := deployKey(t)
	url, known := privateSSH(t, dir, pub)
	src := spec.Source{Type: spec.SourceGit, URL: url, Commit: shas[0], CredentialRef: "src_deploy_key"}

	conn := &connection{access: &source.Access{Connection: "deploy key", Credential: api.GitCredential{
		SSHUser: "git", SSHPrivateKey: secret.New(key), KnownHosts: known,
	}}}
	co, err := source.Sources{Credentials: conn}.Fetch(ctx(), src)
	require.NoError(t, err)
	defer co.Close()
	require.Equal(t, shas[0], co.Commit)
	require.FileExists(t, filepath.Join(co.Dir, "app.py"))

	// Another host key pinned: refused, and the reason is the host key.
	_, otherKnown := privateSSH(t, dir, pub)
	host := strings.TrimPrefix(strings.SplitN(url, "/", 4)[2], "git@")
	wrong := strings.Replace(otherKnown, strings.Fields(otherKnown)[0], knownhosts.Normalize(host), 1)
	pinned := &connection{access: &source.Access{Connection: "deploy key", Credential: api.GitCredential{
		SSHUser: "git", SSHPrivateKey: secret.New(key), KnownHosts: wrong,
	}}}
	_, err = source.Sources{Credentials: pinned}.Fetch(ctx(), src)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SSH key")
	require.NotContains(t, err.Error(), "PRIVATE KEY")

	// No host key pinned at all: refused before connecting.
	unpinned := &connection{access: &source.Access{Connection: "deploy key", Credential: api.GitCredential{
		SSHUser: "git", SSHPrivateKey: secret.New(key),
	}}}
	_, err = source.Sources{Credentials: unpinned}.Fetch(ctx(), src)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pins no host key")
}
