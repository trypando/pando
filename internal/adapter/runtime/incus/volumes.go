package incus

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	incusapi "github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Volumes (notes-incus-runtime.md, Volumes): each Pando volume is its own
// Incus custom filesystem volume, attached to the app's VM at
// volumeRoot/<volume ID>. They outlive the VM (R-204), and backups read and
// write them over Incus's SFTP endpoint for custom volumes, so the VM need not
// run. The archive is a tar with relative paths, as the Docker adapter's, so
// a backup taken on one runtime restores on the other. Pando never reads the
// contents (R-206).

// createVolume makes a custom volume for a Pando volume, or finds it.
func (a *Adapter) createVolume(ctx context.Context, pool, bundleID, volumeID string, sizeBytes int64) (string, error) {
	name := volumeName(volumeID)
	have, err := a.api.Volume(ctx, pool, name)
	if err == nil {
		if have.Config[keyVolume] != volumeID {
			return "", errs.Newf(errs.AdapterFailed, "Incus already has a volume named %s in the pool %s that is not this app's.", name, pool).
				WithRemedy("Rename or remove that volume; Pando names its volumes pando-<volume ID>.")
		}
		return name, nil
	}
	if !errors.Is(err, errNotFound) {
		return "", errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage from Incus.", err)
	}
	err = a.api.CreateVolume(ctx, a.memberTarget(), pool, incusapi.StorageVolumesPost{
		Name:        name,
		Type:        "custom",
		ContentType: "filesystem",
		StorageVolumePut: incusapi.StorageVolumePut{
			Description: "Pando volume " + volumeID + " of " + bundleID,
			Config: map[string]string{
				keyBundle: bundleID, keyVolume: volumeID, keyManaged: "true",
				"size": strconv.FormatInt(max(sizeBytes, a.config.VolumeSizeBytes), 10) + "B",
			},
		},
	})
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "Incus could not create storage for the app.", err)
	}
	return name, nil
}

func (a *Adapter) CreateVolume(ctx context.Context, req api.VolumeRequest) (api.VolumeHandle, error) {
	pool, err := a.storagePool(ctx)
	if err != nil {
		return api.VolumeHandle{}, err
	}
	name, err := a.createVolume(ctx, pool, req.BundleID, req.VolumeID, req.SizeBytes)
	if err != nil {
		return api.VolumeHandle{}, err
	}
	return api.VolumeHandle{VolumeID: req.VolumeID, Handle: name}, nil
}

// DestroyVolume deletes a volume, detaching it from a VM first: the VM
// stays, without that storage, until its next Apply.
func (a *Adapter) DestroyVolume(ctx context.Context, h api.VolumeHandle) error {
	pool, err := a.storagePool(ctx)
	if err != nil {
		return err
	}
	name := handleName(h)
	vol, err := a.api.Volume(ctx, pool, name)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the storage from Incus.", err)
	}
	for _, user := range vol.UsedBy {
		instance, ok := strings.CutPrefix(user, "/1.0/instances/")
		if !ok {
			continue
		}
		instance, _, _ = strings.Cut(instance, "?")
		if err := a.detach(ctx, instance, name); err != nil {
			return err
		}
	}
	if err := a.api.DeleteVolume(ctx, pool, name); err != nil && !errors.Is(err, errNotFound) {
		return errs.Wrap(errs.AdapterFailed, "Incus could not remove the storage.", err)
	}
	return nil
}

// detach removes the disk devices of an instance that use a volume.
func (a *Adapter) detach(ctx context.Context, instance, volume string) error {
	inst, etag, err := a.api.Instance(ctx, instance)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's VM from Incus.", err)
	}
	put := inst.Writable()
	changed := false
	for dev, d := range put.Devices {
		if d["type"] == "disk" && d["source"] == volume {
			delete(put.Devices, dev)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := a.api.UpdateInstance(ctx, instance, put, etag); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Incus could not detach the storage from the app's VM.", err)
	}
	return nil
}

// destroyBundleVolumes deletes every volume labeled with the bundle.
func (a *Adapter) destroyBundleVolumes(ctx context.Context, bundleID string) error {
	pool, err := a.storagePool(ctx)
	if err != nil {
		return err
	}
	vols, err := a.api.Volumes(ctx, pool)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not list the app's storage in Incus.", err)
	}
	for _, v := range vols {
		if v.Type != "custom" || v.Config[keyBundle] != bundleID {
			continue
		}
		if err := a.DestroyVolume(ctx, api.VolumeHandle{VolumeID: v.Config[keyVolume], Handle: v.Name}); err != nil {
			return err
		}
	}
	return nil
}

func handleName(h api.VolumeHandle) string {
	if h.Handle != "" {
		return h.Handle
	}
	return volumeName(h.VolumeID)
}

// files opens a volume's contents.
func (a *Adapter) files(ctx context.Context, h api.VolumeHandle) (*sftp.Client, error) {
	if h.Handle == "" && h.VolumeID == "" {
		return nil, errs.New(errs.ValidInvalid, "That volume has no handle.")
	}
	pool, err := a.storagePool(ctx)
	if err != nil {
		return nil, err
	}
	c, err := a.api.VolumeFiles(ctx, pool, handleName(h))
	if errors.Is(err, errNotFound) {
		return nil, errs.Newf(errs.NotFound, "Incus has no volume %s.", handleName(h))
	}
	if err != nil {
		return nil, errs.Wrap(errs.AdapterUnavailable, "Could not open the storage in Incus.", err)
	}
	return c, nil
}

// SnapshotVolume streams the volume's contents to dst as a tar archive.
func (a *Adapter) SnapshotVolume(ctx context.Context, h api.VolumeHandle, dst io.Writer) error {
	c, err := a.files(ctx, h)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	tw := tar.NewWriter(dst)
	if err := writeTree(ctx, c, tw, "/", "."); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not read the storage to back it up.", err)
	}
	if err := tw.Close(); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not write the backup.", err)
	}
	return nil
}

// writeTree writes dir and everything under it, sorted so the same volume
// archives the same way.
func writeTree(ctx context.Context, c *sftp.Client, tw *tar.Writer, dir, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := c.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, fi := range entries {
		full := path.Join(dir, fi.Name())
		name := rel + "/" + fi.Name()
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if st, ok := fi.Sys().(*sftp.FileStat); ok {
			hdr.Uid, hdr.Gid = int(st.UID), int(st.GID)
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := c.ReadLink(full)
			if err != nil {
				return err
			}
			hdr.Linkname = target
		case fi.IsDir():
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		switch {
		case fi.Mode().IsRegular():
			f, err := c.Open(full)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return err
			}
		case fi.IsDir():
			if err := writeTree(ctx, c, tw, full, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// RestoreVolume replaces the volume's contents with an archive.
//
// Destructive by design, as on Docker: the volume is emptied first, because a
// restore merged into what was there matches neither the backup nor the
// previous state.
func (a *Adapter) RestoreVolume(ctx context.Context, h api.VolumeHandle, src io.Reader) error {
	if h.Handle == "" && h.VolumeID == "" {
		return errs.New(errs.ValidInvalid, "That volume has no handle to restore into.")
	}
	c, err := a.files(ctx, h)
	var e *errs.Error
	if errors.As(err, &e) && e.Code == errs.NotFound {
		// Restoring into a volume that does not exist yet creates it, as an
		// edge's does (EdgeVolumes); the bundle is read from the handle's
		// owner when it comes back.
		pool, perr := a.storagePool(ctx)
		if perr != nil {
			return perr
		}
		if _, cerr := a.createVolume(ctx, pool, "", h.VolumeID, 0); cerr != nil {
			return cerr
		}
		c, err = a.files(ctx, h)
	}
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := empty(c, "/"); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not empty the storage before restoring it.", err)
	}
	if err := extract(ctx, c, tar.NewReader(src)); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not restore the storage from the backup.", err)
	}
	return nil
}

func empty(c *sftp.Client, dir string) error {
	entries, err := c.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, fi := range entries {
		full := path.Join(dir, fi.Name())
		if fi.IsDir() {
			if err := empty(c, full); err != nil {
				return err
			}
			if err := c.RemoveDirectory(full); err != nil {
				return err
			}
			continue
		}
		if err := c.Remove(full); err != nil {
			return err
		}
	}
	return nil
}

// extract writes an archive into the volume. The archive is data from an app
// Pando does not trust: a name is cleaned against the volume's root, so ..
// cannot leave it, and an entry beneath a symlink the archive itself made is
// refused, so a link cannot be used to write somewhere else.
func extract(ctx context.Context, c *sftp.Client, tr *tar.Reader) error {
	links := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + hdr.Name)
		if name == "/" {
			continue
		}
		for dir := path.Dir(name); dir != "/"; dir = path.Dir(dir) {
			if links[dir] {
				return fmt.Errorf("the archive writes %q through a link it made", hdr.Name)
			}
		}
		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := c.MkdirAll(name); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := c.MkdirAll(path.Dir(name)); err != nil {
				return err
			}
			f, err := c.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := c.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			links[name] = true
			continue
		default:
			// Devices, FIFOs and hard links are not app data a volume
			// should hold; skipped rather than recreated.
			continue
		}
		if err := c.Chmod(name, mode); err != nil {
			return err
		}
		_ = c.Chown(name, hdr.Uid, hdr.Gid)
	}
}
