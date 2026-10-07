//go:build kubernetes

package kubernetes_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR256_ADeployResumesOnTheOtherReplicaWhenItsPodIsDeleted: the pod running
// a deploy is force-deleted mid-deploy, as a node loss would take it, and the
// surviving replica takes the deploy from the queue and finishes it (O-32).
// The Deployment then brings the install back to two replicas.
func TestR256_ADeployResumesOnTheOtherReplicaWhenItsPodIsDeleted(t *testing.T) {
	c := login(t)
	app := c.createApp(t, "k8s-lost-"+stamp())
	// A readiness check that never passes holds the deploy open for its
	// health wait: time enough to take its replica away.
	dep := c.startDeploy(t, app, imageSpec("nginx:alpine", 80,
		`, "healthcheck": {"command": ["CMD", "false"], "interval_seconds": 1, "retries": 1}`))

	var host string
	eventually(t, time.Minute, "the deploy is in flight on a replica", func() bool {
		host = psql(t, `SELECT r.hostname FROM deployments d JOIN pando_replicas r ON r.id = d.replica_id
			WHERE d.id = '`+dep+`' AND d.status IN ('pending', 'building', 'applying')`)
		return host != ""
	})

	// No grace: the process is gone without a goodbye.
	mustKubectl(t, "-n", "pando", "delete", "pod", host, "--grace-period=0", "--force", "--wait=false")

	var other string
	eventually(t, 3*time.Minute, "another replica took the deploy", func() bool {
		other = psql(t, `SELECT r.hostname FROM deployments d JOIN pando_replicas r ON r.id = d.replica_id
			WHERE d.id = '`+dep+`'`)
		return other != "" && other != host
	})

	require.NoError(t, fwd.ensure())
	eventually(t, 5*time.Minute, "the resumed deploy finished", func() bool {
		d := c.get(t, "/apps/"+app+"/deployments/"+dep)
		return d["status"] == "succeeded" || d["status"] == "failed"
	})

	eventually(t, 3*time.Minute, "the Deployment is back to two ready replicas", func() bool {
		out, _ := kubectl("-n", "pando", "get", "deployment", "pando", "-o", "jsonpath={.status.readyReplicas}")
		return out == "2"
	})
	next := c.must(t, http.MethodPost, "/apps/"+app+"/deployments", "{}", http.StatusAccepted)
	require.NotEmpty(t, next["id"], "the app's next deploy is accepted")
}
