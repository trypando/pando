package incusapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// Client talks to one Incus server, in one project.
type Client struct {
	base    string // http://incus.socket or https://host:port, no trailing slash
	hc      *http.Client
	project string
	target  string
}

// ConnectUnix is a client over the server's unix socket. Nothing is
// contacted until the first request.
func ConnectUnix(path, project string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
	return &Client{
		base:    "http://incus.socket",
		hc:      &http.Client{Transport: &http.Transport{DialContext: dial}},
		project: project,
	}
}

// ConnectHTTPS is a client over HTTPS, presenting a client certificate and
// accepting only the server certificate given, as PEM, whatever name it
// carries: Incus servers present self-signed certificates, so they are
// pinned rather than verified against an authority.
func ConnectHTTPS(address, serverCertPEM string, client tls.Certificate, project string) (*Client, error) {
	block, _ := pem.Decode([]byte(serverCertPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the server certificate is not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("the server certificate cannot be read: %w", err)
	}
	pinned := block.Bytes
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{client},
		// The pin below is the verification.
		InsecureSkipVerify: true, //nolint:gosec // pinned in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, pinned) {
				return errors.New("the Incus server presented a certificate other than the one configured")
			}
			return nil
		},
	}
	return &Client{
		base:    strings.TrimSuffix(address, "/"),
		hc:      &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: false}},
		project: project,
	}, nil
}

// UseTarget is the same client, sending requests to one cluster member.
func (c *Client) UseTarget(member string) *Client {
	d := *c
	d.target = member
	return &d
}

// StatusError is an error Incus answered with.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// IsStatus reports whether err is an answer from Incus with that status.
func IsStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

// response is Incus's envelope.
type response struct {
	Type       string          `json:"type"`
	StatusCode int             `json:"status_code"`
	ErrorCode  int             `json:"error_code"`
	Error      string          `json:"error"`
	Operation  string          `json:"operation"`
	Metadata   json.RawMessage `json:"metadata"`
}

// Operation is a background operation.
type Operation struct {
	ID         string                     `json:"id"`
	Status     string                     `json:"status"`
	StatusCode int                        `json:"status_code"`
	Err        string                     `json:"err"`
	Metadata   map[string]json.RawMessage `json:"metadata"`
}

func (c *Client) url(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	if c.project != "" && q.Get("project") == "" {
		q.Set("project", c.project)
	}
	if c.target != "" {
		q.Set("target", c.target)
	}
	return c.base + path + "?" + q.Encode()
}

// do sends a request and decodes a sync answer's metadata into out. An
// async answer returns its operation, which the caller waits for.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, etag string, out any) (string, *Operation, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rd)
	if err != nil {
		return "", nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var env response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&env); err != nil {
		return "", nil, fmt.Errorf("incus answered %s %s with %s and no readable body: %w", method, path, resp.Status, err)
	}
	switch env.Type {
	case "error":
		status := env.ErrorCode
		if status == 0 {
			status = resp.StatusCode
		}
		return "", nil, &StatusError{Status: status, Message: env.Error}
	case "async":
		var op Operation
		if err := json.Unmarshal(env.Metadata, &op); err != nil {
			return "", nil, err
		}
		return "", &op, nil
	}
	if out != nil && len(env.Metadata) > 0 {
		if err := json.Unmarshal(env.Metadata, out); err != nil {
			return "", nil, err
		}
	}
	return resp.Header.Get("ETag"), nil, nil
}

// Get decodes a sync GET into out and returns its ETag.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) (string, error) {
	etag, _, err := c.do(ctx, http.MethodGet, path, q, nil, "", out)
	return etag, err
}

// Send sends a request and, when Incus answers with an operation, waits for
// it to end.
func (c *Client) Send(ctx context.Context, method, path string, body any, etag string) error {
	_, op, err := c.do(ctx, method, path, nil, body, etag, nil)
	if err != nil || op == nil {
		return err
	}
	_, err = c.Wait(ctx, op.ID)
	return err
}

// Wait waits for an operation to end, and returns it, or its error.
func (c *Client) Wait(ctx context.Context, id string) (*Operation, error) {
	for {
		var op Operation
		// A bounded server-side wait, so the context is checked between.
		_, err := c.Get(ctx, "/1.0/operations/"+url.PathEscape(id)+"/wait", url.Values{"timeout": {"30"}}, &op)
		if err != nil {
			return nil, err
		}
		switch op.Status {
		case "Success":
			return &op, nil
		case "Failure", "Cancelled":
			return &op, &StatusError{Status: op.StatusCode, Message: op.Err}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// Exec runs command in an instance with its stdio on the readers and
// writers given, and returns its exit status once it ends and its output is
// copied. stdin may be nil.
func (c *Client) Exec(ctx context.Context, instance string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	_, op, err := c.do(ctx, http.MethodPost, "/1.0/instances/"+url.PathEscape(instance)+"/exec", nil,
		InstanceExecPost{Command: command, WaitForWS: true, Interactive: false}, "", nil)
	if err != nil {
		return -1, err
	}
	if op == nil {
		return -1, errors.New("incus did not start the command as an operation")
	}
	var fds map[string]string
	if raw, ok := op.Metadata["fds"]; ok {
		if err := json.Unmarshal(raw, &fds); err != nil {
			return -1, err
		}
	}
	// Every websocket is opened before the command starts.
	conns := map[string]*websocket.Conn{}
	defer func() {
		for _, ws := range conns {
			_ = ws.CloseNow()
		}
	}()
	for _, fd := range []string{"control", "0", "1", "2"} {
		ws, err := c.websocket(ctx, op.ID, fds[fd])
		if err != nil {
			return -1, fmt.Errorf("could not attach to the command's %s: %w", fd, err)
		}
		ws.SetReadLimit(-1)
		conns[fd] = ws
	}

	copied := make(chan error, 2)
	go func() { copied <- readStream(ctx, conns["1"], stdout) }()
	go func() { copied <- readStream(ctx, conns["2"], stderr) }()
	go writeStream(ctx, conns["0"], stdin)

	done, err := c.Wait(ctx, op.ID)
	if err != nil {
		return -1, err
	}
	for range 2 {
		select {
		case <-copied:
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	var code float64
	if raw, ok := done.Metadata["return"]; ok {
		if err := json.Unmarshal(raw, &code); err != nil {
			return -1, err
		}
		return int(code), nil
	}
	return -1, fmt.Errorf("incus did not report how %q ended", command[0])
}

func (c *Client) websocket(ctx context.Context, op, secret string) (*websocket.Conn, error) {
	u := c.url("/1.0/operations/"+url.PathEscape(op)+"/websocket", url.Values{"secret": {secret}})
	u = "ws" + strings.TrimPrefix(u, "http")
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.hc})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return ws, err
}

// readStream copies a command's output until Incus marks its end: an empty
// text message, or the socket closing.
func readStream(ctx context.Context, ws *websocket.Conn, w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			// Incus closing the socket is the end of the stream too.
			if websocket.CloseStatus(err) != -1 || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if typ == websocket.MessageText {
			return nil
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
}

// writeStream sends stdin and then an empty text message, which is how Incus
// is told it has ended.
func writeStream(ctx context.Context, ws *websocket.Conn, r io.Reader) {
	if r != nil {
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				break
			}
		}
	}
	_ = ws.Write(ctx, websocket.MessageText, []byte{})
}

// Upgrade opens path as a raw connection speaking protocol, as Incus serves
// SFTP.
func (c *Client) Upgrade(ctx context.Context, path, protocol string) (io.ReadWriteCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path, nil), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Upgrade", protocol)
	req.Header.Set("Connection", "Upgrade")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		var env response
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env)
		status := env.ErrorCode
		if status == 0 {
			status = resp.StatusCode
		}
		return nil, &StatusError{Status: status, Message: strings.TrimSpace(env.Error + " " + resp.Status)}
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, errors.New("incus upgraded the connection, and it cannot be written to")
	}
	return rwc, nil
}
