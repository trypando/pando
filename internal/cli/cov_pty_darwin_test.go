//go:build darwin

package cli_test

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// openTerminal opens a pseudo-terminal and returns its terminal side, which
// is what standard output is in an interactive shell.
func openTerminal() (*os.File, func(), error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := master.Fd()
	for _, req := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, 0); e != 0 {
			_ = master.Close()
			return nil, nil, e
		}
	}
	var name [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		_ = master.Close()
		return nil, nil, e
	}
	slave, err := os.OpenFile(strings.TrimRight(string(name[:]), "\x00"), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return slave, func() { _ = slave.Close(); _ = master.Close() }, nil
}
