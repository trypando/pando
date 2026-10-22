package buildkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// writeTLSFiles writes a CA, and a certificate and key, as an operator would
// mount them into Pando's container.
func writeTLSFiles(t *testing.T) (ca, cert, key string) {
	t.Helper()
	authority, err := loadAuthority(t.TempDir())
	require.NoError(t, err)
	issued, err := authority.issueServer("buildkit.example")
	require.NoError(t, err)
	dir := t.TempDir()
	ca, cert, key = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(ca, authority.certPEM(), 0o600))
	require.NoError(t, os.WriteFile(cert, issued.certPEM, 0o600))
	require.NoError(t, os.WriteFile(key, issued.keyPEM, 0o600))
	return ca, cert, key
}

// TestR117_AnInstallTopologysBuildKitCanBeReachedOverTLS covers the BuildKit
// an install topology runs beside Pando, which every app shares: Pando
// reaches it with the CA and client certificate it is given, from its
// configuration or the environment, and refuses half a client certificate.
func TestR117_AnInstallTopologysBuildKitCanBeReachedOverTLS(t *testing.T) {
	ca, cert, key := writeTLSFiles(t)

	a := New()
	raw, _ := json.Marshal(Config{Address: "tcp://buildkit.example:1234", TLSCAFile: ca, TLSCertFile: cert, TLSKeyFile: key})
	require.NoError(t, a.Configure(context.Background(), raw))
	require.NotNil(t, a.cli)
	require.Nil(t, a.daemons, "shared, not one per app")
	opts, err := a.externalTLS()
	require.NoError(t, err)
	require.Len(t, opts, 2, "the CA, and the client certificate")

	t.Setenv("PANDO_BUILDKIT_TLS_CA", ca)
	t.Setenv("PANDO_BUILDKIT_TLS_SERVER_NAME", "buildkit.internal")
	b := New()
	require.NoError(t, b.Configure(context.Background(), json.RawMessage(`{"address":"tcp://10.0.0.5:1234"}`)))
	opts, err = b.externalTLS()
	require.NoError(t, err)
	require.Len(t, opts, 1, "the CA alone, from the environment")

	half := New()
	raw, _ = json.Marshal(Config{Address: "tcp://buildkit.example:1234", TLSCertFile: cert})
	err = half.Configure(context.Background(), raw)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.ErrorContains(t, err, "PANDO_BUILDKIT_TLS_KEY")
}

// A build on an install topology's BuildKit uses the one it shares, and lets
// nothing go when done; with neither that nor Pando's own, it is refused.
func TestABuildFindsItsBuildKit(t *testing.T) {
	a := New()
	_, _, err := a.daemonFor(context.Background(), "app_01A/web")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"address":"tcp://127.0.0.1:1"}`)))
	cli, done, err := a.daemonFor(context.Background(), "app_01A/web")
	require.NoError(t, err)
	require.Same(t, a.cli, cli)
	done()

	p, l, _, _ := testPool(2)
	own := &Adapter{daemons: p}
	own.LimitDaemons(func(context.Context) int { return 1 })
	_, done, err = own.daemonFor(context.Background(), "app_01A/web")
	require.NoError(t, err)
	_, _, err = own.daemonFor(context.Background(), "app_01A/worker")
	require.NoError(t, err, "a compose app's services share their app's")
	launched, _ := l.state()
	require.Equal(t, []string{"app_01A"}, launched)
	done()
	require.Equal(t, 1, p.count())
}

// TestR117_ForgettingAnAppRemovesItsDaemonsAndItsCache asserts deleting an
// app removes its BuildKits — this Pando's, and other replicas' — as well as
// its build cache, and that a failure to find them is reported.
func TestR117_ForgettingAnAppRemovesItsDaemonsAndItsCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PANDO_BUILD_CACHE_DIR", root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "app_01GONE", "web"), 0o755))

	f := newFakeDocker("pando-1")
	p, l, _, _ := testPool(4)
	a := &Adapter{managed: testManaged(t, f), daemons: p}
	d, err := p.acquire(context.Background(), "app_01GONE")
	require.NoError(t, err)
	p.release(context.Background(), d)

	require.NoError(t, a.Forget(context.Background(), "app_01GONE/web"))
	_, removed := l.state()
	require.Equal(t, []string{"app_01GONE"}, removed)
	require.NoDirExists(t, filepath.Join(root, "app_01GONE"))

	f.fail = map[string]error{"ContainerList": errDaemon}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.Forget(context.Background(), "app_01GONE")))
}

// The health check of an install topology's BuildKit asks it, and says so
// when it does not answer or its TLS settings do not agree.
func TestAnInstallTopologysBuildKitIsAskedWhetherItAnswers(t *testing.T) {
	a := New()
	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"address":"tcp://127.0.0.1:1"}`)))
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.HealthCheck(context.Background())))

	_, cert, _ := writeTLSFiles(t)
	b := &Adapter{address: "tcp://127.0.0.1:1", config: Config{TLSCertFile: cert}}
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(b.HealthCheck(context.Background())))
	keepShared()
}

func TestTheBuildNetworkPoolIsPandosUnlessSetOtherwise(t *testing.T) {
	for raw, want := range map[string]string{
		"":             "10.213.0.0/16",
		"10.50.0.0/20": "10.50.0.0/20",
		"10.50.0.9/20": "10.50.0.0/20",
		"off":          "",
		"docker":       "",
		"not a prefix": "",
		"10.50.0.0/30": "",
		"fd00::/64":    "",
	} {
		t.Setenv("PANDO_BUILDKIT_NETWORK_POOL", raw)
		pool, ok := buildNetworkPool()
		if want == "" {
			require.False(t, ok, raw)
			continue
		}
		require.True(t, ok, raw)
		require.Equal(t, want, pool.String(), raw)
	}
	require.Empty(t, firstName(nil))
	require.GreaterOrEqual(t, defaultDaemonLimit(), 2)
}

// A TLS file for the install topology's BuildKit that is missing or damaged
// is reported by its setting when Pando configures the adapter.
func TestATLSFileThatCannotBeReadIsReportedByItsSetting(t *testing.T) {
	ca, cert, key := writeTLSFiles(t)
	junk := filepath.Join(t.TempDir(), "junk.pem")
	require.NoError(t, os.WriteFile(junk, []byte("not PEM"), 0o600))
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{TLSCAFile: filepath.Join(t.TempDir(), "missing.pem")}, "PANDO_BUILDKIT_TLS_CA"},
		{Config{TLSCAFile: junk}, "PANDO_BUILDKIT_TLS_CA"},
		{Config{TLSCAFile: ca, TLSCertFile: cert, TLSKeyFile: junk}, "PANDO_BUILDKIT_TLS_KEY"},
		{Config{TLSCertFile: key, TLSKeyFile: cert}, "PANDO_BUILDKIT_TLS_CERT"},
	} {
		c.cfg.Address = "tcp://buildkit.example:1234"
		raw, _ := json.Marshal(c.cfg)
		err := New().Configure(context.Background(), raw)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "%+v", c.cfg)
		require.ErrorContains(t, err, c.want)
	}
}
