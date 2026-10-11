package buildkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// .NET apps are planned by Pando rather than by nixpacks.
//
// nixpacks 1.41 installs the .NET 6 SDK, which cannot build a project targeting
// .NET 8 ("NETSDK1045: The current .NET SDK does not support targeting .NET
// 8.0", issue #55). The project file names its target framework, and
// Microsoft's SDK and ASP.NET images exist for each.

type dotnetBuild struct {
	Project  string // path relative to the context, e.g. "Api.csproj"
	Assembly string
	Version  string // "8.0"
}

var (
	targetFramework = regexp.MustCompile(`<TargetFramework>\s*net(\d+\.\d+)\s*</TargetFramework>`)
	assemblyName    = regexp.MustCompile(`<AssemblyName>\s*([A-Za-z0-9_.\-]+)\s*</AssemblyName>`)
	safeDotnetName  = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
)

// readDotnetBuild reports a single .NET project at the top of the repository.
// More than one is a solution whose entry point is not something to guess.
func readDotnetBuild(contextDir string) (dotnetBuild, bool) {
	var projects []string
	for _, pattern := range []string{"*.csproj", "*.fsproj"} {
		found, _ := filepath.Glob(filepath.Join(contextDir, pattern))
		projects = append(projects, found...)
	}
	if len(projects) != 1 {
		return dotnetBuild{}, false
	}
	body, err := os.ReadFile(projects[0])
	if err != nil {
		return dotnetBuild{}, false
	}
	m := targetFramework.FindSubmatch(body)
	if m == nil {
		return dotnetBuild{}, false
	}
	base := filepath.Base(projects[0])
	assembly := strings.TrimSuffix(strings.TrimSuffix(base, ".csproj"), ".fsproj")
	if a := assemblyName.FindSubmatch(body); a != nil {
		assembly = string(a[1])
	}
	if !safeDotnetName.MatchString(assembly) || !safeDotnetName.MatchString(base) {
		return dotnetBuild{}, false
	}
	return dotnetBuild{Project: base, Assembly: assembly, Version: string(m[1])}, true
}

// committedProgramImage runs a program committed in the repository. [P] Debian
// slim, because a binary built for Linux most often links against glibc.
const committedProgramImage = "debian:bookworm-slim"

// committedProgram returns the start command when its program is a file
// committed in the repository, like `./server`, and "" otherwise.
func committedProgram(contextDir, start string) string {
	fields := strings.Fields(start)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "./") {
		return ""
	}
	program := filepath.Clean(fields[0])
	if strings.HasPrefix(program, "..") || filepath.IsAbs(program) {
		return ""
	}
	info, err := os.Stat(filepath.Join(contextDir, program))
	if err != nil || info.IsDir() {
		return ""
	}
	return start
}

// writeCommittedProgramPlan runs a committed program as it is. The command goes
// to a shell, as every plan's does, so a Procfile's $PORT is expanded.
func writeCommittedProgramPlan(contextDir, command string) (string, error) {
	quoted, err := json.Marshal(command)
	if err != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	program := strings.Fields(command)[0]
	content := fmt.Sprintf(`# A program committed in the repository, run as it is.
FROM %s
WORKDIR /app
COPY . /app
RUN chmod +x %s
ENTRYPOINT ["/bin/sh", "-c"]
CMD [%s]
`, committedProgramImage, program, quoted)
	return writePlan(contextDir, content)
}

// writePlan writes a Dockerfile Pando made into .pando-plan/, where detection
// collects a plan and the build replays it (R-020).
func writePlan(contextDir, content string) (string, error) {
	name := filepath.Join(planDir, "Dockerfile")
	full := filepath.Join(contextDir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	// G306: a generated build input in the build context, read by the rootless
	// builder as a different user. It holds no secret.
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	return name, nil
}

// writeDotnetPlan writes the plan into .pando-plan/, where detection collects a
// plan and the build replays it (R-020). The ASP.NET image listens on 8080,
// which the deploy's port check confirms against the built image (R-097).
func writeDotnetPlan(contextDir string, b dotnetBuild) (string, error) {
	content := fmt.Sprintf(`# A .NET %[1]s project, built with the SDK it targets.
FROM mcr.microsoft.com/dotnet/sdk:%[1]s AS build
WORKDIR /src
COPY . .
RUN dotnet publish %[2]s -c Release -o /out

FROM mcr.microsoft.com/dotnet/aspnet:%[1]s
WORKDIR /app
COPY --from=build /out .
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec dotnet %[3]s.dll"]
`, b.Version, b.Project, b.Assembly)

	name := filepath.Join(planDir, "Dockerfile")
	full := filepath.Join(contextDir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	// G306: a generated build input in the build context, read by the rootless
	// builder as a different user. It holds no secret.
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	return name, nil
}
