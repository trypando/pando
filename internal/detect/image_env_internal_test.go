package detect

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
)

// TestR130_AConnectionTheImageIsConfiguredForIsATypedSlot asserts R-130 and
// R-107. mongo-express's image sets ME_CONFIG_MONGODB_URL=mongodb://mongo:27017,
// naming the variable and the kind of database: a mention, so a MongoDB slot
// Pando can provision. Optional until the trial run stops for want of it; a
// path that is not a URL is not a connection.
func TestR130_AConnectionTheImageIsConfiguredForIsATypedSlot(t *testing.T) {
	draft := Draft{Workloads: []spec.Workload{{Name: "web", Image: "mongo-express", Primary: true, Exposed: true}}}
	draft, _, evidence := applyImageConfig(draft, nil, &oci.Config{Env: []string{
		"PATH=/usr/local/bin:/usr/bin",
		"ME_CONFIG_MONGODB_URL=mongodb://admin:hunter2@mongo:27017",
		"DB_URL=/appdata/db/db.sqlite",
	}})
	require.Len(t, draft.Slots, 1)
	slot := draft.Slots[0]
	require.Equal(t, "ME_CONFIG_MONGODB_URL", slot.Key)
	require.Equal(t, spec.SlotMongoDB, slot.Type)
	require.False(t, slot.Required)
	require.NotContains(t, slot.Evidence[0], "hunter2", "a password in the image is not repeated")
	require.NotEmpty(t, evidence)

	draft, _ = ApplyTrial(draft, nil, Trial{Ran: true, Started: true, Crashed: true,
		Log: "MongoServerSelectionError: getaddrinfo ENOTFOUND mongo\n"})
	require.True(t, draft.Slots[0].Required, "the app stopped trying to reach it")
}
