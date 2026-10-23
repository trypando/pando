package buildkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// webSDK is the project SDK of an ASP.NET Core app, the one kind of .NET
// project that serves HTTP. A Blazor WebAssembly client has its own SDK and is
// served by the server project that references it.
var webSDK = regexp.MustCompile(`<Project\s+Sdk="Microsoft\.NET\.Sdk\.Web(?:/[^"]*)?"`)

// dotnetProjectDepth is how many directories below the repository root a
// solution's projects are looked for: MyApp/MyApp.csproj, and
// src/MyApp/MyApp.csproj.
const dotnetProjectDepth = 2

// readDotnetBuild reports the .NET project in the repository that runs.
//
// A single project at the top is that project. Otherwise the repository is a
// solution — the server, its Blazor client, class libraries and tests side by
// side, under a .sln or a .slnx — and the project that runs is the one web
// app among them. Detection found no project in a solution at all and asked
// how to build it (issue #90). Several web apps is a choice that is not
// Pando's to make, and none is not an app Pando knows how to run.
func readDotnetBuild(contextDir string) (dotnetBuild, bool) {
	var projects []string
	for _, pattern := range []string{"*.csproj", "*.fsproj"} {
		found, _ := filepath.Glob(filepath.Join(contextDir, pattern))
		projects = append(projects, found...)
	}
	switch len(projects) {
	case 1:
		return readDotnetProject(contextDir, projects[0])
	case 0:
		var web []string
		for _, p := range nestedDotnetProjects(contextDir) {
			body, err := os.ReadFile(p)
			if err == nil && webSDK.Match(body) {
				web = append(web, p)
			}
		}
		if len(web) == 1 {
			return readDotnetProject(contextDir, web[0])
		}
	}
	return dotnetBuild{}, false
}

// nestedDotnetProjects are the project files below the root, to
// dotnetProjectDepth, outside directories that hold build output or packages.
func nestedDotnetProjects(contextDir string) []string {
	var found []string
	dirs := contextDir
	for range dotnetProjectDepth {
		dirs = filepath.Join(dirs, "*")
		for _, ext := range []string{".csproj", ".fsproj"} {
			matches, _ := filepath.Glob(filepath.Join(dirs, "*"+ext))
			for _, m := range matches {
				rel, err := filepath.Rel(contextDir, m)
				if err == nil && !skippedDotnetDir(rel) {
					found = append(found, m)
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

func skippedDotnetDir(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
		switch strings.ToLower(part) {
		case "bin", "obj", "node_modules", "packages":
			return true
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// readDotnetProject reads one project file, at a path relative to the context
// made only of names that can be written plainly into a Dockerfile.
func readDotnetProject(contextDir, project string) (dotnetBuild, bool) {
	body, err := os.ReadFile(project)
	if err != nil {
		return dotnetBuild{}, false
	}
	m := targetFramework.FindSubmatch(body)
	if m == nil {
		return dotnetBuild{}, false
	}
	rel, err := filepath.Rel(contextDir, project)
	if err != nil {
		return dotnetBuild{}, false
	}
	rel = filepath.ToSlash(rel)
	for _, part := range strings.Split(rel, "/") {
		if !safeDotnetName.MatchString(part) || part == ".." {
			return dotnetBuild{}, false
		}
	}
	base := filepath.Base(project)
	assembly := strings.TrimSuffix(strings.TrimSuffix(base, ".csproj"), ".fsproj")
	if a := assemblyName.FindSubmatch(body); a != nil {
		assembly = string(a[1])
	}
	if !safeDotnetName.MatchString(assembly) {
		return dotnetBuild{}, false
	}
	return dotnetBuild{Project: rel, Assembly: assembly, Version: string(m[1])}, true
}

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
		return "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	program := strings.Fields(command)[0]
	content := fmt.Sprintf(`# A program committed in the repository, run as it is.
FROM %s
WORKDIR /app
COPY . /app
RUN chmod +x %s
ENTRYPOINT ["/bin/sh", "-c"]
CMD [%s]
`, programRuntimeImage, program, quoted)
	return writePlan(contextDir, content)
}

// writePlan writes a Dockerfile Pando made into .pando-plan/, where detection
// collects a plan and the build replays it (R-020).
func writePlan(contextDir, content string) (string, error) {
	name := filepath.Join(planDir, "Dockerfile")
	if err := writeContextFile(contextDir, name, []byte(content)); err != nil {
		return "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	return name, nil
}

// writeDotnetPlan writes the plan into .pando-plan/, where detection collects a
// plan and the build replays it (R-020). The ASP.NET image listens on 8080,
// which the deploy's port check confirms against the built image (R-097).
//
// The project's own publish profile is cleared. ServiceStack's template names
// DefaultContainer, which publishes a container image through docker or
// podman; a build has neither (R-112), and publish stopped with "Cannot find
// docker/podman executable" (issue #90). The image is this plan's to make.
func writeDotnetPlan(contextDir string, b dotnetBuild) (string, error) {
	content := fmt.Sprintf(`# A .NET %[1]s project, built with the SDK it targets.
FROM mcr.microsoft.com/dotnet/sdk:%[1]s AS build
WORKDIR /src
COPY . .
RUN dotnet publish %[2]s -c Release -o /out -p:PublishProfile=

FROM mcr.microsoft.com/dotnet/aspnet:%[1]s
WORKDIR /app
COPY --from=build /out .
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec dotnet %[3]s.dll"]
`, b.Version, b.Project, b.Assembly)

	name := filepath.Join(planDir, "Dockerfile")
	if err := writeContextFile(contextDir, name, []byte(content)); err != nil {
		return "", errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	return name, nil
}
