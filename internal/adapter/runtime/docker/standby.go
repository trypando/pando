package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// What one Docker host does for a warm standby (R-419, R-420). The
// multi-host adapter decides what goes where; these are the pieces it runs on
// each host. Every piece that reads or writes data is a helper container, as
// a backup is (volumes_backup.go): busybox, no network, the volumes it needs
// and only the capabilities it uses. Nothing touches the host's filesystem
// outside Docker's own volumes (R-420).
//
// A standby's copies live in one staging volume per app volume on the
// standby's host:
//
//	/standby/copies/<seq>/     a copy of the volume
//	/standby/manifests/<seq>   what was in it: VolumeListing
//
// A new copy starts as a hard-linked clone of the newest (cp -al), so a file
// that did not change takes no space in it; a changed file is removed from
// the clone and arrives whole from the app's host. Copies a ship does not
// finish never get a manifest and are removed by the next.

const (
	// labelStandby marks a staging volume with the bundle it is a standby
	// copy of. Never labelBundle: the multi-host adapter reads that as where
	// the bundle runs (Bundles), and the standby's host is not that.
	labelStandby = "io.pando.standby"
	// labelHome marks the host a bundle was last promoted to, and
	// labelHomeAt when, so a lost host that comes back holding the bundle
	// too is known for the stale one.
	labelHome   = "io.pando.home"
	labelHomeAt = "io.pando.home.at"

	standbyMount = "/standby"
)

type createOnlyKey struct{}

// createOnly reports a context in which Apply creates workloads and starts
// none of them.
func createOnly(ctx context.Context) bool {
	v, _ := ctx.Value(createOnlyKey{}).(bool)
	return v
}

// ApplyStopped creates the bundle's workloads, network and volumes as Apply
// does, and starts nothing: a standby (R-419). Its egress rules are left out,
// since a gateway would be something running for a bundle that must not be;
// the workloads are re-created with the app's own plan when it is promoted.
func (a *Adapter) ApplyStopped(ctx context.Context, p api.BundlePlan) error {
	p.Network.Egress = api.EgressRules{}
	_, err := a.Apply(context.WithValue(ctx, createOnlyKey{}, true), p)
	return err
}

// VolumeEntry is one thing in a volume: a file, a directory or a link.
type VolumeEntry struct {
	// Path is relative to the volume, as find prints it: ./a/b.
	Path string
	// Mode is the raw mode, type bits included, as stat's %f prints it.
	Mode uint32
	Size int64
	// MTime is the modification time in Unix seconds.
	MTime int64
}

// IsDir reports a directory.
func (e VolumeEntry) IsDir() bool { return e.Mode&0o170000 == 0o040000 }

// VolumeListing is what was in a volume, and when it was read.
type VolumeListing struct {
	// TakenAt is the host's clock, in Unix seconds, just before the volume
	// was read. A file changed in that second may not show it in MTime.
	TakenAt int64
	Entries []VolumeEntry
}

// ParseVolumeListing reads a listing as the list helper prints it: a first
// line "T <seconds>", then "<mode hex> <size> <mtime> <path>" per entry.
func ParseVolumeListing(b []byte) (VolumeListing, error) {
	var out VolumeListing
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			t, ok := strings.CutPrefix(line, "T ")
			n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
			if !ok || err != nil {
				return out, errs.New(errs.AdapterFailed, "Pando could not read the list of files in this app's storage.")
			}
			out.TakenAt = n
			continue
		}
		if line == "" {
			continue
		}
		e, err := parseEntry(line)
		if err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, e)
	}
	if first {
		return out, errs.New(errs.AdapterFailed, "Pando could not read the list of files in this app's storage: it was empty.")
	}
	return out, sc.Err()
}

func parseEntry(line string) (VolumeEntry, error) {
	fields := strings.SplitN(line, " ", 4)
	bad := errs.Newf(errs.AdapterFailed,
		"A file in this app's storage has a name Pando cannot copy to its standby: %q.", line).
		WithRemedy("Rename the file so its name has no line break, or turn this app's standby off with availability.standby set to off.")
	if len(fields) != 4 {
		return VolumeEntry{}, bad
	}
	mode, err1 := strconv.ParseUint(fields[0], 16, 32)
	size, err2 := strconv.ParseInt(fields[1], 10, 64)
	mtime, err3 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || (fields[3] != "." && !safePath(fields[3])) {
		return VolumeEntry{}, bad
	}
	return VolumeEntry{Path: fields[3], Mode: uint32(mode), Size: size, MTime: mtime}, nil
}

// safePath is a path find could have printed inside the volume: ./ and then
// names, none of them empty, . or .. — so no helper given one writes outside
// the copy it is working on.
func safePath(p string) bool {
	rest, ok := strings.CutPrefix(p, "./")
	if !ok || rest == "" {
		return false
	}
	for _, part := range strings.Split(rest, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Format writes the listing as ParseVolumeListing reads it.
func (l VolumeListing) Format() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "T %d\n", l.TakenAt)
	for _, e := range l.Entries {
		fmt.Fprintf(&b, "%x %d %d %s\n", e.Mode, e.Size, e.MTime, e.Path)
	}
	return b.Bytes()
}

// StandbyCopies is what a staging volume holds.
type StandbyCopies struct {
	// Seqs are the finished copies, oldest first.
	Seqs []int
	// Newest is the newest copy's listing; empty when there is none.
	Newest VolumeListing
}

// StandbyVolumeName is the staging volume of one app volume.
func StandbyVolumeName(bundleID, volumeID string) string {
	return "pando-standby-" + bundleID + "-" + volumeID
}

func homeVolumeName(bundleID string) string { return "pando-home-" + bundleID }

func seqName(seq int) string { return fmt.Sprintf("%010d", seq) }

func appMount(bundleID, volumeID string, readOnly bool) mount.Mount {
	return mount.Mount{Type: mount.TypeVolume, Source: volumeName(bundleID, volumeID), Target: snapshotMount, ReadOnly: readOnly}
}

func stagingMount(bundleID, volumeID string, readOnly bool) mount.Mount {
	return mount.Mount{Type: mount.TypeVolume, Source: StandbyVolumeName(bundleID, volumeID), Target: standbyMount, ReadOnly: readOnly}
}

// vanishedOK runs cmd, its output to the script's, and fails only for an
// error other than a file being gone. A live volume's files come and go
// while it is read (R-420): one removed between being found and being read
// is left out, as one the listing shows removed, rather than failing the
// whole ship. Any other error, or a failure that says nothing, fails it with
// what cmd said.
func vanishedOK(cmd string) string {
	return `{ err=$( { ` + cmd + `; } 2>&1 1>&3 3>&-); s=$?; } 3>&1; ` +
		`if [ "$s" -ne 0 ] && printf '%s\n' "$err" | grep -v -e 'No such file or directory' -e 'error exit delayed from previous errors' >&2; then exit "$s"; fi`
}

// listScript prints the time, then every entry under /volume. -xdev keeps it
// in the volume.
var listScript = `cd /volume || exit 1; echo "T $(date +%s)"; ` + vanishedOK(`find . -xdev -exec stat -c '%f %s %Y %n' {} +`)

// readScript archives the paths on stdin that still exist, each on its own:
// a directory's entry carries its owner and mode, not its contents. One gone
// by the time tar reaches it is left out (vanishedOK).
var readScript = `cd /volume || exit 1; ` +
	vanishedOK(`while IFS= read -r p; do if [ -e "$p" ] || [ -L "$p" ]; then printf '%s\n' "$p"; fi; done | tar -cf - --no-recursion -T -`)

// ListVolume lists what is in one of the bundle's volumes, live.
func (a *Adapter) ListVolume(ctx context.Context, bundleID, volumeID string) (VolumeListing, error) {
	var out bytes.Buffer
	if err := a.runHelper(ctx, helperRun{
		Cmd:    []string{"sh", "-c", listScript},
		Mounts: []mount.Mount{appMount(bundleID, volumeID, true)},
		Caps:   snapshotCaps,
		Stdout: &out,
		Verb:   "list",
	}); err != nil {
		return VolumeListing{}, err
	}
	return ParseVolumeListing(out.Bytes())
}

// ReadVolumeFiles streams the named entries of one of the bundle's volumes
// to dst as a tar archive. An entry removed since it was listed is left out.
func (a *Adapter) ReadVolumeFiles(ctx context.Context, bundleID, volumeID string, paths []string, dst io.Writer) error {
	return a.runHelper(ctx, helperRun{
		Cmd:    []string{"sh", "-c", readScript},
		Mounts: []mount.Mount{appMount(bundleID, volumeID, true)},
		Caps:   snapshotCaps,
		Stdin:  strings.NewReader(lines(paths)),
		Stdout: dst,
		Verb:   "copy",
	})
}

func lines(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return strings.Join(paths, "\n") + "\n"
}

// ensureStaging makes the staging volume of one app volume. Making one that
// exists is not an error in Docker.
func (a *Adapter) ensureStaging(ctx context.Context, bundleID, volumeID string) error {
	_, err := a.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   StandbyVolumeName(bundleID, volumeID),
		Labels: map[string]string{labelStandby: bundleID, "io.pando.volume": volumeID},
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not create storage for this app's standby.", err)
	}
	return nil
}

const copiesScript = `S=/standby; [ -d $S/manifests ] || exit 0; ls -1 $S/manifests | sort; latest=$(ls -1 $S/manifests | sort | tail -n 1); [ -z "$latest" ] || { echo; cat "$S/manifests/$latest"; }`

// StandbyCopies reads which copies of one app volume this host holds.
func (a *Adapter) StandbyCopies(ctx context.Context, bundleID, volumeID string) (StandbyCopies, error) {
	if err := a.ensureStaging(ctx, bundleID, volumeID); err != nil {
		return StandbyCopies{}, err
	}
	var out bytes.Buffer
	if err := a.runHelper(ctx, helperRun{
		Cmd:    []string{"sh", "-c", copiesScript},
		Mounts: []mount.Mount{stagingMount(bundleID, volumeID, true)},
		Caps:   snapshotCaps,
		Stdout: &out,
		Verb:   "read the standby of",
	}); err != nil {
		return StandbyCopies{}, err
	}
	return parseCopies(out.Bytes())
}

func parseCopies(b []byte) (StandbyCopies, error) {
	var out StandbyCopies
	head, manifest, _ := bytes.Cut(b, []byte("\n\n"))
	for _, line := range strings.Split(string(head), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			continue // not a copy of Pando's
		}
		out.Seqs = append(out.Seqs, n)
	}
	sort.Ints(out.Seqs)
	if len(out.Seqs) == 0 {
		return out, nil
	}
	newest, err := ParseVolumeListing(manifest)
	if err != nil {
		return out, err
	}
	out.Newest = newest
	return out, nil
}

// prepareScript starts a copy: removes copies a ship did not finish, clones
// the newest with hard links, and removes from the clone every path on stdin.
const prepareScript = `set -e
S=/standby
mkdir -p $S/copies $S/manifests
rm -rf $S/incoming
for d in $S/copies/*; do [ -e "$d" ] || continue; [ -f "$S/manifests/${d##*/}" ] || rm -rf "$d"; done
latest=$(ls -1 $S/manifests | sort | tail -n 1)
if [ -n "$latest" ]; then cp -a -l "$S/copies/$latest" $S/incoming; else mkdir $S/incoming; fi
while IFS= read -r p; do rm -rf "$S/incoming/$p"; done`

// StageStandbyCopy starts a new copy of one app volume from the newest,
// without the paths in remove, and extracts files into it.
func (a *Adapter) StageStandbyCopy(ctx context.Context, bundleID, volumeID string, remove []string, files io.Reader) error {
	for _, p := range remove {
		if !safePath(p) {
			return errs.Newf(errs.AdapterFailed, "Pando will not remove %q from this app's standby: it is not a path inside the copy.", p)
		}
	}
	if err := a.ensureStaging(ctx, bundleID, volumeID); err != nil {
		return err
	}
	if err := a.runHelper(ctx, helperRun{
		Cmd:    []string{"sh", "-c", prepareScript},
		Mounts: []mount.Mount{stagingMount(bundleID, volumeID, false)},
		Caps:   restoreCaps,
		Stdin:  strings.NewReader(lines(remove)),
		Verb:   "prepare the standby of",
	}); err != nil {
		return err
	}
	if files == nil {
		return nil
	}
	return a.runHelper(ctx, helperRun{
		Cmd:    []string{"tar", "-xf", "-", "-C", standbyMount + "/incoming"},
		Mounts: []mount.Mount{stagingMount(bundleID, volumeID, false)},
		Caps:   restoreCaps,
		Stdin:  files,
		Verb:   "copy",
	})
}

const commitScript = `set -e
S=/standby; seq=$1; shift
cat > $S/manifests/.incoming
mv $S/incoming "$S/copies/$seq"
mv $S/manifests/.incoming "$S/manifests/$seq"
for old in "$@"; do rm -rf "$S/copies/$old" "$S/manifests/$old"; done`

// CommitStandbyCopy finishes the copy StageStandbyCopy started, as seq, with
// its listing, and removes the copies in drop.
func (a *Adapter) CommitStandbyCopy(ctx context.Context, bundleID, volumeID string, seq int, listing VolumeListing, drop []int) error {
	cmd := []string{"sh", "-c", commitScript, "sh", seqName(seq)}
	for _, d := range drop {
		cmd = append(cmd, seqName(d))
	}
	return a.runHelper(ctx, helperRun{
		Cmd:    cmd,
		Mounts: []mount.Mount{stagingMount(bundleID, volumeID, false)},
		Caps:   restoreCaps,
		Stdin:  bytes.NewReader(listing.Format()),
		Verb:   "finish the standby copy of",
	})
}

// RestoreStandbyCopy empties the bundle's volume on this host, making it if
// it does not exist, and fills it from copy seq.
func (a *Adapter) RestoreStandbyCopy(ctx context.Context, bundleID, volumeID string, seq int) error {
	if _, err := a.CreateVolume(ctx, api.VolumeRequest{VolumeID: volumeID, BundleID: bundleID}); err != nil {
		return err
	}
	script := "set -e; " + emptyDir(snapshotMount) + " || true; cp -a \"" + standbyMount + "/copies/$1/.\" " + snapshotMount + "/"
	return a.runHelper(ctx, helperRun{
		Cmd:    []string{"sh", "-c", script, "sh", seqName(seq)},
		Mounts: []mount.Mount{stagingMount(bundleID, volumeID, true), appMount(bundleID, volumeID, false)},
		Caps:   restoreCaps,
		Verb:   "start the standby of",
	})
}

// RemoveStandbyStorage removes every staging volume of the bundle on this
// host.
func (a *Adapter) RemoveStandbyStorage(ctx context.Context, bundleID string) error {
	return a.removeLabeled(ctx, labelStandby+"="+bundleID, "Could not remove this app's standby copies.")
}

// MarkHome records that the bundle was promoted to this host at at.
func (a *Adapter) MarkHome(ctx context.Context, bundleID string, at time.Time) error {
	if err := a.ClearHome(ctx, bundleID); err != nil {
		return err
	}
	_, err := a.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   homeVolumeName(bundleID),
		Labels: map[string]string{labelHome: bundleID, labelHomeAt: at.UTC().Format(time.RFC3339Nano)},
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not record this app's new home.", err)
	}
	return nil
}

// ClearHome removes the bundle's home mark on this host, if it has one.
func (a *Adapter) ClearHome(ctx context.Context, bundleID string) error {
	return a.removeLabeled(ctx, labelHome+"="+bundleID, "Could not clear this app's old home.")
}

// HomeMarks is when each bundle was promoted to this host.
func (a *Adapter) HomeMarks(ctx context.Context) (map[string]time.Time, error) {
	list, err := a.cli.VolumeList(ctx, client.VolumeListOptions{Filters: make(client.Filters).Add("label", labelHome)})
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, v := range list.Items {
		at, err := time.Parse(time.RFC3339Nano, v.Labels[labelHomeAt])
		if b := v.Labels[labelHome]; b != "" && err == nil {
			out[b] = at
		}
	}
	return out, nil
}

func (a *Adapter) removeLabeled(ctx context.Context, label, message string) error {
	list, err := a.cli.VolumeList(ctx, client.VolumeListOptions{Filters: make(client.Filters).Add("label", label)})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, message, err)
	}
	for _, v := range list.Items {
		if _, err := a.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
			return errs.Wrap(errs.AdapterFailed, message, err)
		}
	}
	return nil
}
