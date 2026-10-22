//go:build integration

package buildkit

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// realManaged is the launcher Pando uses, on the Docker daemon the docker CLI
// talks to, under a project name of the test's own: it never touches an
// installation's BuildKits on the same daemon, and everything it made is
// removed by that name afterwards.
func realManaged(t *testing.T) *managed {
	t.Helper()
	// One daemon for the adapter and the docker CLI the test inspects and
	// cleans up with: unset, the client takes the default socket and the CLI
	// its current context, which can be two daemons.
	if os.Getenv("DOCKER_HOST") == "" {
		t.Setenv("DOCKER_HOST", client.DefaultDockerHost)
	}
	dc, err := client.New(client.FromEnv)
	require.NoError(t, err)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	m := newManaged(dc)
	m.project, m.named = "pando-it-"+stamp, true
	m.self = "not-a-container-" + stamp // the test runs on the host
	m.dataDir = t.TempDir()
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range namesWithPrefix(t, "ps", m.daemonPrefix()) {
				logs, _ := exec.Command("docker", "logs", "--tail", "40", c).CombinedOutput()
				t.Logf("%s's log:\n%s", c, logs)
			}
		}
		for _, c := range namesWithPrefix(t, "ps", m.daemonPrefix()) {
			_ = exec.Command("docker", "rm", "-f", "-v", c).Run()
		}
		for _, n := range namesWithPrefix(t, "network", m.daemonPrefix()) {
			_ = exec.Command("docker", "network", "rm", n).Run()
		}
	})
	return m
}

// namesWithPrefix lists containers ("ps") or networks ("network") whose name
// starts with prefix.
func namesWithPrefix(t *testing.T, kind, prefix string) []string {
	t.Helper()
	args := []string{"ps", "-a", "--format", "{{.Names}}"}
	if kind == "network" {
		args = []string{"network", "ls", "--format", "{{.Name}}"}
	}
	out, err := exec.Command("docker", args...).Output()
	require.NoError(t, err)
	var names []string
	for _, n := range strings.Fields(string(out)) {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n)
		}
	}
	return names
}

func dockerInspect(t *testing.T, format, name string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, name).CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

// TestR111_PandosOwnBuildKitStartsAndAnswers asserts R-111 against a real
// Docker daemon: a daemon Pando starts runs under the compiled-in profile,
// unprivileged and with no runtime socket (R-112), and answers BuildKit's API
// once launch returns. It logs the cold start, create to ready for a solve,
// which design 03 records beside the idle window.
func TestR111_PandosOwnBuildKitStartsAndAnswers(t *testing.T) {
	ctx := context.Background()
	m := realManaged(t)
	require.NoError(t, m.ensureImage(ctx), "the image is pulled before the clock starts")

	began := time.Now()
	inst, err := m.launch(ctx, "app_01COLDSTART")
	require.NoError(t, err)
	t.Logf("cold start, create to answering: %s", time.Since(began).Round(10*time.Millisecond))
	t.Cleanup(func() { m.remove(context.Background(), inst) })

	name := m.daemonName("app_01COLDSTART")
	opts := dockerInspect(t, "{{json .HostConfig.SecurityOpt}}", name)
	require.Contains(t, opts, `seccomp={\"defaultAction\":\"SCMP_ACT_ERRNO\"`, "the profile itself, inline")
	require.Contains(t, opts, "Rootless BuildKit")
	require.Equal(t, "false", dockerInspect(t, "{{.HostConfig.Privileged}}", name))
	require.NotContains(t, dockerInspect(t, "{{json .Mounts}}", name), ".sock", "no runtime socket")

	workers, err := inst.cli.ListWorkers(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, workers)
}

// TestR117_TwoAppsBuildOnTwoDaemonsAndNothingOutlivesTheIdleWindow asserts
// R-117 against a real Docker daemon: two apps' builds run on two daemons,
// each on a network of its own, and once their idle window ends nothing made
// for either is left — no container, no network, no volume.
func TestR117_TwoAppsBuildOnTwoDaemonsAndNothingOutlivesTheIdleWindow(t *testing.T) {
	ctx := context.Background()
	m := realManaged(t)
	pool := newDaemons(m)
	pool.idle = 2 * time.Second

	a, err := pool.acquire(ctx, "app_01AAA")
	require.NoError(t, err)
	b, err := pool.acquire(ctx, "app_01BBB")
	require.NoError(t, err)
	require.NotEqual(t, a.inst.container, b.inst.container, "two apps, two daemons")
	require.NotEqual(t, a.inst.network, b.inst.network, "on two networks")
	require.Len(t, namesWithPrefix(t, "ps", m.daemonPrefix()), 2)

	var volumes []string
	for _, inst := range []*instance{a.inst, b.inst} {
		for _, v := range strings.Fields(dockerInspect(t, `{{range .Mounts}}{{.Name}} {{end}}`, inst.container)) {
			volumes = append(volumes, v)
		}
		nets := dockerInspect(t, `{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}`, inst.container)
		require.Equal(t, inst.network, strings.TrimSpace(nets), "its own network and no other")
	}
	require.NotEmpty(t, volumes, "the image's state volume, anonymous")

	pool.release(a)
	pool.release(b)
	require.Eventually(t, func() bool {
		return len(namesWithPrefix(t, "ps", m.daemonPrefix())) == 0 &&
			len(namesWithPrefix(t, "network", m.daemonPrefix())) == 0
	}, time.Minute, 500*time.Millisecond, "no daemon and no network once the idle window ends")
	for _, v := range volumes {
		require.Error(t, exec.Command("docker", "volume", "inspect", v).Run(), "volume %s outlived its daemon", v)
	}
	require.Zero(t, pool.count())
}

// TestR117_ABuildStepCannotDriveItsDaemon asserts R-117 against a real
// daemon: its API refuses a client without a certificate from the install's
// build authority — from outside, and from a build step, which shares the
// daemon's network namespace and reaches its listener on loopback.
func TestR117_ABuildStepCannotDriveItsDaemon(t *testing.T) {
	ctx := context.Background()
	m := realManaged(t)
	inst, err := m.launch(ctx, "app_01STEP")
	require.NoError(t, err)
	t.Cleanup(func() { m.remove(context.Background(), inst) })

	addr, err := m.address(ctx, inst.container, m.daemonName("app_01STEP"))
	require.NoError(t, err)
	refused := func(opts ...bkclient.ClientOpt) {
		t.Helper()
		c, err := bkclient.New(ctx, addr, opts...)
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err = c.ListWorkers(check)
		require.Error(t, err)
	}
	refused()
	refused(bkclient.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // a client that trusts anything, and presents nothing
		MinVersion:         tls.VersionTLS12,
	}))))

	// A step that tries, as it could on a daemon without TLS. It succeeds only
	// if buildctl is refused; the marker says it ran at all.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+managedImage+"\n"+
		"RUN if buildctl --addr tcp://127.0.0.1:"+managedPort+" debug workers; then exit 1; fi; "+
		"echo REFUSED\n"), 0o600))
	fs, err := fsutil.NewFS(dir)
	require.NoError(t, err)
	ch := make(chan *bkclient.SolveStatus)
	var log strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s := range ch {
			for _, l := range s.Logs {
				log.Write(l.Data)
			}
		}
	}()
	_, err = inst.cli.Solve(ctx, nil, bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{"no-cache": ""},
		LocalMounts:   map[string]fsutil.FS{"context": fs, "dockerfile": fs},
	}, ch)
	<-done
	require.NoError(t, err, "a step reached its daemon's API:\n%s", log.String())
	require.Contains(t, log.String(), "REFUSED")
}
