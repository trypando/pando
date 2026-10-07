//go:build integration

package acceptance_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Provisioned slots, end to end (R-131, R-134, R-135).
//
// R-131 calls provisioning "the hobbyist default" and it is the resolution that
// asks the most of Pando: the other two hand back a string somebody else is
// responsible for. This exercises the shipped topology, because the failure
// modes here are all in the seams — the credential has to survive a redeploy,
// the service has to be unreachable from outside, and its data has to be
// recorded as the app's storage or it is never backed up.

// TestR131_ProvisioningStandsUpADatabaseTheAppCanReach asserts R-131.
func TestR131_ProvisioningStandsUpADatabaseTheAppCanReach(t *testing.T) {
	requireStack(t)
	c := login(t)

	app := c.createApp(t, "provisioned-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "postgres:17-alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"command": ["sleep", "3600"],
			"env": [{"key": "DATABASE_URL", "slot_ref": "DATABASE_URL"}]}],
		"slots": [{"key": "DATABASE_URL", "type": "postgres", "required": true,
			"resolution": {"mode": "provisioned"}}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9131},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 1)
	final := c.awaitDeployment(t, app, dep["id"].(string), 4*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"deploy log:\n%s", c.deploymentLogs(t, app, dep["id"].(string)))

	// The app got a connection string, and it reaches something that answers.
	//
	// Asserted from inside the app's own container, because that is the claim:
	// not that a Postgres exists somewhere, but that this app can reach it with
	// the value Pando put in its environment.
	out := inApp(t, app, "web", "sh", "-c",
		`psql "$DATABASE_URL" -tAc "select 'reachable'"`)
	require.Contains(t, out, "reachable",
		"the app could not reach the database Pando provisioned for it")
}

// TestR134_AProvisionedServiceIsNotReachableFromOutside asserts R-134.
func TestR134_AProvisionedServiceIsNotReachableFromOutside(t *testing.T) {
	requireStack(t)
	c := login(t)

	app := c.createApp(t, "private-db-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "redis:7-alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"command": ["sleep", "3600"],
			"env": [{"key": "REDIS_URL", "slot_ref": "REDIS_URL"}]}],
		"slots": [{"key": "REDIS_URL", "type": "redis", "required": true,
			"resolution": {"mode": "provisioned"}}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9132},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 1)
	final := c.awaitDeployment(t, app, dep["id"].(string), 4*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"deploy log:\n%s", c.deploymentLogs(t, app, dep["id"].(string)))

	// Nothing is published. A provisioned service reachable from the host is
	// reachable from anywhere the host is, which is the whole of R-134 undone.
	//
	// The service is found first and the count asserted, because "no row
	// published a port" is also true of no rows — and a filter on the wrong
	// label produces exactly that, passing while asserting nothing.
	ports := docker(t, "ps", "--filter", "label=io.pando.bundle="+app, "--format", "{{.Names}}\t{{.Ports}}")

	services := 0
	for _, line := range strings.Split(strings.TrimSpace(ports), "\n") {
		if !strings.Contains(line, "svc-") {
			continue
		}
		services++
		require.NotContains(t, line, "->",
			"a provisioned service published a port to the host: %s", line)
		require.NotContains(t, line, "0.0.0.0",
			"a provisioned service published a port to the host: %s", line)
	}
	require.Equal(t, 1, services, "the provisioned Redis is running:\n%s", ports)
}

// TestR135_ProvisionedDataIsRecordedAsTheAppsStorage asserts R-135.
//
// Not bookkeeping. Storage Pando does not know about is storage Pando never
// backs up, never offers to keep when the app is deleted, and never reclaims —
// so a provisioned database missing from this list is a database that is
// silently absent from every DR bundle the install ever takes.
func TestR135_ProvisionedDataIsRecordedAsTheAppsStorage(t *testing.T) {
	requireStack(t)
	c := login(t)

	app := c.createApp(t, "db-storage-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "redis:7-alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"command": ["sleep", "3600"],
			"env": [{"key": "REDIS_URL", "slot_ref": "REDIS_URL"}]}],
		"slots": [{"key": "REDIS_URL", "type": "redis", "required": true,
			"resolution": {"mode": "provisioned"}}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9133},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 1)
	final := c.awaitDeployment(t, app, dep["id"].(string), 4*time.Minute)
	require.Equal(t, "succeeded", final["status"],
		"deploy log:\n%s", c.deploymentLogs(t, app, dep["id"].(string)))

	volumes := c.get(t, fmt.Sprintf("/apps/%s/volumes", app))
	list, _ := volumes["volumes"].([]any)
	require.NotEmpty(t, list,
		"the provisioned service's data was not recorded as this app's storage, so it would never be backed up")

	for _, v := range list {
		row := v.(map[string]any)
		require.NotEmpty(t, row["handle"],
			"a recorded volume with no handle cannot be snapshotted")
	}
}

// TestR131_RedeployingKeepsTheDatabaseTheAppAlreadyHas asserts the property
// that is invisible until the second deploy.
//
// A database sets its password when its data directory is created and ignores
// the variable ever after. An adapter that generated a fresh password each
// deploy would produce a deploy that succeeds and an app that cannot log in —
// with the symptom nowhere near the cause.
func TestR131_RedeployingKeepsTheDatabaseTheAppAlreadyHas(t *testing.T) {
	requireStack(t)
	c := login(t)

	app := c.createApp(t, "redeploy-db-"+stamp())
	spec := `{
		"schema_version": 1,
		"source": {"type": "image", "image": "postgres:17-alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"command": ["sleep", "3600"],
			"env": [{"key": "DATABASE_URL", "slot_ref": "DATABASE_URL"}]}],
		"slots": [{"key": "DATABASE_URL", "type": "postgres", "required": true,
			"resolution": {"mode": "provisioned"}}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9134},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`
	c.putSpec(t, app, spec)
	c.pinSpec(t, app, 1)

	first := c.deploy(t, app, 1)
	require.Equal(t, "succeeded",
		c.awaitDeployment(t, app, first["id"].(string), 4*time.Minute)["status"],
		"deploy log:\n%s", c.deploymentLogs(t, app, first["id"].(string)))

	// Write something, so "the same database" is a claim about data and not
	// only about credentials.
	inApp(t, app, "web", "sh", "-c",
		`psql "$DATABASE_URL" -c "create table survived (n int)" -c "insert into survived values (1)"`)

	second := c.deploy(t, app, 1)
	require.Equal(t, "succeeded",
		c.awaitDeployment(t, app, second["id"].(string), 4*time.Minute)["status"],
		"deploy log:\n%s", c.deploymentLogs(t, app, second["id"].(string)))

	out := inApp(t, app, "web", "sh", "-c",
		`psql "$DATABASE_URL" -tAc "select count(*) from survived"`)
	require.Contains(t, out, "1",
		"the redeployed app could not read what it wrote, so it is talking to a different database")
}

// TestR131_ProvisioningWhatPandoDoesNotProvisionIsRefused asserts R-010 by its
// consequence: Pando says so at plan time rather than at deploy.
func TestR131_ProvisioningWhatPandoDoesNotProvisionIsRefused(t *testing.T) {
	requireStack(t)
	c := login(t)

	app := c.createApp(t, "no-provisioner-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"env": [{"key": "S3_BUCKET", "slot_ref": "S3_BUCKET"}]}],
		"slots": [{"key": "S3_BUCKET", "type": "s3", "required": true,
			"resolution": {"mode": "provisioned"}}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9135},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	// Pinned, because /plan plans what the app is set to run, not a draft.
	c.pinSpec(t, app, 1)

	body, status := c.postRaw(t, fmt.Sprintf("/apps/%s/plan", app), `{"spec_revision": 1}`)
	require.GreaterOrEqual(t, status, 400, body)
	require.Contains(t, body, "PLAN_ADAPTER_NOT_CONFIGURED", body)
	// R-105: the message names what is missing and the remedy names the slot.
	require.Contains(t, body, "S3_BUCKET", body)
}

// inApp runs a command inside one of an app's workloads and returns its output.
//
// Through Docker rather than through /exec, because the assertion is about what
// the container can reach and the exec endpoint is a websocket — this is a test
// probe, not a second client.
func inApp(t *testing.T, appID, workload string, command ...string) string {
	t.Helper()

	name := containerFor(t, appID, workload)
	args := append([]string{"exec", name}, command...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker exec in %s: %s", name, out)
	return string(out)
}

// containerFor finds one workload's container, waiting for it to be running.
//
// Filtered on the runtime adapter's own labels — `io.pando.bundle` and
// `io.pando.workload`, the ones Observe uses to find a bundle. A filter on a
// label nothing sets matches nothing, and a loop over nothing passes without
// asserting anything, which is how a test ends up green about a feature that
// does not work.
func containerFor(t *testing.T, appID, workload string) string {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	for {
		out := docker(t, "ps", "--filter", "label=io.pando.bundle="+appID,
			"--filter", "label=io.pando.workload="+workload,
			"--filter", "status=running", "--format", "{{.Names}}")
		if name := strings.TrimSpace(out); name != "" {
			return strings.Split(name, "\n")[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no running container for %s/%s; saw:\n%s", appID, workload,
				docker(t, "ps", "-a", "--filter", "label=io.pando.bundle="+appID,
					"--format", "{{.Names}} {{.Status}}"))
		}
		time.Sleep(2 * time.Second)
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %v: %s", args, out)
	return string(out)
}

// TestR148_AKilledProvisionedServiceIsRestored asserts R-148 reaches the
// database Pando stood up, not only the app's own workloads.
//
// The reconciler compares what should be running against what is. A
// provisioned service missing from "what should be running" is never restored
// when it is killed — and, worse, the one that *is* running is reported every
// tick as a workload the spec does not declare, because the spec genuinely does
// not declare it. Both follow from the same omission and neither is visible
// until something kills a container.
func TestR148_AKilledProvisionedServiceIsRestored(t *testing.T) {
	requireStack(t)
	c := login(t)

	app := c.createApp(t, "kill-db-"+stamp())
	c.putSpec(t, app, `{
		"schema_version": 1,
		"source": {"type": "image", "image": "redis:7-alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"command": ["sleep", "3600"],
			"env": [{"key": "REDIS_URL", "slot_ref": "REDIS_URL"}]}],
		"slots": [{"key": "REDIS_URL", "type": "redis", "required": true,
			"resolution": {"mode": "provisioned"}}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9136},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	c.pinSpec(t, app, 1)

	dep := c.deploy(t, app, 1)
	require.Equal(t, "succeeded",
		c.awaitDeployment(t, app, dep["id"].(string), 4*time.Minute)["status"],
		"deploy log:\n%s", c.deploymentLogs(t, app, dep["id"].(string)))

	before := serviceContainer(t, app)
	require.NotEmpty(t, before, "the provisioned Redis is running")

	// Reach in and break it, exactly as a person would.
	require.NoError(t, exec.Command("docker", "rm", "-f", before).Run())

	var after string
	require.Eventually(t, func() bool {
		after = serviceContainer(t, app)
		return after != ""
	}, 3*time.Minute, 2*time.Second,
		"the reconciler never brought the provisioned service back")

	require.NotEqual(t, before, after, "it is a new container, not the old one resurrected")
}

// serviceContainer is the ID of the running provisioned service in an app's
// bundle, if there is one.
//
// The ID, not the name: Pando names a bundle's containers deterministically, so
// a service the reconciler recreates has the same name as the one it replaces,
// and only the ID tells the two apart.
func serviceContainer(t *testing.T, appID string) string {
	t.Helper()

	out := docker(t, "ps", "--filter", "label=io.pando.bundle="+appID,
		"--filter", "status=running", "--format", "{{.ID}} {{.Names}}")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		id, name, ok := strings.Cut(line, " ")
		if ok && strings.Contains(name, "svc-") {
			return id
		}
	}
	return ""
}
