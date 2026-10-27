//go:build !windows

package docker

import (
	"os"
	"strconv"
	"syscall"
)

// socketGroup is the numeric group that owns the file at path, or "" when it
// cannot be read.
func socketGroup(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(uint64(st.Gid), 10)
}
