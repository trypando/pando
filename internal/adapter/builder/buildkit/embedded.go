package buildkit

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Two declarations naming one directory.
//
// macscout with its Makefile deleted still says this twice, from opposite ends:
//
//	web/vite.config.ts     build: { outDir: "../cmd/server/dist" }
//	cmd/server/main.go     //go:embed all:dist
//
// The first is the client saying where it builds to. The second is the Go build
// saying it needs that directory populated. Neither mentions the other, and
// together they say the client has to be built first — which is a fact read out
// of two files rather than a guess about a layout.
//
// This matters because of how it fails. A Go module that embeds a directory
// compiles whether or not anything filled it: `all:dist` matches the committed
// placeholder that exists so the build works on a fresh clone. So the image is
// built, the container starts, the health check passes, and the app serves an
// empty page. R-203 makes the same argument about persistence and calls it the
// worst failure mode in the system: what works until it doesn't, silently.
//
// It ranks below the sources that name commands because it says less. It knows
// the client must be built before the Go build; it does not know what the Go
// build is. That is what the second planning pass is for.

// embedPattern matches a go:embed directive and captures its patterns.
var embedPattern = regexp.MustCompile(`^//go:embed\s+(.+)$`)

// readEmbeddedBuild finds a client whose declared output is a directory the Go
// build embeds.
func readEmbeddedBuild(contextDir string) declaredBuild {
	embeds := goEmbedTargets(contextDir)
	if len(embeds) == 0 {
		return declaredBuild{}
	}

	for _, client := range clientProjects(contextDir) {
		if client.OutDir == "" || !embeds[client.OutDir] {
			continue
		}

		// A subshell, because the `cd` must not outlive the client build.
		//
		// npm has to run where the package.json is, and Railpack runs the build
		// command from the repository root — but this command is a prefix, and
		// whatever Railpack chose runs after it in the same shell. Without the
		// parentheses `go build ./cmd/server` runs from web/ and the image fails
		// to build with "stat /app/web/cmd/server: directory not found", which
		// is how this comment came to exist.
		build := "(cd " + client.Dir + " && npm ci && npm run " + client.Script + ")"
		if !safeCommand(build) {
			continue
		}

		return declaredBuild{
			Rank:     rankEmbedded,
			Source:   path.Join(client.Dir, client.ConfigFile),
			Before:   build,
			Packages: []string{"node"},
			Why: path.Join(client.Dir, client.ConfigFile) + " builds into " + client.OutDir +
				", which this app embeds — so the client is built before the binary that carries it",
		}
	}
	return declaredBuild{}
}

// goEmbedTargets is every directory a go:embed directive needs populated.
//
// Resolved against the directory of the file that declares it, because that is
// how go:embed works: a pattern is relative to its own package, never to the
// module root. macscout's `//go:embed all:dist` lives in cmd/server/main.go and
// means cmd/server/dist.
func goEmbedTargets(root string) map[string]bool {
	targets := map[string]bool{}

	walkRepository(root, func(rel string, entry os.DirEntry) {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()

		pkgDir := path.Dir(rel)
		scanner := bufio.NewScanner(io.LimitReader(f, 256<<10))
		for scanner.Scan() {
			m := embedPattern.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
			if m == nil {
				continue
			}
			for _, pattern := range strings.Fields(m[1]) {
				// `all:` asks for otherwise-ignored files too; it says nothing
				// about which directory is meant.
				pattern = strings.TrimPrefix(pattern, "all:")
				// A pattern with a wildcard names a set of files rather than a
				// directory to fill, and is not what this is looking for.
				if pattern == "" || strings.ContainsAny(pattern, "*?[") {
					continue
				}
				targets[path.Clean(path.Join(pkgDir, pattern))] = true
			}
		}
	})
	return targets
}

// clientProject is a front-end build that says where its output goes.
type clientProject struct {
	Dir        string // relative to the repository root
	ConfigFile string // the file that declared OutDir, relative to Dir
	OutDir     string // relative to the repository root
	Script     string // the npm script that runs the build
}

// clientProjects finds every package.json with a build script, and asks its
// bundler config where the build lands.
//
// Only declared outputs count. A project whose bundler config does not say
// where it writes is one whose output directory would have to be assumed, and
// an assumed path that happens to match an embed directive is a coincidence
// this should not act on.
func clientProjects(root string) []clientProject {
	var found []clientProject

	walkRepository(root, func(rel string, entry os.DirEntry) {
		if entry.IsDir() || entry.Name() != "package.json" {
			return
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(body, &pkg) != nil || pkg.Scripts["build"] == "" {
			return
		}

		dir := path.Dir(rel)
		configFile, outDir := declaredOutDir(root, dir, pkg.Scripts)
		if outDir == "" {
			return
		}
		found = append(found, clientProject{
			Dir: dir, ConfigFile: configFile, OutDir: outDir, Script: "build",
		})
	})

	sort.Slice(found, func(i, j int) bool { return found[i].Dir < found[j].Dir })
	return found
}

// walkRepository visits the files of a checkout, skipping the directories that
// hold other people's code and stopping before it descends forever.
func sortStrings(s []string) { sort.Strings(s) }

// walkRepository visits the files of a checkout, skipping the directories that
// hold other people's code and stopping before it descends forever.
func walkRepository(root string, visit func(rel string, entry os.DirEntry)) {
	const maxDepth = 5

	// A directory that cannot be read is skipped rather than fatal: this walks
	// somebody else's repository, and one unreadable path is not a reason to
	// refuse to plan the build.
	_ = filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil //nolint:nilerr // see above.
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(p, root), string(filepath.Separator)))
		if entry.IsDir() {
			base := entry.Name()
			if strings.HasPrefix(base, ".") || base == "node_modules" || base == "vendor" ||
				base == "testdata" || strings.Count(rel, "/") >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		visit(rel, entry)
		return nil
	})
}
