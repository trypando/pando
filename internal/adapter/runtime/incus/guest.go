package incus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/trypando/pando/internal/errs"
)

// The guest image (notes-incus-runtime.md, The VM): Debian's cloud image with
// Podman serving its Docker-compatible API and socat to bridge to it,
// prepared once per recipe and published in the adapter's project. The alias
// names the recipe's digest, so a changed recipe is a new image rather than
// an old one silently reused.

const (
	guestRemote   = "https://images.linuxcontainers.org"
	guestBase     = "debian/13/cloud"
	guestAliasFmt = "pando-guest-"
)

// guestRecipe is the cloud-init that turns the base image into the guest.
//
// podman-restart brings back, after a reboot, the containers whose restart
// policy says so — which is every workload and the agent. Short image names
// resolve on Docker Hub, as they do for the Docker runtime.
const guestRecipe = `#cloud-config
package_update: true
packages: [podman, socat]
write_files:
  - path: /etc/containers/registries.conf.d/50-pando.conf
    content: |
      unqualified-search-registries = ["docker.io"]
      short-name-mode = "permissive"
runcmd:
  - [systemctl, enable, --now, podman.socket]
  - [systemctl, enable, podman-restart.service]
`

func guestAlias() string {
	sum := sha256.Sum256([]byte(guestBase + "\n" + guestRecipe))
	return guestAliasFmt + hex.EncodeToString(sum[:6])
}

var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// guestImage is the source a VM is created from, preparing the guest image
// first when it does not exist yet.
func (a *Adapter) guestImage(ctx context.Context, pool string) (incusapi.InstanceSource, error) {
	if img := a.config.GuestImage; img != "" {
		if fingerprintRE.MatchString(img) {
			return incusapi.InstanceSource{Type: "image", Fingerprint: img}, nil
		}
		return incusapi.InstanceSource{Type: "image", Alias: img}, nil
	}
	a.guestMu.Lock()
	defer a.guestMu.Unlock()
	alias := guestAlias()
	if a.guestAlias == alias {
		return incusapi.InstanceSource{Type: "image", Alias: alias}, nil
	}
	if _, err := a.api.ImageAlias(ctx, alias); err == nil {
		a.guestAlias = alias
		return incusapi.InstanceSource{Type: "image", Alias: alias}, nil
	} else if !errors.Is(err, errNotFound) {
		return incusapi.InstanceSource{}, errs.Wrap(errs.AdapterUnavailable, "Could not read Incus's images.", err)
	}
	if err := a.prepareGuest(ctx, pool, alias); err != nil {
		return incusapi.InstanceSource{}, err
	}
	a.guestAlias = alias
	return incusapi.InstanceSource{Type: "image", Alias: alias}, nil
}

// prepareGuest builds the guest image: a VM from the base image, provisioned
// by cloud-init, cleaned, stopped and published. A builder left by an attempt
// that was interrupted is removed first.
func (a *Adapter) prepareGuest(ctx context.Context, pool, alias string) error {
	builder := alias + "-build"
	fail := func(msg string, err error) error {
		return errs.Wrap(errs.AdapterFailed, "Pando could not prepare the VM image apps run in: "+msg, err).
			WithRemedy("The VM must reach " + guestRemote + " and Debian's package mirrors. Or set guest_image in " + settingIn + " to an image of your own with Podman serving " + podmanSocket + " and socat.")
	}
	_ = a.stop(ctx, builder)
	if err := a.api.DeleteInstance(ctx, builder); err != nil && !errors.Is(err, errNotFound) {
		return fail("an earlier attempt could not be removed.", err)
	}
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_ = a.stop(cleanup, builder)
		_ = a.api.DeleteInstance(cleanup, builder)
	}()

	err := a.api.CreateInstance(ctx, a.memberTarget(), incusapi.InstancesPost{
		Name: builder,
		Type: incusapi.InstanceTypeVM,
		Source: incusapi.InstanceSource{
			Type: "image", Server: guestRemote, Protocol: "simplestreams", Alias: guestBase,
		},
		InstancePut: incusapi.InstancePut{
			Profiles: []string{},
			Config: map[string]string{
				"limits.cpu":           "2",
				"limits.memory":        "2GiB",
				"security.secureboot":  "false",
				"cloud-init.user-data": guestRecipe,
				keyManaged:             "true",
			},
			Devices: map[string]map[string]string{
				// Sized, because a project that limits disk refuses a root
				// device without a size.
				"root":  {"type": "disk", "path": "/", "pool": pool, "size": "8GiB"},
				nicName: a.nic(),
			},
		},
	})
	if err != nil {
		return fail("the base image could not be started.", err)
	}
	if err := a.api.SetState(ctx, builder, "start", false); err != nil {
		return fail("the base image could not be started.", err)
	}
	if err := a.waitCloudInit(ctx, builder); err != nil {
		return fail("provisioning it did not finish.", err)
	}
	// A published image boots as a new machine: cloud-init and the machine
	// ID start fresh in every VM made from it.
	if code, err := a.api.Exec(ctx, builder, []string{"cloud-init", "clean", "--logs", "--machine-id"}, nil, nil, nil); err != nil || code != 0 {
		return fail("it could not be cleaned for publishing.", err)
	}
	if err := a.stop(ctx, builder); err != nil {
		return err
	}
	if err := a.api.Publish(ctx, builder, alias, map[string]string{
		"description": "Pando guest: " + guestBase + " with Podman",
		"os":          "Debian",
	}); err != nil {
		// Another replica may have published it first.
		if _, aerr := a.api.ImageAlias(ctx, alias); aerr == nil {
			return nil
		}
		return fail("it could not be published.", err)
	}
	return nil
}

// waitCloudInit waits for cloud-init to finish in an instance, through the
// Incus agent, which answers once the guest has booted.
func (a *Adapter) waitCloudInit(ctx context.Context, name string) error {
	var lastErr error
	for {
		var out bytes.Buffer
		code, err := a.api.Exec(ctx, name, []string{"cloud-init", "status", "--wait"}, nil, &out, &out)
		switch {
		case err == nil && code == 0:
			return nil
		case err == nil:
			return errors.New(strings.TrimSpace(out.String()))
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		case <-a.after(3):
		}
	}
}
