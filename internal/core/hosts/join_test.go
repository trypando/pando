package hosts_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/secret"
)

// cluster is a control host's side of a join: its service, with the keys it
// hands over on disk, behind a TLS listener issued from its authority.
type cluster struct {
	svc   *hosts.Service
	store *fakeStore
	srv   *httptest.Server
	keys  string
	// peersDown makes the listener refuse the peer list, which a joining
	// host confirms its join with.
	peersDown *atomic.Bool
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	return newClusterAt(t, nil)
}

// newClusterAt is newCluster whose service and listener read the time from
// now, when it is set.
func newClusterAt(t *testing.T, now func() time.Time) *cluster {
	t.Helper()
	store := &fakeStore{live: map[string]*state.JoinToken{}}
	svc := newService(t, store)
	if now != nil {
		svc.Now = now
	}
	svc.Version = "1.2.3"

	keys := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(keys, "token.key"), []byte("token-key-bytes"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(keys, "secrets.key"), []byte("secrets-key-bytes"), 0o600))
	svc.Keys = hosts.KeySource{
		DatabaseURL:    secret.New("postgres://pando:pw@db.example.com/pando"),
		TokenKeyPath:   filepath.Join(keys, "token.key"),
		SecretsKeyPath: func(context.Context) string { return "/var/lib/pando/secrets.key" },
	}

	down := &atomic.Bool{}
	srv := httptest.NewUnstartedServer(listener(svc, down))
	tlsConfig, err := svc.Authority.ListenerTLS([]string{"127.0.0.1", "localhost"}, time.Now())
	require.NoError(t, err)
	tlsConfig.Time = now
	srv.TLS = tlsConfig
	srv.StartTLS()
	srv.TLS.Certificates = nil // httptest adds its own; the listener serves from GetCertificate
	t.Cleanup(srv.Close)
	return &cluster{svc: svc, store: store, srv: srv, keys: keys, peersDown: down}
}

// listener stands in for httpapi's cluster routes: decode, Join or Renew,
// answer.
func listener(svc *hosts.Service, peersDown *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == hosts.PeersPath && peersDown.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var out any
		var err error
		status := http.StatusCreated
		switch r.URL.Path {
		case hosts.PeersPath:
			var list []hosts.Peer
			list, err = svc.Peers(r.Context(), presented(r))
			out, status = map[string]any{"hosts": list}, http.StatusOK
		case hosts.RenewPath:
			var req hosts.WireRenewRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			var res hosts.RenewResult
			res, err = svc.Renew(r.Context(), presented(r), []byte(req.CSR))
			out = res.Wire()
		default:
			var req hosts.WireJoinRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			var res hosts.JoinResult
			res, err = svc.Join(r.Context(), req.Request())
			out = res.Wire()
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			e := errs.As(err)
			w.WriteHeader(e.Status())
			_ = json.NewEncoder(w).Encode(e)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	})
}

// presented is the verified client certificate of r, as the cluster
// listener takes it.
func presented(r *http.Request) []byte {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return nil
	}
	return r.TLS.VerifiedChains[0][0].Raw
}

// token mints a live token for roles and returns the string a host joins with.
func (c *cluster) token(roles ...string) string {
	id := "jt_01" + roles[0]
	c.store.live[id+".plain"] = &state.JoinToken{ID: id, Roles: roles, MaxUses: 1}
	return hosts.Encode(c.srv.URL, c.svc.Authority.Hash(), secret.New(id+".plain"))
}

// joiningHost is a fresh Pando about to join.
func joiningHost(t *testing.T) *hosts.Joiner {
	t.Helper()
	dir := t.TempDir()
	return &hosts.Joiner{
		Fresh: func(context.Context) (bool, bool, error) { return false, false, nil },
		Files: hosts.JoinFiles{
			Dir:             filepath.Join(dir, "cluster"),
			DatabaseURLPath: filepath.Join(dir, "cluster", "database-url"),
			TokenKeyPath:    filepath.Join(dir, "token.key"),
			AuthorityPath:   filepath.Join(dir, "cluster-ca.pem"),
		},
		Version: "1.2.3", Platform: "linux/amd64", AgentAddress: "edge-2:7443",
		DefaultName: "Edge-2.example.com",
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// TestR415_AJoiningHostRefusesAServerWhoseAuthorityDiffers asserts R-415: the
// joining host checks the server's authority against the token's hash before
// it sends anything, so a token used against another server reveals nothing.
func TestR415_AJoiningHostRefusesAServerWhoseAuthorityDiffers(t *testing.T) {
	c := newCluster(t)
	impostor := newCluster(t)
	reached := false
	impostor.srv.Config.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	// The real cluster's token, pointed at the impostor's address.
	token := hosts.Encode(impostor.srv.URL, c.svc.Authority.Hash(), secret.New("jt_01app.plain"))
	_, err := joiningHost(t).Join(context.Background(), token, "edge-2")
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "Nothing was sent to it")
	require.False(t, reached, "the request must not reach a server with another authority")
}

// TestR415_AJoinedAppHostReceivesNoKeys asserts R-415: an app host is given
// its certificate and the authority's, and none of the keys a control host
// needs.
func TestR415_AJoinedAppHostReceivesNoKeys(t *testing.T) {
	c := newCluster(t)
	j := joiningHost(t)
	joined, err := j.Join(context.Background(), c.token("app"), "edge-2")
	require.NoError(t, err)
	require.True(t, joined.AppOnly())
	require.Equal(t, []string{"app"}, joined.Roles)

	cert := read(t, filepath.Join(j.Files.Dir, hosts.HostCertFile))
	block, _ := pem.Decode([]byte(cert))
	parsed, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.Equal(t, hostagent.ServerName("edge-2"), parsed.Subject.CommonName, "named as the agent expects (R-416)")
	require.Equal(t, []string{"app"}, parsed.Subject.OrganizationalUnit)
	require.ElementsMatch(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, parsed.ExtKeyUsage)
	require.Equal(t, string(c.svc.Authority.CertPEM()), read(t, filepath.Join(j.Files.Dir, hosts.AuthorityCertFile)))
	info, err := os.Stat(filepath.Join(j.Files.Dir, hosts.HostKeyFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	for _, path := range []string{j.Files.DatabaseURLPath, j.Files.TokenKeyPath, j.Files.AuthorityPath} {
		_, err := os.Stat(path)
		require.True(t, errors.Is(err, os.ErrNotExist), "an app host keeps no %s", path)
	}
	require.Equal(t, "edge-2:7443", c.store.hosts[0].Address)
	require.Len(t, c.store.hosts[0].CertSHA256, 64)

	again, done, err := j.Files.Read()
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, joined.HostID, again.HostID)
}

// TestR415_AJoinedControlHostReceivesTheKeys asserts R-415 and R-417: a
// control host is handed the database address, the token key, the secrets
// key and the cluster authority, and writes each where its configuration
// reads it.
func TestR415_AJoinedControlHostReceivesTheKeys(t *testing.T) {
	c := newCluster(t)
	j := joiningHost(t)
	// The joining host writes the secrets key where the local secrets
	// adapter's configuration, shared through the database, says: the path
	// the server answers with. Here that is the server's own file, rewritten
	// with what it holds, so the server's copy is checked to be intact.
	c.svc.Keys.SecretsKeyPath = func(context.Context) string { return filepath.Join(c.keys, "secrets.key") }
	require.NoError(t, os.WriteFile(j.Files.TokenKeyPath, []byte("its own"), 0o600))

	_, err := j.Join(context.Background(), c.token("app", "control"), "")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "an empty name is refused by Join; Setup fills it in")
	joined, err := j.Join(context.Background(), c.token("app", "control"), "core-2")
	require.NoError(t, err)
	require.False(t, joined.AppOnly())

	require.Equal(t, "postgres://pando:pw@db.example.com/pando", read(t, j.Files.DatabaseURLPath))
	require.Equal(t, "token-key-bytes", read(t, j.Files.TokenKeyPath), "the cluster's key replaces its own")
	a, err := hosts.LoadOrCreateAuthority(j.Files.AuthorityPath)
	require.NoError(t, err)
	require.Equal(t, c.svc.Authority.Hash(), a.Hash())
	require.Equal(t, "secrets-key-bytes", read(t, filepath.Join(c.keys, "secrets.key")))
}

// TestR194_TheKeysCrossTheWireOnlyAsTheJoinsBody asserts R-194 around R-415:
// a join result's keys redact themselves anywhere but the wire form, and
// survive the trip there and back.
func TestR194_TheKeysCrossTheWireOnlyAsTheJoinsBody(t *testing.T) {
	res := hosts.JoinResult{Keys: &hosts.ControlKeys{
		DatabaseURL: secret.New("postgres://u:p@h/d"), TokenKey: secret.New("\x00\x01key"),
		SecretsKey: secret.New("sk"), SecretsKeyPath: "/k", Authority: secret.New("PEM"),
	}}
	logged, err := json.Marshal(res)
	require.NoError(t, err)
	require.NotContains(t, string(logged), "u:p")

	wire, err := json.Marshal(res.Wire())
	require.NoError(t, err)
	require.Contains(t, string(wire), "postgres://u:p@h/d")
}

// TestR415_ARedeemedSingleUseTokenCannotJoinTwice asserts R-415.
func TestR415_ARedeemedSingleUseTokenCannotJoinTwice(t *testing.T) {
	c := newCluster(t)
	token := c.token("app")
	_, err := joiningHost(t).Join(context.Background(), token, "edge-2")
	require.NoError(t, err)
	_, err = joiningHost(t).Join(context.Background(), token, "edge-3")
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
	require.Len(t, c.store.hosts, 1)
}

// TestR415_APandoWithAnAccountCannotJoin asserts R-415: a Pando that has an
// account or an app cannot join, and nothing is sent to the cluster.
func TestR415_APandoWithAnAccountCannotJoin(t *testing.T) {
	c := newCluster(t)
	for name, fresh := range map[string]func(context.Context) (bool, bool, error){
		"account": func(context.Context) (bool, bool, error) { return true, false, nil },
		"app":     func(context.Context) (bool, bool, error) { return false, true, nil },
	} {
		t.Run(name, func(t *testing.T) {
			j := joiningHost(t)
			j.Fresh = fresh
			_, err := j.Join(context.Background(), c.token("app"), "edge-2")
			require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
			require.Contains(t, errs.As(err).Message, "cannot join a cluster")
		})
	}
	require.Empty(t, c.store.hosts)

	// A Pando that already joined cannot join again.
	j := joiningHost(t)
	_, err := j.Join(context.Background(), c.token("app"), "edge-2")
	require.NoError(t, err)
	_, err = j.Join(context.Background(), c.token("control"), "edge-2")
	require.Contains(t, errs.As(err).Message, "already joined")
}

// TestR415_AJoinAtAnotherVersionIsRefused asserts R-415's [P] default: a
// host joins only at the cluster's version, and the refusal spends no use.
func TestR415_AJoinAtAnotherVersionIsRefused(t *testing.T) {
	c := newCluster(t)
	token := c.token("app")
	j := joiningHost(t)
	j.Version = "1.2.2"
	_, err := j.Join(context.Background(), token, "edge-2")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "runs Pando 1.2.2 and the cluster runs 1.2.3")
	require.Contains(t, errs.As(err).Remedy, "Install Pando 1.2.3")

	_, err = joiningHost(t).Join(context.Background(), token, "edge-2")
	require.NoError(t, err, "the refused join spent no use")
}

type notReady struct{ roles []string }

func (n *notReady) ReadyToJoin(_ context.Context, roles []string) error {
	n.roles = roles
	return errs.New(errs.StateInvalid, "Set a backup destination off this host first.")
}

func TestReadinessIsAskedWithTheTokensRolesFirst(t *testing.T) {
	c := newCluster(t)
	ready := &notReady{}
	c.svc.Readiness = ready
	token := c.token("app", "control")
	_, err := joiningHost(t).Join(context.Background(), token, "edge-2")
	require.Contains(t, errs.As(err).Message, "backup destination")
	require.Equal(t, []string{"app", "control"}, ready.roles)
	require.Empty(t, c.store.hosts)
	require.Equal(t, 0, c.store.live["jt_01app.plain"].Uses)
}

func TestSetupNamesTheHostAndRestarts(t *testing.T) {
	c := newCluster(t)
	j := joiningHost(t)
	restarted := false
	j.Restart = func() { restarted = true }
	joined, err := j.Setup(context.Background(), c.token("app"), " ")
	require.NoError(t, err)
	require.Equal(t, "edge-2", joined.Name, "the machine's hostname, first label, lowercase")
	require.True(t, restarted)

	_, err = joiningHost(t).Setup(context.Background(), "not a token", "x")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

func TestAJoinThatCannotReachTheClusterSaysSo(t *testing.T) {
	c := newCluster(t)
	token := c.token("app")
	c.srv.Close()
	_, err := joiningHost(t).Join(context.Background(), token, "edge-2")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "can reach")
}

func TestAJoinNeedsAHostNamePandoCanUse(t *testing.T) {
	c := newCluster(t)
	for _, name := range []string{"Edge_2", "-edge", "a.b"} {
		_, err := joiningHost(t).Join(context.Background(), c.token("app"), name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
	}
	_, err := c.svc.Join(context.Background(), hosts.JoinRequest{Version: "1.2.3", Name: "Bad"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	_, err = c.svc.Join(context.Background(), hosts.JoinRequest{Version: "1.2.3", Name: "ok", Secret: secret.New("jt_01app.plain"), CSR: []byte("x")})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "a CSR that is not PEM")
}

// TestR415_AJoinThatFailsOnTheJoiningHostCanBeTriedAgain asserts R-415 for a
// join answered by the cluster that the joining host cannot finish — its
// files cannot be written, or it cannot confirm the join: the cluster lists
// no such host, the same single-use token joins again under the same name,
// and the host is listed once it confirms.
func TestR415_AJoinThatFailsOnTheJoiningHostCanBeTriedAgain(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	token := c.token("app")

	// The files cannot be written: the cluster directory is read-only.
	j := joiningHost(t)
	require.NoError(t, os.MkdirAll(j.Files.Dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(j.Files.Dir, 0o700) })
	_, err := j.Join(ctx, token, "edge-2")
	require.Contains(t, errs.As(err).Message, "could not write")
	list, err := c.svc.Hosts(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "no ghost host is listed")

	// The join cannot be confirmed: this Pando is left unjoined.
	c.peersDown.Store(true)
	j = joiningHost(t)
	_, err = j.Join(ctx, token, "edge-2")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "same join token and name")
	_, joined, err := j.Files.Read()
	require.NoError(t, err)
	require.False(t, joined, "this Pando can join again")
	list, err = c.svc.Hosts(ctx)
	require.NoError(t, err)
	require.Empty(t, list)

	c.peersDown.Store(false)
	done, err := j.Join(ctx, token, "edge-2")
	require.NoError(t, err, "the same token and name join again")
	list, err = c.svc.Hosts(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, done.HostID, list[0].ID)
	require.Len(t, c.store.revoked, 2, "the unfinished joins' certificates are refused")
}

// hung answers once its context ends.
type hung struct{}

func (hung) ReadyToJoin(ctx context.Context, _ []string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestR415_AJoinCheckThatNeverAnswersIsGivenUp asserts R-415: a check a join
// makes before spending its token that never answers is given up on after
// JoinCheckTimeout, the refusal says so, and the token's use is not spent.
func TestR415_AJoinCheckThatNeverAnswersIsGivenUp(t *testing.T) {
	c := newCluster(t)
	c.svc.Readiness = hung{}
	c.svc.JoinCheckTimeout = 10 * time.Millisecond
	_, err := joiningHost(t).Join(context.Background(), c.token("app"), "edge-2")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "did not finish within 10ms")
	require.Contains(t, errs.As(err).Message, "not spent")
	require.Zero(t, c.store.live["jt_01app.plain"].Uses)
	require.Empty(t, c.store.hosts)
}
