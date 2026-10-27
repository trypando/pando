package hostagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// echoingRuntime is a Docker API on a unix socket whose attach and exec-start
// calls upgrade the connection the way Docker's do, read the caller's stdin
// to its end, and only then answer it upper-cased and close: a helper
// container running tar, which writes nothing until its input has ended.
func echoingRuntime(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ps")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\n" +
			"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		in, _ := io.ReadAll(rw)
		_, _ = conn.Write(bytes.ToUpper(in))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// upgraded makes one upgraded call through the agent at addr, writes stdin,
// closes its side for writing, and returns all the runtime answered.
func upgraded(ctx context.Context, t *testing.T, addr string, tlsConfig *tls.Config, path, stdin string) string {
	t.Helper()
	conn, err := DialRuntime(ctx, addr, tlsConfig)
	require.NoError(t, err)
	defer conn.Close()
	// Well short of the agent's halfCloseWait: an end of input that never
	// arrives fails here, not after it.
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n"+
		"Connection: Upgrade\r\nUpgrade: tcp\r\n\r\n", path)
	require.NoError(t, err)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	// A 101's body is empty; the stream goes on in br.
	require.NoError(t, resp.Body.Close())
	_, err = io.WriteString(conn, stdin)
	require.NoError(t, err)
	require.NoError(t, conn.(*tls.Conn).CloseWrite())
	out, err := io.ReadAll(br)
	require.NoError(t, err, "the runtime's answer arrives once the input has ended")
	return string(out)
}

// TestR420_AStreamThroughAnAgentClosesItsInputWithoutLosingItsOutput asserts
// R-420's volume ship, backup and restore through a joined host's agent: a
// helper container's stdin, attached through the agent and closed by the
// control host, reaches the runtime as the end of its input, and everything
// the helper writes after it arrives — through the fence's proxy (R-421) or
// without one, for an attach and an exec alike.
func TestR420_AStreamThroughAnAgentClosesItsInputWithoutLosingItsOutput(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	sock := echoingRuntime(t)
	big := string(bytes.Repeat([]byte("volume data "), 1<<14))
	for _, fence := range []Fence{&switchFence{}, nil} {
		addr := agentOn(ctx, t, as, &Agent{RuntimeSocket: sock, Fence: fence})
		tlsConfig := clientFor(t, as)
		for _, path := range []string{
			"/v1.47/containers/abc/attach?stream=1&stdin=1&stdout=1&stderr=1",
			"/v1.47/exec/abc/start",
		} {
			t.Run(fmt.Sprintf("fence %v %s", fence != nil, path), func(t *testing.T) {
				require.Equal(t, "TAR", upgraded(ctx, t, addr, tlsConfig, path, "tar"))
				require.Equal(t, bytes.ToUpper([]byte(big)), []byte(upgraded(ctx, t, addr, tlsConfig, path, big)))
			})
		}
	}
}

func TestLogSafeDropsLineBreaks(t *testing.T) {
	if got := logSafe("/v1.47/containers/x/start\nforged line\r"); got != "/v1.47/containers/x/startforged line" {
		t.Fatalf("logSafe kept a line break: %q", got)
	}
}
