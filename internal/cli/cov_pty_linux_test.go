//go:build linux

package cli_test

import (
	"os"
	"strconv"
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
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		_ = master.Close()
		return nil, nil, e
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		_ = master.Close()
		return nil, nil, e
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return slave, func() { _ = slave.Close(); _ = master.Close() }, nil
}
