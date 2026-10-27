//go:build !unix

package hostagent

import (
	"fmt"
	"net"
)

// streamPair is two connected ends of a stream that can each be closed for
// writing alone (CloseWrite), unlike net.Pipe's (R-420). Without unix socket
// pairs, a loopback TCP connection; an accepted connection that is not the
// one dialed is refused.
func streamPair() (net.Conn, net.Conn, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("the agent could not open a stream to the runtime: %w", err)
	}
	defer func() { _ = ln.Close() }()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, nil, fmt.Errorf("the agent could not open a stream to the runtime: %w", err)
	}
	for {
		accepted, err := ln.Accept()
		if err != nil {
			_ = dialed.Close()
			return nil, nil, fmt.Errorf("the agent could not open a stream to the runtime: %w", err)
		}
		if accepted.RemoteAddr().String() == dialed.LocalAddr().String() {
			return dialed, accepted, nil
		}
		_ = accepted.Close()
	}
}
