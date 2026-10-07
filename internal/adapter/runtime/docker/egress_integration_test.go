//go:build integration

package docker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dockeradapter "github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/egress"
)

// gatewayImage builds an image holding this checkout's pando binary at the
// path the adapter runs the gateway from, and nothing else: FROM scratch, so
// nothing is pulled and the gateway has no shell to fall back on.
func gatewayImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "pando"), "./cmd/pando")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY pando /usr/local/bin/pando\n"), 0o600))
	tag := "pando-egress-test:" + time.Now().Format("150405")
	out, err = exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	return tag
}

// destinations starts a container standing in for somewhere on the internet:
// "allowed-destination" on port 18080 and "denied-destination" on 18081.
// Nothing leaves the host. Returns its name.
//
// The test joins it to whichever network it is reached from and uses its
// address there. It used to be reached at a port published on the host,
// through the network's gateway address — which is the host on Linux and is
// not on Docker Desktop, where the daemon runs in a VM and published ports
// are forwarded from the Mac, so the test failed there before it reached
// anything about egress. The ports are still published, so the test still
// shows a restricted app cannot reach the host's either.
func destinations(t *testing.T) string {
	t.Helper()
	name := "egress-dest-" + time.Now().Format("150405.000")
	name = strings.ReplaceAll(name, ".", "")
	serve := func(port, body string) string {
		resp := "HTTP/1.0 200 OK\\r\\nContent-Type: text/plain\\r\\n\\r\\n" + body
		return "while true; do printf '" + resp + "' | nc -l -p " + port + " >/dev/null; done"
	}
	script := "(" + serve("18080", "allowed-destination") + ") & " + serve("18081", "denied-destination")
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-p", "18080:18080", "-p", "18081:18081", "alpine:3.20", "sh", "-c", script).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	return name
}

// reachableOn joins container to network and returns its address there.
func reachableOn(t *testing.T, container, network string) string {
	t.Helper()
	out, err := dockerCLI("network", "connect", network, container)
	require.NoError(t, err, out)
	addr, err := dockerCLI("inspect", "-f",
		`{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`, container)
	require.NoError(t, err, addr)
	require.NotEmpty(t, addr)
	return addr
}

// leave takes container off network, so a network the adapter removes is
// not held open by the test.
func leave(t *testing.T, container, network string) {
	t.Helper()
	out, err := dockerCLI("network", "disconnect", network, container)
	require.NoError(t, err, out)
}

// inApp runs a shell command in a workload and returns what it printed and
// whether it succeeded.
func inApp(bundleID, workload, script string) (string, bool) {
	out, err := exec.Command("docker", "exec", "pando-"+bundleID+"-"+workload, "sh", "-c", script).CombinedOutput()
	return string(out), err == nil
}

// networkGateway is the host's address on a network: its IPv4 gateway.
//
// Read from the network's IPAM configuration, and worked out from the subnet
// when that leaves the gateway empty — newer Docker does for a network created
// with a subnet and no gateway, and puts the gateway at the subnet's first
// address, which is what this returns.
func networkGateway(name string) (string, error) {
	raw, err := dockerCLI("network", "inspect", "-f", "{{json .IPAM.Config}}", name)
	if err != nil {
		return "", err
	}
	var configs []struct {
		Subnet  string `json:"Subnet"`
		Gateway string `json:"Gateway"`
	}
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		return "", fmt.Errorf("reading %s's address configuration %q: %w", name, raw, err)
	}
	for _, c := range configs {
		if gw, err := netip.ParseAddr(c.Gateway); err == nil && gw.Is4() {
			return gw.String(), nil
		}
		if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
			return p.Masked().Addr().Next().String(), nil
		}
	}
	return "", fmt.Errorf("network %s has no IPv4 address configuration: %s", name, raw)
}

// TestR187_ARestrictedAppReachesOnlyWhatItsRulesAllowThroughTheGateway asserts
// R-185 – R-187 end to end on a real daemon: a restricted app's workload has no
// route out of its network, reaches an allowed destination through the
// gateway — as plain HTTP and as a CONNECT tunnel — and is refused one its
// rules do not allow, with the reason.
func TestR187_ARestrictedAppReachesOnlyWhatItsRulesAllowThroughTheGateway(t *testing.T) {
	ctx := context.Background()
	a := dockeradapter.New()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"egress_gateway_image":"`+gatewayImage(t)+`"}`)))
	if err := a.HealthCheck(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.SupportsEgressRestriction)

	dest := destinations(t)

	id := "test-egress-" + time.Now().Format("150405")
	cleanup(t, a, id)

	// First unrestricted, to see an unrestricted app reach the destination
	// directly, as it always could (R-186).
	plan := bundle(id, nil)
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)
	hostIP, err := networkGateway("pando-" + id)
	require.NoError(t, err)
	require.NotEmpty(t, hostIP)
	destIP := reachableOn(t, dest, "pando-"+id)
	out, ok := inApp(id, "web", "wget -q -O - -T 5 http://"+destIP+":18080/")
	require.True(t, ok, "an unrestricted app reaches the destination directly: %s", out)
	require.Contains(t, out, "allowed-destination")
	leave(t, dest, "pando-"+id)

	// Now restricted to one destination, listed by port across the private
	// ranges because its address is whatever the daemon hands out.
	plan.Network.Egress = egress.Rules{Layers: []egress.Layer{{
		Mode: egress.Allowlist, From: "install",
		List: []string{"10.0.0.0/8:18080", "172.16.0.0/12:18080", "192.168.0.0/16:18080"},
	}}}
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)
	gwIP, err := networkGateway("pando-" + id + "-outbound")
	require.NoError(t, err)
	// Reachable from the gateway's side, which is the only way out.
	outIP := reachableOn(t, dest, "pando-"+id+"-outbound")
	internal, err := dockerCLI("network", "inspect", "-f", "{{.Internal}}", "pando-"+id+"-internal")
	require.NoError(t, err)
	require.Equal(t, "true", internal)

	// The gateway takes a moment to listen.
	require.Eventually(t, func() bool {
		_, ok := inApp(id, "web", "echo | nc -w 2 pando-egress 3128")
		return ok
	}, 30*time.Second, 500*time.Millisecond, "the gateway never answered")

	t.Run("there is no route out except the gateway", func(t *testing.T) {
		for _, target := range []string{hostIP, gwIP, outIP} {
			out, ok := inApp(id, "web", "wget -Y off -q -O - -T 3 http://"+target+":18080/")
			require.False(t, ok, "reached %s directly: %s", target, out)
		}
		// Nor the address the internal network's bridge has on the host,
		// where a published port would otherwise answer.
		if bridge, err := dockerCLI("network", "inspect", "-f", "{{range .IPAM.Config}}{{.Gateway}}{{end}}", "pando-"+id+"-internal"); err == nil && bridge != "" {
			out, ok := inApp(id, "web", "wget -Y off -q -O - -T 3 http://"+bridge+":18080/")
			require.False(t, ok, "reached the host through the internal network's bridge %s: %s", bridge, out)
		}
	})

	t.Run("an allowed destination through the gateway, as plain HTTP", func(t *testing.T) {
		out, ok := inApp(id, "web", "wget -q -O - -T 5 http://"+outIP+":18080/")
		require.True(t, ok, out)
		require.Contains(t, out, "allowed-destination")
	})

	t.Run("and as a CONNECT tunnel", func(t *testing.T) {
		out, _ := inApp(id, "web",
			`printf 'CONNECT `+outIP+`:18080 HTTP/1.1\r\nHost: `+outIP+`:18080\r\n\r\nGET / HTTP/1.0\r\n\r\n' | nc -w 5 pando-egress 3128`)
		require.Contains(t, out, "200 Connection Established")
		require.Contains(t, out, "allowed-destination")
	})

	t.Run("a destination the rules do not allow is refused, and says why", func(t *testing.T) {
		out, ok := inApp(id, "web", "wget -q -O - -T 5 http://"+outIP+":18081/")
		require.False(t, ok)
		require.Contains(t, out, "403")

		out, _ = inApp(id, "web",
			`printf 'CONNECT `+outIP+`:18081 HTTP/1.1\r\nHost: `+outIP+`:18081\r\n\r\n' | nc -w 5 pando-egress 3128`)
		require.Contains(t, out, "403 Forbidden")
		require.Contains(t, out, "not on the installation's allowlist")
		require.NotContains(t, out, "denied-destination")

		logs, err := dockerCLI("logs", "pando-egress-"+id)
		require.NoError(t, err)
		require.Contains(t, logs, `"msg":"egress refused"`)
		require.Contains(t, logs, `"port":18081`)
	})

	t.Run("blocking private ranges refuses even a listed private destination", func(t *testing.T) {
		before, err := dockerCLI("inspect", "-f", "{{.Id}}", "pando-"+id+"-web")
		require.NoError(t, err)

		plan.Network.Egress.BlockPrivate = true
		_, err = a.Apply(ctx, plan)
		require.NoError(t, err)
		after, err := dockerCLI("inspect", "-f", "{{.Id}}", "pando-"+id+"-web")
		require.NoError(t, err)
		require.Equal(t, before, after, "new rules recreate the gateway, not the app")

		require.Eventually(t, func() bool {
			out, _ := inApp(id, "web",
				`printf 'CONNECT `+outIP+`:18080 HTTP/1.1\r\nHost: x\r\n\r\n' | nc -w 5 pando-egress 3128`)
			return strings.Contains(out, "private addresses are blocked")
		}, 30*time.Second, 500*time.Millisecond)
	})

	t.Run("and unrestricted again, it runs as it always did", func(t *testing.T) {
		leave(t, dest, "pando-"+id+"-outbound")
		plan.Network.Egress = egress.Rules{}
		_, err := a.Apply(ctx, plan)
		require.NoError(t, err)
		_, err = dockerCLI("inspect", "pando-egress-"+id)
		require.Error(t, err, "the gateway is gone")
		env := dockerInspect(t, "pando-"+id+"-web", "{{json .Config.Env}}")
		require.NotContains(t, env, "HTTP_PROXY")
		destIP := reachableOn(t, dest, "pando-"+id)
		out, ok := inApp(id, "web", "wget -q -O - -T 5 http://"+destIP+":18080/")
		require.True(t, ok, out)
		require.Contains(t, out, "allowed-destination")
	})

}
