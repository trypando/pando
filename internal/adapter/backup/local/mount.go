package local

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultPath is where bundles go when no path is configured: a directory on
// Pando's data volume, which every replica shares.
const DefaultPath = "/var/lib/pando/backups"

// dataDir is the directory Pando's data volume is mounted at in the shipped
// image and Compose file.
const dataDir = "/var/lib/pando"

// mountinfoPath is the kernel's mount table for this process. A variable so
// the tests can point it at a fake one.
var mountinfoPath = "/proc/self/mountinfo"

// Location is what a backup directory is stored on, as far as the mount table
// tells.
type Location int

const (
	// LocationUnknown: there is no mount table to read (not Linux, such as a
	// development build on macOS), or the path is on no mount in it.
	LocationUnknown Location = iota
	// LocationDataVolume: the same mount as Pando's data directory. The
	// default, and what the shipped install expects.
	LocationDataVolume
	// LocationOtherMount: a mount of its own, such as a bind-mounted host
	// directory.
	LocationOtherMount
	// LocationHostRoot: the root filesystem of a Pando not running in a
	// container, which is a real disk.
	LocationHostRoot
	// LocationContainerLayer: the container's own writable layer, which is
	// discarded when the container is recreated.
	LocationContainerLayer
	// LocationMemory: a tmpfs or ramfs, emptied when Pando restarts.
	LocationMemory
)

type mount struct {
	point  string
	fstype string
}

// Locate reports what path is stored on, from a mount table in
// /proc/self/mountinfo's format. dataDir is Pando's data directory.
//
// A path not mounted into Pando's container is the case this exists for: it
// passes the write probe when it is writable, such as /tmp, and the bundles
// written there are gone the next time the container is recreated.
func Locate(mountinfo io.Reader, path, dataDir string) Location {
	mounts := parseMountinfo(mountinfo)
	m := covering(mounts, path)
	if m == nil {
		return LocationUnknown
	}
	switch {
	case m.fstype == "tmpfs" || m.fstype == "ramfs":
		return LocationMemory
	case m.point == "/" && isContainerLayer(m.fstype):
		return LocationContainerLayer
	case m.point == "/":
		return LocationHostRoot
	case covering(mounts, dataDir) == m:
		return LocationDataVolume
	default:
		return LocationOtherMount
	}
}

// Warnings says what is wrong with a configuration whose bundles would not
// survive Pando's container being recreated or restarted. Empty when nothing
// is, and when the mount table cannot be read: a warning Pando cannot back up
// is one people learn to ignore.
//
// A warning, never a refusal (CLAUDE.md §4): the directory works until the
// container goes, and a development install may want exactly that.
func Warnings(raw json.RawMessage) []string {
	cfg := config{Path: DefaultPath}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Path == "" {
			return nil // Configure reports these as errors.
		}
	}
	return pathWarnings(cfg.Path)
}

func pathWarnings(path string) []string {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	resolved := resolve(path)
	if w := Warning(Locate(f, resolved, resolve(dataDir)), path); w != "" {
		return []string{w}
	}
	return nil
}

// Warning is the message for a location, or empty when it needs none.
func Warning(loc Location, path string) string {
	switch loc {
	case LocationContainerLayer:
		return fmt.Sprintf("Backups written to %s are kept inside Pando's container and are lost when the container is recreated, as it is on upgrade. "+
			"Mount a host directory into the container at %s, or use the default, %s, on Pando's data volume.", path, path, DefaultPath)
	case LocationMemory:
		return fmt.Sprintf("Backups written to %s are kept in memory and are lost when Pando restarts. "+
			"Mount a host directory into the container at %s, or use the default, %s, on Pando's data volume.", path, path, DefaultPath)
	default:
		return ""
	}
}

// resolve makes a path absolute, as Writer will use it relative to Pando's
// working directory.
//
// Symlinks are not followed: the directory usually does not exist yet, and a
// symlink inside Pando's container pointing out of the mount it sits on is
// not a configuration anyone arrives at by accident.
func resolve(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// isContainerLayer is a root filesystem that belongs to a container image.
func isContainerLayer(fstype string) bool {
	return strings.Contains(fstype, "overlay") || fstype == "aufs"
}

// covering is the mount path is on: the longest mount point containing it,
// and of two at the same point the later, which is mounted over the earlier.
func covering(mounts []mount, path string) *mount {
	var best *mount
	for i := range mounts {
		m := &mounts[i]
		if !within(path, m.point) {
			continue
		}
		if best == nil || len(m.point) >= len(best.point) {
			best = m
		}
	}
	return best
}

func within(path, point string) bool {
	return point == "/" || path == point || strings.HasPrefix(path, point+"/")
}

// parseMountinfo reads the fields Locate needs. A line it cannot read is
// skipped rather than failing the whole table.
//
// Format (proc(5)): id parent major:minor root mount-point options
// [optional fields...] - fstype source super-options.
func parseMountinfo(r io.Reader) []mount {
	var out []mount
	s := bufio.NewScanner(r)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 5 {
			continue
		}
		sep := -1
		for i := 5; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		out = append(out, mount{point: unescape(fields[4]), fstype: fields[sep+1]})
	}
	return out
}

// unescape decodes the octal escapes mountinfo writes for a space, tab,
// newline or backslash in a mount point, such as \040.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
