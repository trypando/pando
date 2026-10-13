package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR131_AProvisionedPostgresIsSpelledTheWayTheAppSpellsIt asserts R-131: the
// slot records the scheme the app's own configuration wrote, so the URL Pando
// fills it with is one the app's driver accepts. readflow reads only
// postgres:// ("unsupported database: postgresql"), SQLAlchemy 2 only
// postgresql:// (issue #90).
func TestR131_AProvisionedPostgresIsSpelledTheWayTheAppSpellsIt(t *testing.T) {
	compose := func(url string) string {
		return "services:\n" +
			"  web:\n    image: ncarlier/readflow:edge\n    depends_on: [db]\n" +
			"    environment:\n      - READFLOW_DATABASE_URI=" + url + "\n" +
			"  db:\n    image: postgres:17\n"
	}
	for url, want := range map[string]string{
		// readflow's own line, interpolation and sslmode and all.
		"postgres://${POSTGRES_USER:-postgres}:${POSTGRES_PASSWORD:-secret}@db/${POSTGRES_DB:-readflow}?sslmode=disable": "postgres",
		"postgresql://hello_fastapi:hello_fastapi@db/hello_fastapi_dev":                                                  "postgresql",
		"POSTGRES://db/app": "postgres",
	} {
		result, err := auction().Run(context.Background(), memSource{"docker-compose.yml": compose(url)})
		require.NoError(t, err)
		slots := result.Winner.Draft.Slots
		require.Len(t, slots, 1, url)
		require.Equal(t, spec.SlotPostgres, slots[0].Type)
		require.Equal(t, want, slots[0].Scheme, url)
	}

	// From a sample in .env.example the same way, and only for Postgres: a
	// Redis has one spelling and nothing to record.
	result, err := auction().Run(context.Background(), memSource{
		"package.json": `{"name":"app"}`,
		".env.example": "DATABASE_URL=postgres://localhost/app\nREDIS_URL=redis://localhost:6379\nOTHER_DATABASE_URL=\n",
	})
	require.NoError(t, err)
	byKey := map[string]spec.Slot{}
	for _, s := range result.Winner.Draft.Slots {
		byKey[s.Key] = s
	}
	require.Equal(t, "postgres", byKey["DATABASE_URL"].Scheme)
	require.Empty(t, byKey["REDIS_URL"].Scheme)
	require.Empty(t, byKey["OTHER_DATABASE_URL"].Scheme, "no sample, so Pando's default")
}
