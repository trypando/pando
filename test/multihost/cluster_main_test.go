//go:build multihost

package multihost_test

// The cluster topology (#347, docs/design/notes-cluster.md): three Docker-in-
// Docker machines, each running Pando built from this checkout, joined into
// one install with join tokens — no adapter settings typed in, as an operator
// would do it. `make test-cluster` runs it (MH_TEST=1 MH_CLUSTER=1).
//
// On one network (mh-test-cl-net, 192.168.214.0/24):
//
//	mh-test-cl-postgres  .10  the install's database
//	mh-test-cl-registry  .11  the install registry (registry:3, plain HTTP)
//	mh-test-cl-s3        .12  an S3-compatible store (VersityGW): the off-host backup
//	                          destination a second host's join needs (R-417)
//	mh-test-cl-pg2       .13  h2's own database for its first start, before
//	mh-test-cl-pg3       .14  h3's likewise; a joining Pando starts on one
//	mh-test-cl-h1        .21  DinD: the install's first host (control + app),
//	                          Pando as h1, the runtime's agent beside it
//	mh-test-cl-h2        .22  DinD: joined control + app host, Pando as
//	                          pando-agent (its agent runs in that process)
//	mh-test-cl-h3        .23  DinD: joined app-only host, Pando as pando-agent
//
// Everything is named mh-test-cl-… and labeled mh-test-cl=1, and removed when
// the run ends whether or not it passed (MH_KEEP=1 keeps it).

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	clNet      = "mh-test-cl-net"
	clSubnet   = "192.168.214.0/24"
	clLabel    = "mh-test-cl=1"
	clPostgres = "mh-test-cl-postgres"
	clRegistry = "mh-test-cl-registry"
	clS3       = "mh-test-cl-s3"
	// s3Image is an S3-compatible store (MinIO no longer publishes images).
	s3Image = "versity/versitygw:latest"
	clPG2   = "mh-test-cl-pg2"
	clPG3   = "mh-test-cl-pg3"
	clH1    = "mh-test-cl-h1"
	clH2    = "mh-test-cl-h2"
	clH3    = "mh-test-cl-h3"

	clIPPostgres = "192.168.214.10"
	clIPRegistry = "192.168.214.11"
	clIPS3       = "192.168.214.12"
	clIPPG2      = "192.168.214.13"
	clIPPG3      = "192.168.214.14"

	clRegistryAddr = clIPRegistry + ":5000"
	clPandoImage   = clRegistryAddr + "/mh-test-cl/pando:dev"
	clAppImage     = clRegistryAddr + "/mh-test-cl/busybox:1.37"

	// The Pando container inside each machine. A joined host's is named
	// pando-agent: its agent runs in that process, and the per-host Docker
	// adapter joins the container of that name to the host's app networks
	// (notes-cluster.md, "finishing the join").
	clPandoFirst  = "h1"
	clPandoJoined = "pando-agent"

	clS3User   = "mh-test-cl"
	clS3Secret = "mh-test-cl-secret"
	clBucket   = "pando-backups"

	// The app ports each control host's proxy serves, published as
	// themselves inside the machine (port-mode routing, R-166).
	clAppPorts = "9100-9109"
)

// clHost is one machine of the cluster.
type clHost struct {
	name      string // the cluster's name for it: h1, h2, h3
	container string // its DinD container
	ip        string
	pando     string // Pando's container inside it
	pgIP      string // the database its first start uses
	control   bool
}

var (
	clusterEnabled bool
	h1             = &clHost{name: "h1", container: clH1, ip: "192.168.214.21", pando: clPandoFirst, pgIP: clIPPostgres, control: true}
	h2             = &clHost{name: "h2", container: clH2, ip: "192.168.214.22", pando: clPandoJoined, pgIP: clIPPG2, control: true}
	h3             = &clHost{name: "h3", container: clH3, ip: "192.168.214.23", pando: clPandoJoined, pgIP: clIPPG3}
	clHosts        = []*clHost{h1, h2, h3}
	// base2 (main_test.go) is h2's API in this topology.
)

var clContainers = []string{clH1, clH2, clH3, clS3, clRegistry, clPG2, clPG3, clPostgres}

// h2Roles are the roles h2 joins with: control and app, unless
// MH_CLUSTER_H2_ROLES says otherwise (app, to run the failover tests on a
// cluster with one control host).
func h2Roles() []string {
	if r := os.Getenv("MH_CLUSTER_H2_ROLES"); r != "" {
		return strings.Split(r, ",")
	}
	return []string{"control", "app"}
}

func clusterMain(m interface{ Run() int }) int {
	clusterEnabled = true
	h2.control = slices.Contains(h2Roles(), "control")
	if !slices.Contains(h2Roles(), "app") {
		h2.pando = "pando-control" // runs no agent, so not named as one
	}
	clusterTeardown() // whatever an interrupted run left
	if err := clusterSetup(); err != nil {
		fmt.Fprintln(os.Stderr, "cluster setup failed:", err)
		clusterDumpLogs()
		if os.Getenv("MH_KEEP") == "" {
			clusterTeardown()
		}
		return 1
	}
	code := m.Run()
	if code != 0 {
		clusterDumpLogs()
	}
	if os.Getenv("MH_KEEP") == "" {
		clusterTeardown()
	}
	return code
}

// clusterTeardown removes what the run made, by name and by label: the
// machines' anonymous volumes (each one's /var/lib/docker, and so every app,
// standby copy and Pando volume inside it) go with -v.
func clusterTeardown() {
	for _, c := range clContainers {
		_, _ = docker("rm", "-f", "-v", c)
	}
	if ids, err := docker("ps", "-aq", "--filter", "label="+clLabel); err == nil {
		for _, id := range strings.Fields(ids) {
			_, _ = docker("rm", "-f", "-v", id)
		}
	}
	_, _ = docker("network", "rm", clNet)
	if vols, err := docker("volume", "ls", "-q", "--filter", "label="+clLabel); err == nil {
		for _, v := range strings.Fields(vols) {
			_, _ = docker("volume", "rm", v)
		}
	}
}

func clusterDumpLogs() {
	for _, h := range clHosts {
		if out, err := innerLogs(h.container, h.pando, 150); err == nil {
			fmt.Fprintf(os.Stderr, "---- %s (%s) Pando logs (tail) ----\n%s\n", h.name, h.pando, out)
		}
	}
	if out, err := innerLogs(clH1, "pando-agent", 40); err == nil {
		fmt.Fprintf(os.Stderr, "---- h1 runtime agent logs ----\n%s\n", out)
	}
}

// innerLogs is the tail of a container's log inside a machine, stderr
// included: Pando logs there.
func innerLogs(machine, name string, lines int) (string, error) {
	return docker("exec", machine, "sh", "-c", fmt.Sprintf("docker logs --tail %d %s 2>&1", lines, name))
}

func clusterSetup() error {
	image := os.Getenv("MH_IMAGE")
	if image == "" {
		image = "mh-test-pando:dev"
	}
	if _, err := docker("image", "inspect", image); err != nil {
		return fmt.Errorf("the Pando image %s is not built (make test-cluster builds it): %w", image, err)
	}
	if _, err := docker("network", "create", "--subnet", clSubnet, "--label", clLabel, clNet); err != nil {
		return err
	}
	run := func(args ...string) error {
		_, err := docker(append([]string{"run", "-d", "--label", clLabel, "--network", clNet}, args...)...)
		return err
	}
	for _, pg := range []struct{ name, ip string }{{clPostgres, clIPPostgres}, {clPG2, clIPPG2}, {clPG3, clIPPG3}} {
		if err := run("--name", pg.name, "--ip", pg.ip,
			"-e", "POSTGRES_USER=pando", "-e", "POSTGRES_PASSWORD=mh-test", "-e", "POSTGRES_DB=pando",
			"postgres:17-alpine"); err != nil {
			return err
		}
	}
	if err := run("--name", clRegistry, "--ip", clIPRegistry, "registry:3"); err != nil {
		return err
	}
	// VersityGW over a plain directory: the bucket is a directory made
	// before it starts, and object metadata goes to a sidecar directory, so
	// the store needs no extended attributes from the filesystem.
	if err := run("--name", clS3, "--ip", clIPS3, "--entrypoint", "sh", s3Image, "-c",
		"mkdir -p /data/"+clBucket+" /meta && exec versitygw --access "+clS3User+" --secret "+clS3Secret+
			" --port :9000 posix --sidecar /meta /data"); err != nil {
		return err
	}

	for _, h := range clHosts {
		args := []string{"create", "--privileged", "--label", clLabel, "--name", h.container, "--hostname", h.container,
			"--network", clNet, "--ip", h.ip, "-e", "DOCKER_TLS_CERTDIR="}
		if h.control {
			// The API, the app ports each control host's proxy serves, and
			// the cluster listener (h1's, for the certificate checks).
			args = append(args, "-p", "127.0.0.1::8080", "-p", "127.0.0.1::"+clAppPorts, "-p", "127.0.0.1::7444")
		}
		args = append(args, dindImage, "dockerd", "--host=unix:///var/run/docker.sock", "--insecure-registry="+clRegistryAddr)
		if _, err := docker(args...); err != nil {
			return err
		}
		if _, err := docker("start", h.container); err != nil {
			return err
		}
	}
	for _, h := range clHosts {
		if err := awaitDaemon(h.container, 2*time.Minute); err != nil {
			return err
		}
	}

	// Pando's image into h1 and on to the registry, from which h2 and h3
	// pull it as an operator's machines would; the app's image likewise.
	for local, remote := range map[string]string{image: clPandoImage, busyboxImage: clAppImage} {
		saved, err := exec.Command("docker", "save", local).Output()
		if err != nil {
			return fmt.Errorf("docker save %s (pull or build it first): %w", local, err)
		}
		if _, err := dockerStdin(saved, "exec", "-i", clH1, "docker", "load"); err != nil {
			return err
		}
		if _, err := inner(clH1, "tag", local, remote); err != nil {
			return err
		}
		if err := retry(time.Minute, func() error { _, err := inner(clH1, "push", "-q", remote); return err }); err != nil {
			return err
		}
	}
	for _, h := range []*clHost{h2, h3} {
		if _, err := inner(h.container, "pull", "-q", clPandoImage); err != nil {
			return err
		}
	}

	if err := startPando(h1, ""); err != nil {
		return err
	}
	port, err := publishedPort(clH1, "8080/tcp")
	if err != nil {
		return err
	}
	base = "http://127.0.0.1:" + port
	if h2.control {
		if port, err = publishedPort(clH2, "8080/tcp"); err != nil {
			return err
		}
		base2 = "http://127.0.0.1:" + port
	}
	if err := awaitReady(base, 3*time.Minute); err != nil {
		return err
	}
	return readyForASecondHost()
}

// startPando runs Pando in h's machine. join is a join token for a host
// joining at setup (R-415), empty for the install's first host.
func startPando(h *clHost, join string) error {
	// The first host's container is named as its hostname, h1: Pando finds
	// its own container by its hostname, and the cluster names the install's
	// machine by it (R-414). A joined host's name is PANDO_JOIN_NAME.
	args := []string{"run", "-d", "--name", h.pando, "--restart", "unless-stopped",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", "mh-test-cl-pando-data:/var/lib/pando",
		"-p", "7444:7444",
		"-e", "PANDO_DATABASE_URL=postgres://pando:mh-test@" + h.pgIP + ":5432/pando?sslmode=disable",
		"-e", "PANDO_SERVER_ADVERTISE_URL=http://" + h.ip + ":8080",
		"-e", "PANDO_REGISTRY_URL=http://" + clRegistryAddr,
		"-e", "PANDO_REGISTRY_INSECURE=true",
		"-e", "PANDO_POLICY_AUDIT_ARCHIVE=export",
		"-e", "PANDO_RECONCILER_BACKOFF=0s,1s,2s,3s,4s",
		"-e", "PANDO_RECONCILER_FAILURE_WINDOW=2m",
		"-e", "PANDO_SERVER_PORT_RANGE_START=9100",
		"-e", "PANDO_SERVER_PORT_RANGE_END=9109",
		"-e", "PANDO_APPS_CPU_MILLIS=100",
		"-e", "PANDO_APPS_MEMORY_BYTES=67108864",
		"-e", "PANDO_CLUSTER_LIVENESS_PERIOD=" + clT.String(),
		"-e", "PANDO_CLUSTER_STANDBY_EVERY=" + clStandbyEvery.String(),
		"-e", "PANDO_CLUSTER_SHIP_EVERY=" + clShipEvery.String(),
	}
	if h.control {
		args = append(args, "-p", "8080:8080", "-p", clAppPorts+":"+clAppPorts)
	}
	if join == "" {
		args = append(args, "--hostname", h.name, "-e", "PANDO_ADMIN_PASSWORD="+adminPassword)
	} else {
		// A joined host's agent is this process, on the agent port, carrying
		// the runtime calls to the machine's Docker (R-416).
		args = append(args, "-p", "7443:7443",
			"-e", "PANDO_JOIN_TOKEN="+join, "-e", "PANDO_JOIN_NAME="+h.name,
			"-e", "PANDO_AGENT_RUNTIME_SOCKET=/var/run/docker.sock")
	}
	args = append(args, clPandoImage, "serve")
	_, err := inner(h.container, args...)
	return err
}

// readyForASecondHost does what R-417 asks of an install before a second
// host joins, through the API: an off-host backup destination as the
// default. The registry and audit_archive export come from the environment.
func readyForASecondHost() error {
	c, err := session()
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`{"id":"bkp_s3","category":"backup","kind":"s3","name":"S3","is_default":true,
		"config":{"endpoint":"http://%s:9000","region":"us-east-1","bucket":%q,"prefix":"mh-test","path_style":true},
		"credentials":{"access_key_id":%q,"secret_access_key":%q}}`, clIPS3, clBucket, clS3User, clS3Secret)
	out, status, err := c.raw(http.MethodPost, "/adapters", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("configuring the S3 backup destination: %d %s", status, out)
	}
	return restartAndWait(c)
}

// restartAndWait asks Pando to restart (R-253) and waits for it to be back.
func restartAndWait(c *client) error {
	started := startedAt(c)
	if out, status, err := c.raw(http.MethodPost, "/restart", ""); err != nil || status >= 300 {
		return fmt.Errorf("restart: %d %w %s", status, err, out)
	}
	return retry(3*time.Minute, func() error {
		fresh, err := session()
		if err != nil {
			return err
		}
		if now := startedAt(fresh); now == "" || now == started {
			return fmt.Errorf("not restarted yet")
		}
		*c = *fresh
		return nil
	})
}

func startedAt(c *client) string {
	out, status, err := c.raw(http.MethodGet, "/adapters", "")
	if err != nil || status != http.StatusOK {
		return ""
	}
	i := strings.Index(out, `"started_at":"`)
	if i < 0 {
		return ""
	}
	rest := out[i+len(`"started_at":"`):]
	return rest[:strings.IndexByte(rest, '"')]
}
