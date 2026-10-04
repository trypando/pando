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
// Pando carves its own networks out of one range, 64 addresses each: 1,024 app
// networks, and none of Docker's pool. 10.213.0.0/16 because it is an unusual
// corner of private space; an install whose own LAN uses it sets another, or
// "off" to go back to Docker's pool.
const defaultNetworkPool = "10.213.0.0/16"

// subnetBits is the size of each app network: a /26, 64 addresses.
const subnetBits = 26

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
	if err != nil || !pool.Addr().Is4() || pool.Bits() > subnetBits {
		return netip.Prefix{}, false
	}
	return pool.Masked(), true
}

// createNetwork creates a bridge network with an address block from Pando's
// pool, falling back to Docker's own allocation when the pool is off, full, or
// refused.
//
// Serialized, because two deploys choosing at once would both see the same
// free block. Docker still has the last word — a block taken by something
// outside this process is refused as overlapping — so a refusal moves on to the
// next block rather than failing the deploy.
func (a *Adapter) createNetwork(ctx context.Context, name string, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
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
	for block := range freeBlocks(pool, used) {
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

// freeBlocks yields the pool's /26 blocks that overlap nothing in use, in
// order.
func freeBlocks(pool netip.Prefix, used []netip.Prefix) func(func(netip.Prefix) bool) {
	return func(yield func(netip.Prefix) bool) {
		step := uint32(1) << (32 - subnetBits)
		base := pool.Addr().As4()
		start := binary.BigEndian.Uint32(base[:])
		count := uint32(1) << (subnetBits - pool.Bits())
		for i := uint32(0); i < count; i++ {
			var addr [4]byte
			binary.BigEndian.PutUint32(addr[:], start+i*step)
			block := netip.PrefixFrom(netip.AddrFrom4(addr), subnetBits)
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
