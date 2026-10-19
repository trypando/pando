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
		case strings.HasPrefix(cmd, "AUTH"):
			say("235 accepted")
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

func configure(t *testing.T, cfg map[string]any) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	a := New()
	return a, a.Configure(context.Background(), raw)
}

// TestConfigureRefusesSignInOverAnUnencryptedConnection: net/smtp will not
// send a password unencrypted to a remote host, so that combination is
// refused before it is saved rather than failing every send.
func TestConfigureRefusesSignInOverAnUnencryptedConnection(t *testing.T) {
	_, err := configure(t, map[string]any{
		"host": "smtp.example.com", "from": "a@example.com", "security": SecurityNone,
		"username": "apikey", "credentials": map[string]string{"password": "k"},
	})
	require.ErrorContains(t, err, "will not send a password unencrypted to smtp.example.com")
	require.ErrorContains(t, err, "Set security to starttls or tls")

	// No username, nothing to send unencrypted: allowed.
	_, err = configure(t, map[string]any{"host": "smtp.example.com", "from": "a@example.com", "security": SecurityNone})
	require.NoError(t, err)
}

// TestConfigureAllowsUnencryptedSignInToThisMachine: the exception net/smtp
// makes, which the adapter must make too, and which really signs in.
func TestConfigureAllowsUnencryptedSignInToThisMachine(t *testing.T) {
	for _, host := range []string{"localhost", "::1"} {
		_, err := configure(t, map[string]any{
			"host": host, "from": "a@example.com", "security": SecurityNone,
			"username": "u", "credentials": map[string]string{"password": "p"},
		})
		require.NoError(t, err, host)
	}

	srv := startFake(t)
	a, err := configure(t, map[string]any{
		"host": "127.0.0.1", "port": srv.ln.Addr().(*net.TCPAddr).Port, "from": "a@example.com",
		"security": SecurityNone, "username": "u", "credentials": map[string]string{"password": "p"},
	})
	require.NoError(t, err)
	require.NoError(t, a.HealthCheck(context.Background()))
}

func TestConfigureRefusesAUsernameWithoutAPassword(t *testing.T) {
	_, err := configure(t, map[string]any{"host": "smtp.example.com", "from": "a@example.com", "username": "apikey"})
	require.ErrorContains(t, err, "a username is set and no password")
}

// TestConfigurePortFollowsSecurity: an empty port is 465 with TLS and 587
// otherwise, as the field's stated default says, and an explicit port that
// no server would accept with the chosen security is refused, not dialed.
func TestConfigurePortFollowsSecurity(t *testing.T) {
	for security, want := range map[string]int{SecurityTLS: 465, SecurityStartTLS: 587, SecurityNone: 587, "": 587} {
		a, err := configure(t, map[string]any{"host": "h", "from": "a@example.com", "security": security})
		require.NoError(t, err, security)
		require.Equal(t, want, a.cfg.Port, security)
	}

	_, err := configure(t, map[string]any{"host": "h", "from": "a@example.com", "security": SecurityTLS, "port": 587})
	require.ErrorContains(t, err, "port 587 is for STARTTLS")
	for _, security := range []string{SecurityStartTLS, SecurityNone} {
		_, err = configure(t, map[string]any{"host": "h", "from": "a@example.com", "security": security, "port": 465})
		require.ErrorContains(t, err, "port 465 is for TLS", security)
	}
	_, err = configure(t, map[string]any{"host": "h", "from": "a@example.com", "port": 70000})
	require.ErrorContains(t, err, "70000 is not a port")

	// Any other explicit port is honored as given.
	a, err := configure(t, map[string]any{"host": "h", "from": "a@example.com", "security": SecurityTLS, "port": 2465})
	require.NoError(t, err)
	require.Equal(t, 2465, a.cfg.Port)

	for _, f := range Info().Fields {
		if f.Key == "port" {
			require.Equal(t, "587, or 465 with TLS", f.Default)
		}
	}
}
