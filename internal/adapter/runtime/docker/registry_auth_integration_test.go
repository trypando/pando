//go:build integration

package docker_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// A private image is pulled with the credential core resolved for it, and not
// without one (issue #41). Against a real registry that requires signing in,
// because the encoding the daemon expects is the part a unit test cannot check.
func TestAPrivateImageIsPulledWithTheAppsCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	const user, pass = "pando", "correct-horse-registry"
	port := privateRegistry(t, user, pass)
	ref := fmt.Sprintf("127.0.0.1:%d/acme/private:1", port)

	// Something runnable in it: alpine, copied in with the credential.
	src, err := name.ParseReference("alpine:3.20")
	require.NoError(t, err)
	img, err := remote.Image(src)
	if err != nil {
		t.Skipf("cannot fetch alpine:3.20 to seed the registry: %v", err)
	}
	dst, err := name.ParseReference(ref, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(dst, img, remote.WithAuth(&authn.Basic{Username: user, Password: pass})))

	stamp := time.Now().Format("150405.000")
	t.Cleanup(func() { _, _ = dockerCLI("image", "rm", "-f", ref) })

	anonymous := "test-private-anon-" + strings.ReplaceAll(stamp, ".", "")
	cleanup(t, a, anonymous)
	plan := bundle(anonymous, nil)
	plan.Workloads[0].Image = ref
	_, err = a.Apply(ctx, plan)
	require.Error(t, err, "a private image does not pull without its credential")
	// The daemon pulls from its own loopback. On Linux that is this host's;
	// under Docker Desktop, colima or OrbStack it is a VM's, the registry's
	// published port is not there, and the refusal is the network's rather
	// than the registry's. Read from the adapter's own pull, because the
	// docker CLI may be talking to a different engine than the adapter is.
	if msg := strings.ToLower(err.Error()); strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "no route to host") {
		t.Skipf("the Docker daemon cannot reach the test registry on this host's loopback: %v", err)
	}

	signed := "test-private-auth-" + strings.ReplaceAll(stamp, ".", "")
	cleanup(t, a, signed)
	plan = bundle(signed, nil)
	plan.Workloads[0].Image = ref
	plan.Workloads[0].PullAuth = &api.RegistryAuth{
		Registry: fmt.Sprintf("127.0.0.1:%d", port), Username: user, Password: secret.New(pass),
	}
	_, err = a.Apply(ctx, plan)
	require.NoError(t, err)

	env := inContainer(t, signed, "web", "env")
	require.NotContains(t, env, pass, "the credential fetches the image and is never given to the app")
}

// privateRegistry starts registry:2 behind htpasswd and returns its port.
func privateRegistry(t *testing.T, user, pass string) int {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	require.NoError(t, err)
	dir := t.TempDir()
	htpasswd := filepath.Join(dir, "htpasswd")
	require.NoError(t, os.WriteFile(htpasswd, []byte(user+":"+string(hash)+"\n"), 0o644))

	// Pulled first: create would pull it itself, and print the pull's progress
	// into the output the container ID is read from.
	if out, err := dockerCLI("pull", "-q", "registry:2"); err != nil {
		t.Skipf("cannot pull registry:2: %v\n%s", err, out)
	}
	id, err := dockerCLI("create", "-P",
		"-e", "REGISTRY_AUTH=htpasswd",
		"-e", "REGISTRY_AUTH_HTPASSWD_REALM=pando-test",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
		"registry:2")
	if err != nil {
		t.Skipf("cannot create a registry container: %v", err)
	}
	// The ID is the last line; anything before it is the daemon talking.
	lines := strings.Split(strings.TrimSpace(id), "\n")
	id = strings.TrimSpace(lines[len(lines)-1])
	t.Cleanup(func() { _, _ = dockerCLI("rm", "-f", id) })
	// The directory does not exist in the image, so it is copied in whole and
	// becomes /auth.
	out, err := dockerCLI("cp", dir, id+":/auth")
	require.NoError(t, err, out)
	_, err = dockerCLI("start", id)
	require.NoError(t, err)

	out, err = dockerCLI("port", id, "5000/tcp")
	require.NoError(t, err)
	var port int
	line := strings.Split(strings.TrimSpace(out), "\n")[0]
	_, err = fmt.Sscanf(line[strings.LastIndex(line, ":")+1:], "%d", &port)
	require.NoError(t, err, out)

	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v2/", port))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized
	}, 30*time.Second, 200*time.Millisecond, "the registry answers and asks for a credential")

	return port
}
