package detect

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR097_AStandInIsTheKindOfDatabaseTheSourceSays asserts R-097: a Rails app
// on mysql2 reads DATABASE_URL as a MySQL, so its stand-in is one, and the
// rerun stops on the next missing variable rather than on the adapter.
func TestR097_AStandInIsTheKindOfDatabaseTheSourceSays(t *testing.T) {
	v := standIn(requiredVar{Key: "DATABASE_URL", Type: spec.SlotMySQL})
	require.True(t, strings.HasPrefix(v, "mysql://"), v)
	require.Contains(t, v, ".invalid")
	require.True(t, strings.HasPrefix(standIn(requiredVar{Key: "DATABASE_URL"}), "postgresql://"),
		"with nothing said, the name decides")
}
