package docker

import (
	"context"
	"encoding/binary"
	"net/netip"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
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
// pool is off, full, or refused.
//
// Serialized, because two deploys choosing at once would both see the same
// free block. Docker still has the last word — a block taken by something
// outside this process is refused as overlapping — so a refusal moves on to the
// next block rather than failing the deploy.
func (a *Adapter) createNetwork(ctx context.Context, name string, bits int, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	pool, ok := a.networkPool()
	if !ok {
		return a.cli.NetworkCreate(ctx, name, opts)
	}

	a.subnetMu.Lock()
	defer a.subnetMu.Unlock()

	used, err := a.usedSubnets(ctx)
	if err != nil {
		return a.cli.NetworkCreate(ctx, name, opts)
	}

	const attempts = 8
	tried := 0
	for block := range freeBlocks(pool, bits, used) {
		withBlock := opts
		withBlock.IPAM = &network.IPAM{Driver: "default", Config: []network.IPAMConfig{{Subnet: block}}}
		created, err := a.cli.NetworkCreate(ctx, name, withBlock)
		if err == nil {
			return created, nil
		}
		if !blockTaken(err) {
			return created, err
		}
		if tried++; tried >= attempts {
			break
		}
	}
	return a.cli.NetworkCreate(ctx, name, opts)
}

// blockTaken reports a refusal that means only "that block is in use", which
// is a reason to try the next one rather than to fail the deploy.
//
// In the engine's words, because nothing else says which refusal it was. Docker
// answers "Pool overlaps with other one on this address space". Podman
// checks the host's routes as well as its own networks and answers "subnet …
// is already used on the host or by another config" — and a block another
// engine on the same machine holds is exactly what it finds there. Reading
// only Docker's wording failed every deploy on such a Podman at the first
// taken block.
func blockTaken(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "overlap") || strings.Contains(msg, "already used on the host")
}

// usedSubnets lists every IPv4 subnet any Docker network on this host holds.
func (a *Adapter) usedSubnets(ctx context.Context) ([]netip.Prefix, error) {
	networks, err := a.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	var used []netip.Prefix
	for _, n := range networks.Items {
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() && c.Subnet.Addr().Is4() {
				used = append(used, c.Subnet.Masked())
			}
		}
	}
	return used, nil
}

// freeBlocks yields the pool's blocks of the given size that overlap nothing
// in use, in order. Blocks of different sizes share the pool: each is aligned
// to its own size, so the overlap check is all that keeps them apart.
func freeBlocks(pool netip.Prefix, bits int, used []netip.Prefix) func(func(netip.Prefix) bool) {
	return func(yield func(netip.Prefix) bool) {
		if bits < pool.Bits() {
			return
		}
		step := uint32(1) << (32 - bits)
		base := pool.Addr().As4()
		start := binary.BigEndian.Uint32(base[:])
		count := uint64(1) << (bits - pool.Bits())
		for i := uint64(0); i < count; i++ {
			var addr [4]byte
			binary.BigEndian.PutUint32(addr[:], start+uint32(i)*step)
			block := netip.PrefixFrom(netip.AddrFrom4(addr), bits)
			if overlapsAny(block, used) {
				continue
			}
			if !yield(block) {
				return
			}
		}
	}
}

func overlapsAny(p netip.Prefix, used []netip.Prefix) bool {
	for _, u := range used {
		if p.Overlaps(u) {
			return true
		}
	}
	return false
}
