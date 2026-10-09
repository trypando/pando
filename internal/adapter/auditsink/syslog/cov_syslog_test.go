package syslog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// plainCollector accepts plain TCP connections and sends each octet-counted
// frame it reads on the returned channel.
func plainCollector(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return serveFrames(t, ln)
}

// closedAddress is a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func configured(t *testing.T, settings map[string]any) *Adapter {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	a := New()
	a.now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

// TestR382_SyslogOverPlainTCP asserts R-382 with tls off: the health check
// connects and hangs up, a batch arrives framed, an event with no action is
// sent with the nil MSGID, and an OCSF success is informational.
func TestR382_SyslogOverPlainTCP(t *testing.T) {
	addr, frames := plainCollector(t)
	a := configured(t, map[string]any{"address": addr, "tls": "off", "hostname": "pando-1", "facility": "local0", "app_name": "pando-test"})
	require.Equal(t, Kind, a.Kind())
	require.Equal(t, api.CategoryAuditSink, a.Category())

	ctx := context.Background()
	require.NoError(t, a.HealthCheck(ctx))
	require.NoError(t, a.Send(ctx, api.AuditBatch{}), "an empty batch is delivered by sending nothing")
	require.Nil(t, a.conn, "an empty batch does not connect")

	// A deadline sooner than the adapter's own timeout is the one honored.
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, a.Send(short, api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{"status_id":1}`)}}))
	// local0 (16) * 8 + informational (6) = 134.
	require.Equal(t, `<134>1 2026-10-08T12:00:00.000000Z pando-1 pando-test - - [pando@32473 outcome="success"] {"status_id":1}`, receive(t, frames))
}

// TestR382_SyslogUnreachableCollectorSaysWhatToCheck asserts the errors a
// collector that is not there produces: each names the address and what the
// collector must accept, for TLS and for plain TCP, from the health check and
// from Send alike.
func TestR382_SyslogUnreachableCollectorSaysWhatToCheck(t *testing.T) {
	ctx := context.Background()
	addr := closedAddress(t)

	plain := configured(t, map[string]any{"address": addr, "tls": "off"})
	err := plain.HealthCheck(ctx)
	require.ErrorContains(t, err, "Pando could not connect to the syslog collector at "+addr)
	require.ErrorContains(t, err, "accepts TCP on that port")
	err = plain.Send(ctx, api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{}`)}})
	require.ErrorContains(t, err, "accepts TCP on that port")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))

	secure := configured(t, map[string]any{"address": addr})
	err = secure.HealthCheck(ctx)
	require.ErrorContains(t, err, "accepts TCP with TLS, that its certificate is trusted for this address,")
}

// TestR382_SyslogUnconfiguredRefuses asserts an adapter Configure never
// succeeded on neither reports healthy nor pretends to deliver.
func TestR382_SyslogUnconfiguredRefuses(t *testing.T) {
	a := New()
	require.ErrorContains(t, a.HealthCheck(context.Background()), "syslog: not configured")
	err := a.Send(context.Background(), api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{}`)}})
	require.ErrorContains(t, err, "The syslog audit sink is not configured.")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
}

// TestR382_SyslogCanceledSendIsNotRetried asserts that a write blocked on a
// collector that has stopped reading ends when its context is canceled,
// closes the connection so the next send starts clean, and is not retried on
// a fresh connection: the caller canceled it.
func TestR382_SyslogCanceledSendIsNotRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn // held open and never read
			// Canceled once the write is under way and blocked.
			time.AfterFunc(300*time.Millisecond, cancel)
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case c := <-accepted:
				_ = c.Close()
			default:
				return
			}
		}
	})

	a := configured(t, map[string]any{"address": ln.Addr().String(), "tls": "off", "timeout_seconds": 30})
	// Far more than the socket buffers hold, so the write blocks.
	big := json.RawMessage(`{"pad":"` + strings.Repeat("x", 1<<20) + `"}`)
	batch := api.AuditBatch{Events: []json.RawMessage{big, big, big, big, big, big, big, big, big, big, big, big, big, big, big, big}}

	start := time.Now()
	err = a.Send(ctx, batch)
	require.Less(t, time.Since(start), 10*time.Second, "cancellation ended the write, not the 30-second timeout")
	require.ErrorContains(t, err, "Pando could not finish sending audit events to the syslog collector at "+ln.Addr().String()+". They will be sent again.")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Nil(t, a.conn, "a failed write closes the connection")

	<-accepted
	select {
	case <-accepted:
		t.Fatal("a canceled send was retried on a new connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestR382_SyslogReusedConnectionThatFailsIsRedialedOnce asserts the retry in
// Send: a kept connection the collector has since dropped fails the write,
// and the batch goes out once on a fresh connection.
func TestR382_SyslogReusedConnectionThatFailsIsRedialedOnce(t *testing.T) {
	addr, frames := plainCollector(t)
	a := configured(t, map[string]any{"address": addr, "tls": "off"})
	batch := api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{"outcome":"denied"}`)}, Actions: []string{"app.use.denied"}}
	require.NoError(t, a.Send(context.Background(), batch))
	receive(t, frames)
	require.NotNil(t, a.conn, "the connection is kept for the next batch")

	// The kept connection is now one whose writes fail.
	a.mu.Lock()
	_ = a.conn.Close()
	a.mu.Unlock()
	require.NoError(t, a.Send(context.Background(), batch))
	require.Contains(t, receive(t, frames), "app.use.denied")

	// When the collector is gone altogether, the redial's error is the answer.
	a.mu.Lock()
	_ = a.conn.Close()
	a.address = closedAddress(t)
	a.mu.Unlock()
	err := a.Send(context.Background(), batch)
	require.ErrorContains(t, err, "Pando could not connect to the syslog collector")
}

// TestR382_SyslogHealthCheckAfterHandshake asserts the health check's read
// after a TLS handshake: a collector that stays silent is healthy, one that
// hangs up is not, and the message says why it may have.
func TestR382_SyslogHealthCheckAfterHandshake(t *testing.T) {
	serverCert, _, serverPair := selfSigned(t, "collector", x509.ExtKeyUsageServerAuth)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverPair}, MinVersion: tls.VersionTLS12})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	behavior := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			tc := conn.(*tls.Conn)
			_ = tc.Handshake()
			switch <-behavior {
			case "write":
				_, _ = tc.Write([]byte("x"))
				time.Sleep(100 * time.Millisecond)
			case "hangup":
			}
			_ = conn.Close()
		}
	}()

	a := configured(t, map[string]any{"address": ln.Addr().String(), "ca_certificate": serverCert, "server_name": "localhost"})
	behavior <- "hangup"
	err = a.HealthCheck(context.Background())
	require.ErrorContains(t, err, "closed the connection after the TLS handshake. It may require a client certificate, or not accept the one set.")

	behavior <- "write"
	require.NoError(t, a.HealthCheck(context.Background()), "a collector that answers has accepted the connection")
}

// TestSyslogConfigurationRefusals asserts each setting Configure cannot use
// is refused with a message that names the setting and shows a valid one.
func TestSyslogConfigurationRefusals(t *testing.T) {
	ctx := context.Background()
	for raw, want := range map[string]string{
		`{"address":5}`: "syslog: reading configuration",
		`{"address":"a:1","ca_certificate":"nope"}`: "the CA certificate has no certificate Pando can read",
		`{"address":"a:0"}`:                         `"a:0" is not a collector address. Use a host and port, such as siem.example.com:6514`,
		`{"address":"a:65536"}`:                     "is not a collector address",
		`{"address":"a:port"}`:                      "is not a collector address",
		`{"address":":6514"}`:                       "is not a collector address",
		`{"address":"siem/collector:6514"}`:         "is not a collector address",
		`{"address":"a:1","max_batch":-1}`:          "max_batch is -1",
		`{"address":"a:1","tls":"off","credentials":{"client_certificate":"c","client_key":"k"}}`: "tls is off",
	} {
		require.ErrorContains(t, New().Configure(ctx, json.RawMessage(raw)), want, raw)
	}

	// A bracketed IPv6 host without a port takes the transport's default.
	a := New()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"address":"[::1]","tls":"off"}`)))
	require.Equal(t, "[::1]:514", a.AuditSinkCapabilities().Endpoint)
}

func TestSyslogHeaderFields(t *testing.T) {
	require.Equal(t, "-", header("", 48))
	require.Equal(t, "-", header(" \té", 48), "nothing printable is the nil value")
	require.Equal(t, "abc", header("a b c", 48))
	require.Equal(t, "ab", header("abcdef", 2))
	require.Equal(t, `a\"b\\c\]`, sdEscape(`a"b\c]`))
}
