//go:build unix

package hostagent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// streamPair is two connected ends of a stream that, unlike net.Pipe's, can
// each be closed for writing alone (CloseWrite): a helper container's stdin
// ends while its output is still to come (R-420). A unix socket pair.
func streamPair() (net.Conn, net.Conn, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("the agent could not open a stream to the runtime: %w", err)
	}
	a, errA := fdConn(fds[0])
	b, errB := fdConn(fds[1])
	if err := errors.Join(errA, errB); err != nil {
		for _, c := range []net.Conn{a, b} {
			if c != nil {
				_ = c.Close()
			}
		}
		return nil, nil, fmt.Errorf("the agent could not open a stream to the runtime: %w", err)
	}
	return a, b, nil
}

// fdConn makes a net.Conn of fd, which it then closes: net.FileConn holds
// its own copy.
func fdConn(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "runtime-stream")
	defer func() { _ = f.Close() }()
	return net.FileConn(f)
}
