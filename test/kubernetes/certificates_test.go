//go:build kubernetes

package kubernetes_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// portForward forwards a free local port to target's remote port and
// returns the local one once it accepts connections.
func portForward(t *testing.T, ns, target string, remote int) int {
	t.Helper()
	port, err := freePort()
	require.NoError(t, err)
	pf := exec.Command("kubectl", "--context", kubeContext(), "-n", ns, "port-forward", target, fmt.Sprintf("%d:%d", port, remote))
	require.NoError(t, pf.Start())
	t.Cleanup(func() { _ = pf.Process.Kill(); _ = pf.Wait() })
	eventually(t, 30*time.Second, "the port-forward to "+target+" accepts", func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err == nil
	})
	return port
}

// pebbleRoots is the root Pebble signs certificates under. It is made when
// Pebble starts, so it is read from Pebble's management port.
func pebbleRoots(t *testing.T) *x509.CertPool {
	t.Helper()
	port := portForward(t, "pando", "service/pebble", 15000)
	// Pebble's management port serves its own test certificate; what it
	// serves is the root the chain is checked against below.
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // see above
	resp, err := hc.Get(fmt.Sprintf("https://127.0.0.1:%d/roots/0", port))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(body), "Pebble's root: %s", body)
	return pool
}

// secretCertificate is the leaf in a kubernetes.io/tls Secret in pando-edge.
func secretCertificate(t *testing.T, name string) (*x509.Certificate, bool) {
	t.Helper()
	out, err := kubectl("-n", "pando-edge", "get", "secret", name, "-o", `jsonpath={.data.tls\.crt}`)
	if err != nil || out == "" {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(out)
	require.NoError(t, err)
	block, _ := pem.Decode(raw)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert, true
}

// subdomainApp deploys the echo app at <slug>.apps.pando.test, over HTTPS.
func subdomainApp(t *testing.T, c *client, name string) (string, string) {
	t.Helper()
	spec := strings.Replace(echoSpec(), `"mode": "path"`, `"mode": "subdomain"`, 1)
	app := c.deployImage(t, name, spec)
	return app, c.slug(t, app) + ".apps.pando.test"
}

// TestR169_TheLeaderIssuesAnAppsCertificateAndEveryEdgeReplicaServesIt: with
// Traefik's certificates on HTTP-01 and the CA set to Pebble (O-49), Pando's
// leader orders a certificate for an app's hostname; Pebble's validation
// reaches the edge on port 80 and Pando's proxy answers the challenge; the
// certificate lands in a TLS Secret, and every Traefik replica serves it with
// a chain to Pebble's root (R-174).
func TestR169_TheLeaderIssuesAnAppsCertificateAndEveryEdgeReplicaServesIt(t *testing.T) {
	c := login(t)
	_, host := subdomainApp(t, c, "k8s-tls")
	secret := "pando-tls-" + host

	var cert *x509.Certificate
	eventually(t, 5*time.Minute, "the leader issued "+secret, func() bool {
		var ok bool
		cert, ok = secretCertificate(t, secret)
		return ok
	})
	require.Contains(t, cert.DNSNames, host)

	roots := pebbleRoots(t)
	pods := strings.Fields(mustKubectl(t, "-n", "pando-edge", "get", "pods", "-l", "app.kubernetes.io/component=edge",
		"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}"))
	require.GreaterOrEqual(t, len(pods), 2, "the edge runs at least two replicas")
	for _, pod := range pods {
		port := portForward(t, "pando-edge", "pod/"+pod, 443)
		var served *x509.Certificate
		eventually(t, 2*time.Minute, pod+" serves the certificate", func() bool {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
				fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12})
			if err != nil {
				return false
			}
			defer func() { _ = conn.Close() }()
			served = conn.ConnectionState().PeerCertificates[0]
			return true
		})
		require.Contains(t, served.DNSNames, host, "%s serves a certificate naming %s, chained to Pebble's root", pod, host)

		// And the app behind it, through the proxy.
		hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}}}
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/", port), nil)
		require.NoError(t, err)
		req.Host = host
		req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
		resp, err := hc.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
		require.Contains(t, string(body), "x-pando-assertion", "the request passed through Pando's proxy")
	}
}

// TestR169_TheLeaderRenewsACertificateAheadOfExpiry: Pebble issues for 29
// days, inside Pando's 30-day renewal window, so the leader orders a new
// certificate for the same hostname at a later edge pass and writes it over
// the Secret every replica reads.
func TestR169_TheLeaderRenewsACertificateAheadOfExpiry(t *testing.T) {
	c := login(t)
	_, host := subdomainApp(t, c, "k8s-renew")
	secret := "pando-tls-" + host

	var first *x509.Certificate
	eventually(t, 5*time.Minute, "the leader issued "+secret, func() bool {
		var ok bool
		first, ok = secretCertificate(t, secret)
		return ok
	})
	require.Less(t, time.Until(first.NotAfter), 30*24*time.Hour, "Pebble's validity is inside the renewal window")

	var serial *big.Int
	eventually(t, 5*time.Minute, "the leader renewed "+secret, func() bool {
		next, ok := secretCertificate(t, secret)
		if ok && next.SerialNumber.Cmp(first.SerialNumber) != 0 {
			serial = next.SerialNumber
			return true
		}
		return false
	})
	require.NotNil(t, serial)
}
