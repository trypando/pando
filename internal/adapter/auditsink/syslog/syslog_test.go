package syslog

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// selfSigned makes a certificate valid for 127.0.0.1, returned as PEM and as
// a tls.Certificate.
func selfSigned(t *testing.T, cn string, usage x509.ExtKeyUsage) (certPEM, keyPEM string, pair tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	pair, err = tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	require.NoError(t, err)
	return certPEM, keyPEM, pair
}

// collector accepts TLS connections and sends each octet-counted frame it
// reads on the returned channel.
func collector(t *testing.T, cfg *tls.Config) (string, <-chan string) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	frames := make(chan string, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				for {
					n, err := r.ReadString(' ')
					if err != nil {
						return
					}
					size, err := strconv.Atoi(strings.TrimSpace(n))
					if err != nil {
						return
					}
					msg := make([]byte, size)
					if _, err := io.ReadFull(r, msg); err != nil {
						return
					}
					frames <- string(msg)
				}
			}()
		}
	}()
	return ln.Addr().String(), frames
}

func receive(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("the collector received nothing")
		return ""
	}
}

// TestR382_SyslogFramesRFC5424OverTLS asserts R-382: each audit event reaches
// a syslog collector as one octet-counted RFC 5424 message over mutual TLS,
// with the action as MSGID, the id and outcome as structured data, and a
// denied event raised to warning.
func TestR382_SyslogFramesRFC5424OverTLS(t *testing.T) {
	serverCert, _, serverPair := selfSigned(t, "collector", x509.ExtKeyUsageServerAuth)
	clientCert, clientKey, clientPair := selfSigned(t, "pando", x509.ExtKeyUsageClientAuth)
	clientPool := x509.NewCertPool()
	clientPool.AddCert(clientPair.Leaf)
	addr, frames := collector(t, &tls.Config{
		Certificates: []tls.Certificate{serverPair},
		ClientCAs:    clientPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})

	ctx := context.Background()
	configure := func(creds map[string]any) (*Adapter, error) {
		raw, _ := json.Marshal(map[string]any{
			"address": addr, "ca_certificate": serverCert, "hostname": "pando-1",
			"exclude": "app.use, health", "start": "now", "credentials": creds,
		})
		a := New()
		a.now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
		return a, a.Configure(ctx, raw)
	}

	// Without a client certificate the collector refuses the handshake.
	plain, err := configure(nil)
	require.NoError(t, err)
	require.Error(t, plain.HealthCheck(ctx))

	a, err := configure(map[string]any{"client_certificate": clientCert, "client_key": clientKey})
	require.NoError(t, err)
	require.NoError(t, a.HealthCheck(ctx))

	caps := a.AuditSinkCapabilities()
	require.Equal(t, api.AuditSinkCapabilities{
		MaxBatch: DefaultMaxBatch, Format: api.AuditFormatNative, Transport: "syslog", Endpoint: addr,
		Exclude: []string{"app.use", "health"}, StartAtNow: true,
	}, caps)

	batch := api.AuditBatch{
		Events: []json.RawMessage{
			json.RawMessage(`{"id":41,"action":"app.deploy","outcome":"success"}`),
			json.RawMessage(`{"id":42,"action":"app.use.denied","outcome":"denied"}`),
		},
		IDs:     []int64{41, 42},
		Actions: []string{"app.deploy", "app.use.denied with spaces and a very long tail to cut"},
	}
	require.NoError(t, a.Send(ctx, batch))
	first, second := receive(t, frames), receive(t, frames)
	// authpriv (10) * 8 + informational (6) = 86; + warning (4) = 84.
	require.Equal(t, `<86>1 2026-10-08T12:00:00.000000Z pando-1 pando - app.deploy [pando@32473 id="41" outcome="success"] {"id":41,"action":"app.deploy","outcome":"success"}`, first)
	require.True(t, strings.HasPrefix(second, `<84>1 2026-10-08T12:00:00.000000Z pando-1 pando - app.use.deniedwithspacesandavery [pando@32473 id="42" outcome="denied"] `), second)

	// The connection is reused, and a dropped one is replaced.
	a.mu.Lock()
	_ = a.conn.Close()
	a.mu.Unlock()
	require.NoError(t, a.Send(ctx, api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{"status_id":2,"status_detail":"failed"}`)}, IDs: []int64{43}, Actions: []string{"deploy.finish"}}))
	require.Contains(t, receive(t, frames), `<84>1`)
	require.NoError(t, Info().Validate())
}

// TestR190_SyslogCredentialsInPlainConfigAreRefused asserts R-190: a client
// key in the unencrypted configuration is refused, and half a key pair is too.
func TestR190_SyslogCredentialsInPlainConfigAreRefused(t *testing.T) {
	ctx := context.Background()
	require.ErrorContains(t, New().Configure(ctx, json.RawMessage(`{"address":"a:1","client_key":"x"}`)), "set it as a credential")
	require.ErrorContains(t, New().Configure(ctx, json.RawMessage(`{"address":"a:1","credentials":{"client_key":"x"}}`)), "both")

	// Built rather than written out, so the source holds no key-shaped
	// literal for the credential scan to flag; the value only has to be
	// distinctive enough to search the error for.
	marker := "redaction-marker-" + "7f3k"
	raw, err := json.Marshal(map[string]any{"address": "a:1",
		"credentials": map[string]string{"client_certificate": "not a cert", "client_key": marker}})
	require.NoError(t, err)
	err = New().Configure(ctx, raw)
	require.ErrorContains(t, err, "could not be read as a pair")
	require.NotContains(t, err.Error(), marker)
}

func TestSyslogSettingsAreChecked(t *testing.T) {
	ctx := context.Background()
	for raw, want := range map[string]string{
		`{}`:                                                 "no address",
		`{"address":"a:1","tls":"maybe"}`:                    "not a tls setting",
		`{"address":"a:1","facility":"mail"}`:                "not a facility",
		`{"address":"a:1","format":"cef"}`:                   "not an event format",
		`{"address":"a:1","start":"later"}`:                  "not a starting point",
		`{"address":"a:1","tls":"off","ca_certificate":"x"}`: "tls is off",
	} {
		require.ErrorContains(t, New().Configure(ctx, json.RawMessage(raw)), want, raw)
	}
	a := New()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"address":"siem.example.com","tls":"off","actions":["deploy"]}`)))
	require.Equal(t, "siem.example.com:514", a.AuditSinkCapabilities().Endpoint)
	require.Equal(t, []string{"deploy"}, a.AuditSinkCapabilities().Actions)
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"address":"siem.example.com"}`)))
	require.Equal(t, "siem.example.com:6514", a.AuditSinkCapabilities().Endpoint)
}
