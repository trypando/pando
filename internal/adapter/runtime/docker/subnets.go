package docker

import (
	"context"
	"net/netip"
	"strings"

	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/dockernet"
)

// defaultNetworkPool is where app networks take their addresses from. [P]
//
// Every app gets a private network (R-025), and Docker's default address pool
// holds about thirty of them — fifteen /16s and sixteen /20s, handed out whole.
// A deleted app's network is reclaimed only at the next restart (see Destroy
// for why Pando cannot detach itself sooner), so after about twenty-five
// deletions every deploy on the host failed with "all predefined address pools
// have been fully subnetted" (issue #55).
//
// A bundle is a handful of containers and needs a few addresses, not 65,536. So
// Pando carves its own networks out of one range, and none of Docker's pool.
// 10.213.0.0/16 because it is an unusual corner of private space; an install
// whose own LAN uses it sets another — a wider one, such as 10.208.0.0/12, for
// more apps — or "off" to go back to Docker's pool.
const defaultNetworkPool = "10.213.0.0/16"

// DefaultNetworkPool is the range app networks take their addresses from when
// none is set. The Incus runtime uses it inside each VM, where this adapter
// makes the networks (notes-incus-runtime.md).
func DefaultNetworkPool() string { return defaultNetworkPool }

// defaultBlockBits is the size of an app network: a /28, 16 addresses, of
// which 13 hold containers once the network, broadcast and bridge addresses
// are taken. [P] (issue #72)
//
// It was a /26, which put a ceiling of 1,024 app networks on the default
// pool — fewer apps than that, since an app with restricted egress used two.
// A /28 gives 4,096. Thirteen is enough for an app's workloads and services
// plus what joins every app network besides them (blockReserve); a bundle
// that needs more gets a larger block when its network is made (blockBitsFor).
const defaultBlockBits = 28

// blockReserve is the addresses an app network keeps beyond the bundle's own
// workloads: one for each Pando replica that joins it (attachProxy, and
// RejoinNetworks on every replica), one for a restricted bundle's egress
// gateway, and room for a workload or two added by a later deploy, since a
// network is not resized once it exists.
const blockReserve = 8

// outboundBlockBits is the size of a restricted bundle's outbound network: a
// /29. Only the bundle's egress gateway is ever on it, so it needs one address
// besides the bridge's, and a full app-sized block would be wasted.
const outboundBlockBits = 29

// minBlockBits is the largest block an install may configure, or a bundle
// grow to: a /24.
const minBlockBits = 24

// blockBits is the configured app network size, or the default.
func (a *Adapter) blockBits() int {
	bits := a.config.NetworkBlockBits
	if bits < minBlockBits || bits > outboundBlockBits {
		return defaultBlockBits
	}
	return bits
}

// blockBitsFor is the block a network for a bundle of n workloads takes: the
// configured size, or the smallest larger one that holds n plus blockReserve.
func (a *Adapter) blockBitsFor(n int) int {
	bits := a.blockBits()
	for bits > minBlockBits && usable(bits) < n+blockReserve {
		bits--
	}
	return bits
}

// usable is how many containers a block of the given size holds: every
// address but the network's, the broadcast and the bridge's own.
func usable(bits int) int { return (1 << (32 - bits)) - 3 }

// networkPool returns the range app networks are allocated from, or false when
// allocation is left to Docker.
func (a *Adapter) networkPool() (netip.Prefix, bool) {
	raw := strings.TrimSpace(a.config.NetworkPool)
	switch raw {
	case "off", "docker":
		return netip.Prefix{}, false
	case "":
		raw = defaultNetworkPool
	}
	pool, err := netip.ParsePrefix(raw)
	if err != nil || !pool.Addr().Is4() || pool.Bits() > minBlockBits {
		return netip.Prefix{}, false
	}
	return pool.Masked(), true
}

// createNetwork creates a bridge network with an address block of the given
// size from Pando's pool, falling back to Docker's own allocation when the
// pool is off, full, or refused (dockernet.Create).
//
// Serialized, because two deploys choosing at once would both see the same
// free block.
func (a *Adapter) createNetwork(ctx context.Context, name string, bits int, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	pool, ok := a.networkPool()
	if !ok {
		return a.cli.NetworkCreate(ctx, name, opts)
	}
	a.subnetMu.Lock()
	defer a.subnetMu.Unlock()
	return dockernet.Create(ctx, a.cli, name, pool, bits, opts)
}

// freeBlocks is dockernet's, which the Docker runtime and the BuildKit builder
// share.
var freeBlocks = dockernet.FreeBlocks
