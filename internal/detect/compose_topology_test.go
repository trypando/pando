package detect_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// TestR096_AServiceScaledToZeroIsNotStarted asserts R-096: `docker compose up`
// starts what the file starts, and a service at `deploy.replicas: 0` is a tool
// run by hand. Vapor's templates declare their migrate and revert services so.
func TestR096_AServiceScaledToZeroIsNotStarted(t *testing.T) {
	const file = `
services:
  app:
    image: app:latest
    build: .
    ports: ["8080:8080"]
  migrate:
    image: app:latest
    build: .
    command: ["migrate", "--yes"]
    deploy:
      replicas: 0
`
	draft, err := detect.ImportCompose(memSource{"compose.yaml": file, "Dockerfile": "FROM swift\n"}, "compose.yaml")
	require.NoError(t, err)
	byName := workloadsByName(draft)
	require.Len(t, byName, 1)
	require.Contains(t, byName, "app")
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "replicas: 0"))
}

// TestR096_AServiceRunningTheImageAnotherBuildsRunsThatBuild asserts R-096.
// `image: app` naming the image the app service builds is that image under
// compose, not docker.io/library/app.
func TestR096_AServiceRunningTheImageAnotherBuildsRunsThatBuild(t *testing.T) {
	const file = `
services:
  app:
    build:
      context: .
      dockerfile: Dockerfile
    image: app
  migration:
    image: app
    command: ["python", "manage.py", "migrate"]
`
	draft, err := detect.ImportCompose(memSource{"compose.yaml": file, "Dockerfile": "FROM python\n"}, "compose.yaml")
	require.NoError(t, err)
	byName := workloadsByName(draft)
	require.NotNil(t, byName["migration"].Build, "the migration service is built, not pulled")
	require.Equal(t, byName["app"].Build, byName["migration"].Build,
		"one build, which the deploy builds once for both")
}
