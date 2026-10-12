package detect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"

	"github.com/trypando/pando/internal/adapter/api"
)

// What a workspace manifest declares, read rather than counted.
//
// A file's name is not its meaning. pnpm reads pnpm-workspace.yaml for its
// settings as well as for a list of packages, and a single-package app that
// pins onlyBuiltDependencies there — Nuxt's, Adonis's and Laravel's starter
// kits all do — declares no workspace at all. Treated as one, every such app
// was asked which of its projects to deploy, with no project to name (issue
// #90). And a workspace whose root package.json starts the app has already
// said what to deploy: the root, with the workspace tool doing what it is for.

// jsWorkspaceMarkers are the files a JavaScript workspace tool reads. A root
// start script answers what such a workspace deploys.
var jsWorkspaceMarkers = map[string]bool{
	"pnpm-workspace.yaml": true, "pnpm-workspace.yml": true,
	"lerna.json": true, "nx.json": true, "rush.json": true, "turbo.json": true,
	"package.json workspaces": true,
}

// declaredWorkspaceMarkers is workspaceMarkers, less a pnpm-workspace.yaml that
// lists no package beyond the root, and package.json's workspaces when they
// are the same.
func declaredWorkspaceMarkers(src api.SourceView) []string {
	var markers []string
	for _, name := range workspaceMarkers {
		info, err := src.Stat(name)
		if err != nil || info.IsDir {
			continue
		}
		if strings.HasPrefix(name, "pnpm-workspace.") && len(pnpmWorkspacePackages(src, name)) == 0 {
			continue
		}
		markers = append(markers, name)
	}
	if len(packageJSONWorkspaces(src)) > 0 {
		markers = append(markers, "package.json workspaces")
	}
	if hasCargoWorkspace(src) {
		markers = append(markers, "Cargo.toml [workspace]")
	}
	return markers
}

// workspaceResolved reports whether the repository says what a workspace
// deploys, so there is nothing to ask: a JavaScript workspace whose root
// package.json has a start script, a Go workspace with one program in it, a
// Cargo workspace with one binary. Each is built from the root, which is what
// the workspace's own tooling expects (R-104: Pando asks only when it cannot
// proceed).
func workspaceResolved(src api.SourceView, markers []string) bool {
	for _, marker := range markers {
		switch {
		case jsWorkspaceMarkers[marker]:
			if !hasStartScript(src) {
				return false
			}
		case marker == "go.work":
			if len(goWorkPrograms(src)) != 1 {
				return false
			}
		case marker == "Cargo.toml [workspace]":
			if len(cargoWorkspaceBinaries(src)) != 1 {
				return false
			}
		default:
			return false
		}
	}
	return len(markers) > 0
}

// workspaceMembers are the directories a workspace's manifests name, plus the
// projects under the directories multi-project repositories conventionally
// use. Directories only, sorted.
func workspaceMembers(src api.SourceView) []string {
	var patterns []string
	for _, name := range []string{"pnpm-workspace.yaml", "pnpm-workspace.yml"} {
		patterns = append(patterns, pnpmWorkspacePackages(src, name)...)
	}
	patterns = append(patterns, packageJSONWorkspaces(src)...)
	patterns = append(patterns, goWorkUses(src)...)
	patterns = append(patterns, cargoWorkspaceMembers(src)...)
	for _, dir := range packageDirs {
		patterns = append(patterns, dir+"/*")
	}

	seen := map[string]bool{}
	var found []string
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(path.Clean(strings.TrimPrefix(pattern, "./")), "/")
		if pattern == "." || strings.HasPrefix(pattern, "..") || strings.HasPrefix(pattern, "!") {
			continue
		}
		// "packages/**" means the packages under packages/, for what is asked.
		pattern = strings.ReplaceAll(pattern, "**", "*")
		var matches []string
		if strings.ContainsAny(pattern, "*?[") {
			matches, _ = src.Glob(pattern)
		} else {
			matches = []string{pattern}
		}
		for _, m := range matches {
			if seen[m] || strings.HasPrefix(path.Base(m), ".") {
				continue
			}
			// The Glob matches basenames too; only what the pattern names
			// as a path is a member.
			if ok, _ := path.Match(pattern, m); !ok {
				continue
			}
			if info, err := src.Stat(m); err == nil && info.IsDir {
				seen[m] = true
				found = append(found, m)
			}
		}
	}
	sort.Strings(found)
	return found
}

// hasStartScript reports whether package.json has a start script.
func hasStartScript(src api.SourceView) bool {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	return readJSON(src, "package.json", &pkg) && strings.TrimSpace(pkg.Scripts["start"]) != ""
}

// pnpmWorkspacePackages are the package patterns a pnpm-workspace.yaml lists,
// less the root itself. A file pnpm could not read either is treated as
// declaring a workspace, which is what it was taken to mean before.
func pnpmWorkspacePackages(src api.SourceView, name string) []string {
	raw, ok := readAll(src, name)
	if !ok {
		return nil
	}
	var file struct {
		Packages []string `yaml:"packages"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return []string{"*"}
	}
	return beyondRoot(file.Packages)
}

// packageJSONWorkspaces are package.json's workspaces: an array of globs, or
// an object with a packages array (Yarn's form). Less the root itself.
func packageJSONWorkspaces(src api.SourceView) []string {
	var pkg struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if !readJSON(src, "package.json", &pkg) || len(pkg.Workspaces) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(pkg.Workspaces, &list) != nil {
		var object struct {
			Packages []string `json:"packages"`
		}
		if json.Unmarshal(pkg.Workspaces, &object) != nil {
			return nil
		}
		list = object.Packages
	}
	return beyondRoot(list)
}

// beyondRoot drops the patterns that name only the root, and exclusions.
func beyondRoot(patterns []string) []string {
	var out []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "!") {
			continue
		}
		if clean := path.Clean(p); clean == "." || clean == "/" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// goWorkUses are the module directories a go.work file uses, root excluded.
func goWorkUses(src api.SourceView) []string {
	raw, ok := readAll(src, "go.work")
	if !ok {
		return nil
	}
	var uses []string
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "":
			uses = append(uses, strings.Trim(line, `"`))
		case line == "use (":
			inBlock = true
		case strings.HasPrefix(line, "use "):
			uses = append(uses, strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "use ")), `"`))
		}
	}
	return beyondRoot(uses)
}

// goWorkPrograms are the modules a go.work uses that hold a main package, at
// the module's root or one under its cmd/.
func goWorkPrograms(src api.SourceView) []string {
	var programs []string
	for _, use := range goWorkUses(src) {
		dir := path.Clean(use)
		if goDirHasMain(src, dir) {
			programs = append(programs, dir)
			continue
		}
		cmds, _ := src.Glob(dir + "/cmd/*")
		for _, c := range cmds {
			if ok, _ := path.Match(dir+"/cmd/*", c); ok && goDirHasMain(src, c) {
				programs = append(programs, dir)
				break
			}
		}
	}
	return programs
}

// goDirHasMain reports a non-test Go file directly in dir declaring package
// main.
func goDirHasMain(src api.SourceView, dir string) bool {
	files, _ := src.Glob(dir + "/*.go")
	for _, f := range files {
		if ok, _ := path.Match(dir+"/*.go", f); !ok || strings.HasSuffix(f, "_test.go") {
			continue
		}
		if goPackageOf(src, f) == "main" {
			return true
		}
	}
	return false
}

// cargoManifest is the part of a Cargo.toml read here.
type cargoManifest struct {
	Package *struct {
		Name string `toml:"name"`
	} `toml:"package"`
	Workspace *struct {
		Members []string `toml:"members"`
	} `toml:"workspace"`
	Bin []struct {
		Name string `toml:"name"`
	} `toml:"bin"`
}

func readCargoManifest(src api.SourceView, name string) (cargoManifest, bool) {
	var m cargoManifest
	raw, ok := readAll(src, name)
	if !ok || toml.Unmarshal(raw, &m) != nil {
		return cargoManifest{}, false
	}
	return m, true
}

// cargoWorkspaceMembers are the member patterns of the root Cargo workspace.
func cargoWorkspaceMembers(src api.SourceView) []string {
	m, ok := readCargoManifest(src, "Cargo.toml")
	if !ok || m.Workspace == nil {
		return nil
	}
	return beyondRoot(m.Workspace.Members)
}

// cargoWorkspaceBinaries are the members of the root Cargo workspace that
// build a program — a src/main.rs or a [[bin]] — counting the root package
// too, when the workspace has one.
func cargoWorkspaceBinaries(src api.SourceView) []string {
	var dirs []string
	if m, ok := readCargoManifest(src, "Cargo.toml"); ok && m.Package != nil {
		dirs = append(dirs, ".")
	}
	for _, member := range cargoWorkspaceMembers(src) {
		pattern := path.Clean(member)
		if !strings.ContainsAny(pattern, "*?[") {
			dirs = append(dirs, pattern)
			continue
		}
		matches, _ := src.Glob(pattern)
		for _, m := range matches {
			if ok, _ := path.Match(pattern, m); ok {
				dirs = append(dirs, m)
			}
		}
	}
	var bins []string
	for _, dir := range dirs {
		m, ok := readCargoManifest(src, path.Join(dir, "Cargo.toml"))
		if !ok || m.Package == nil {
			continue
		}
		if info, err := src.Stat(path.Join(dir, "src/main.rs")); (err == nil && !info.IsDir) || len(m.Bin) > 0 {
			bins = append(bins, dir)
		}
	}
	return bins
}

func readAll(src api.SourceView, name string) ([]byte, bool) {
	f, err := src.Open(name)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	return raw, err == nil
}

// --- Projects one level down ------------------------------------------------

// projectManifests are files that make the directory holding them a project
// somebody could deploy.
var projectManifests = []string{
	"Dockerfile", "Containerfile",
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
	"package.json", "requirements.txt", "pyproject.toml", "Pipfile", "Procfile",
	"go.mod", "Gemfile", "composer.json", "Cargo.toml", "pom.xml",
	"build.gradle", "build.gradle.kts", "mix.exs", "deno.json", "deno.jsonc",
}

// rootSourceExtensions are files at the root that make the root itself an app
// some detector or the build planner may read, manifest or not.
var rootSourceExtensions = []string{
	".py", ".js", ".mjs", ".ts", ".go", ".rb", ".php", ".rs", ".java", ".html", ".csproj", ".sln",
}

// nestedProjects are the projects in a repository whose root is not one: the
// directories one level down, and one level under apps/, services/ and the
// rest of packageDirs, that hold a project manifest. Nil when the root holds a
// project itself, which every other detector reads.
func nestedProjects(src api.SourceView) []string {
	if holdsProject(src, ".") {
		return nil
	}
	for _, ext := range rootSourceExtensions {
		files, _ := src.Glob("*" + ext)
		for _, f := range files {
			if !strings.Contains(f, "/") {
				return nil
			}
		}
	}

	conventional := map[string]bool{}
	for _, dir := range packageDirs {
		conventional[dir] = true
	}
	var found []string
	consider := func(dir string) {
		base := path.Base(dir)
		if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || underNotDeployable(dir) {
			return
		}
		if holdsProject(src, dir) {
			found = append(found, dir)
		}
	}
	entries, _ := src.Glob("*")
	for _, entry := range entries {
		if strings.Contains(entry, "/") {
			continue
		}
		if info, err := src.Stat(entry); err != nil || !info.IsDir {
			continue
		}
		if !conventional[entry] {
			consider(entry)
			continue
		}
		children, _ := src.Glob(entry + "/*")
		for _, child := range children {
			if ok, _ := path.Match(entry+"/*", child); !ok {
				continue
			}
			if info, err := src.Stat(child); err == nil && info.IsDir {
				consider(child)
			}
		}
	}
	sort.Strings(found)
	return found
}

func holdsProject(src api.SourceView, dir string) bool {
	for _, name := range projectManifests {
		if info, err := src.Stat(path.Join(dir, name)); err == nil && !info.IsDir {
			return true
		}
	}
	return false
}

// nestedProjectsBid is the monorepo detector's reading of a repository with no
// workspace manifest and no app at its root, whose projects sit one level
// down: a Go module in service/, two Python services under services/.
//
// The bid is low on purpose. It only ever wins when nothing recognized the
// root, and it must lose clearly to anything that did, rather than tie with
// it into a question about strategies.
func nestedProjectsBid(src api.SourceView) Candidate {
	found := nestedProjects(src)
	if len(found) == 0 {
		return Candidate{Confidence: 0}
	}
	q := Question{
		Key:     KeyDeployableProject,
		Kind:    api.QuestionChoice,
		Options: found,
		Why:     "Pando deploys one app at a time, and the app in this repository is not at its top level.",
	}
	evidence := fmt.Sprintf("no app at the repository root; %d projects one level down: %s",
		len(found), strings.Join(truncate(found, 12), ", "))
	if len(found) == 1 {
		evidence = fmt.Sprintf("no app at the repository root; the only project is in %s/", found[0])
		q.Prompt = fmt.Sprintf(
			"This repository has no app at its top level. The only project in it is in the directory %s, "+
				"so Pando reads that directory as the app. Valid answer: %s, or the path within the "+
				"repository of another directory to deploy instead.", found[0], found[0])
	} else {
		q.Prompt = fmt.Sprintf(
			"This repository has no app at its top level; it holds %d separate projects in "+
				"subdirectories: %s. Pando deploys one app at a time and cannot tell which of these "+
				"you mean. Valid answer: one of those paths.",
			len(found), strings.Join(truncate(found, 12), ", "))
	}
	return Candidate{
		Strategy:   StrategyUnknown,
		Confidence: 0.25,
		Evidence:   []string{evidence},
		Questions:  []Question{q},
	}
}
