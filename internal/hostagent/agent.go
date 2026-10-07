package hostagent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DefaultPort is where an agent listens, and the one port an app host
// publishes (R-026 concerns app workloads; this port reaches none without
// Pando's certificate).
const DefaultPort = 7443

// The protocol, after the TLS handshake: the proxy sends one line naming the
// container and port,
//
//	PANDO-AGENT/1 <container> <port>\n
//
// and the agent answers "OK\n" and from then on carries bytes both ways, or
// "NO <reason>\n" and closes. Nothing else is said by either side, so HTTP,
// websockets and server-sent events pass through unchanged.
const (
	protocolVersion = "PANDO-AGENT/1"
	maxHeaderLine   = 512
	headerTimeout   = 10 * time.Second
)

// targetName is what a container name Pando gives an app's workload looks
// like (the Docker adapter's containerName). The agent refuses anything else
// before it resolves a name, so it never looks up a name outside Docker's own
// DNS on its networks.
var targetName = regexp.MustCompile(`^pando-[A-Za-z0-9][A-Za-z0-9_.-]{0,200}$`)

// Agent accepts Pando's proxy and carries each connection to one workload.
type Agent struct {
	// TLS must require and verify Pando's client certificate (ServerTLS).
	TLS *tls.Config

	// Pool is the range the app networks on this host take their addresses
	// from: the Docker adapter's network_pool. A target outside it is
	// refused, which is what keeps the agent off the network it is published
	// on and off anything else on the host.
	Pool netip.Prefix

	// Resolve looks a container name up. Nil is the system resolver, which in
	// a container on user-defined networks is Docker's.
	Resolve func(ctx context.Context, name string) ([]netip.Addr, error)

	// Interfaces are the agent's own addresses with their prefix lengths: the
	// networks it is joined to. Nil reads them from the system each time.
	Interfaces func() ([]netip.Prefix, error)

	// Dial connects to the permitted address. Nil is a TCP dial.
	Dial func(ctx context.Context, addr string) (net.Conn, error)

	Logger *zap.Logger
}

// Serve accepts on ln, which must be a plain TCP listener: the agent does the
// TLS handshake itself. It returns when ctx is done or ln fails.
func (a *Agent) Serve(ctx context.Context, ln net.Listener) error {
	if a.TLS == nil || a.TLS.ClientAuth != tls.RequireAndVerifyClientCert {
		return errors.New("the host agent will not start without requiring Pando's client certificate")
	}
	if !a.Pool.IsValid() || !a.Pool.Addr().Is4() {
		return errors.New("the host agent will not start without the app network range it forwards into")
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.handle(ctx, conn)
		}()
	}
}

func (a *Agent) logger() *zap.Logger {
	if a.Logger == nil {
		return zap.NewNop()
	}
	return a.Logger
}

func (a *Agent) handle(ctx context.Context, raw net.Conn) {
	defer func() { _ = raw.Close() }()
	conn := tls.Server(raw, a.TLS)
	_ = conn.SetDeadline(time.Now().Add(headerTimeout))
	if err := conn.HandshakeContext(ctx); err != nil {
		// No certificate, or not Pando's: nothing is read and nothing is
		// forwarded.
		a.logger().Info("refused a connection without Pando's certificate",
			zap.String("from", raw.RemoteAddr().String()), zap.Error(err))
		return
	}

	reader := bufio.NewReaderSize(conn, maxHeaderLine)
	line, err := readLine(reader)
	if err != nil {
		return
	}
	name, port, err := parseHeader(line)
	if err != nil {
		refuse(conn, err.Error())
		return
	}
	upstream, err := a.connect(ctx, name, port)
	if err != nil {
		a.logger().Info("refused a forward", zap.String("target", name), zap.Int("port", port), zap.Error(err))
		refuse(conn, err.Error())
		return
	}
	defer func() { _ = upstream.Close() }()
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	// Bytes both ways until either side closes. The reader, not conn, so
	// anything the proxy sent after the header line is not lost.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, reader)
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		_ = conn.CloseWrite()
		done <- struct{}{}
	}()
	select {
	case <-done:
		<-done
	case <-ctx.Done():
	}
}

func refuse(conn net.Conn, reason string) {
	_, _ = io.WriteString(conn, "NO "+strings.ReplaceAll(reason, "\n", " ")+"\n")
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func parseHeader(line string) (string, int, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != protocolVersion {
		return "", 0, errors.New("the request did not name a container and port")
	}
	if !targetName.MatchString(fields[1]) {
		return "", 0, fmt.Errorf("%q is not the name of an app container", fields[1])
	}
	port, err := strconv.Atoi(fields[2])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q is not a port", fields[2])
	}
	return fields[1], port, nil
}

// connect resolves name and dials the first of its addresses the agent may
// forward to. Dialed by address, never by name again, so the answer that was
// checked is the one that is used.
func (a *Agent) connect(ctx context.Context, name string, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, headerTimeout)
	defer cancel()

	addrs, err := a.resolve(ctx, name)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("%s is not on any app network this agent is joined to", name)
	}
	own, err := a.interfaces()
	if err != nil {
		return nil, errors.New("the agent could not read its own networks")
	}
	var lastErr error
	for _, addr := range addrs {
		if err := Permitted(addr, a.Pool, own); err != nil {
			lastErr = err
			continue
		}
		target := netip.AddrPortFrom(addr.Unmap(), uint16(port)).String() //nolint:gosec // G115: parseHeader bounds port to 1–65535.
		conn, err := a.dial(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("%s did not accept a connection on port %d", name, port)
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s is not on any app network this agent is joined to", name)
	}
	return nil, lastErr
}

func (a *Agent) resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	if a.Resolve != nil {
		return a.Resolve(ctx, name)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", name)
}

func (a *Agent) interfaces() ([]netip.Prefix, error) {
	if a.Interfaces != nil {
		return a.Interfaces()
	}
	return SystemInterfaces()
}

func (a *Agent) dial(ctx context.Context, addr string) (net.Conn, error) {
	if a.Dial != nil {
		return a.Dial(ctx, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// SystemInterfaces is every IPv4 address of this process's network
// interfaces, with its prefix.
func SystemInterfaces() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() {
			continue
		}
		bits, _ := ipnet.Mask.Size()
		out = append(out, netip.PrefixFrom(ip, bits))
	}
	return out, nil
}

// Permitted reports whether the agent may forward to addr.
//
// Only to a container on an app network on this host: addr must be inside
// the app network range, and on a network the agent itself is joined to —
// which on a multi-host install the adapter does for app networks and nothing
// else. Within such a network, never to the network's first address (Docker
// gives it to the bridge, which is the host itself), its network or broadcast
// address, or the agent's own address.
func Permitted(addr netip.Addr, pool netip.Prefix, own []netip.Prefix) error {
	addr = addr.Unmap()
	if !addr.Is4() {
		return fmt.Errorf("%s is not an IPv4 address on an app network", addr)
	}
	if !pool.Contains(addr) {
		return fmt.Errorf("%s is outside the app network range %s", addr, pool)
	}
	for _, p := range own {
		subnet := p.Masked()
		if !subnet.Contains(addr) || !pool.Contains(subnet.Addr()) || subnet.Bits() < pool.Bits() {
			continue
		}
		switch addr {
		case p.Addr():
			return fmt.Errorf("%s is the agent itself", addr)
		case subnet.Addr(), subnet.Addr().Next():
			return fmt.Errorf("%s is the network's own address, not a container's", addr)
		case lastAddr(subnet):
			return fmt.Errorf("%s is the network's broadcast address", addr)
		}
		return nil
	}
	return fmt.Errorf("%s is not on an app network this agent is joined to", addr)
}

func lastAddr(p netip.Prefix) netip.Addr {
	ones := [...]byte{0, 1, 3, 7, 15, 31, 63, 127, 255}
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	for i := 3; i >= 0 && host > 0; i-- {
		n := min(host, 8)
		b[i] |= ones[n]
		host -= n
	}
	return netip.AddrFrom4(b)
}

// Dial opens a connection through the agent at addr to the container name on
// port, as the client tlsConfig (ClientTLS) authenticates. The connection it
// returns is a stream to the workload.
func Dial(ctx context.Context, addr string, tlsConfig *tls.Config, name string, port int) (net.Conn, error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		Config:    tlsConfig,
	}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("could not reach the host agent at %s: %w", addr, err)
	}
	conn := raw.(*tls.Conn)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(headerTimeout))
	}
	if _, err := fmt.Fprintf(conn, "%s %s %d\n", protocolVersion, name, port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the host agent at %s closed the connection: %w", addr, err)
	}
	// One byte at a time, so nothing the workload sends after the answer is
	// read into a buffer this function then drops.
	var answer []byte
	one := make([]byte, 1)
	for len(answer) < maxHeaderLine {
		if _, err := io.ReadFull(conn, one); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("the host agent at %s closed the connection: %w", addr, err)
		}
		if one[0] == '\n' {
			break
		}
		answer = append(answer, one[0])
	}
	_ = conn.SetDeadline(time.Time{})
	switch reply := string(answer); {
	case reply == "OK":
		return conn, nil
	case strings.HasPrefix(reply, "NO "):
		_ = conn.Close()
		return nil, fmt.Errorf("the host agent at %s refused: %s", addr, strings.TrimPrefix(reply, "NO "))
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("the host agent at %s answered %q", addr, reply)
	}
}
