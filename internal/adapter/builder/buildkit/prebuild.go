package buildkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// A Dockerfile that copies what the repository's own build makes.
//
// Some Dockerfiles are written to run after a build on the author's machine:
// Open Liberty's guides copy target/*.war, which `./mvnw package` makes, and
// an Nx workspace's generated Dockerfile copies dist/api, which `nx build`
// makes. Built as they are, from the repository, the COPY finds nothing
// ("lstat /target: no such file or directory", issue #90). The Dockerfile
// names the directory and the repository names the tool that writes it — a
// pom.xml, a Gradle wrapper, a package.json build script — so the build is
// two declarations joined, as `embedded` is in declared.go: the tool's build
// runs first, in a stage of its own, and each COPY of its output takes it
// from there. Everything else in the Dockerfile is the author's, unchanged.

// prebuildStage names the stage the repository's own build runs in.
const prebuildStage = "pando-prebuild"

// prebuildTool is a build the repository declares, and the directories it
// writes.
type prebuildTool struct {
	Name    string   // shown in the plan and the evidence
	Outputs []string // top-level directories the build writes
	Stage   string   // the stage, ending in the build's RUN
}

// PlanPrebuild is the Dockerfile at dockerfile with the repository's own
// build run before it, when the Dockerfile copies that build's output and
// the repository does not have it. An empty plan when it does not.
func (a *Adapter) PlanPrebuild(_ context.Context, src api.SourceView, dockerfile string) (api.BuildPlan, error) {
	dir, ok := src.(interface{ Root() string })
	if !ok {
		return api.BuildPlan{}, errs.New(errs.BuildFailed, "Planning needs the source on disk.")
	}
	root := dir.Root()
	content, tool, missing, ok := prebuildPlan(root, dockerfile)
	if !ok {
		return api.BuildPlan{}, nil
	}
	name, err := writePlan(root, content)
	if err == nil {
		err = writePlanIgnore(root, name)
	}
	if err != nil {
		return api.BuildPlan{}, err
	}
	files, err := collectPlan(root)
	if err != nil {
		return api.BuildPlan{}, err
	}
	return api.BuildPlan{
		Files: files,
		Entry: filepath.ToSlash(name),
		Declared: &api.PlanDeclaration{
			Source: filepath.ToSlash(dockerfile),
			Why: fmt.Sprintf("%s copies %s, which the repository does not have until it is built, so %s runs first",
				filepath.ToSlash(dockerfile), strings.Join(missing, ", "), tool),
		},
	}, nil
}

// prebuildPlan is the rewritten Dockerfile, the build it runs first, and the
// sources it copies that were missing. Not ok when every source is in the
// repository, or when one that is not is something no declared build makes.
func prebuildPlan(contextDir, dockerfile string) (content, tool string, missing []string, ok bool) {
	body := readFile(contextDir, dockerfile)
	if body == "" {
		return "", "", nil, false
	}
	instructions, missing := readInstructions(contextDir, body)
	if len(missing) == 0 {
		return "", "", nil, false
	}

	t, ok := prebuildFor(contextDir, missing)
	if !ok {
		return "", "", nil, false
	}

	stage := fmt.Sprintf("# Written by Pando: %s copies %s, which only the repository's own %s build makes.\n"+
		"# The build runs first, in the stage below, and each such COPY takes it from there.\n%s\n",
		dockerfile, strings.Join(missing, ", "), t.Name, t.Stage)
	content, ok = withStage(instructions, stage)
	if !ok {
		return "", "", nil, false
	}
	return content, t.Name, missing, true
}

// dockerfileInstruction is one instruction of a Dockerfile.
type dockerfileInstruction struct {
	raw     string // as written, continuations and all
	rewrite string // the replacement, or ""
}

// readInstructions splits a Dockerfile into its instructions, each COPY whose
// sources are missing rewritten to take them from the prebuild stage, and the
// sources that were missing.
func readInstructions(contextDir, body string) (instructions []dockerfileInstruction, missing []string) {
	var pending strings.Builder
	var joined string
	for _, line := range strings.SplitAfter(body, "\n") {
		pending.WriteString(line)
		trimmed := strings.TrimRight(line, " \t\r\n")
		if strings.HasSuffix(trimmed, "\\") {
			joined += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		joined += trimmed
		in := dockerfileInstruction{raw: pending.String()}
		if rewrite, srcs := rewriteCopy(contextDir, joined); rewrite != "" {
			in.rewrite = rewrite
			missing = append(missing, srcs...)
		}
		instructions = append(instructions, in)
		pending.Reset()
		joined = ""
	}
	if pending.Len() > 0 {
		instructions = append(instructions, dockerfileInstruction{raw: pending.String()})
	}
	return instructions, missing
}

// withStage is the Dockerfile with stage written before its first FROM. Not
// ok when it has none.
func withStage(instructions []dockerfileInstruction, stage string) (string, bool) {
	var b strings.Builder
	staged := false
	for _, in := range instructions {
		if !staged && isFrom(in.raw) {
			b.WriteString(stage)
			staged = true
		}
		if in.rewrite != "" {
			b.WriteString(in.rewrite + "\n")
			continue
		}
		b.WriteString(in.raw)
	}
	return b.String(), staged
}

func isFrom(raw string) bool {
	f := strings.Fields(raw)
	return len(f) > 0 && strings.EqualFold(f[0], "FROM")
}

// rewriteCopy is a COPY from the build context whose sources are not all in
// the repository, taken from the prebuild stage instead, and the sources that
// were missing. "" for any other instruction.
func rewriteCopy(contextDir, line string) (string, []string) {
	flags, args, ok := contextCopy(line)
	if !ok {
		return "", nil
	}
	sources, dest := args[:len(args)-1], args[len(args)-1]
	missing, ok := missingSources(contextDir, sources)
	if !ok || len(missing) == 0 {
		return "", nil
	}
	from := make([]string, 0, len(sources)+1)
	for _, s := range sources {
		from = append(from, "/src/"+cleanSource(s))
	}
	quoted, err := json.Marshal(append(from, dest))
	if err != nil {
		return "", nil
	}
	parts := append([]string{"COPY", "--from=" + prebuildStage}, flags...)
	return strings.Join(append(parts, string(quoted)), " "), missing
}

// contextCopy reads a COPY from the build context: its flags, and its sources
// and destination in either form. Not ok for any other instruction, a COPY
// from a stage or image, or a heredoc.
func contextCopy(line string) (flags, args []string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.EqualFold(fields[0], "COPY") || strings.Contains(line, "<<") {
		return nil, nil, false
	}
	rest := fields[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
		if strings.HasPrefix(rest[0], "--from") {
			return nil, nil, false
		}
		flags = append(flags, rest[0])
		rest = rest[1:]
	}
	args = rest
	if strings.HasPrefix(strings.TrimSpace(strings.Join(rest, " ")), "[") {
		if json.Unmarshal([]byte(strings.Join(rest, " ")), &args) != nil {
			return nil, nil, false
		}
	}
	return flags, args, len(args) >= 2
}

// missingSources are the COPY sources the build context does not have. Not ok
// when a source names a variable, which only the build can expand.
func missingSources(contextDir string, sources []string) ([]string, bool) {
	var missing []string
	for _, s := range sources {
		if strings.Contains(s, "$") {
			return nil, false
		}
		if !inContext(contextDir, s) {
			missing = append(missing, cleanSource(s))
		}
	}
	return missing, true
}

// cleanSource is a COPY source as a path inside the build context.
func cleanSource(s string) string {
	return strings.TrimPrefix(path.Clean("/"+s), "/")
}

// inContext reports whether the build context has what a COPY source names: a
// file or directory, or for a pattern, at least one match.
func inContext(contextDir, source string) bool {
	rel := cleanSource(source)
	if rel == "" {
		return true
	}
	full := filepath.Join(contextDir, filepath.FromSlash(rel))
	if strings.ContainsAny(rel, "*?[") {
		matches, _ := filepath.Glob(full)
		return len(matches) > 0
	}
	_, err := os.Stat(full)
	return err == nil
}

// prebuildFor is the build the repository declares that writes every one of
// missing, or not ok.
func prebuildFor(contextDir string, missing []string) (prebuildTool, bool) {
	var tools []prebuildTool
	if fileExists(contextDir, "pom.xml") {
		jdk := mavenBuild(contextDir).JDK
		tools = append(tools, prebuildTool{Name: "Maven", Outputs: []string{"target"}, Stage: fmt.Sprintf(`FROM maven:3-eclipse-temurin-%d AS %s
WORKDIR /src
COPY . .
RUN if [ -f mvnw ]; then chmod +x mvnw && env -u MAVEN_CONFIG ./mvnw -B -DskipTests package; else mvn -B -DskipTests package; fi
`, jdk, prebuildStage)})
	}
	if fileExists(contextDir, "gradlew") && (fileExists(contextDir, "build.gradle") || fileExists(contextDir, "build.gradle.kts")) {
		jdk := gradleBuild(contextDir).JDK
		tools = append(tools, prebuildTool{Name: "Gradle", Outputs: []string{"build"}, Stage: fmt.Sprintf(`FROM eclipse-temurin:%d-jdk AS %s
WORKDIR /src
COPY . .
RUN chmod +x gradlew && ./gradlew --no-daemon build -x test
`, jdk, prebuildStage)})
	}
	if pkg, ok := readPackageJSON(contextDir); ok && pkg.Scripts["build"] != "" {
		node := nodeMajor(contextDir, pkg.Engines)
		tools = append(tools, prebuildTool{Name: "npm", Outputs: []string{"dist", "build", "out"}, Stage: fmt.Sprintf(`FROM node:%s-slim AS %s
WORKDIR /src
COPY . .
RUN %s
RUN npm run build
`, node, prebuildStage, installCommand(contextDir))})
	}
	for _, t := range tools {
		if writesAll(t, missing) {
			return t, true
		}
	}
	return prebuildTool{}, false
}

// writesAll reports whether every path in missing is under one of t's
// output directories.
func writesAll(t prebuildTool, missing []string) bool {
	for _, m := range missing {
		top, _, _ := strings.Cut(m, "/")
		found := false
		for _, out := range t.Outputs {
			if top == out {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
