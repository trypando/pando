package smtp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// fakeServer is just enough SMTP to receive messages: it records each
// recipient and the data that followed.
type fakeServer struct {
	ln   net.Listener
	mu   sync.Mutex
	rcpt []string
	data []string
}

func startFake(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &fakeServer{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	say := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			s.mu.Lock()
			s.rcpt = append(s.rcpt, strings.Trim(strings.TrimSpace(line)[len("RCPT TO:"):], "<>"))
			s.mu.Unlock()
			say("250 ok")
		case cmd == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = append(s.data, b.String())
			s.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// TestR232_EmailReachesEachRecipientAtTheirAddress asserts R-232 and R-374:
// the SMTP adapter reaches people, one message each, and skips an account
// with no address rather than failing.
func TestR232_EmailReachesEachRecipientAtTheirAddress(t *testing.T) {
	srv := startFake(t)
	port := srv.ln.Addr().(*net.TCPAddr).Port

	a := New()
	require.Equal(t, api.AudiencePeople, a.Capabilities().Audience)
	raw, _ := json.Marshal(map[string]any{
		"host": "127.0.0.1", "port": port, "security": SecurityNone, "from": "Pando <pando@example.com>",
	})
	require.NoError(t, a.Configure(context.Background(), raw))
	require.NoError(t, a.HealthCheck(context.Background()))

	require.NoError(t, a.Notify(context.Background(), api.Notification{
		Kind:    api.NotifyAppFailed,
		Subject: "billing has failed",
		Body:    "Pando stopped restarting it.",
		Recipients: []api.Recipient{
			{UserID: "usr_1", Email: "ada@example.com"},
			{UserID: "usr_2"}, // no address: not reachable by email
			{UserID: "usr_3", Email: "grace@example.com"},
		},
		EventID: "evt_1",
	}))

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, []string{"ada@example.com", "grace@example.com"}, srv.rcpt)
	require.Len(t, srv.data, 2)
	require.Contains(t, srv.data[0], "Subject: billing has failed")
	require.Contains(t, srv.data[0], "X-Pando-Event-Id: evt_1")
	require.NotContains(t, srv.data[0], "grace@example.com", "one message per recipient")
}

func TestConfigureRefusesAnInlinePassword(t *testing.T) {
	err := New().Configure(context.Background(), json.RawMessage(`{"host":"h","from":"a@b.c","password":"x"}`))
	require.ErrorContains(t, err, "set it as a credential")
	require.ErrorContains(t, New().Configure(context.Background(), json.RawMessage(`{"from":"a@b.c"}`)), "no server")
	require.ErrorContains(t, New().Configure(context.Background(), json.RawMessage(`{"host":"h","from":"nope"}`)), "not an address")
	require.NoError(t, Info().Validate())
}

func TestMessageSubjectCannotCarryAHeader(t *testing.T) {
	from, _ := mail.ParseAddress("pando@example.com")
	msg := string(Message(from, "a@example.com", api.Notification{
		Subject: "billing\r\nBcc: everyone@example.com",
		Body:    "line one\nline two",
	}, time.Unix(0, 0)))
	headers := msg[:strings.Index(msg, "\r\n\r\n")]
	require.NotContains(t, headers, "\r\nBcc:")
	require.Contains(t, msg, "line one\r\nline two")
	require.Equal(t, 1, strings.Count(headers, "Subject:"), strconv.Quote(headers))
}
