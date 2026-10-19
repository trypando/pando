//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/migrations"
)

// A Docker services adapter stored with its image overrides in the one
// `images` map has them moved into the per-type settings a form shows, and a
// setting already there is kept: the next save from the console keeps them.
func TestStoredServiceImageOverridesBecomeSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)
	require.NoError(t, state.Migrate(ctx, ownerURL))

	conn, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	_, err = conn.Exec(ctx, `INSERT INTO adapter_configs (id, category, kind, name, config)
		VALUES ('svcs_docker', 'services', 'docker', 'Docker',
		        '{"images": {"postgres": "postgres:16-alpine", "redis": "redis:6"}, "redis_image": "redis:7.4"}')
		ON CONFLICT (id) DO UPDATE SET config = EXCLUDED.config`)
	require.NoError(t, err)

	up, err := migrations.FS.ReadFile("000076_services_image_settings.up.sql")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, string(up))
	require.NoError(t, err)

	var config map[string]any
	require.NoError(t, conn.QueryRow(ctx, `SELECT config FROM adapter_configs WHERE id = 'svcs_docker'`).Scan(&config))
	require.Equal(t, map[string]any{"postgres_image": "postgres:16-alpine", "redis_image": "redis:7.4"}, config)
}
