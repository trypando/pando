package buildkit

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const libertyDockerfile = `FROM icr.io/appcafe/open-liberty:kernel-slim-java11-openj9-ubi

ARG VERSION=1.0
LABEL \
  version="$VERSION" \
  summary="The system microservice"

COPY --chown=1001:0 src/main/liberty/config/ /config/
RUN features.sh
COPY --chown=1001:0 target/*.war /config/apps/
RUN configure.sh
`

// TestR094_ADockerfileThatCopiesBuildOutputRunsTheBuildFirst asserts R-094
// tier 2 read with tier 4: a Dockerfile that copies what the repository's own
// build makes — Open Liberty's target/*.war, an Nx workspace's dist/api — is
// built with that build run first, in a stage of its own, and is otherwise
// the author's Dockerfile unchanged (issue #90). A Dockerfile whose sources
// are all in the repository is left alone, and so is one that copies
// something no declared build makes.
func TestR094_ADockerfileThatCopiesBuildOutputRunsTheBuildFirst(t *testing.T) {
	maven := writeTree(t, map[string]string{
		"Dockerfile":                         libertyDockerfile,
		"pom.xml":                            "<project><properties><maven.compiler.release>11</maven.compiler.release></properties><packaging>war</packaging></project>",
		"mvnw":                               "#!/bin/sh\n",
		"src/main/liberty/config/server.xml": "<server/>",
	})
	plan, err := New().PlanPrebuild(context.Background(), view{maven}, "Dockerfile")
	require.NoError(t, err)
	require.Equal(t, ".pando-plan/Dockerfile", plan.Entry)
	body := plan.Files[plan.Entry]
	require.Contains(t, body, "FROM maven:3-eclipse-temurin-11 AS pando-prebuild\nWORKDIR /src\nCOPY . .\n")
	require.Contains(t, body, "./mvnw -B -DskipTests package")
	require.Contains(t, body, `COPY --from=pando-prebuild --chown=1001:0 ["/src/target/*.war","/config/apps/"]`)
	require.Contains(t, body, "COPY --chown=1001:0 src/main/liberty/config/ /config/\n", "a source the repository has is copied as written")
	require.Contains(t, body, "LABEL \\\n  version=\"$VERSION\" \\\n", "the rest of the Dockerfile is the author's, continuations and all")
	require.Less(t, strings.Index(body, "AS pando-prebuild"), strings.Index(body, "FROM icr.io/appcafe"), "the build runs first")
	require.Contains(t, plan.Declared.Why, "Dockerfile copies target/*.war, which the repository does not have until it is built, so Maven runs first")

	nx := writeTree(t, map[string]string{
		"Dockerfile":        "FROM docker.io/node:lts-alpine\nWORKDIR /app\nCOPY dist/api api\nRUN npm --prefix api --omit=dev -f install\nCMD [ \"node\", \"api\" ]\n",
		"package.json":      `{"scripts":{"build":"nx build"}}`,
		"package-lock.json": "{}",
	})
	plan, err = New().PlanPrebuild(context.Background(), view{nx}, "Dockerfile")
	require.NoError(t, err)
	body = plan.Files[plan.Entry]
	require.Contains(t, body, "AS pando-prebuild\nWORKDIR /src\nCOPY . .\nRUN npm ci\nRUN npm run build\n")
	require.Contains(t, body, `COPY --from=pando-prebuild ["/src/dist/api","api"]`)

	whole := writeTree(t, map[string]string{"Dockerfile": "FROM node:22\nCOPY . .\nCOPY package.json /x/\n", "package.json": `{"scripts":{"build":"tsc"}}`})
	plan, err = New().PlanPrebuild(context.Background(), view{whole}, "Dockerfile")
	require.NoError(t, err)
	require.Empty(t, plan.Entry, "every source is in the repository")

	nothing := writeTree(t, map[string]string{"Dockerfile": "FROM alpine\nCOPY bin/server /server\n", "package.json": `{"scripts":{"build":"tsc"}}`})
	plan, err = New().PlanPrebuild(context.Background(), view{nothing}, "Dockerfile")
	require.NoError(t, err)
	require.Empty(t, plan.Entry, "bin/ is not what npm run build writes")

	staged := writeTree(t, map[string]string{"Dockerfile": "FROM node:22 AS build\nRUN make\nFROM alpine\nCOPY --from=build /out/dist /dist\n", "package.json": `{"scripts":{"build":"tsc"}}`})
	plan, err = New().PlanPrebuild(context.Background(), view{staged}, "Dockerfile")
	require.NoError(t, err)
	require.Empty(t, plan.Entry, "a COPY from another stage is not from the repository")
}
