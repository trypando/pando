package detect_test

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// memSource is an in-memory SourceView, so detection can be tested against
// exact repository shapes rather than whatever a real repo happens to contain.
type memSource map[string]string

func (m memSource) Open(name string) (io.ReadCloser, error) {
	content, ok := m[path.Clean(name)]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (m memSource) Stat(name string) (api.FileInfo, error) {
	clean := path.Clean(name)
	if content, ok := m[clean]; ok {
		return api.FileInfo{Name: path.Base(clean), Size: int64(len(content))}, nil
	}
	if isDir, ok := m.paths()[clean]; ok && isDir {
		return api.FileInfo{Name: path.Base(clean), IsDir: true}, nil
	}
	return api.FileInfo{}, io.EOF
}

// Glob mirrors source.dirView.Glob, which matches a pattern against the
// relative path as well as the basename. Implied parent directories are walked
// too, so that a pattern like "apps/*" sees them.
func (m memSource) Glob(pattern string) ([]string, error) {
	var out []string
	for name := range m.paths() {
		if ok, _ := path.Match(pattern, name); ok {
			out = append(out, name)
			continue
		}
		if ok, _ := path.Match(pattern, path.Base(name)); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// paths returns every file plus the directories they imply.
func (m memSource) paths() map[string]bool {
	all := map[string]bool{}
	for name := range m {
		all[name] = false
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			all[dir] = true
		}
	}
	return all
}

func auction() *detect.Auction {
	return detect.NewAuction(
		detect.DockerfileDetector{},
		detect.ComposeDetector{},
		detect.StaticDetector{},
		detect.BuildpackDetector{},
		detect.MonorepoDetector{},
	)
}

// --- R-105, the hard one ---------------------------------------------------

// TestR105_EveryQuestionAnyDetectorProducesIsSelfContained asserts R-105 across
// every repository shape the detectors recognize.
//
// This is the check the phase plan calls a hard content requirement rather than
// a style note. A question is pasted into the assistant that wrote the app, so
// it has to be answerable by something that cannot see the repository — and the
// only way to keep that true is to assert it on every question, not to review
// them once.
func TestR105_EveryQuestionAnyDetectorProducesIsSelfContained(t *testing.T) {
	shapes := map[string]memSource{
		"dockerfile at root":        {"Dockerfile": "FROM alpine\nCMD [\"sh\"]\n"},
		"dockerfile without expose": {"Dockerfile": "FROM alpine\n"},
		"dockerfiles nested only": {
			"services/api/Dockerfile": "FROM alpine\n",
			"services/web/Dockerfile": "FROM alpine\n",
		},
		"compose with several services": {
			"docker-compose.yml": "services:\n  web:\n    image: nginx\n  db:\n    image: postgres:17\n",
		},
		"static site":     {"index.html": "<h1>hi</h1>"},
		"node project":    {"package.json": `{"name":"app"}`},
		"python project":  {"requirements.txt": "flask\n"},
		"go project":      {"go.mod": "module example.com/app\n"},
		"nothing at all":  {"README.md": "# hello"},
		"static and node": {"index.html": "<h1>hi</h1>", "package.json": `{"name":"app"}`},
	}

	for name, src := range shapes {
		result, err := auction().Run(context.Background(), src)
		require.NoError(t, err, name)

		require.NoError(t, detect.ValidateAll(result.Questions),
			"%s produced a question that does not meet R-105", name)

		// Every question is something a person has to answer before their app
		// runs, and R-005 says they may not know what a port is.
		require.LessOrEqual(t, len(result.Questions), 3,
			"%s asks %d questions; each one is a barrier", name, len(result.Questions))
	}
}

// The validator has to actually reject the design's own counter-example, or it
// is decoration.
func TestR105_ValidatorRejectsTheDesignsCounterExample(t *testing.T) {
	bad := detect.Question{
		Key: "port", Kind: api.QuestionPort,
		Prompt: "Which port?",
		Why:    "Pando needs it.",
	}
	err := bad.Validate()
	require.Error(t, err, `"Which port?" is the design's example of a question that fails R-105`)

	good := detect.Question{
		Key: "port", Kind: api.QuestionPort,
		Prompt: "This app appears to be a Node.js service. Pando could not determine which port it " +
			"serves HTTP on. Valid answer: a port number such as 3000.",
		Why: "Pando needs to know where to send traffic once the app is running.",
	}
	require.NoError(t, good.Validate(), "the design's passing example must pass")
}

func TestR105_ValidatorCatchesTheRealFailureModes(t *testing.T) {
	cases := map[string]detect.Question{
		"assumes the reader can see the repo": {
			Key: "k", Why: "because",
			Prompt: "Pando could not determine the port for the service defined in this file. " +
				"Valid answer: a port number such as 3000.",
		},
		"never says what an answer looks like": {
			Key: "k", Why: "because",
			Prompt: "This app appears to be a Node.js service and Pando could not determine which " +
				"port it serves its web interface on, so it needs to be told before deploying.",
		},
		"no why": {
			Key: "k",
			Prompt: "This app appears to be a Node.js service. Pando could not determine which port " +
				"it serves HTTP on. Valid answer: a port number such as 3000.",
		},
		"choice with nothing to choose": {
			Key: "k", Why: "because", Kind: api.QuestionChoice,
			Prompt: "Pando found several services and could not tell which one serves the web " +
				"interface. Valid answer: one of the service names.",
		},
	}

	for name, q := range cases {
		require.Error(t, q.Validate(), "should reject: %s", name)
	}
}

// --- the auction -----------------------------------------------------------

// R-093: the user sees the auction, not a verdict.
func TestR093_RunnersUpAreReturned(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"index.html":   "<h1>hi</h1>",
		"package.json": `{"name":"app"}`,
	})
	require.NoError(t, err)

	require.NotEmpty(t, result.RunnersUp, "a losing bid is shown, not discarded")
	require.NotEmpty(t, result.Winner.Evidence, "the winner says why it won")
	for _, c := range result.RunnersUp {
		require.NotEmpty(t, c.Detector)
		require.NotEmpty(t, c.Evidence, "a runner-up says why it bid too")
	}
}

// R-102: a close call is asked about, not settled.
func TestR102_ACloseCallBecomesAQuestion(t *testing.T) {
	// A repository that is plausibly a static site and plausibly a Node app.
	result, err := auction().Run(context.Background(), memSource{
		"index.html":   "<h1>hi</h1>",
		"package.json": `{"name":"app","scripts":{"build":"vite build"}}`,
	})
	require.NoError(t, err)

	require.Equal(t, detect.StatusNeedsAnswers, result.Status)

	var asked bool
	for _, q := range result.Questions {
		if q.Key == "build_strategy" || q.Key == "start_command" {
			asked = true
		}
	}
	require.True(t, asked, "an ambiguous repository produces a question rather than a pick")
}

func TestDockerfileAtRootWins(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"Dockerfile":   "FROM node:20\nEXPOSE 3000\nCMD [\"npm\",\"start\"]\n",
		"package.json": `{"name":"app"}`,
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildDockerfile, result.Winner.Strategy,
		"a Dockerfile says how to build this app; a package.json only says what language it is in")
	require.Greater(t, result.Winner.Confidence, 0.9)
	require.Equal(t, detect.StatusReady, result.Status, "nothing left to ask")
	require.Empty(t, result.Questions)

	// EXPOSE is the author's declaration, not a guess — and the review UI shows
	// which (design 01 §2.3).
	ports := result.Winner.Draft.Workloads[0].Ports
	require.Len(t, ports, 1)
	require.Equal(t, 3000, ports[0].Number)
	require.Equal(t, spec.PortExpose, ports[0].Source)
}

func TestComposeBeatsNestedDockerfiles(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"docker-compose.yml": "services:\n  web:\n    build: ./web\n  worker:\n    build: ./worker\n",
		"web/Dockerfile":     "FROM node:20\n",
		"worker/Dockerfile":  "FROM node:20\n",
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildCompose, result.Winner.Strategy,
		"the compose file says how the parts fit together, which the Dockerfiles do not")
}

// R-026: one canonical endpoint. Which service that is, the file does not say.
func TestComposeAsksWhichServiceIsPrimary(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"docker-compose.yml": "services:\n  web:\n    image: nginx\n  admin:\n    image: nginx\n",
	})
	require.NoError(t, err)

	var q *detect.Question
	for i := range result.Questions {
		if result.Questions[i].Key == "primary_service" {
			q = &result.Questions[i]
		}
	}
	require.NotNil(t, q, "Pando must ask which service the app's URL points at")
	require.ElementsMatch(t, []string{"web", "admin"}, q.Options,
		"the question names the candidates it found")
	require.Contains(t, q.Prompt, "web")
	require.Contains(t, q.Prompt, "admin")
}

// R-021: fill declared slots, never invent topology.
func TestR021_SlotsComeFromWhatTheRepoDeclares(t *testing.T) {
	withDatabase, err := auction().Run(context.Background(), memSource{
		"docker-compose.yml": "services:\n  web:\n    image: nginx\n    environment:\n      DATABASE_URL: postgres://app:pw@db:5432/app\n  db:\n    image: postgres:17\n",
	})
	require.NoError(t, err)
	require.NotEmpty(t, withDatabase.Winner.Draft.Slots, "a declared postgres service the app connects to by URL becomes a slot")
	require.Equal(t, spec.SlotPostgres, withDatabase.Winner.Draft.Slots[0].Type)
	require.NotEmpty(t, withDatabase.Winner.Draft.Slots[0].Evidence, "and says why")

	// The same app without the declaration gets no slot invented for it.
	withoutDatabase, err := auction().Run(context.Background(), memSource{
		"docker-compose.yml": "services:\n  web:\n    image: nginx\n",
	})
	require.NoError(t, err)
	require.Empty(t, withoutDatabase.Winner.Draft.Slots,
		"Pando does not decide an app needs a database because it looks like it might")
}

// O-4's [P] fallback: default to optional, let the trial run promote what
// actually breaks (design 01 §2.5).
func TestO4_UnrecognizedEnvKeysStartOptional(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"package.json": `{"name":"app"}`,
		".env.example": "DATABASE_URL=\nREDIS_URL=redis://localhost:6379\n",
	})
	require.NoError(t, err)

	byKey := map[string]spec.Slot{}
	for _, s := range result.Winner.Draft.Slots {
		byKey[s.Key] = s
	}

	require.True(t, byKey["DATABASE_URL"].Required, "a dependency the file gives no value for")
	require.False(t, byKey["REDIS_URL"].Required,
		"one with a sample value starts optional — the trial run promotes it if its absence breaks the app")
}

// TestR130_AValueIsNotADependency asserts R-130.
//
// A variable in `.env.example` is a hole with a type, and the type says what
// kind of hole. `REDIS_URL` names a Redis somewhere, which is a dependency:
// Pando can run one, connect to one, or take a connection string (R-131).
// `VAPID_PRIVATE_KEY` names a value. There is nothing to connect it to.
//
// Every key became a slot, so an app arrived declaring its push-notification
// keys as dependencies of type unknown, each offering "connect to one that
// already exists" — and `POSTGRES_PASSWORD` offered to connect a password to a
// database, because the name contains the word postgres.
func TestR130_AValueIsNotADependency(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"package.json": `{"name":"app"}`,
		".env.example": "DATABASE_URL=\n" +
			"POSTGRES_PASSWORD=changeme\n" +
			"VAPID_PUBLIC_KEY=\n" +
			"VAPID_PRIVATE_KEY=\n" +
			"LOG_LEVEL=info\n" +
			"UPGRADE_URL=https://example.test/upgrade\n" +
			"CACHE=redis://localhost:6379\n",
	})
	require.NoError(t, err)

	draft := result.Winner.Draft

	slots := map[string]spec.SlotType{}
	for _, slot := range draft.Slots {
		slots[slot.Key] = slot.Type
	}
	require.Equal(t, map[string]spec.SlotType{
		"DATABASE_URL": spec.SlotPostgres,
		// Typed by its value, not its name: a URL scheme is direct evidence.
		"CACHE": spec.SlotRedis,
	}, slots)

	// Everything else is a variable, declared with nothing in it, where a
	// person fills it in.
	env := map[string]string{}
	for _, e := range draft.Workloads[0].Env {
		require.NotNil(t, e.Value)
		env[e.Key] = *e.Value
		require.Equal(t, spec.EnvFromDetection, e.Source)
	}
	require.Equal(t, map[string]string{
		"POSTGRES_PASSWORD": "",
		"VAPID_PUBLIC_KEY":  "",
		"VAPID_PRIVATE_KEY": "",
		"LOG_LEVEL":         "",
		// A URL, and nothing Pando runs. "UPGRADE" contains "PG" and is not a
		// database.
		"UPGRADE_URL": "",
	}, env)

	// The sample value is not carried in: `POSTGRES_PASSWORD=changeme` filled
	// in is worse than empty, because it looks answered.
	require.Empty(t, env["POSTGRES_PASSWORD"])
}

// R-102 at its most tempting: a repository with almost nothing in it.
func TestAnUnrecognizableRepositoryAsksRatherThanGuesses(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"README.md": "# hello"})
	require.NoError(t, err)

	require.Equal(t, detect.StatusUnknown, result.Status)
	require.Zero(t, result.Winner.Confidence, "no detector pretends to recognize it")
	require.Len(t, result.Questions, 1)
	require.NoError(t, result.Questions[0].Validate())
	require.Contains(t, result.Questions[0].Prompt, "Valid answer")
}

// A language is not a deployment, and the bid should say so.
func TestALanguageAloneIsALowConfidenceBid(t *testing.T) {
	for file, language := range map[string]string{
		"package.json":     "Node.js",
		"requirements.txt": "Python",
		"go.mod":           "Go",
		"Gemfile":          "Ruby",
	} {
		result, err := auction().Run(context.Background(), memSource{file: "x"})
		require.NoError(t, err)

		require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy, file)
		require.Less(t, result.Winner.Confidence, 0.6,
			"%s tells Pando the language, not how to run the app", file)
		require.Equal(t, detect.StatusNeedsAnswers, result.Status, file)
		require.Contains(t, result.Winner.Evidence[0], language)
	}
}

// R-021 again: a Dockerfile that is not at the root is not automatically the one.
func TestNestedDockerfilesAreAskedAboutNotPicked(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"services/api/Dockerfile": "FROM alpine\n",
		"services/web/Dockerfile": "FROM alpine\n",
	})
	require.NoError(t, err)

	require.Equal(t, detect.StatusNeedsAnswers, result.Status)
	require.NotEmpty(t, result.Questions)
	require.Contains(t, result.Questions[0].Options, "services/api/Dockerfile")
	require.Contains(t, result.Questions[0].Options, "services/web/Dockerfile")
}

// A detector that panics or errors must not take detection down with it.
func TestOneFailingDetectorDoesNotFailDetection(t *testing.T) {
	a := detect.NewAuction(failingDetector{}, detect.DockerfileDetector{})

	result, err := a.Run(context.Background(), memSource{"Dockerfile": "FROM alpine\nEXPOSE 80\n"})
	require.NoError(t, err)
	require.Equal(t, spec.BuildDockerfile, result.Winner.Strategy)
}

type failingDetector struct{}

func (failingDetector) Name() string { return "broken" }
func (failingDetector) Bid(context.Context, api.SourceView) (detect.Candidate, error) {
	return detect.Candidate{}, io.ErrUnexpectedEOF
}

// --- what the corpus caught -------------------------------------------------
//
// The three tests below are each a real detection failure the corpus found on
// its first run against real repositories. They are kept as unit tests because
// the corpus needs the network and these do not: a regression should fail on a
// laptop, not only in CI.

// A Dockerfile under examples/ or fixtures/ builds something the repository is
// demonstrating, not the repository.
//
// vercel/turbo carries five Dockerfiles and not one of them builds turbo. The
// detector bid 0.45 and offered a choice between all five — five wrong answers
// presented as the way forward, to a person R-005 says may not know what a
// Dockerfile is. Saying nothing is better.
func TestADockerfileInAnExampleIsNotEvidenceOfHowToBuild(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"examples/with-docker/apps/web/Dockerfile": "FROM alpine\n",
		"examples/with-docker/apps/api/Dockerfile": "FROM alpine\n",
		".devcontainer/Dockerfile":                 "FROM alpine\n",
		"lockfile-tests/fixtures/a/Dockerfile":     "FROM alpine\n",
		"README.md":                                "# turbo\n",
	})
	require.NoError(t, err)

	require.NotEqual(t, spec.BuildDockerfile, result.Winner.Strategy,
		"Dockerfiles that only exist under examples/, fixtures/ and .devcontainer/ "+
			"are not evidence that this repository builds with a Dockerfile")
	for _, q := range result.Questions {
		require.NotContains(t, q.Options, "examples/with-docker/apps/web/Dockerfile",
			"an example project must not be offered as something to deploy")
	}
}

// A workspace declaration is the repository saying it holds several projects.
func TestAWorkspaceDeclarationIsNotOneApp(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"pnpm-workspace.yaml":      "packages:\n  - apps/*\n",
		"package.json":             `{"name":"turbo","private":true}`,
		"turbo.json":               "{}",
		"apps/web/package.json":    `{"name":"web"}`,
		"apps/docs/package.json":   `{"name":"docs"}`,
		"packages/ui/package.json": `{"name":"ui"}`,
	})
	require.NoError(t, err)

	require.Equal(t, detect.StrategyUnknown, result.Winner.Strategy)
	require.Equal(t, detect.StatusNeedsAnswers, result.Status)
	require.NoError(t, detect.ValidateAll(result.Questions))

	// R-105: the question has to name what it found, because the person reading
	// it — or the assistant they paste it into — cannot see the repository.
	require.Len(t, result.Questions, 1)
	require.ElementsMatch(t,
		[]string{"apps/docs", "apps/web", "packages/ui"},
		result.Questions[0].Options)
}

// The veto stands down when the repository does say what to deploy.
func TestAWorkspaceWithARootDockerfileIsStillBuildable(t *testing.T) {
	for name, src := range map[string]memSource{
		"root Dockerfile": {
			"pnpm-workspace.yaml": "packages:\n  - apps/*\n",
			"Dockerfile":          "FROM alpine\nEXPOSE 8080\n",
			"apps/web/index.js":   "",
		},
		"root compose file": {
			"pnpm-workspace.yaml": "packages:\n  - apps/*\n",
			"compose.yaml":        "services:\n  web:\n    image: nginx\n",
			"apps/web/index.js":   "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := auction().Run(context.Background(), src)
			require.NoError(t, err)
			require.NotEqual(t, detect.StrategyUnknown, result.Winner.Strategy,
				"a root build artifact answers the question the monorepo veto would ask")
		})
	}
}

// Where an index.html sits decides what a package.json beside it means.
//
// h5bp/html5-boilerplate keeps its built site in dist/. A blanket "package.json
// means this needs building" penalty sent it to a buildpack that would have
// tried to npm-start a folder of HTML.
func TestWhereIndexHTMLSitsDecidesWhatPackageJSONMeans(t *testing.T) {
	buildScript := `{"name":"site","scripts":{"build":"gulp build"}}`

	t.Run("committed build output wins, and asks whether it is stale", func(t *testing.T) {
		result, err := auction().Run(context.Background(), memSource{
			"dist/index.html": "<!doctype html>",
			"package.json":    buildScript,
			"src/index.html":  "<!doctype html>",
		})
		require.NoError(t, err)

		require.Equal(t, spec.BuildStatic, result.Winner.Strategy)
		require.GreaterOrEqual(t, result.Winner.Confidence, 0.5)
		require.Len(t, detect.Asked(result.Questions), 1,
			"a committed copy beside a build command is a real fork and worth one question")
		require.NoError(t, detect.ValidateAll(result.Questions))
	})

	t.Run("a root index.html beside a build command is source, not output", func(t *testing.T) {
		result, err := auction().Run(context.Background(), memSource{
			"index.html":   `<script type="module" src="/src/main.jsx"></script>`,
			"package.json": buildScript,
			"src/main.jsx": "",
		})
		require.NoError(t, err)

		require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy,
			"serving an unbuilt Vite root produces a blank page, so static must lose outright")
		for _, q := range result.Questions {
			require.NotEqual(t, "build_strategy", q.Key,
				"static should not land close enough to buildpack to make the auction ask")
		}
	})

	t.Run("no build command leaves it genuinely uncertain", func(t *testing.T) {
		result, err := auction().Run(context.Background(), memSource{
			"index.html":   "<!doctype html>",
			"package.json": `{"name":"site","dependencies":{"normalize.css":"^8"}}`,
		})
		require.NoError(t, err)
		require.Equal(t, detect.StatusNeedsAnswers, result.Status)
	})
}

// R-097: a question the trial run answers is not a question a person answers.
func TestPortQuestionsAreDeferredToTheTrialRun(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"Dockerfile": "FROM alpine\nCMD [\"sh\"]\n",
	})
	require.NoError(t, err)

	var port detect.Question
	for _, q := range result.Questions {
		if q.Key == "primary_port" {
			port = q
		}
	}
	require.Equal(t, "primary_port", port.Key,
		"a Dockerfile that declares no EXPOSE should raise the port question")
	require.True(t, port.Deferred,
		"R-097 exists so Pando watches the app bind rather than asking someone who may not know what a port is")

	require.NotContains(t, detect.Asked(result.Questions), port)

	// Deferred is not discarded. The trial run can fail to observe a port, and
	// then it becomes a real question — so it has to still meet R-105.
	require.NoError(t, port.Validate())
}

// R-104: a framework default is configuration, not a question.
//
// A language bid used to raise a port question and defer it to the trial run.
// That works when the app arrives with an image, and a source build has none:
// the trial has nothing to start, so the question was promoted to one a person
// had to answer — a question about a port, put to someone R-005 says may not
// know what a port is, on every repository with no Dockerfile.
//
// The default belongs in the draft, where the review screen shows it, says it
// came from the framework rather than from watching, and lets it be changed.
func TestR104_AFrameworkPortIsADefaultRatherThanAQuestion(t *testing.T) {
	for _, tc := range []struct {
		file string
		body string
		port int
	}{
		{"go.mod", "module app\n", 8080},
		{"package.json", `{"name":"app"}`, 3000},
		{"requirements.txt", "flask\n", 8000},
	} {
		t.Run(tc.file, func(t *testing.T) {
			result, err := auction().Run(context.Background(), memSource{tc.file: tc.body})
			require.NoError(t, err)

			for _, q := range result.Questions {
				require.NotEqual(t, api.QuestionPort, q.Kind,
					"a language with a known default port must not ask about it, deferred or otherwise")
			}

			require.Equal(t,
				[]spec.Port{{Number: tc.port, Protocol: "http", Source: spec.PortFramework}},
				result.Winner.Draft.Workloads[0].Ports,
				"the default rides in the draft, marked as the guess it is")

			// Still visible. A default nobody is told about is one that looks
			// like Pando knew something it did not.
			require.Contains(t, strings.Join(result.Winner.Evidence, "\n"), fmt.Sprint(tc.port))
		})
	}
}

// R-104: a start command the build plan worked out is not a question either.
//
// The question and the plan used to be produced two lines apart, so a prompt
// opening "it does not include a Dockerfile or any other instructions for
// running it" was shown about a repository Pando had just written a Dockerfile
// for, with the start command in it.
func TestR104_AStartCommandFromTheBuildPlanIsNotAsked(t *testing.T) {
	withPlan := detect.NewAuction(detect.BuildpackDetector{Planner: plannerWritingCMD{}})
	planned, err := withPlan.Run(context.Background(), memSource{"go.mod": "module app\n"})
	require.NoError(t, err)

	require.Empty(t, detect.Asked(planned.Questions),
		"the plan says how to build it and how to start it, so there is nothing left to ask")
	require.Contains(t, strings.Join(planned.Winner.Evidence, "\n"), `["./out"]`,
		"what the plan decided is evidence, and it is what the app will run")

	// Without one, the question is real and comes back.
	bare, err := detect.NewAuction(detect.BuildpackDetector{}).
		Run(context.Background(), memSource{"go.mod": "module app\n"})
	require.NoError(t, err)

	asked := detect.Asked(bare.Questions)
	require.Len(t, asked, 1)
	require.Equal(t, "start_command", asked[0].Key,
		"with nothing to work it out from, Pando has to ask")
	require.NoError(t, asked[0].Validate())
}

// plannerWritingCMD stands in for nixpacks: it writes a multi-stage Dockerfile
// whose build stage carries its own ENTRYPOINT, which is the shape that made
// reading the first start instruction rather than the last report
// `/bin/bash -l -c` as the way to run the app.
type plannerWritingCMD struct{}

func (plannerWritingCMD) Plan(context.Context, api.SourceView) (map[string]string, string, *api.PlanDeclaration, error) {
	return map[string]string{
		".nixpacks/Dockerfile": "FROM ubuntu:noble\n" +
			"ENTRYPOINT [\"/bin/bash\", \"-l\", \"-c\"]\n" +
			"RUN nix-env -if .nixpacks/nixpkgs-e89cf1c9.nix && nix-collect-garbage -d\n" +
			"RUN --mount=type=cache,id=x,target=/root/.cache/go-build go mod download\n" +
			"RUN --mount=type=cache,id=x,target=/root/.cache/go-build go build -o out ./cmd/server\n\n" +
			"FROM ubuntu:noble\n" +
			"ENTRYPOINT [\"/bin/bash\", \"-l\", \"-c\"]\n" +
			"WORKDIR /app/\n" +
			"CMD [\"./out\"]\n",
	}, ".nixpacks/Dockerfile", nil, nil
}

// --- compose import (R-096, R-099) ------------------------------------------

const composeStack = `
services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
    depends_on:
      - db
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/health"]
      interval: 30s
      timeout: 5s
      retries: 3
    environment:
      DATABASE_URL: postgres://db:5432/app
    restart: unless-stopped
  db:
    image: postgres:16
    volumes:
      - dbdata:/var/lib/postgresql/data
volumes:
  dbdata:
`

// R-096: a compose file is a complete answer, so it is imported rather than
// used as evidence for a guess.
func TestR096_AComposeFileIsImportedNotInterpreted(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeStack})
	require.NoError(t, err)
	require.Equal(t, spec.BuildCompose, result.Winner.Strategy)

	draft := result.Winner.Draft

	// One workload, not two: the `db` service is a PostgreSQL, which Pando
	// supplies rather than runs (see TestR131_AComposeDatabaseBecomesTheOneP\
	// andoProvisions below).
	require.Len(t, draft.Workloads, 1)

	web := draft.Workloads[0]
	require.Equal(t, "web", web.Name)
	require.Equal(t, "nginx:alpine", web.Image)
	require.Empty(t, web.DependsOn, "nothing to wait for that Pando does not start first")

	require.NotNil(t, web.Health, "healthchecks are imported")
	require.Equal(t, []string{"curl", "-f", "http://localhost/health"}, web.Health.Command,
		"the CMD prefix says how to run the test, not what to run")
	require.Equal(t, 30, web.Health.IntervalSeconds)
	require.Equal(t, 5, web.Health.TimeoutSeconds)
	require.Equal(t, 3, web.Health.Retries)

	// `dbdata` belonged to the database Pando now provisions, and the
	// provisioned service brings its own storage. An empty volume nothing
	// writes to is still a volume somebody has to answer for at delete time
	// (R-204).
	require.Empty(t, draft.Volumes)

	// R-131: a service running postgres is a dependency the app declares.
	require.True(t, hasSlot(draft.Slots, spec.SlotPostgres))
}

// TestR131_AComposeDatabaseBecomesTheOnePandoProvisions asserts R-131.
//
// `db: image: postgres:16` says this app needs a PostgreSQL beside it, and
// `DATABASE_URL: postgres://db:5432/app` says which variable reaches it. Pando
// provisions the database, fills that variable from it, and does not also run
// the compose container.
//
// Importing both was two databases: the compose one, which the app connected to
// with whatever password the file interpolated from a shell that does not exist
// here, and the one Pando provisioned for the slot it had also created. The
// app's logs were `DATABASE_URL is required`, and the fix for that was
// nowhere on the screen.
func TestR131_AComposeDatabaseBecomesTheOnePandoProvisions(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeStack})
	require.NoError(t, err)

	draft := result.Winner.Draft
	for _, w := range draft.Workloads {
		require.NotEqual(t, "db", w.Name, "Pando runs this one itself")
	}

	require.Len(t, draft.Slots, 1)
	slot := draft.Slots[0]

	// Named for the variable the app reads, not for the compose service: a
	// name somebody will recognize beats one Pando made up.
	require.Equal(t, "DATABASE_URL", slot.Key)
	require.Equal(t, spec.SlotPostgres, slot.Type)
	require.NotNil(t, slot.Resolution)
	require.Equal(t, spec.ResolutionProvisioned, slot.Resolution.Mode)

	// And the variable is filled from it rather than pointing at a host that
	// is no longer there.
	var wired bool
	for _, e := range draft.Workloads[0].Env {
		if e.Key != "DATABASE_URL" {
			continue
		}
		wired = true
		require.Nil(t, e.Value, "the compose literal is gone")
		require.NotNil(t, e.SlotRef)
		require.Equal(t, "DATABASE_URL", *e.SlotRef)
	}
	require.True(t, wired)

	// Said out loud, because it is a change to how the app runs (R-102).
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "DATABASE_URL"))
}

// The container's port is kept; the host's is replaced by Pando's routing.
func TestR099_APublishedHostPortIsRewrittenNotHonored(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeStack})
	require.NoError(t, err)

	web := result.Winner.Draft.Workloads[0]
	require.Equal(t, []spec.Port{{Number: 80, Protocol: "http", Source: spec.PortCompose}}, web.Ports,
		"80 is what the service listens on; 8080 is a host publishing Pando replaces")

	require.True(t, hasWarning(result.Winner.Draft.Warnings, spec.WarnComposeConstructRewritten,
		"8080:80"), "a rewrite says exactly what changed")
	require.True(t, hasWarning(result.Winner.Draft.Warnings, spec.WarnComposeConstructRewritten,
		"restart: unless-stopped"))
}

// R-099: constructs that cannot cross the boundary are refused, with the reason.
func TestR099_ConstructsThatBreakTheBoundaryAreRejectedWithReasons(t *testing.T) {
	for name, service := range map[string]string{
		"host networking":     "    network_mode: host\n",
		"privileged":          "    privileged: true\n",
		"host PID":            "    pid: host\n",
		"host IPC":            "    ipc: host\n",
		"device pass-through": "    devices:\n      - /dev/kvm\n",
		"replicas":            "    deploy:\n      replicas: 3\n",
		"host bind mount":     "    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n",
	} {
		t.Run(name, func(t *testing.T) {
			result, err := auction().Run(context.Background(), memSource{
				"compose.yaml": "services:\n  web:\n    image: nginx\n" + service,
			})
			require.NoError(t, err, "a rejection is a result, not a failure of detection")

			require.Equal(t, detect.StatusBlocked, result.Status)
			require.Equal(t, spec.BuildCompose, result.Winner.Strategy,
				"a compose file at the root is still the right reading of the repository")

			require.Error(t, result.Blocked)
			e := errs.As(result.Blocked)
			require.Equal(t, errs.PlanComposeConstructRejected, e.Code)
			require.NotEmpty(t, e.Remedy, "a rejection a user cannot act on is just a wall")
			require.Contains(t, e.Details, "rejected")
		})
	}
}

// The reason must reach the user rather than being swallowed into a fallback.
func TestR099_ARejectedComposeFileDoesNotSilentlyBecomeABuildpackGuess(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  web:\n    image: nginx\n    privileged: true\n",
		"package.json": `{"name":"app"}`,
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildCompose, result.Winner.Strategy,
		"falling through to buildpack would hide the one useful thing Pando knows")
	require.Equal(t, detect.StatusBlocked, result.Status)
	require.Empty(t, result.Questions,
		"there is nothing to ask about a compose file that cannot be imported")
}

// TestR099_ARefusedComposeFileIsNotOutbidByTheDockerfile asserts R-099.
//
// A repository with both files bids twice, and the Dockerfile bids higher when
// it declares a port. So a compose file refused for one construct lost quietly
// and the app was imported as the Dockerfile alone — losing the database beside
// it, the variable that reached it, and every other service. The result built,
// deployed, and logged `DATABASE_URL is required` forever, with nothing on any
// screen about a compose file or why it had not been used.
func TestR099_ARefusedComposeFileIsNotOutbidByTheDockerfile(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"Dockerfile": "FROM node:20\nEXPOSE 8080\nCMD [\"node\", \"server.js\"]\n",
		"compose.yaml": "services:\n  app:\n    build: .\n    privileged: true\n" +
			"  db:\n    image: postgres:16\n",
	})
	require.NoError(t, err)

	require.Equal(t, detect.StatusBlocked, result.Status)
	require.Equal(t, spec.BuildCompose, result.Winner.Strategy)

	e := errs.As(result.Blocked)
	require.NotNil(t, e)
	require.Equal(t, errs.PlanComposeConstructRejected, e.Code)
	require.Contains(t, e.Message, "privileged")
}

// TestR096_AnEnvFileIsPartOfTheComposeFile asserts R-096.
//
// `env_file: .env` was read by nothing. A compose service that keeps its
// variables in a file got two of them — the ones spelled out under
// `environment:` — and none of the other eight, so the app deployed, started,
// and crash-looped on `APP_BASE_URL is required` with nothing in the console
// naming a variable at all.
//
// The file itself is almost never committed: it holds the app's own passwords
// and is the first line of a .gitignore. The template beside it is, so Pando
// takes the names from there and leaves the values empty — a hole, which is
// what R-130 says a declared variable is.
func TestR096_AnEnvFileIsPartOfTheComposeFile(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  app:\n    image: nginx\n    env_file: .env\n" +
			"    environment:\n      TRUST_PROXY: \"true\"\n",
		".env.example": "# Required\nAPP_BASE_URL=https://app.example.com\nVAPID_PUBLIC_KEY=\n",
	})
	require.NoError(t, err)

	env := map[string]string{}
	sources := map[string]spec.EnvSource{}
	slotRefs := map[string]bool{}
	for _, e := range result.Winner.Draft.Workloads[0].Env {
		sources[e.Key] = e.Source
		if e.SlotRef != nil {
			slotRefs[e.Key] = true
			continue
		}
		require.NotNil(t, e.Value)
		env[e.Key] = *e.Value
	}

	require.Equal(t, "true", env["TRUST_PROXY"])
	require.Equal(t, spec.EnvFromCompose, sources["TRUST_PROXY"])

	// Named, and empty: the sample value in a template is an example, not an
	// answer, and `APP_BASE_URL=https://app.example.com` filled in looks
	// answered.
	require.Contains(t, env, "APP_BASE_URL")
	require.Empty(t, env["APP_BASE_URL"])
	require.Equal(t, spec.EnvFromDetection, sources["APP_BASE_URL"])

	// A name the template leaves blank is a value the app has to be given, and
	// compose will not start without the file, so it is asked for before the
	// deploy rather than discovered when the app refuses to start (issue #55).
	require.True(t, slotRefs["VAPID_PUBLIC_KEY"])
	var vapid *spec.Slot
	for i, s := range result.Winner.Draft.Slots {
		if s.Key == "VAPID_PUBLIC_KEY" {
			vapid = &result.Winner.Draft.Slots[i]
		}
	}
	require.NotNil(t, vapid)
	require.True(t, vapid.Required)
	require.Nil(t, vapid.Resolution)

	// And it says where they came from and what is left to do (R-102).
	require.True(t, hasWarning(result.Winner.Draft.Warnings,
		spec.WarnComposeConstructRewritten, ".env.example"))
	require.True(t, hasWarning(result.Winner.Draft.Warnings,
		spec.WarnComposeConstructRewritten, "APP_BASE_URL"))
}

// A committed env file is read as written: those are values somebody chose to
// commit, and Pando does not second-guess them.
func TestAnEnvFileInTheRepositoryIsReadAsWritten(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  app:\n    image: nginx\n" +
			"    env_file:\n      - ./config/app.env\n",
		"config/app.env": "LOG_LEVEL=debug\n# a comment\nREGION=eu-west-1\n",
	})
	require.NoError(t, err)

	env := map[string]string{}
	for _, e := range result.Winner.Draft.Workloads[0].Env {
		env[e.Key] = *e.Value
	}
	require.Equal(t, "debug", env["LOG_LEVEL"])
	require.Equal(t, "eu-west-1", env["REGION"])
}

// `environment:` wins over `env_file:`, which is compose's own precedence.
func TestEnvironmentOverridesTheEnvFile(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  app:\n    image: nginx\n    env_file: .env\n" +
			"    environment:\n      LOG_LEVEL: warn\n",
		".env": "LOG_LEVEL=debug\n",
	})
	require.NoError(t, err)

	var count int
	for _, e := range result.Winner.Draft.Workloads[0].Env {
		if e.Key == "LOG_LEVEL" {
			count++
			require.Equal(t, "warn", *e.Value)
		}
	}
	require.Equal(t, 1, count, "one entry, not two with undefined precedence")
}

// A file inside the service's own build context is already in the image that
// build produces, so the mount is dropped rather than carried — and rather than
// refusing the import, which is what a 300 KB `package-lock.json` did.
//
// docker/awesome-compose's react-express-mysql is the shape: `build: backend`
// beside `./backend/package.json:/code/package.json`, the dev trick for seeing
// edits without rebuilding. Pando deploys a built image, so a deploy is how a
// change reaches the app.
func TestAFileTheBuildAlreadyProvidesIsNotCarried(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  backend:\n    build: backend\n" +
			"    volumes:\n      - ./backend/package.json:/code/package.json\n" +
			"      - ./config/app.conf:/etc/app.conf\n",
		"backend/package.json": `{"name":"backend"}`,
		"backend/Dockerfile":   "FROM node:20\n",
		"config/app.conf":      "listen = 8080\n",
	})
	require.NoError(t, err)
	require.NotEqual(t, detect.StatusBlocked, result.Status)

	files := result.Winner.Draft.Workloads[0].Files
	require.Len(t, files, 1, "only the one the build does not provide")
	require.Equal(t, "/etc/app.conf", files[0].Path)

	// And says what happened to the other, because it is a change to how the
	// app runs (R-102).
	require.True(t, hasWarning(result.Winner.Draft.Warnings,
		spec.WarnComposeConstructRewritten, "build context"))
}

// The build context is the whole repository when `build: .`, so everything in
// it is already in the image.
func TestAWholeRepositoryBuildContextCoversEverything(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  app:\n    build: .\n" +
			"    volumes:\n      - ./app.conf:/etc/app.conf\n",
		"Dockerfile": "FROM node:20\n",
		"app.conf":   "listen = 8080\n",
	})
	require.NoError(t, err)
	require.Empty(t, result.Winner.Draft.Workloads[0].Files)
}

// A relative bind mount is data until proven otherwise (R-203).
func TestARelativeBindMountBecomesAManagedVolume(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		// Not a database: that one is provisioned, and brings its own
		// storage.
		"compose.yaml": "services:\n  app:\n    image: nginx\n" +
			"    volumes:\n      - ./uploads:/var/lib/app/uploads\n",
	})
	require.NoError(t, err)
	require.NotEqual(t, detect.StatusBlocked, result.Status,
		"refusing this would refuse most real compose stacks")

	draft := result.Winner.Draft
	require.Len(t, draft.Volumes, 1)
	require.Len(t, draft.Workloads[0].Mounts, 1)
	require.Equal(t, "/var/lib/app/uploads", draft.Workloads[0].Mounts[0].Path)
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "./uploads"),
		"the rewrite names the path, so the user can see what moved")
}

// TestR020_ASingleFileBindMountIsCarriedInTheSpec asserts R-020.
//
// `./Caddyfile:/etc/caddy/Caddyfile` is not the same construct as
// `./pgdata:/var/lib/postgresql/data`, though compose writes them the same way.
// A directory of data becomes a volume Pando manages. A single file cannot:
// storage is a directory, and Docker will not mount one over a file in the
// image — which is how this used to surface, as `apply failed: Could not
// create "proxy".` at the last step of a deploy.
//
// So the file travels in the spec, read once here and placed in the container
// at every start. That is R-020 rather than an exception to it: the spec is the
// sole record of how the app runs, and nothing is read from the repository when
// it is deployed. Build.GeneratedFiles is the same idea one layer along.
func TestR020_ASingleFileBindMountIsCarriedInTheSpec(t *testing.T) {
	const caddyfile = ":80 {\n  respond \"ok\"\n}\n"

	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  proxy:\n    image: caddy:2\n" +
			"    volumes:\n      - ./Caddyfile:/etc/caddy/Caddyfile:ro\n",
		"Caddyfile": caddyfile,
	})
	require.NoError(t, err)
	require.NotEqual(t, detect.StatusBlocked, result.Status)

	draft := result.Winner.Draft
	require.Len(t, draft.Workloads, 1)
	proxy := draft.Workloads[0]

	require.Len(t, proxy.Files, 1)
	require.Equal(t, "/etc/caddy/Caddyfile", proxy.Files[0].Path)
	require.Equal(t, caddyfile, proxy.Files[0].Content, "the bytes, not a reference to them")

	// And not also a volume: two mechanisms for one path, one of which the
	// daemon refuses.
	require.Empty(t, proxy.Mounts)
	require.Empty(t, draft.Volumes)

	// A copy taken now is a copy taken now, and the warning says so — editing
	// the repository does nothing until the app is read again (R-102).
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "./Caddyfile"))
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "read again"))
}

// What cannot be carried is still refused, and says which of the two reasons it
// is. A spec is something a person reads and a database row holds.
func TestR099_AFileTooBigOrTooBinaryToCarryIsRefused(t *testing.T) {
	t.Run("too big", func(t *testing.T) {
		result, err := auction().Run(context.Background(), memSource{
			"compose.yaml": "services:\n  app:\n    image: nginx\n" +
				"    volumes:\n      - ./blob.json:/etc/app/blob.json\n",
			"blob.json": strings.Repeat("x", spec.FileSizeLimit+1),
		})
		require.NoError(t, err)
		require.Equal(t, detect.StatusBlocked, result.Status)

		e := errs.As(result.Blocked)
		rejected, ok := e.Details["rejected"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, rejected, 1)
		require.Contains(t, rejected[0]["reason"], "build input")
		require.Contains(t, rejected[0]["reason"], "COPY")
	})

	t.Run("not text", func(t *testing.T) {
		result, err := auction().Run(context.Background(), memSource{
			"compose.yaml": "services:\n  app:\n    image: nginx\n" +
				"    volumes:\n      - ./logo.png:/etc/app/logo.png\n",
			"logo.png": "\x89PNG\r\n\x1a\n\xff\xfe\xfd",
		})
		require.NoError(t, err)
		require.Equal(t, detect.StatusBlocked, result.Status)

		e := errs.As(result.Blocked)
		rejected, ok := e.Details["rejected"].([]map[string]any)
		require.True(t, ok)
		require.Contains(t, rejected[0]["reason"], "not a text file")
	})
}

// The same mount, with no such file in the repository, is the ordinary case:
// compose creates the directory on first run, and Pando manages it as a volume.
// Refusing on the shape of the path alone would refuse those too.
func TestAPathTheRepositoryDoesNotHoldIsStillADirectory(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  app:\n    image: nginx\n" +
			"    volumes:\n      - ./config:/etc/nginx/conf.d\n",
	})
	require.NoError(t, err)

	require.NotEqual(t, detect.StatusBlocked, result.Status)
	require.Len(t, result.Winner.Draft.Workloads[0].Mounts, 1)
}

// TestR096_AComposeSubstitutionIsResolvedToItsDefault asserts R-096.
//
// `APP_DOMAIN: ${APP_DOMAIN:-localhost}` reached a running container as those
// twenty-four characters. Compose's own semantics say an unset variable takes
// its default, and Pando is reading the file out of a repository, so the
// default is the only value there is — the same reading mount sources already
// got.
func TestR096_AComposeSubstitutionIsResolvedToItsDefault(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yaml": "services:\n  web:\n    image: nginx\n    environment:\n" +
			"      APP_DOMAIN: ${APP_DOMAIN:-localhost}\n" +
			"      APP_BASE_URL: ${APP_BASE_URL}\n",
	})
	require.NoError(t, err)

	env := map[string]string{}
	for _, e := range result.Winner.Draft.Workloads[0].Env {
		require.NotNil(t, e.Value)
		env[e.Key] = *e.Value
	}
	require.Equal(t, "localhost", env["APP_DOMAIN"])

	// One with no default has no value to resolve to. It keeps its spelling —
	// an empty string would be an app misconfigured with nothing to show for
	// it — and says so, so somebody sets it (R-102).
	require.Equal(t, "${APP_BASE_URL}", env["APP_BASE_URL"])
	require.True(t, hasWarning(result.Winner.Draft.Warnings,
		spec.WarnComposeConstructRewritten, "APP_BASE_URL"))
}

// Compose services live in a map, and Go randomizes map iteration.
func TestComposeImportIsStableAcrossRuns(t *testing.T) {
	src := memSource{"compose.yaml": composeStack}

	first, err := auction().Run(context.Background(), src)
	require.NoError(t, err)

	for range 20 {
		again, err := auction().Run(context.Background(), src)
		require.NoError(t, err)
		require.Equal(t, first.Winner.Draft.Workloads, again.Winner.Draft.Workloads)
		require.Equal(t, first.Questions, again.Questions,
			"the options in a question must not reshuffle between runs of the same repo")
	}
}

func hasSlot(slots []spec.Slot, want spec.SlotType) bool {
	for _, s := range slots {
		if s.Type == want {
			return true
		}
	}
	return false
}

func hasWarning(warnings []spec.Warning, code, mentions string) bool {
	for _, w := range warnings {
		if w.Code == code && strings.Contains(w.Message, mentions) {
			return true
		}
	}
	return false
}

// --- what real repositories caught -----------------------------------------
//
// Both of these came from running detection against apps in the bemeek-io org.
// Neither shape appears in the corpus or in any fixture written from
// imagination, and both were producing confidently wrong output.

// A HEALTHCHECK's continuation line begins with CMD.
//
// From bemeek-io/skyjo-online. Detection reported the health probe as the app's
// start command and never reached the real CMD two lines below it.
func TestAHealthcheckContinuationIsNotTheStartCommand(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"Dockerfile": "FROM node:22-alpine\n" +
			"EXPOSE 3001\n" +
			"HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \\\n" +
			"  CMD wget --no-verbose --tries=1 --spider http://localhost:3001/ || exit 1\n" +
			"\n" +
			`CMD ["node", "dist/server/index.js"]` + "\n",
	})
	require.NoError(t, err)

	evidence := strings.Join(result.Winner.Evidence, " | ")
	require.Contains(t, evidence, "dist/server/index.js",
		"the real CMD is two lines below the healthcheck and has to win")
	require.NotContains(t, evidence, "--spider",
		"evidence that is confidently wrong is worse than none")
	require.Contains(t, evidence, "EXPOSE 3001")
}

// A compose volume source carrying a variable substitution.
//
// From bemeek-io/mashboard: "${CONFIG_DIR:-./config}:/app/config:delegated".
// Splitting that on ":" yields a volume named "${CONFIG_DIR" mounted at
// "-./config}" — not a parse error anywhere, just a bundle that comes up with a
// garbage volume on a nonsense path and an app that cannot find its config.
func TestAComposeVolumeWithAVariableDefaultIsResolved(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yml": "services:\n" +
			"  backend:\n" +
			"    image: example/backend:latest\n" +
			"    volumes:\n" +
			"      - ${CONFIG_DIR:-./config}:/app/config:delegated\n" +
			"      - redis-data:/data\n" +
			"volumes:\n" +
			"  redis-data:\n",
	})
	require.NoError(t, err)
	require.NotEqual(t, detect.StatusBlocked, result.Status)

	draft := result.Winner.Draft
	for _, v := range draft.Volumes {
		require.NotContains(t, v.Name, "$", "a volume name is not a shell expression")
		require.NotContains(t, v.Name, "{")
	}

	mounts := draft.Workloads[0].Mounts
	require.Len(t, mounts, 2)

	var configPath string
	for _, m := range mounts {
		if m.Path != "/data" {
			configPath = m.Path
		}
	}
	require.Equal(t, "/app/config", configPath,
		"the container path is the field after the source, not the middle of a substitution")
}

// ":ro" is the third field, and finding it depends on splitting correctly.
func TestAReadOnlyMountBehindAVariableIsStillReadOnly(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yml": "services:\n  web:\n    image: nginx\n" +
			"    volumes:\n      - ${CONFIG_DIR:-./config}:/app/config:ro\n",
	})
	require.NoError(t, err)

	mounts := result.Winner.Draft.Workloads[0].Mounts
	require.Len(t, mounts, 1)
	require.Equal(t, "/app/config", mounts[0].Path)
	require.True(t, mounts[0].ReadOnly)
}

// A substitution with no default cannot be resolved, and says so rather than
// becoming an empty string — which would mount the repository root.
func TestAVariableWithNoDefaultIsNotSilentlyEmptied(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"compose.yml": "services:\n  web:\n    image: nginx\n" +
			"    volumes:\n      - ${DATA_DIR}:/var/data\n",
	})
	require.NoError(t, err)

	mounts := result.Winner.Draft.Workloads[0].Mounts
	require.Len(t, mounts, 1)
	require.Equal(t, "/var/data", mounts[0].Path,
		"an unresolvable source must not shift every other field along")
}

// A compose file that builds from source resolves to a build the builder can
// actually do.
//
// It used to import as `strategy: compose` with a pointer to the file, and
// nothing implements compose — BuildKit declares dockerfile and nothing else.
// So every compose app needing a build was refused at plan time with
// "bld_buildkit cannot build this app the way it is set up", which names the
// builder and not the cause.
//
// It also left the build instructions in the repository to be read later, which
// R-020 forbids: the spec is the sole record of how an app runs.
func TestR020_AComposeBuildIsResolvedIntoTheSpec(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeBuilds})
	require.NoError(t, err)

	draft := result.Winner.Draft
	require.Equal(t, spec.BuildCompose, draft.Build.Strategy,
		"a compose app stays a compose app; how each service builds is on the workload")

	// Kept for provenance: somebody reading this spec later should be able to
	// see where it came from without guessing.
	require.Equal(t, "compose.yaml", draft.Build.ComposeFile)

	require.Len(t, draft.Workloads, 1)
	wb := draft.Workloads[0].Build
	require.NotNil(t, wb, "the service that builds says how")
	require.Equal(t, "./frontend", wb.Context)
	require.Equal(t, "Dockerfile.prod", wb.Dockerfile)
	require.Equal(t, "release", wb.Target)
}

// The short spelling — `build: ./dir` — is the context and nothing else.
func TestAComposeBuildStringIsTheContext(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeBuildString})
	require.NoError(t, err)

	draft := result.Winner.Draft
	require.Equal(t, spec.BuildCompose, draft.Build.Strategy)

	wb := draft.Workloads[0].Build
	require.NotNil(t, wb)
	require.Equal(t, ".", wb.Context)
	require.Empty(t, wb.Dockerfile, "compose's own default applies; Pando does not invent a path")
}

// Every service carrying an image: needs no builder at all.
func TestAComposeFileOfPrebuiltImagesNeedsNoBuilder(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeStack})
	require.NoError(t, err)

	require.Equal(t, spec.BuildPrebuilt, result.Winner.Draft.Build.Strategy,
		"nothing here is built from source, so requiring a builder would refuse an app that needs none")
}

// Every service that builds gets its own build. A compose file with a frontend
// and a worker is two images, not one image and a warning about the other.
func TestEveryBuildableServiceIsBuilt(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"compose.yaml": composeTwoBuilds})
	require.NoError(t, err)

	draft := result.Winner.Draft
	require.Equal(t, spec.BuildCompose, draft.Build.Strategy,
		"a compose app is built as a compose app")

	built := map[string]string{}
	for _, w := range draft.Workloads {
		if w.Build != nil {
			built[w.Name] = w.Build.Context
		}
	}
	require.Equal(t, map[string]string{"web": "./web", "worker": "./worker"}, built)
}

const composeBuilds = `
services:
  web:
    build:
      context: ./frontend
      dockerfile: Dockerfile.prod
      target: release
    ports:
      - "3000:3000"
`

const composeBuildString = `
services:
  web:
    build: .
    ports:
      - "3000:3000"
`

const composeTwoBuilds = `
services:
  web:
    build: ./web
    ports:
      - "3000:3000"
  worker:
    build: ./worker
`

// TestR102_AnsweringTheTieBreakAdoptsThatReading asserts the answer means what
// it says.
//
// The auction asks which of two close readings is right, and answering used to
// overwrite the winning draft's strategy *name* while keeping its workloads —
// so choosing "compose" produced the Dockerfile detector's spec labeled
// `strategy: compose`, which no builder implements. The app was then refused at
// plan time with a message about the builder, nowhere near the cause.
func TestR102_AnsweringTheTieBreakAdoptsThatReading(t *testing.T) {
	// A repository both detectors recognize: a Dockerfile at the root and a
	// compose file naming two services.
	src := memSource{"Dockerfile": "FROM node:22\nEXPOSE 3000\n", "compose.yaml": composeStack}

	result, err := auction().Run(context.Background(), src)
	require.NoError(t, err)

	// Close enough that Pando asks rather than guesses.
	var question bool
	for _, q := range result.Questions {
		if q.Key == detect.KeyBuildStrategy {
			question = true
		}
	}
	require.True(t, question, "questions: %+v", result.Questions)

	proposal := detect.Proposal{
		Winner:    result.Winner,
		RunnersUp: result.RunnersUp,
		DraftSpec: detect.Assemble("app_1", spec.Source{Type: spec.SourceGit}, result.Winner.Draft),
	}

	// Answering with the winner's own strategy changes nothing.
	same := proposal.WithAnswers(map[string]string{
		detect.KeyBuildStrategy: string(result.Winner.Strategy),
	})
	require.Equal(t, len(proposal.DraftSpec.Workloads), len(same.Workloads))

	// Answering with the other one adopts that candidate's draft entirely —
	// its workloads, not just its name.
	other := result.RunnersUp[0]
	switched := proposal.WithAnswers(map[string]string{
		detect.KeyBuildStrategy: string(other.Strategy),
	})
	require.Equal(t, other.Draft.Build.Strategy, switched.Build.Strategy,
		"the strategy is the adopted draft's own, not the answer string")
	require.Equal(t, len(other.Draft.Workloads), len(switched.Workloads),
		"the workloads come from the adopted reading")

	// And the spec stays coherent: whatever strategy it now claims is one the
	// adopted draft actually describes.
	require.NotEqual(t, spec.BuildStrategy("compose"), switched.Build.Strategy,
		"no spec claims a strategy nothing implements")
}

// An answer naming something that never bid is ignored rather than stamped on.
func TestAnAnswerNamingNoCandidateIsIgnored(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"Dockerfile": "FROM node:22\n"})
	require.NoError(t, err)

	proposal := detect.Proposal{
		Winner:    result.Winner,
		RunnersUp: result.RunnersUp,
		DraftSpec: detect.Assemble("app_1", spec.Source{Type: spec.SourceGit}, result.Winner.Draft),
	}

	out := proposal.WithAnswers(map[string]string{detect.KeyBuildStrategy: "nonsense"})
	require.Equal(t, result.Winner.Draft.Build.Strategy, out.Build.Strategy)
}

// plannerRunningMake stands in for the builder once a Makefile has told it what
// to run: the plan's build step is `make build` rather than a guessed one.
type plannerRunningMake struct{}

func (plannerRunningMake) Plan(context.Context, api.SourceView) (map[string]string, string, *api.PlanDeclaration, error) {
	// The shape nixpacks actually emits, taken from a real run rather than
	// invented: the environment is built first, then the install step, and the
	// build command is the third RUN. A tidier fixture with one RUN passed
	// while the real plan did not — buildsWithMake answered from the first RUN
	// it saw, which is always nix-env.
	return map[string]string{
			".nixpacks/Dockerfile": "FROM ubuntu:noble\n" +
				"ENTRYPOINT [\"/bin/bash\", \"-l\", \"-c\"]\n" +
				"RUN nix-env -if .nixpacks/nixpkgs-e89cf1c9.nix && nix-collect-garbage -d\n" +
				"RUN --mount=type=cache,id=x,target=/root/.cache/go-build go mod download\n" +
				"RUN --mount=type=cache,id=x,target=/root/.cache/go-build make build\n" +
				"RUN true\n\n" +
				"FROM ubuntu:noble\n" +
				"WORKDIR /app/\n" +
				"CMD [\"./macscout\"]\n",
		}, ".nixpacks/Dockerfile", &api.PlanDeclaration{
			Source:     "Makefile",
			Why:        "Makefile declares how this app is built, and the plan runs it rather than guessing",
			Confidence: 0.72,
		}, nil
}

// R-094 tier 3: the maintainer's own build commands rank above tier 4's
// ecosystem manifests, and the proposal says which one it used.
func TestR094_AMakefileDrivenPlanOutranksConventionMatching(t *testing.T) {
	src := memSource{
		"go.mod":           "module example.com/app\n",
		"Makefile":         "build:\n\tcd web && npm ci && npm run build\n\tgo build -o macscout ./cmd/server\n\nrun: build\n\t./macscout\n",
		"web/package.json": `{"name":"client"}`,
	}

	withMake, err := detect.NewAuction(detect.BuildpackDetector{Planner: plannerRunningMake{}}).
		Run(context.Background(), src)
	require.NoError(t, err)

	conventional, err := detect.NewAuction(detect.BuildpackDetector{Planner: plannerWritingCMD{}}).
		Run(context.Background(), src)
	require.NoError(t, err)

	require.Greater(t, withMake.Winner.Confidence, conventional.Winner.Confidence,
		"a plan the repository dictated is worth more than one inferred from go.mod")
	require.Contains(t, strings.Join(withMake.Winner.Evidence, "\n"), "Makefile declares how this app is built",
		"and the proposal says so, because the difference is the whole point")
	require.Empty(t, detect.Asked(withMake.Questions))
}

// The evidence reports what the builder decided, so a Makefile that did not
// drive the plan does not claim to have.
func TestAMakefileThatDidNotDriveThePlanIsNotClaimedAsEvidence(t *testing.T) {
	src := memSource{
		"go.mod": "module example.com/app\n",
		// Present, but it declares no build target, so the builder ignored it
		// and planned by convention.
		"Makefile": "lint:\n\tgolangci-lint run\n",
	}
	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: plannerWritingCMD{}}).
		Run(context.Background(), src)
	require.NoError(t, err)
	require.NotContains(t, strings.Join(result.Winner.Evidence, "\n"), "Makefile")
}

// A status that says answers are needed while asking for none is one somebody
// has to open the database to understand.
func TestStatusIsReadyWhenThereIsNothingToAnswer(t *testing.T) {
	result, err := detect.NewAuction(detect.BuildpackDetector{Planner: plannerWritingCMD{}}).
		Run(context.Background(), memSource{"go.mod": "module example.com/app\n"})
	require.NoError(t, err)

	require.Empty(t, detect.Asked(result.Questions))
	require.Equal(t, detect.StatusReady, result.Status,
		"the buildpack detector bids 0.45, and a low bid used to say needs_answers on its own")
}
