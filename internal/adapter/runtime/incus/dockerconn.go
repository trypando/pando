package incus

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// podmanSocket is where the guest's Podman serves its Docker-compatible API.
const podmanSocket = "/run/podman/podman.sock"

// The Docker API inside a VM is reached through the Incus API, never over the
// network (notes-incus-runtime.md, Shape): each connection is an exec of socat
// bridging its stdio to Podman's socket. The Incus API's own authentication is
// the only credential, and nothing in the VM listens for one. HTTP keep-alive
// means an exec per burst of calls rather than one per call.

// dockerClient is a Docker client for the Podman in one VM.
func dockerClient(api incusAPI, instance string) (*client.Client, error) {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialExec(ctx, api, instance, []string{"socat", "STDIO", "UNIX-CONNECT:" + podmanSocket})
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext:     dial,
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}}
	return client.New(
		client.WithHost("unix://"+podmanSocket),
		client.WithHTTPClient(hc),
		client.WithDialContext(dial),
	)
}

// dialExec starts command in the instance and returns its stdio as a
// connection. Closing the connection closes the command's stdin, which ends
// socat, and the exec with it.
func dialExec(ctx context.Context, api incusAPI, instance string, command []string) (net.Conn, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &execConn{in: inW, out: outR, done: make(chan struct{})}

	// The exec outlives the dial: it is the connection. Its context is not
	// the dial's, which ends when the dial returns.
	execCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	go func() {
		defer close(c.done)
		_, err := api.Exec(execCtx, instance, command, inR, outW, io.Discard)
		if err == nil {
			err = io.EOF
		}
		_ = outW.CloseWithError(err)
		_ = inR.CloseWithError(err)
	}()
	return c, nil
}

// execConn is a net.Conn over an exec's stdin and stdout.
type execConn struct {
	in     *io.PipeWriter
	out    *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (c *execConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *execConn) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *execConn) Close() error {
	c.once.Do(func() {
		_ = c.in.Close()
		_ = c.out.CloseWithError(net.ErrClosed)
		// socat ends when its stdin does; the exec is cancelled if it has
		// not ended shortly after, rather than left running.
		go func() {
			select {
			case <-c.done:
			case <-time.After(5 * time.Second):
				c.cancel()
			}
		}()
	})
	return nil
}

func (c *execConn) LocalAddr() net.Addr  { return execAddr{} }
func (c *execConn) RemoteAddr() net.Addr { return execAddr{} }

// Deadlines are not supported by a pipe; the Docker client bounds its calls
// with contexts instead.
func (c *execConn) SetDeadline(time.Time) error      { return nil }
func (c *execConn) SetReadDeadline(time.Time) error  { return nil }
func (c *execConn) SetWriteDeadline(time.Time) error { return nil }

type execAddr struct{}

func (execAddr) Network() string { return "incus-exec" }
func (execAddr) String() string  { return "incus-exec" }

var errNoVM = errors.New("the app's VM does not exist")
