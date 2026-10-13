package buildkit

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Taskfile and justfile, which are the Makefile's idea in different syntax: a
// file at the root naming the commands this repository is built and run with.
//
// Both are parsed the same shallow way the Makefile is — which targets exist
// and what their recipes say — and neither is interpreted. A Taskfile with
// includes, or a justfile whose recipe is a shell function, is simply not
// recognized, and the repository is planned by convention exactly as before.
// Guessing wrong here produces an image that builds and does the wrong thing,
// which is the failure this whole file exists to prevent.

// --- Taskfile ---------------------------------------------------------------

var taskfileNames = []string{"Taskfile.yml", "Taskfile.yaml", "taskfile.yml", "taskfile.yaml"}

// taskNamePattern matches a task name: two spaces of indent under `tasks:`,
// then a name and a colon. Deeper indentation is the task's body.
var taskNamePattern = regexp.MustCompile(`^  ([A-Za-z0-9][A-Za-z0-9_.\-]*)\s*:\s*$`)

// taskCmdPattern matches a command line in a task's `cmds:` list.
var taskCmdPattern = regexp.MustCompile(`^\s+-\s+(?:cmd:\s*)?(.+)$`)

func readTaskfileAt(dir string) (declaration, string) {
	for _, name := range taskfileNames {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		d := parseTaskfile(f)
		_ = f.Close()
		d.File = name
		return d, "task"
	}
	return declaration{}, "task"
}

// parseTaskfile reads a Taskfile's task names and their commands.
//
// Enough YAML to find `tasks:`, the names two spaces in, and the `cmds:` lists
// under them. Not a YAML parser: anchors, includes and templated `{{.VAR}}`
// commands are all things it will fail to understand, and failing to understand
// means declining rather than guessing.
func parseTaskfile(r io.Reader) declaration {
	recipes := map[string][]string{}
	var current string
	inTasks, inCmds := false, false

	scanner := bufio.NewScanner(io.LimitReader(r, 256<<10))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		if !strings.HasPrefix(line, " ") {
			inTasks = strings.HasPrefix(line, "tasks:")
			current, inCmds = "", false
			continue
		}
		if !inTasks {
			continue
		}

		if m := taskNamePattern.FindStringSubmatch(line); m != nil {
			current, inCmds = m[1], false
			if _, seen := recipes[current]; !seen {
				recipes[current] = nil
			}
			continue
		}
		if current == "" {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), "cmds:") {
			inCmds = true
			continue
		}
		if !inCmds {
			continue
		}
		if m := taskCmdPattern.FindStringSubmatch(line); m != nil {
			if step := strings.TrimSpace(m[1]); step != "" {
				recipes[current] = append(recipes[current], step)
			}
		}
	}
	return declarationFrom(recipes, "task")
}

// --- justfile ---------------------------------------------------------------

var justfileNames = []string{"justfile", "Justfile", ".justfile"}

// justRecipePattern matches a recipe line: a name at the start of a line, its
// parameters, then a colon. Same shape as make, which is deliberate on just's
// part.
var justRecipePattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_\-]*)[^:=]*:[^=]?`)

func readJustfileAt(dir string) (declaration, string) {
	for _, name := range justfileNames {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		d := parseJustfile(f)
		_ = f.Close()
		d.File = name
		return d, "just"
	}
	return declaration{}, "just"
}

// parseJustfile reads a justfile's recipe names and their bodies.
//
// Indentation separates a body from the next recipe, as in make — except just
// accepts spaces as well as a tab, so the test is "indented" rather than
// "starts with a tab".
func parseJustfile(r io.Reader) declaration {
	recipes := map[string][]string{}
	var current string

	scanner := bufio.NewScanner(io.LimitReader(r, 256<<10))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if line != trimmed {
			if current != "" {
				recipes[current] = append(recipes[current], trimmed)
			}
			continue
		}
		// just spells assignments `name := value`, which shares the blind
		// spot described on assignmentPattern.
		if assignmentPattern.MatchString(trimmed) {
			current = ""
			continue
		}
		if m := justRecipePattern.FindStringSubmatch(trimmed); m != nil {
			current = m[1]
			if _, seen := recipes[current]; !seen {
				recipes[current] = nil
			}
			continue
		}
		// A variable assignment, a setting, an import — none of them a recipe.
		current = ""
	}
	return declarationFrom(recipes, "just")
}

// --- shared -----------------------------------------------------------------

// declarationFrom picks the build and start targets out of a set of recipes.
//
// Shared by every runner because the question is the same one in each: which of
// these names means "build the app", and which means "run it".
func declarationFrom(recipes map[string][]string, invoke string) declaration {
	var d declaration
	for _, name := range buildTargets {
		if _, ok := recipes[name]; ok {
			d.BuildTarget = name
			d.BuildOutput = goBuildOutput(recipes[name])
			break
		}
	}
	for _, name := range startTargets {
		steps, ok := recipes[name]
		if !ok {
			continue
		}
		if cmd := startCommandFrom(steps, invoke); cmd != "" {
			d.StartCommand = cmd
			break
		}
	}
	return d
}

var goBuildOutputFlag = regexp.MustCompile(`(?:^|\s)go build\b[^\n]*?\s-o\s+([A-Za-z0-9_.\-/]+)`)

// goBuildOutput is where a recipe's `go build -o` writes the program, relative
// to the repository, or "" when no line says, or says somewhere else.
func goBuildOutput(steps []string) string {
	for _, step := range steps {
		m := goBuildOutputFlag.FindStringSubmatch(step)
		if m == nil {
			continue
		}
		out := strings.TrimPrefix(m[1], "./")
		if safeRelative(out) && !strings.HasSuffix(out, "/") {
			return out
		}
	}
	return ""
}
