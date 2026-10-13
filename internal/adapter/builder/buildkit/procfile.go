package buildkit

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A Procfile says how an app is run, and R-094 names it in tier 2 among the
// explicit deployment artifacts.
//
// It does not say how the app is *built*, which is why it ranks where it does:
// above the embed pairing, which knows an ordering and no commands, and below
// the runners, which know both. What it contributes is a start command, and
// Railpack still plans the build.
//
//	web: ./macscout
//	worker: ./macscout --worker
//
// `web` is the process type Pando wants. Everything else in the file is another
// process, and R-021 is clear that Pando does not invent topology: a repository
// that declares a worker has not asked for one to be deployed, and turning its
// Procfile into two workloads would be reading the file as a request rather
// than as a description of what it can run.

var procfileNames = []string{"Procfile", "procfile"}

// procfileTypes are the process names taken to mean "the app", in preference
// order. `web` is the convention every platform that reads a Procfile shares.
var procfileTypes = []string{"web", "app", "server"}

func readProcfileBuild(contextDir string) declaredBuild {
	for _, name := range procfileNames {
		f, err := os.Open(filepath.Join(contextDir, name))
		if err != nil {
			continue
		}
		processes := parseProcfile(f)
		_ = f.Close()

		// safeStart, not safeCommand: a Procfile's command reads $PORT, and
		// the shell that starts the container expands it.
		for _, kind := range procfileTypes {
			command := processes[kind]
			if command == "" || !safeStart(command) {
				continue
			}
			return declaredBuild{
				Rank:   rankProcfile,
				Source: name,
				Start:  command,
				Why:    name + " declares the " + kind + " process, and that is what the container runs",
			}
		}
		return declaredBuild{}
	}
	return declaredBuild{}
}

// parseProcfile reads `type: command` lines.
func parseProcfile(r io.Reader) map[string]string {
	processes := map[string]string{}

	scanner := bufio.NewScanner(io.LimitReader(r, 64<<10))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, command, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		kind, command = strings.TrimSpace(kind), strings.TrimSpace(command)
		if kind == "" || command == "" || strings.ContainsAny(kind, " \t") {
			continue
		}
		processes[strings.ToLower(kind)] = command
	}
	return processes
}
