package deploy

import (
	"context"
	"net"
	"net/url"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
)

// startWait is how long a deploy waits for a new app to come up: every part
// running for the settle period, healthy where it has a health check, and the
// primary workload accepting connections where it has none (R-221).
//
// [P] Three minutes, raised from two (issue #90). A Spring Boot app on a
// loaded host took 40 to 91 seconds to open its port, and a deploy waits from
// the moment the containers are started, so two minutes left one slow start
// and nothing more. An app still not up when this ends is degraded, not failed,
// and the reconciler keeps working it (R-147, R-150).
const startWait = 3 * time.Minute

// dialTimeout bounds one attempt to connect to the app's port.
const dialTimeout = 2 * time.Second

// waitPort is the workload and port a deploy waits to accept a connection
// before calling the app up: the primary workload's, when it has no health
// check of its own.
//
// R-221's order is a compose healthcheck, then an HTTP endpoint, then a TCP
// connect, then the process merely running. A deploy with neither of the first
// two waited only for the process, and declared a Spring Boot app deployed
// while it was still a minute from listening — the first request got 502
// (issue #90). A port that accepts a connection is the next signal down, and
// one every app that serves anything has.
func waitPort(bundle api.BundlePlan) (string, int, bool) {
	for _, w := range bundle.Workloads {
		if !w.Exposed || w.Health != nil || len(w.Ports) == 0 {
			continue
		}
		port := w.Ports[0].Number
		for _, p := range w.Ports {
			if p.Protocol == "" || p.Protocol == "http" || p.Protocol == "https" {
				port = p.Number
				break
			}
		}
		if port <= 0 {
			return "", 0, false
		}
		return w.Name, port, true
	}
	return "", 0, false
}

// portOpen reports whether the workload accepts a TCP connection on port,
// reached the way Pando's proxy reaches it (R-023). A runtime that cannot say
// how — an Upstream error, an address that cannot be read — is not a closed
// port: there is no TCP signal to wait on, and R-221 falls back to the process
// running.
func portOpen(ctx context.Context, runtime api.RuntimeAdapter, appID, workload string, port int) bool {
	up, err := runtime.Upstream(ctx, api.WorkloadRef{BundleID: appID, Workload: workload}, port)
	if err != nil {
		return true
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var conn net.Conn
	if up.Dial != nil {
		conn, err = up.Dial(dialCtx)
	} else {
		u, parseErr := url.Parse(up.URL)
		if parseErr != nil || u.Host == "" {
			return true
		}
		var d net.Dialer
		conn, err = d.DialContext(dialCtx, "tcp", u.Host)
	}
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
