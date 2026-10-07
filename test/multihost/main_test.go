//go:build multihost

// Package multihost_test runs the docker-hosts runtime (internal/adapter/
// runtime/multidocker) against real Docker daemons: two Docker-in-Docker
// containers stand in for app hosts, a third for the control host Pando runs
// on, each reached over TLS with certificates this package generates
// (notes-multi-host-docker-issue-72.md, "Tests this PR is done with").
//
// `make test-multihost` builds the image and runs it with MH_TEST=1; without
// MH_TEST=1 every test is skipped. Everything this package creates is named
// mh-test-… and is removed when it finishes, whether or not a test failed
// (MH_KEEP=1 keeps it to look around).
//
// The topology, on one user-defined network (mh-test-net, 192.168.213.0/24):
//
//	mh-test-postgres  .10   Pando's database
//	mh-test-registry  .11   the install registry (registry:3, plain HTTP)
//	mh-test-buildkit  .12   rootless BuildKit, which pushes to the registry
//	mh-test-control   .20   DinD: Pando (mh-test-pando-1) and the control agent
//	mh-test-host-a    .21   DinD app host, Docker API on tcp://…:2376 with TLS
//	mh-test-host-b    .22   DinD app host, likewise
//
// Pando's image is loaded into the control host and pushed to the registry;
// the app hosts pull it from there for their agents, as a real install's would.
package multihost_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trypando/pando/internal/hostagent"
)

const (
	netName     = "mh-test-net"
	netSubnet   = "192.168.213.0/24"
	ipPostgres  = "192.168.213.10"
	ipRegistry  = "192.168.213.11"
	ipBuildKit  = "192.168.213.12"
	ipControl   = "192.168.213.20"
	ipHostA     = "192.168.213.21"
	ipHostB     = "192.168.213.22"
	cPostgres   = "mh-test-postgres"
	cRegistry   = "mh-test-registry"
	cBuildKit   = "mh-test-buildkit"
	cControl    = "mh-test-control"
	cHostA      = "mh-test-host-a"
	cHostB      = "mh-test-host-b"
	cPando      = "mh-test-pando-1" // inside mh-test-control
	cPando2     = "mh-test-pando-2" // likewise
	registry    = ipRegistry + ":5000"
	pandoRemote = registry + "/mh-test/pando:dev"

	// The configured size of each app host: small, so placement and the
	// plan-time capacity check can be driven by a few apps.
	hostAMemory = 2 << 30
	hostBMemory = 1536 << 20
	hostCPU     = 4000

	adminPassword = "mh-test-admin-password"
	echoImage     = "mendhak/http-https-echo:34"
	busyboxImage  = "busybox:1.37"
	echoRemote    = registry + "/mh-test/echo:34"
	busyboxRemote = registry + "/mh-test/busybox:1.37"
	dindImage     = "docker:28-dind"
)

// hostNames maps the adapter's host names to their DinD containers.
var hostNames = map[string]string{"control": cControl, "host-a": cHostA, "host-b": cHostB}

var (
	enabled bool
	// base is Pando's address as the test reaches it: the control host's
	// published 8080.
	base string
	// base2 is the second replica, published as 8081.
	base2 string
	// agentPorts is where each app host's agent port is published on
	// 127.0.0.1, for the handshake tests.
	agentPorts = map[string]string{}
	// authorityPEM is the agents' authority, as the adapter's credential
	// holds it. The test holds it too, to show what holding it means.
	authorityPEM []byte
)

func TestMain(m *testing.M) {
	enabled = os.Getenv("MH_TEST") == "1"
	if !enabled {
		os.Exit(m.Run())
	}
	if os.Getenv("MH_REUSE") == "1" && reuse() == nil {
		// A topology an earlier run kept (MH_KEEP=1), for iterating on tests.
		code := m.Run()
		os.Exit(code)
	}
	teardown() // whatever an interrupted run left
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "multihost setup failed:", err)
		dumpLogs()
		if os.Getenv("MH_KEEP") == "" {
			teardown()
		}
		os.Exit(1)
	}
	code := m.Run()
	if code != 0 {
		dumpLogs()
	}
	if os.Getenv("MH_KEEP") == "" {
		teardown()
	}
	os.Exit(code)
}

// docker runs the docker CLI on the outer daemon (DOCKER_CONTEXT applies).
func docker(args ...string) (string, error) {
	return dockerStdin(nil, args...)
}

func dockerStdin(stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// inner runs the docker CLI against the daemon inside a DinD container.
func inner(container string, args ...string) (string, error) {
	return docker(append([]string{"exec", container, "docker"}, args...)...)
}

// containers is everything setup creates on the outer daemon.
var containers = []string{cControl, cHostA, cHostB, cBuildKit, cRegistry, cPostgres}

// teardown removes what setup made, by name. -v removes each container's
// anonymous volumes: the DinD hosts' /var/lib/docker, BuildKit's cache.
func teardown() {
	for _, c := range containers {
		_, _ = docker("rm", "-f", "-v", c)
	}
	_, _ = docker("network", "rm", netName)
}

func dumpLogs() {
	if out, err := inner(cControl, "logs", "--tail", "40", cPando2); err == nil {
		fmt.Fprintln(os.Stderr, "---- pando replica 2 logs (tail) ----\n"+out)
	}
	if out, err := inner(cControl, "logs", "--tail", "200", cPando); err == nil || out != "" {
		fmt.Fprintln(os.Stderr, "---- pando logs (tail) ----\n"+out)
	}
	for _, h := range []string{cHostA, cHostB} {
		if out, err := inner(h, "logs", "--tail", "30", "pando-agent"); err == nil {
			fmt.Fprintf(os.Stderr, "---- %s agent logs ----\n%s\n", h, out)
		}
	}
}

func setup() error {
	image := os.Getenv("MH_IMAGE")
	if image == "" {
		image = "mh-test-pando:dev"
	}
	if _, err := docker("image", "inspect", image); err != nil {
		return fmt.Errorf("the Pando image %s is not built (make test-multihost builds it): %w", image, err)
	}

	dir, err := os.MkdirTemp("", "mh-test-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeCerts(dir); err != nil {
		return err
	}

	if _, err := docker("network", "create", "--subnet", netSubnet, "--label", "mh-test=1", netName); err != nil {
		return err
	}
	run := func(args ...string) error {
		_, err := docker(append([]string{"run", "-d", "--label", "mh-test=1", "--network", netName}, args...)...)
		return err
	}
	if err := run("--name", cPostgres, "--ip", ipPostgres,
		"-e", "POSTGRES_USER=pando", "-e", "POSTGRES_PASSWORD=mh-test", "-e", "POSTGRES_DB=pando",
		"postgres:17-alpine"); err != nil {
		return err
	}
	if err := run("--name", cRegistry, "--ip", ipRegistry, "registry:3"); err != nil {
		return err
	}
	// BuildKit reaches the registry over plain HTTP, as the hosts do.
	if _, err := docker("create", "--label", "mh-test=1", "--network", netName, "--name", cBuildKit, "--ip", ipBuildKit,
		"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined",
		"moby/buildkit:v0.17.2-rootless", "--addr", "tcp://0.0.0.0:1234", "--oci-worker-no-process-sandbox"); err != nil {
		return err
	}
	if _, err := docker("cp", filepath.Join(dir, "buildkit")+"/.", cBuildKit+":/home/user"); err != nil {
		return err
	}
	if _, err := docker("start", cBuildKit); err != nil {
		return err
	}

	// The three Docker hosts. The app hosts serve the Docker API over TLS
	// with a client certificate, as the adapter's tcp:// endpoints require;
	// the control host only on its socket, which Pando's container mounts.
	type dind struct {
		name, ip string
		tls      bool
		publish  []string
	}
	for _, d := range []dind{
		{cControl, ipControl, false, []string{"127.0.0.1::8080", "127.0.0.1::8081"}},
		{cHostA, ipHostA, true, []string{"127.0.0.1::7443"}},
		{cHostB, ipHostB, true, []string{"127.0.0.1::7443"}},
	} {
		args := []string{"create", "--privileged", "--label", "mh-test=1", "--name", d.name, "--hostname", d.name,
			"--network", netName, "--ip", d.ip, "-e", "DOCKER_TLS_CERTDIR="}
		for _, p := range d.publish {
			args = append(args, "-p", p)
		}
		args = append(args, dindImage, "dockerd", "--host=unix:///var/run/docker.sock",
			"--insecure-registry="+registry)
		if d.tls {
			args = append(args, "--host=tcp://0.0.0.0:2376", "--tlsverify",
				"--tlscacert=/mh-certs/ca.pem", "--tlscert=/mh-certs/"+d.name+".pem", "--tlskey=/mh-certs/"+d.name+"-key.pem")
		}
		if _, err := docker(args...); err != nil {
			return err
		}
		if _, err := docker("cp", filepath.Join(dir, "certs")+"/.", d.name+":/mh-certs"); err != nil {
			return err
		}
		if _, err := docker("start", d.name); err != nil {
			return err
		}
	}
	for _, h := range []string{cControl, cHostA, cHostB} {
		if err := awaitDaemon(h, 2*time.Minute); err != nil {
			return err
		}
	}
	for name, c := range map[string]string{"host-a": cHostA, "host-b": cHostB} {
		port, err := publishedPort(c, "7443/tcp")
		if err != nil {
			return err
		}
		agentPorts[name] = port
	}

	// Pando's image onto the control host, and from there into the registry,
	// from which each app host pulls it for its agent.
	saved, err := exec.Command("docker", "save", image).Output()
	if err != nil {
		return fmt.Errorf("docker save %s: %w", image, err)
	}
	if _, err := dockerStdin(saved, "exec", "-i", cControl, "docker", "load"); err != nil {
		return err
	}
	if _, err := inner(cControl, "tag", image, pandoRemote); err != nil {
		return err
	}
	if err := retry(time.Minute, func() error { _, err := inner(cControl, "push", "-q", pandoRemote); return err }); err != nil {
		return err
	}
	// The images the tests run and build from, pushed to the install
	// registry from the outer daemon's copies, so the run never depends on
	// Docker Hub's anonymous pull limit: Pando reads an image's digest from
	// its registry at deploy time, and the hosts and BuildKit pull it.
	for local, remote := range map[string]string{echoImage: echoRemote, busyboxImage: busyboxRemote} {
		saved, err := exec.Command("docker", "save", local).Output()
		if err != nil {
			return fmt.Errorf("docker save %s (pull it first): %w", local, err)
		}
		if _, err := dockerStdin(saved, "exec", "-i", cControl, "docker", "load"); err != nil {
			return err
		}
		if _, err := inner(cControl, "tag", local, remote); err != nil {
			return err
		}
		if _, err := inner(cControl, "push", "-q", remote); err != nil {
			return err
		}
	}

	// Pando on the control host, configured with the docker-hosts runtime as
	// the default (docs/reference.md, "Running apps on several Docker hosts").
	if _, err := docker("exec", cControl, "mkdir", "-p", "/mh-test"); err != nil {
		return err
	}
	if _, err := docker("cp", filepath.Join(dir, "etc")+"/.", cControl+":/mh-test"); err != nil {
		return err
	}
	if _, err := docker("exec", cControl, "chmod", "-R", "a+rX", "/mh-test"); err != nil {
		return err
	}
	// Two replicas sharing /var/lib/pando, the supported topology on the
	// control host (O-47). The first migrates and claims the install; the
	// second starts once it has.
	if err := startReplica(cPando, "8080"); err != nil {
		return err
	}
	if err := findBases(); err != nil {
		return err
	}
	if err := awaitReady(base, 3*time.Minute); err != nil {
		return err
	}
	claim()
	if err := startReplica(cPando2, "8081"); err != nil {
		return err
	}
	if err := awaitReady(base2, 3*time.Minute); err != nil {
		return err
	}
	return awaitRuntime(5 * time.Minute)
}

func startReplica(name, port string) error {
	_, err := inner(cControl, "run", "-d", "--name", name, "--restart", "unless-stopped",
		"-p", port+":8080",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", "mh-test-pando-data:/var/lib/pando",
		"-v", "/mh-test:/etc/mh-test:ro",
		"-e", "PANDO_DATABASE_URL=postgres://pando:mh-test@"+ipPostgres+":5432/pando?sslmode=disable",
		"-e", "PANDO_ADMIN_PASSWORD="+adminPassword,
		"-e", "PANDO_BUILDKIT_ADDRESS=tcp://"+ipBuildKit+":1234",
		"-e", "PANDO_REGISTRY_URL=http://"+registry,
		"-e", "PANDO_REGISTRY_INSECURE=true",
		"-e", "PANDO_RECONCILER_BACKOFF=0s,1s,2s,3s,4s",
		"-e", "PANDO_RECONCILER_FAILURE_WINDOW=2m",
		"-e", "PANDO_APPS_CPU_MILLIS=100",
		"-e", "PANDO_APPS_MEMORY_BYTES=134217728",
		pandoRemote, "serve", "--config", "/etc/mh-test/pando.yaml")
	return err
}

// findBases reads where the control host publishes each replica's port.
func findBases() error {
	port, err := publishedPort(cControl, "8080/tcp")
	if err != nil {
		return err
	}
	base = "http://127.0.0.1:" + port
	port, err = publishedPort(cControl, "8081/tcp")
	if err != nil {
		return err
	}
	base2 = "http://127.0.0.1:" + port
	return nil
}

// reuse picks up a running topology instead of building one.
func reuse() error {
	if err := findBases(); err != nil {
		return err
	}
	for name, c := range map[string]string{"host-a": cHostA, "host-b": cHostB} {
		p, err := publishedPort(c, "7443/tcp")
		if err != nil {
			return err
		}
		agentPorts[name] = p
	}
	pem, err := docker("exec", cControl, "cat", "/mh-test/agent_authority.pem")
	if err != nil {
		return err
	}
	authorityPEM = []byte(pem + "\n")
	return awaitReady(base, 10*time.Second)
}

func retry(within time.Duration, fn func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := fn()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}

func awaitDaemon(c string, within time.Duration) error {
	return retry(within, func() error { _, err := inner(c, "info", "--format", "{{.ServerVersion}}"); return err })
}

func publishedPort(c, port string) (string, error) {
	out, err := docker("port", c, port)
	if err != nil {
		return "", err
	}
	line := strings.Split(out, "\n")[0]
	_, p, err := net.SplitHostPort(line)
	return p, err
}

func awaitReady(base string, within time.Duration) error {
	return retry(within, func() error {
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("readyz: %d", resp.StatusCode)
		}
		return nil
	})
}

// writeCerts writes the Docker TLS material (a CA, a server certificate per
// app host for its address, a client certificate), the agents' authority,
// and Pando's configuration file.
func writeCerts(dir string) error {
	certs := filepath.Join(dir, "certs")
	etc := filepath.Join(dir, "etc")
	for _, d := range []string{certs, etc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	caKey, caCert, caPEM, err := newCA()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certs, "ca.pem"), caPEM, 0o644); err != nil {
		return err
	}
	for name, ip := range map[string]string{cHostA: ipHostA, cHostB: ipHostB} {
		certPEM, keyPEM, err := leaf(caCert, caKey, name, []net.IP{net.ParseIP(ip)}, x509.ExtKeyUsageServerAuth)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(certs, name+".pem"), certPEM, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(certs, name+"-key.pem"), keyPEM, 0o644); err != nil {
			return err
		}
	}
	clientPEM, clientKey, err := leaf(caCert, caKey, "pando", nil, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return err
	}
	bundle := append(append(append([]byte{}, caPEM...), clientPEM...), clientKey...)
	if err := os.WriteFile(filepath.Join(etc, "docker_tls.pem"), bundle, 0o644); err != nil {
		return err
	}

	bk := filepath.Join(dir, "buildkit", ".config", "buildkit")
	if err := os.MkdirAll(bk, 0o755); err != nil {
		return err
	}
	toml := fmt.Sprintf("[registry.%q]\n  http = true\n  insecure = true\n", registry)
	if err := os.WriteFile(filepath.Join(bk, "buildkitd.toml"), []byte(toml), 0o644); err != nil {
		return err
	}

	authorityPEM, err = hostagent.NewAuthority()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(etc, "agent_authority.pem"), authorityPEM, 0o644); err != nil {
		return err
	}

	config := fmt.Sprintf(`adapters:
  rt_hosts:
    category: runtime
    kind: docker-hosts
    name: Docker hosts
    default: true
    config:
      agent_image: %s
      hosts:
        - {name: control, control: true, no_placement: true, endpoint: "unix:///var/run/docker.sock", agent_address: "%s:7443"}
        - {name: host-a, endpoint: "tcp://%s:2376", total_memory_bytes: %d, total_cpu_millis: %d}
        - {name: host-b, endpoint: "tcp://%s:2376", total_memory_bytes: %d, total_cpu_millis: %d}
    credentials:
      agent_authority: {file: /etc/mh-test/agent_authority.pem}
      docker_tls: {file: /etc/mh-test/docker_tls.pem}
`, pandoRemote, ipControl, ipHostA, hostAMemory, hostCPU, ipHostB, hostBMemory, hostCPU)
	return os.WriteFile(filepath.Join(etc, "pando.yaml"), []byte(config), 0o644)
}

func newCA() (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mh-test docker CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return key, cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

var serial int64 = 1

func leaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, ips []net.IP, usage x509.ExtKeyUsage) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}
