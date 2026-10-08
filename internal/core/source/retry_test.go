package source

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// cutOffServer accepts connections and resets each one once the request has
// arrived, the way a pooled connection that died under a request does. It
// returns the address and a count of the connections it has taken.
func cutOffServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var n atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				// Closing with no linger sends a reset rather than a clean
				// end, which is what a dead connection looks like to the
				// client.
				if tcp, ok := c.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String() + "/app.git", &n
}

// A clone whose connection fails under it is tried again with a new one, and a
// clone that keeps failing that way is reported after the last attempt.
func TestACloneCutOffByTheConnectionIsTriedAgain(t *testing.T) {
	url, conns := cutOffServer(t)

	_, err := fetchGit(context.Background(), spec.Source{Type: spec.SourceGit, URL: url}, nil, nil)
	require.Error(t, err)
	require.True(t, transient(err), "the failure was the connection's: %v", err)
	require.GreaterOrEqual(t, int(conns.Load()), fetchAttempts, "every attempt reached the server")
}

func TestRetryingACloneStopsWithTheContext(t *testing.T) {
	url, _ := cutOffServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := fetchGit(ctx, spec.Source{Type: spec.SourceGit, URL: url}, nil, nil)
	require.Error(t, err)
	require.Less(t, time.Since(started), 900*time.Millisecond, "a canceled deploy does not wait out the next attempt")
}

// An upload that cannot be removed is reported rather than silently kept.
func TestAnUploadThatCannotBeRemovedIsReported(t *testing.T) {
	s := Sources{UploadDir: t.TempDir()}

	stuck := filepath.Join(s.UploadDir, "app_01STUCK.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Join(stuck, "inside"), 0o700))

	err := s.DiscardUpload("app_01STUCK")
	require.ErrorContains(t, err, "could not remove the app's uploaded source")
}
