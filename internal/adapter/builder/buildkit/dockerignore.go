package buildkit

import (
	"path/filepath"

	"github.com/trypando/pando/internal/errs"
)

// A Dockerfile Pando writes builds the repository from source, and the
// repository's .dockerignore was written for a different build.
//
// BuildKit applies the context's .dockerignore to every Dockerfile built from
// it. A repository's own is written for its own Dockerfile, which often copies
// a jar or a binary built on the developer's machine: every Quarkus quickstart
// ignores `*` and lets only target/ back in. Applied to a plan that runs
// `COPY . .` and then the build tool, it left no pom.xml to build, and Maven
// stopped with "there is no POM in this directory" (issue #90).
//
// BuildKit reads `<Dockerfile>.dockerignore` beside the Dockerfile in place of
// the context's own, so every Dockerfile Pando writes gets one. It ignores
// nothing: Pando's checkout is already exactly the source to build, and an
// exclusion Pando chose would be a guess about what a build needs — a Maven
// plugin that stamps the commit reads .git, and a repository may commit its
// node_modules. A Railpack plan is not a Dockerfile and has its own exclude
// list.
const planIgnore = `# Written by Pando. A Dockerfile Pando writes builds this repository from
# source, so the repository's own .dockerignore, written for a different
# build, does not apply to it. Nothing is ignored.
`

// writePlanIgnore writes the ignore file beside a Dockerfile Pando wrote into
// .pando-plan/, named relative to the checkout. One already there — replayed
// from the spec — is left as it is. A Railpack plan gets none, and neither does
// a plan stored under .nixpacks/, which is replayed as it was built.
func writePlanIgnore(contextDir, dockerfile string) error {
	if isRailpackPlan(dockerfile) || filepath.Dir(filepath.Clean(dockerfile)) != planDir {
		return nil
	}
	name := filepath.Clean(dockerfile) + ".dockerignore"
	if statContext(contextDir, name) {
		return nil
	}
	if err := writeContextFile(contextDir, name, []byte(planIgnore)); err != nil {
		return errs.Wrap(errs.BuildFailed, msgPrepareBuild, err)
	}
	return nil
}
