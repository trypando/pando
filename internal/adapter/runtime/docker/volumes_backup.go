package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Volume snapshot and restore for R-212's "app volumes".
//
// A Docker volume's contents are not readable from outside a container, so both
// directions run a throwaway helper container with the volume mounted and tar
// streaming through its stdio. That is the only mechanism Docker offers, and it
// is what every backup tool for Docker does.
//
// The helper mounts the volume at /volume and nothing else. It gets no network,
// no other mounts, and drops every capability: it is handling data Pando has
// not inspected, from an app Pando does not trust, and a helper with more reach
// than tar needs is a way for that data to become code.

// helperImage is the image the snapshot and restore containers run.
//
// BusyBox because it is small and has tar. Pinned by tag rather than digest for
// now, and that is a real weakness worth naming: an image pulled at restore
// time is an image whose contents can change between the backup and the
// disaster. A digest belongs here once there is a way to update it.
const helperImage = "busybox:stable"

// snapshotMount is where the volume appears inside the helper.
const snapshotMount = "/volume"

// SnapshotVolume streams the volume's contents to dst as a tar archive.
func (a *Adapter) SnapshotVolume(ctx context.Context, h api.VolumeHandle, dst io.Writer) error {
	if h.Handle == "" {
		return errs.New(errs.ValidInvalid, "That volume has no handle to back up.")
	}
	if err := a.ensureImage(ctx, helperImage, imageClaim{}); err != nil {
		return err
	}

	// Reading, so the mount is read-only. A tar that can write to the volume it
	// is reading is a tar that can corrupt the thing being backed up.
	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: helperImage,
			// `.` rather than `/volume` so paths in the archive are relative,
			// which is what lets restore extract into a different volume.
			Cmd:             []string{"tar", "-cf", "-", "-C", snapshotMount, "."},
			AttachStdout:    true,
			AttachStderr:    true,
			NetworkDisabled: true,
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: h.Handle, Target: snapshotMount, ReadOnly: true,
			}},
			AutoRemove:  false, // removed below, after the exit code is read
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not start the storage backup.", err)
	}
	defer a.removeQuietly(ctx, created.ID)

	return a.streamHelper(ctx, created.ID, dst, nil, "back up")
}

// RestoreVolume extracts a tar archive produced by SnapshotVolume into the
// volume.
//
// Destructive by design: the volume is emptied first. A restore that merged
// into whatever was already there would produce a volume that matches neither
// the backup nor the previous state, which is the worst of the three outcomes.
func (a *Adapter) RestoreVolume(ctx context.Context, h api.VolumeHandle, src io.Reader) error {
	if h.Handle == "" {
		return errs.New(errs.ValidInvalid, "That volume has no handle to restore into.")
	}
	if err := a.ensureImage(ctx, helperImage, imageClaim{}); err != nil {
		return err
	}

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: helperImage,
			// Empty, then extract. Both in one shell so the window where the
			// volume is empty is as short as possible and never spans a
			// container start.
			Cmd: []string{"sh", "-c",
				fmt.Sprintf("rm -rf %s/..?* %s/.[!.]* %s/* 2>/dev/null; exec tar -xf - -C %s",
					snapshotMount, snapshotMount, snapshotMount, snapshotMount)},
			AttachStdin:     true,
			AttachStdout:    true,
			AttachStderr:    true,
			OpenStdin:       true,
			StdinOnce:       true,
			NetworkDisabled: true,
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: h.Handle, Target: snapshotMount,
			}},
			AutoRemove:  false,
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not start the storage restore.", err)
	}
	defer a.removeQuietly(ctx, created.ID)

	return a.streamHelper(ctx, created.ID, io.Discard, src, "restore")
}

// streamHelper attaches, starts, pumps, and reports the helper's exit code.
//
// The exit code is the point. tar failing halfway produces a short archive and
// a non-zero status, and without checking it the caller records a backup that
// contains part of a volume — which verifies against its own manifest perfectly,
// because the manifest was written from the same short stream.
func (a *Adapter) streamHelper(ctx context.Context, id string, stdout io.Writer, stdin io.Reader, verb string) error {
	attached, err := a.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true, Stdin: stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not %s this app's storage.", verb), err)
	}
	defer attached.Close()

	if _, err := a.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not %s this app's storage.", verb), err)
	}

	copyDone := make(chan error, 1)
	go func() {
		if stdin != nil {
			_, err := io.Copy(attached.Conn, stdin)
			_ = attached.CloseWrite()
			if err != nil {
				copyDone <- err
				return
			}
		}
		// The attach stream is multiplexed because the helper has no TTY, so it
		// must be demultiplexed: stdout is the tar archive and stderr is
		// diagnostic, and reading them as one stream corrupts the archive with
		// tar's own warnings. Only the exit code decides success.
		_, err := stdcopy.StdCopy(stdout, io.Discard, attached.Reader)
		copyDone <- err
	}()

	wait := a.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})

	if err := <-copyDone; err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not %s this app's storage.", verb), err)
	}

	select {
	case err := <-wait.Error:
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not %s this app's storage.", verb), err)
	case status := <-wait.Result:
		if status.StatusCode != 0 {
			return errs.Newf(errs.AdapterFailed,
				"Pando could not %s this app's storage: the helper exited with status %d.",
				verb, status.StatusCode)
		}
		return nil
	case <-ctx.Done():
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not %s this app's storage.", verb), ctx.Err())
	}
}

// removeQuietly deletes the helper container.
//
// The context has cancellation stripped, not replaced: a canceled backup must
// still clean up its helper, but the request's values and deadline-free
// lifetime are worth keeping so the removal is traceable to the backup that
// created it. Failures are ignored — a leaked helper is untidy, and reporting
// it over the real error would bury the reason the backup failed.
func (a *Adapter) removeQuietly(ctx context.Context, id string) {
	_, _ = a.cli.ContainerRemove(context.WithoutCancel(ctx), id,
		client.ContainerRemoveOptions{Force: true})
}
