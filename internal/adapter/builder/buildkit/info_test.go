package buildkit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR111_PandoSetsUpItsOwnBuildKit asserts R-111's "Pando starts it" as the
// forms see it: the kind is managed, so no form offers to add one or point it
// elsewhere, and the only settings it lists are the advanced cache caps. The
// address, registry and networks belong to the install topology.
func TestR111_PandoSetsUpItsOwnBuildKit(t *testing.T) {
	info := Info()
	require.NoError(t, info.Validate())
	require.True(t, info.Managed)

	keys := map[string]string{}
	for _, f := range info.Fields {
		require.True(t, f.Advanced, "%s is advanced: a managed kind has no basic settings", f.Key)
		require.Equal(t, "int", f.Type, f.Key)
		keys[f.Key] = f.Default
	}
	require.Equal(t, map[string]string{
		"cache_max_bytes":       "10737418240 (10 GiB)",
		"local_cache_max_bytes": "21474836480 (20 GiB)",
	}, keys)
	for _, internal := range []string{"address", "registry_host", "registry_ca_file", "networks"} {
		require.NotContains(t, keys, internal)
	}
}

// TestR224_TheCacheCapsInfoListsAreTheOnesConfigureReads asserts that the
// settings Info offers are read by Configure under the same keys, so a value
// set through the API reaches the caps R-224 depends on.
func TestR224_TheCacheCapsInfoListsAreTheOnesConfigureReads(t *testing.T) {
	t.Setenv("PANDO_BUILDKIT_ADDRESS", "tcp://127.0.0.1:1")
	raw, err := json.Marshal(map[string]int64{"cache_max_bytes": 123, "local_cache_max_bytes": -1})
	require.NoError(t, err)

	a := New()
	require.NoError(t, a.Configure(t.Context(), raw))
	require.Equal(t, int64(123), a.config.CacheMaxBytes)
	require.Equal(t, int64(-1), a.config.LocalCacheMaxBytes)
	require.Equal(t, int64(-1), a.localCacheLimit())
}
