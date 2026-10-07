package docker

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func blocks(pool string, bits int, used []string, n int) []string {
	var parsed []netip.Prefix
	for _, u := range used {
		parsed = append(parsed, netip.MustParsePrefix(u))
	}
	var out []string
	for b := range freeBlocks(netip.MustParsePrefix(pool), bits, parsed) {
		out = append(out, b.String())
		if len(out) == n {
			break
		}
	}
	return out
}

// TestR025_AppNetworksAreSmallAndSkipWhatIsTaken asserts R-025.
//
// Docker's default pool held about thirty networks and Pando took one per app,
// so a busy host ran out (issue #55). Each app now takes a /28 from Pando's own
// range, and never one that overlaps a network that already exists.
func TestR025_AppNetworksAreSmallAndSkipWhatIsTaken(t *testing.T) {
	require.Equal(t, []string{"10.213.0.0/28", "10.213.0.16/28", "10.213.0.32/28"},
		blocks("10.213.0.0/16", defaultBlockBits, nil, 3))

	require.Equal(t, []string{"10.213.1.0/28"},
		blocks("10.213.0.0/16", defaultBlockBits, []string{"10.213.0.0/24", "172.17.0.0/16"}, 1),
		"a block anything already holds is skipped")

	require.Len(t, blocks("10.213.0.0/16", defaultBlockBits, nil, 10000), 4096,
		"a /16 holds 4,096 app networks (issue #72), where a /26 each held 1,024")
	require.Len(t, blocks("10.208.0.0/12", defaultBlockBits, nil, 70000), 65536, "a wider pool holds more")
}

// TestR025_BlocksOfDifferentSizesShareThePool asserts that a larger block, a
// bundle's that needed one, and the small outbound blocks never overlap the
// app networks around them.
func TestR025_BlocksOfDifferentSizesShareThePool(t *testing.T) {
	used := []string{"10.213.0.0/28", "10.213.0.16/29"}
	require.Equal(t, []string{"10.213.0.32/27"}, blocks("10.213.0.0/16", 27, used, 1),
		"a /27 is aligned to its size and skips the blocks already out")
	require.Equal(t, []string{"10.213.0.24/29"}, blocks("10.213.0.0/16", outboundBlockBits, used, 1),
		"a /29 fills the gap a /28 cannot")
}

// TestR025_AnAppNetworkHoldsItsWorkloadsAndWhatJoinsIt asserts R-025's private
// network has room for everything that must be on it: the bundle's workloads,
// a Pando replica each, and a restricted bundle's egress gateway.
func TestR025_AnAppNetworkHoldsItsWorkloadsAndWhatJoinsIt(t *testing.T) {
	a := &Adapter{}
	require.Equal(t, 28, a.blockBitsFor(1))
	require.Equal(t, 13, usable(28), "a /28 holds 13 containers")
	require.Equal(t, 28, a.blockBitsFor(5), "five workloads and the reserve fit a /28")
	require.Equal(t, 27, a.blockBitsFor(6), "six do not, and get a /27")
	require.Equal(t, 26, a.blockBitsFor(30))
	require.Equal(t, minBlockBits, a.blockBitsFor(1000), "never larger than a /24")

	a.config.NetworkBlockBits = 26
	require.Equal(t, 26, a.blockBitsFor(1), "the configured size is a floor")
	a.config.NetworkBlockBits = 31
	require.Equal(t, defaultBlockBits, a.blockBits(), "a size that cannot hold a container is ignored")

	// The outbound network holds the gateway and nothing else.
	require.GreaterOrEqual(t, usable(outboundBlockBits), 1)
}

// Another install's app networks are not this one's to join or reclaim. Two
// installs on one Docker host each rejoined the other's apps and removed the
// other's empty networks at startup (issue #55).
func TestR025_AnotherInstallsNetworksAreLeftAlone(t *testing.T) {
	mine := func(bundle string) bool { return bundle == "app_mine" }
	require.True(t, ownedBundle(mine, "app_mine"))
	require.False(t, ownedBundle(mine, "app_theirs"))
	require.False(t, ownedBundle(mine, ""), "a trial's network removes itself")
	require.True(t, ownedBundle(nil, "app_any"), "no predicate owns every bundle, as before")
}

func TestTheNetworkPoolCanBeTurnedOffOrMoved(t *testing.T) {
	a := &Adapter{}
	pool, ok := a.networkPool()
	require.True(t, ok)
	require.Equal(t, defaultNetworkPool, pool.String())

	a.config.NetworkPool = "off"
	_, ok = a.networkPool()
	require.False(t, ok)

	a.config.NetworkPool = "10.50.0.0/20"
	pool, ok = a.networkPool()
	require.True(t, ok)
	require.Equal(t, "10.50.0.0/20", pool.String())

	a.config.NetworkPool = "not a range"
	_, ok = a.networkPool()
	require.False(t, ok, "a range that cannot be read falls back to Docker's")
}
