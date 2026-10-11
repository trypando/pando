package buildkit

import (
	_ "embed"
	"strings"
)

// basesDockerfile names the base images Pando supplies, one stage each. It is a
// Dockerfile so that Dependabot can propose a newer tag; see the file itself.
//
//go:embed bases/Dockerfile
var basesDockerfile string

// bases maps each stage name in basesDockerfile to its image.
var bases = parseBases(basesDockerfile)

var (
	// staticServerImage serves a built static site.
	staticServerImage = base("static-server")

	// programRuntimeImage runs a compiled program: Go that Pando built, or a
	// program committed in the repository.
	programRuntimeImage = base("program-runtime")
)

// parseBases reads `FROM <image> AS <name>` lines. Comments and anything else
// are skipped.
func parseBases(dockerfile string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(dockerfile, "\n") {
		f := strings.Fields(line)
		if len(f) == 4 && strings.EqualFold(f[0], "FROM") && strings.EqualFold(f[2], "AS") {
			out[f[3]] = f[1]
		}
	}
	return out
}

// base is the image a stage of basesDockerfile names. A missing stage is a
// mistake in this package, caught by its tests before a build ever runs.
func base(name string) string {
	image, ok := bases[name]
	if !ok {
		panic("buildkit: bases/Dockerfile has no stage " + name)
	}
	return image
}
