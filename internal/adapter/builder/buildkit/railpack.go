package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	gateway "github.com/moby/buildkit/frontend/gateway/client"

	"github.com/trypando/pando/internal/errs"
)

// Railpack is the tier-4 planner (R-095): it reads a repository that carries
// no deployment instructions and writes a build plan, which BuildKit builds.
//
// The plan is JSON rather than a Dockerfile, and BuildKit reads it through
// Railpack's frontend image — a gateway frontend, which BuildKit runs inside
// the build service like any other build step. So the build goes through
// Pando's own rootless BuildKit with nothing privileged and no container
// runtime socket anywhere (R-112). Railpack's own `railpack build` and its
// README's `--privileged` BuildKit are not used.

// railpackBinary writes plans. It is called only to plan — `railpack prepare`
// writes a JSON file and builds nothing.
const railpackBinary = "railpack"

// railpackPlanFile is the plan's name in the checkout, and the name Railpack's
// frontend looks for.
const railpackPlanFile = "railpack-plan.json"

// The images a Railpack build runs, pinned by digest like every other image
// Pando supplies (issue #130). Listed in design 03 §8.
//
// The frontend is the version of the railpack binary in Pando's image: a plan
// is read by the same release that wrote it. The builder and runtime images
// are the ones that release's plans name, tagged by the mise release it
// bundles; pinRailpackImages rewrites a plan's tags to these digests, so the
// stored plan says exactly which image it runs on. Bump all four together
// with RAILPACK_VERSION and MISE_VERSION in the Dockerfile.
const (
	railpackFrontendImage = "ghcr.io/railwayapp/railpack-frontend:v0.40.0@sha256:fc6d5fa434c9310500dc18bebb0a4eb4854fee8546a6d7a090e7a36e39d9d153"
	railpackBuilderImage  = "ghcr.io/railwayapp/railpack-builder:mise-2026.9.12@sha256:a104c45734b7c59fa7f52ab5afac87c3a4dfa5ee1c5495ae0c798756c670c865"
	railpackRuntimeImage  = "ghcr.io/railwayapp/railpack-runtime:mise-2026.9.12@sha256:b699280f7b492ddba483ee1d03badaae238b8846ff2ef1e71ad8a2fd637c25b5"
)

// isRailpackPlan reports whether the file a build starts from is a Railpack
// plan rather than a Dockerfile.
func isRailpackPlan(name string) bool {
	return path.Base(name) == railpackPlanFile
}

// runRailpack asks Railpack what this repository needs, and returns where the
// plan was written, relative to the checkout.
//
// env is Railpack's own settings (RAILPACK_BUILD_CMD and the rest), each one
// either read out of the repository and passed safeCommand, or Pando's.
func runRailpack(contextDir string, env []string) (string, error) {
	// Made through the checkout's root, and any plan file already there
	// removed, so the path Railpack writes to is a directory inside the
	// checkout and not a link the repository carried (R-020).
	name := filepath.Join(planDir, railpackPlanFile)
	if err := mkdirContext(contextDir, planDir); err != nil {
		return "", errs.Wrap(errs.BuildFailed, msgPreparePlan, err)
	}
	if err := removeContextFile(contextDir, name); err != nil {
		return "", errs.Wrap(errs.BuildFailed, msgPreparePlan, err)
	}
	planPath := filepath.Join(contextDir, name)

	// Railpack's HOME, and nothing else: it writes only the plan, and keeps
	// its version lookups under /tmp/railpack, where the image put mise.
	home, err := os.MkdirTemp("", "pando-railpack-*")
	if err != nil {
		return "", errs.Wrap(errs.BuildFailed, msgPreparePlan, err)
	}
	defer func() { _ = os.RemoveAll(home) }()

	args := []string{"prepare", contextDir, "--plan-out", planPath}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}

	// G204: exec.Command takes an argv, so nothing here reaches a shell on this
	// host. contextDir is a checkout this process made, and every command in
	// env was read out of the repository and passed safeCommand.
	//
	// Those commands do become the built image's build steps and start
	// command — so the repository chooses what its own build and container
	// run. That is the same authority a Dockerfile already has, and the
	// container is the boundary either way (R-112, R-114).
	cmd := exec.Command(railpackBinary, args...) //nolint:gosec

	// A scrubbed environment, so nothing of Pando's (credentials, the
	// database address) is visible to a planner reading untrusted source.
	// This is not a network sandbox: Railpack resolves versions through mise,
	// which asks the package indexes (issue #78). What the repository's own
	// code can reach is decided by the BuildKit build, not here (R-112, R-114).
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "NO_COLOR=1"}

	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errs.New(errs.PlanCapabilityUnsupported,
				"This installation cannot work out how to build an app that carries no deployment instructions.").
				WithRemedy("Add a Dockerfile to the repository, or ask an administrator to install a build planner.")
		}
		// 75 is Railpack's EX_TEMPFAIL: a version lookup that could not reach
		// its index, which is worth trying again rather than fixing.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 75 {
			return "", errs.Newf(errs.BuildFailed,
				"Pando could not look up the language versions this app needs: %s", trim(out)).
				WithRemedy("Check that this installation can reach the internet, then try again.")
		}
		// Railpack's own message, which names what it looked for and did not
		// find. Replacing it with something generic would delete the only
		// useful detail (R-105).
		return "", errs.Newf(errs.BuildFailed,
			"Pando could not work out how to build this app: %s", trim(out)).
			WithRemedy("Add a Dockerfile to the repository saying how it should be built.")
	}

	body, err := readContextFile(contextDir, name)
	if err != nil {
		return "", errs.Newf(errs.BuildFailed,
			"Pando could not work out how to build this app: %s", trim(out)).
			WithRemedy("Add a Dockerfile to the repository saying how it should be built.")
	}
	body, err = finishRailpackPlan(body, contextDir)
	if err != nil {
		return "", err
	}
	if err := writeContextFile(contextDir, name, body); err != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not write the build plan.", err)
	}
	return name, nil
}

// finishRailpackPlan makes Railpack's plan the one Pando stores.
//
//   - The images Railpack supplies are pinned by digest, in the plan itself, so
//     an owner reading it sees exactly what it runs on (issue #130).
//   - Its secrets are cleared. Railpack lists every setting it was passed as a
//     build secret and mounts each one as required, so the build failed on
//     secrets nobody provides. None of them is a secret.
//   - The plan's own directory is left out of the image: it is in the build
//     context only because the checkout is.
//   - A copy of an optional file (`config/config.exs*`) from a directory the
//     repository does not have is dropped. Railpack writes those for files an
//     app may or may not have, and BuildKit fails the whole build when the
//     directory is missing ("lstat /config: no such file or directory", issue
//     #172) where it would have copied nothing.
func finishRailpackPlan(body []byte, contextDir string) ([]byte, error) {
	var plan map[string]any
	if err := json.Unmarshal(body, &plan); err != nil {
		return nil, errs.Wrap(errs.BuildFailed, "Railpack wrote a build plan Pando could not read.", err)
	}

	delete(plan, "secrets")
	if steps, ok := plan["steps"].([]any); ok {
		for _, s := range steps {
			if step, ok := s.(map[string]any); ok {
				delete(step, "secrets")
				dropCopiesFromMissingDirs(step, contextDir)
			}
		}
	}
	pinImages(plan)
	startFromBin(plan)

	exclude, _ := plan["exclude"].([]any)
	plan["exclude"] = append(exclude, planDir)

	out, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return nil, errs.Wrap(errs.BuildFailed, "Could not write the build plan.", err)
	}
	return append(out, '\n'), nil
}

// binCopy is a build command that copies a compiled program into bin/, as
// Railpack's Rust plans do: `cp target/release/sample-rust bin`.
var binCopy = regexp.MustCompile(`^cp\s+\S*/([A-Za-z0-9][A-Za-z0-9._-]*)\s+bin/?$`)

// startFromBin runs a program the plan builds into bin/ from there when the
// start command names it bare. A Procfile written for Heroku does that, since
// Heroku puts bin/ on PATH and Railpack does not: DigitalOcean's Rust sample
// runs `sample-rust`, and exited 127 with "sample-rust: command not found"
// (issue #90).
func startFromBin(plan map[string]any) {
	deploy, _ := plan["deploy"].(map[string]any)
	start, _ := deploy["startCommand"].(string)
	fields := strings.Fields(start)
	if len(fields) == 0 || strings.Contains(fields[0], "/") {
		return
	}
	steps, _ := plan["steps"].([]any)
	for _, s := range steps {
		step, _ := s.(map[string]any)
		commands, _ := step["commands"].([]any)
		for _, c := range commands {
			cmd, _ := c.(map[string]any)
			line, _ := cmd["cmd"].(string)
			if m := binCopy.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[1] == fields[0] {
				deploy["startCommand"] = "./bin/" + strings.TrimSpace(start)
				return
			}
		}
	}
}

// dropCopiesFromMissingDirs removes a step's copies of an optional file from
// the repository whose directory the repository does not have. A copy from an
// image, or of a path with no wildcard, is kept: that one is meant to exist.
func dropCopiesFromMissingDirs(step map[string]any, contextDir string) {
	commands, ok := step["commands"].([]any)
	if !ok {
		return
	}
	kept := commands[:0]
	for _, c := range commands {
		cmd, ok := c.(map[string]any)
		src, isCopy := cmd["src"].(string)
		_, fromImage := cmd["image"]
		if ok && isCopy && !fromImage && strings.Contains(src, "*") {
			dir := path.Dir(path.Clean("/" + src))
			if dir != "/" {
				if info, err := os.Stat(filepath.Join(contextDir, filepath.FromSlash(dir))); err != nil || !info.IsDir() {
					continue
				}
			}
		}
		kept = append(kept, c)
	}
	step["commands"] = kept
}

// railpackPins maps the tags Railpack's plans name to the digests Pando pins.
var railpackPins = map[string]string{
	strings.SplitN(railpackBuilderImage, "@", 2)[0]: railpackBuilderImage,
	strings.SplitN(railpackRuntimeImage, "@", 2)[0]: railpackRuntimeImage,
}

// pinImages rewrites every "image" in a plan that names one of Railpack's own
// images by tag to the pinned digest. An image the app's toolchain chose, such
// as PHP's FrankenPHP release, is left as the plan names it.
func pinImages(v any) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if s, ok := child.(string); ok && k == "image" {
				if pinned, ok := railpackPins[s]; ok {
					node[k] = pinned
				}
				continue
			}
			pinImages(child)
		}
	case []any:
		for _, child := range node {
			pinImages(child)
		}
	}
}

// railpackPlanView is the part of a Railpack plan Pando reads back.
type railpackPlanView struct {
	Steps []struct {
		Name     string `json:"name"`
		Commands []struct {
			Cmd string `json:"cmd"`
		} `json:"commands"`
	} `json:"steps"`
	Deploy struct {
		StartCommand string `json:"startCommand"`
	} `json:"deploy"`
}

// railpackBuildCommand is the build step Railpack chose, as one command, or ""
// when the plan has none.
func railpackBuildCommand(body []byte) string {
	var plan railpackPlanView
	if json.Unmarshal(body, &plan) != nil {
		return ""
	}
	for _, step := range plan.Steps {
		if step.Name != "build" {
			continue
		}
		var cmds []string
		for _, c := range step.Commands {
			if cmd := unwrapShell(c.Cmd); cmd != "" {
				cmds = append(cmds, cmd)
			}
		}
		return strings.Join(cmds, " && ")
	}
	return ""
}

// unwrapShell undoes the `sh -c '…'` Railpack wraps a configured command in.
// Pando's commands carry no single quote (safeCommand), so the wrapper is the
// only one there is.
func unwrapShell(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if inner, ok := strings.CutPrefix(cmd, "sh -c '"); ok && strings.HasSuffix(inner, "'") {
		return strings.TrimSuffix(inner, "'")
	}
	return cmd
}

// railpackStartCommand is the command the plan's image starts with.
func railpackStartCommand(body []byte) string {
	var plan railpackPlanView
	if json.Unmarshal(body, &plan) != nil {
		return ""
	}
	return plan.Deploy.StartCommand
}

// railpackBuild is the client-side build that hands a plan to Railpack's
// frontend.
//
// A gateway frontend takes no `label:` options — those are the Dockerfile
// frontend's — so the bundle label every image carries is added to the image
// config the frontend returns. Without it a deleted app's images could not be
// found again (issue #55).
//
// Every option the solve was given is forwarded, which is how the frontend
// learns the plan's filename, the per-app cache key and the cache to import:
// BuildKit's client puts the cache imports into the options rather than
// applying them to a frontend's solve, and Railpack's frontend reads them back
// out.
func railpackBuild(labels map[string]string) gateway.BuildFunc {
	return func(ctx context.Context, c gateway.Client) (*gateway.Result, error) {
		opts := map[string]string{}
		for k, v := range c.BuildOpts().Opts {
			opts[k] = v
		}
		opts["source"] = railpackFrontendImage

		res, err := c.Solve(ctx, gateway.SolveRequest{
			Frontend:    "gateway.v0",
			FrontendOpt: opts,
		})
		if err != nil {
			return nil, err
		}
		if err := labelImage(res, labels); err != nil {
			return nil, err
		}
		return res, nil
	}
}

// imageConfigKey is where a frontend puts the image's config in a result,
// exptypes.ExporterImageConfigKey.
const imageConfigKey = "containerimage.config"

const (
	msgUnreadableConfig = "The build produced an image configuration Pando could not read."
	msgCannotLabel      = "Could not label the image."
)

// labelImage adds labels to the image config in a frontend's result, keeping
// every other field as it came.
func labelImage(res *gateway.Result, labels map[string]string) error {
	raw := res.Metadata[imageConfigKey]
	if len(raw) == 0 {
		return errs.New(errs.BuildFailed, "The build produced no image configuration.")
	}

	var image map[string]json.RawMessage
	if err := json.Unmarshal(raw, &image); err != nil {
		return errs.Wrap(errs.BuildFailed, msgUnreadableConfig, err)
	}
	var config map[string]json.RawMessage
	if body, ok := image["config"]; ok {
		if err := json.Unmarshal(body, &config); err != nil {
			return errs.Wrap(errs.BuildFailed, msgUnreadableConfig, err)
		}
	}
	if config == nil {
		config = map[string]json.RawMessage{}
	}
	existing := map[string]string{}
	if body, ok := config["Labels"]; ok {
		_ = json.Unmarshal(body, &existing)
	}
	if existing == nil {
		existing = map[string]string{}
	}
	for k, v := range labels {
		existing[k] = v
	}

	var err error
	if config["Labels"], err = json.Marshal(existing); err != nil {
		return errs.Wrap(errs.BuildFailed, msgCannotLabel, err)
	}
	if image["config"], err = json.Marshal(config); err != nil {
		return errs.Wrap(errs.BuildFailed, msgCannotLabel, err)
	}
	out, err := json.Marshal(image)
	if err != nil {
		return errs.Wrap(errs.BuildFailed, msgCannotLabel, err)
	}
	res.AddMeta(imageConfigKey, out)
	return nil
}
