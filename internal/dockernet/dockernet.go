// Package dockernet carves Docker bridge networks out of an address range of
// Pando's own rather than Docker's default pool, which holds about thirty
// networks (issue #55). The Docker runtime adapter makes app networks with it,
// and the BuildKit builder each app's build network.
//
// It imports nothing from core.
package dockernet

import (
	"context"
	"encoding/binary"
	"net/netip"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// DefaultPool is the range Pando's networks take their addresses from when
// none is set: app networks (/28s) and build networks (/29s) alike, kept apart
// by the overlap check in Create. An unusual corner of private space; an
// install whose own LAN uses it sets another.
const DefaultPool = "10.213.0.0/16"

// API is the part of the Docker client a network is made with.
type API interface {
	NetworkList(ctx context.Context, opts client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkCreate(ctx context.Context, name string, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error)
}

// attempts is how many free blocks Create tries before it leaves the choice to
// Docker.
const attempts = 8

// Create creates a bridge network with an address block of bits from pool,
// falling back to Docker's own allocation when the network list cannot be
// read or every block tried was refused.
//
// The caller serializes calls that share a pool: two creates choosing at once
// would both see the same free block. Docker still has the last word — a block
// taken by something outside the process is refused as overlapping — so a
// refusal moves on to the next block rather than failing.
func Create(ctx context.Context, cli API, name string, pool netip.Prefix, bits int, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	used, err := UsedSubnets(ctx, cli)
	if err != nil {
		return cli.NetworkCreate(ctx, name, opts)
	}
	tried := 0
	for block := range FreeBlocks(pool, bits, used) {
		withBlock := opts
		withBlock.IPAM = &network.IPAM{Driver: "default", Config: []network.IPAMConfig{{Subnet: block}}}
		created, err := cli.NetworkCreate(ctx, name, withBlock)
		if err == nil {
			return created, nil
		}
		if !BlockTaken(err) {
			return created, err
		}
		if tried++; tried >= attempts {
			break
		}
	}
	return cli.NetworkCreate(ctx, name, opts)
}

// BlockTaken reports a refusal that means only "that block is in use", which
// is a reason to try the next one rather than to fail.
//
// In the engine's words, because nothing else says which refusal it was. Docker
// answers "Pool overlaps with other one on this address space". Podman
// checks the host's routes as well as its own networks and answers "subnet …
// is already used on the host or by another config" — and a block another
// engine on the same machine holds is exactly what it finds there. Reading
// only Docker's wording failed every deploy on such a Podman at the first
// taken block.
func BlockTaken(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "overlap") || strings.Contains(msg, "already used on the host")
}

// UsedSubnets lists every IPv4 subnet any Docker network on this host holds.
func UsedSubnets(ctx context.Context, cli API) ([]netip.Prefix, error) {
	networks, err := cli.NetworkList(ctx, client.NetworkListOptions{})
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

// FreeBlocks yields the pool's blocks of the given size that overlap nothing
// in use, in order. Blocks of different sizes share the pool: each is aligned
// to its own size, so the overlap check is all that keeps them apart.
func FreeBlocks(pool netip.Prefix, bits int, used []netip.Prefix) func(func(netip.Prefix) bool) {
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
