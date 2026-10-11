package incusapi

import (
	"bufio"
	"bytes"
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// fakeIncus answers the parts of the API the client speaks.
type fakeIncus struct {
	mu       sync.Mutex
	requests []string
	ops      map[string]*Operation
	stdin    bytes.Buffer
}

func sync200(w http.ResponseWriter, metadata any) {
	b, _ := json.Marshal(metadata)
	w.Header().Set("ETag", `"tag"`)
	_ = json.NewEncoder(w).Encode(response{Type: "sync", StatusCode: 200, Metadata: b})
}

func asyncOp(w http.ResponseWriter, op *Operation) {
	b, _ := json.Marshal(op)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response{Type: "async", StatusCode: 100, Operation: "/1.0/operations/" + op.ID, Metadata: b})
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response{Type: "error", ErrorCode: status, Error: msg})
}

func (f *fakeIncus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/1.0":
		sync200(w, Server{APIExtensions: []string{"x"}, Environment: ServerEnvironment{Project: r.URL.Query().Get("project")}})
	case path == "/1.0/instances/missing":
		fail(w, http.StatusNotFound, "Instance not found")
	case path == "/1.0/instances/vm" && r.Method == http.MethodPut:
		f.put(w, r)
	case path == "/1.0/instances" && r.Method == http.MethodPost:
		asyncOp(w, f.op("create", "Failure", nil))
	case path == "/1.0/instances/vm/exec":
		f.exec(w, r)
	case strings.HasSuffix(path, "/websocket"):
		f.websocket(w, r)
	case strings.HasPrefix(path, "/1.0/operations/") && strings.HasSuffix(path, "/wait"):
		f.wait(w, strings.TrimSuffix(strings.TrimPrefix(path, "/1.0/operations/"), "/wait"))
	case path == "/1.0/volume/sftp":
		f.upgrade(w, r)
	case path == "/1.0/volume-missing/sftp":
		fail(w, http.StatusNotFound, "Storage volume not found")
	default:
		fail(w, http.StatusInternalServerError, "unexpected "+path)
	}
}

// put accepts an update only with the instance's current ETag.
func (f *fakeIncus) put(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("If-Match") != `"tag"` {
		fail(w, http.StatusPreconditionFailed, "ETag doesn't match")
		return
	}
	asyncOp(w, f.op("put", "Success", nil))
}

// exec starts a command whose stdio is on four websockets.
func (f *fakeIncus) exec(w http.ResponseWriter, r *http.Request) {
	var post InstanceExecPost
	_ = json.NewDecoder(r.Body).Decode(&post)
	fds, _ := json.Marshal(map[string]string{"0": "s0", "1": "s1", "2": "s2", "control": "sc"})
	asyncOp(w, f.op("exec", "Running", map[string]json.RawMessage{"fds": fds}))
}

// wait answers an operation's wait; the exec ends once its stdin has.
func (f *fakeIncus) wait(w http.ResponseWriter, id string) {
	f.mu.Lock()
	op := f.ops[id]
	f.mu.Unlock()
	for i := 0; id == "exec" && i < 200 && !f.finished(op); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sync200(w, op)
}

func (f *fakeIncus) finished(op *Operation) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return op.Status == "Success"
}

// upgrade switches to a raw connection that echoes one line.
func (f *fakeIncus) upgrade(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "sftp" {
		fail(w, http.StatusBadRequest, "no upgrade")
		return
	}
	hj, _ := w.(http.Hijacker)
	conn, rw, _ := hj.Hijack()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: sftp\r\nConnection: Upgrade\r\n\r\n")
	_ = rw.Flush()
	go func() {
		defer func() { _ = conn.Close() }()
		line, _ := bufio.NewReader(rw).ReadString('\n')
		_, _ = conn.Write([]byte("echo " + line))
	}()
}

func (f *fakeIncus) op(id, status string, metadata map[string]json.RawMessage) *Operation {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ops == nil {
		f.ops = map[string]*Operation{}
	}
	op := &Operation{ID: id, Status: status, Metadata: metadata}
	if status == "Failure" {
		op.Err = "Failed creating instance record: no room"
		op.StatusCode = 400
	}
	f.ops[id] = op
	return op
}

// websocket plays a command that echoes stdin to stdout, says hello on
// stderr, and exits 3 once stdin ends.
func (f *fakeIncus) websocket(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := r.Context()
	switch r.URL.Query().Get("secret") {
	case "s0":
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil || typ == websocket.MessageText {
				break
			}
			f.mu.Lock()
			f.stdin.Write(data)
			f.mu.Unlock()
		}
		f.mu.Lock()
		got := f.stdin.String()
		f.mu.Unlock()
		f.finishExec(got)
	case "s1":
		for {
			f.mu.Lock()
			op := f.ops["exec"]
			done := op.Status == "Success"
			out := f.stdin.String()
			f.mu.Unlock()
			if done {
				_ = ws.Write(ctx, websocket.MessageBinary, []byte(out))
				_ = ws.Write(ctx, websocket.MessageText, []byte{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "s2":
		_ = ws.Write(ctx, websocket.MessageBinary, []byte("hello"))
		_ = ws.Write(ctx, websocket.MessageText, []byte{})
	case "sc":
		_, _, _ = ws.Read(ctx)
	}
}

func (f *fakeIncus) finishExec(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, _ := json.Marshal(3)
	f.ops["exec"].Status = "Success"
	f.ops["exec"].Metadata = map[string]json.RawMessage{"return": code}
}

func selfSigned(t *testing.T) (tls.Certificate, string) { return selfSignedFor(t, "incus-test") }

func selfSignedFor(t *testing.T, names ...string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root@incus"}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// tlsServer is fakeIncus over HTTPS with a self-signed certificate that has
// no name the client could verify, as Incus's has none.
func tlsServer(t *testing.T) (*fakeIncus, *httptest.Server, string) {
	t.Helper()
	f := &fakeIncus{}
	ts := httptest.NewUnstartedServer(f)
	cert, certPEM := selfSigned(t)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return f, ts, certPEM
}

func client(t *testing.T) (*fakeIncus, *Client) {
	t.Helper()
	f, ts, certPEM := tlsServer(t)
	clientCert, _ := selfSigned(t)
	c, err := ConnectHTTPS(ts.URL+"/", certPEM, clientCert, "pando")
	require.NoError(t, err)
	return f, c
}

func TestTheServerCertificateIsPinned(t *testing.T) {
	_, ts, _ := tlsServer(t)
	clientCert, _ := selfSigned(t)
	_, other := selfSigned(t)
	c, err := ConnectHTTPS(ts.URL, other, clientCert, "pando")
	require.NoError(t, err)
	_, err = c.Get(context.Background(), "/1.0", nil, &Server{})
	require.ErrorContains(t, err, "failed to verify certificate", "only the configured certificate verifies")

	_, err = ConnectHTTPS(ts.URL, "not a certificate", clientCert, "pando")
	require.Error(t, err)

	// A certificate that names no host cannot be verified, so it is refused
	// rather than trusted without a check.
	_, nameless := selfSignedFor(t)
	_, err = ConnectHTTPS(ts.URL, nameless, clientCert, "pando")
	require.ErrorContains(t, err, "names no host")
}

func TestRequestsCarryTheProjectAndTarget(t *testing.T) {
	f, c := client(t)
	var srv Server
	_, err := c.Get(context.Background(), "/1.0", nil, &srv)
	require.NoError(t, err)
	require.Equal(t, "pando", srv.Environment.Project)
	_, _ = c.UseTarget("member-2").Get(context.Background(), "/1.0", nil, &srv)
	require.Contains(t, f.requests[1], "target=member-2")
	require.NotContains(t, f.requests[0], "target=")
}

func TestErrorsKeepIncussStatus(t *testing.T) {
	_, c := client(t)
	_, err := c.Get(context.Background(), "/1.0/instances/missing", nil, &Instance{})
	require.True(t, IsStatus(err, http.StatusNotFound))
	require.Equal(t, "Instance not found", err.Error())
	require.False(t, IsStatus(err, http.StatusForbidden))
	require.False(t, IsStatus(io.EOF, http.StatusNotFound))
}

func TestSendWaitsForTheOperationAndSendsTheETag(t *testing.T) {
	_, c := client(t)
	require.NoError(t, c.Send(context.Background(), http.MethodPut, "/1.0/instances/vm", InstancePut{}, `"tag"`))
	err := c.Send(context.Background(), http.MethodPut, "/1.0/instances/vm", InstancePut{}, `"old"`)
	require.True(t, IsStatus(err, http.StatusPreconditionFailed))

	err = c.Send(context.Background(), http.MethodPost, "/1.0/instances", InstancesPost{Name: "vm"}, "")
	require.ErrorContains(t, err, "no room")
	require.True(t, IsStatus(err, 400))
}

func TestExecCopiesStdioAndReportsTheExitStatus(t *testing.T) {
	_, c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := c.Exec(ctx, "vm", []string{"cat"}, strings.NewReader("ping"), &stdout, &stderr)
	require.NoError(t, err)
	require.Equal(t, 3, code)
	require.Equal(t, "ping", stdout.String())
	require.Equal(t, "hello", stderr.String())
}

func TestUpgradeOpensARawConnection(t *testing.T) {
	_, c := client(t)
	conn, err := c.Upgrade(context.Background(), "/1.0/volume/sftp", "sftp")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("hi\n"))
	require.NoError(t, err)
	got, _ := bufio.NewReader(conn).ReadString('\n')
	require.Equal(t, "echo hi\n", got)

	_, err = c.Upgrade(context.Background(), "/1.0/volume-missing/sftp", "sftp")
	require.True(t, IsStatus(err, http.StatusNotFound))
}

func TestAUnixSocketClientNeedsNoCertificates(t *testing.T) {
	dir, err := os.MkdirTemp("", "incus")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "unix.socket")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: &fakeIncus{}, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var s Server
	_, err = ConnectUnix(sock, "default").Get(context.Background(), "/1.0", nil, &s)
	require.NoError(t, err)
	require.Equal(t, "default", s.Environment.Project)
}

func TestWritableIsWhatPutTakes(t *testing.T) {
	i := Instance{InstancePut: InstancePut{Profiles: []string{"a"}}, Name: "vm"}
	require.Equal(t, []string{"a"}, i.Writable().Profiles)
	n := Network{NetworkPut: NetworkPut{Config: map[string]string{"k": "v"}}}
	require.Equal(t, "v", n.Writable().Config["k"])
}
