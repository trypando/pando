//go:build windows

package docker

// socketGroup has no meaning on Windows, where the runtime is reached over a
// named pipe.
func socketGroup(string) string { return "" }
