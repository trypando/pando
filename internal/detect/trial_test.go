package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

func fullyObservant() api.RuntimeCapabilities {
	return api.RuntimeCapabilities{
		SupportsTrialRun: true, SupportsPortObservation: true, SupportsWriteObservation: true,
	}
}

// A repository whose Dockerfile declares no EXPOSE.
//
// This is the case where a port question is still a real one: the app brings
// its own image, so the trial run has something to start and R-097's watch-
// rather-than-ask applies to it. The language detectors no longer raise a port
// question at all — a framework default is a reasonable default, and R-104 puts
// those in the draft as configuration rather than in front of a person — so the
// trial machinery is exercised through the path that still produces one.
func noExposeSource() memSource {
	return memSource{"Dockerfile": "FROM alpine\nCMD [\"sh\"]\n"}
}

// R-097: the port is watched, not asked about. This is the requirement that
// makes R-005 survivable — the person deploying may not know what a port is.
func TestR097_AWatchedPortAnswersTheQuestionInsteadOfAPerson(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{"package.json": `{"name":"app"}`})
	require.NoError(t, err)
	require.Len(t, detect.Asked(result.Questions), 1, "before the trial: the start command")

	draft, questions := detect.ApplyTrial(result.Winner.Draft, result.Questions,
		detect.FromTrialResult(fullyObservant(), api.TrialResult{
			Started: true, ObservedPorts: []int{8080},
		}))

	for _, q := range questions {
		require.NotEqual(t, api.QuestionPort, q.Kind, "nobody is asked a question that was answered")
	}

	primary := draft.Workloads[0]
	require.Equal(t, []spec.Port{{Number: 8080, Protocol: "http", Source: spec.PortObserved}},
		primary.Ports,
		"the framework guess of 3000 is superseded, not joined — one port, and it was watched")
}

// The distinction the review UI shows.
func TestR097_ObservedPortsAreMarkedObservedNotFramework(t *testing.T) {
	result, _ := auction().Run(context.Background(), memSource{"go.mod": "module app\n"})
	require.Equal(t, spec.PortFramework, result.Winner.Draft.Workloads[0].Ports[0].Source)

	draft, _ := detect.ApplyTrial(result.Winner.Draft, result.Questions,
		detect.FromTrialResult(fullyObservant(), api.TrialResult{Started: true, ObservedPorts: []int{9000}}))
	require.Equal(t, spec.PortObserved, draft.Workloads[0].Ports[0].Source)
}

// A question deferred to a trial that never ran must not vanish.
func TestADeferredQuestionSurvivesATrialThatDidNotHappen(t *testing.T) {
	result, _ := auction().Run(context.Background(), noExposeSource())

	_, questions := detect.ApplyTrial(result.Winner.Draft, result.Questions, detect.Trial{Ran: false})

	require.Len(t, detect.Asked(questions), 1,
		"with no trial, the port question is one a person answers after all")
	for _, q := range questions {
		require.False(t, q.Deferred)
	}
}

// A runtime that cannot observe ports is not the same as an app with no port.
func TestARuntimeThatCannotObservePortsLeavesTheQuestionStanding(t *testing.T) {
	result, _ := auction().Run(context.Background(), noExposeSource())

	blind := api.RuntimeCapabilities{SupportsTrialRun: true}
	_, questions := detect.ApplyTrial(result.Winner.Draft, result.Questions,
		detect.FromTrialResult(blind, api.TrialResult{Started: true, ObservedPorts: []int{8080}}))

	require.Len(t, detect.Asked(questions), 1,
		"a runtime that does not report ports must not have its silence read as an answer")
}

// The trial ran, watched, and saw nothing. The question comes back saying so.
func TestAnAppThatOpensNoPortGetsAQuestionThatSaysPandoLooked(t *testing.T) {
	result, _ := auction().Run(context.Background(), noExposeSource())

	_, questions := detect.ApplyTrial(result.Winner.Draft, result.Questions,
		detect.FromTrialResult(fullyObservant(), api.TrialResult{Started: true}))

	var port detect.Question
	for _, q := range questions {
		if q.Kind == api.QuestionPort {
			port = q
		}
	}
	require.NotEmpty(t, port.Key)
	require.False(t, port.Deferred)
	require.Contains(t, port.Prompt, "watched it",
		"a question that reappears without explanation looks like Pando forgot it had checked")
	require.NoError(t, port.Validate(), "it is a real question now, so R-105 applies")
}

// --- O-4: slot promotion ----------------------------------------------------

func envSlots() []spec.Slot {
	return []spec.Slot{
		{Key: "DATABASE_URL", Type: spec.SlotPostgres, Evidence: []string{"declared in .env.example"}},
		{Key: "LOG_LEVEL", Type: spec.SlotUnknown, Evidence: []string{"declared in .env.example"}},
		{Key: "FEATURE_X", Type: spec.SlotUnknown, Evidence: []string{"declared in .env.example"}},
	}
}

// The O-4 fallback: the trial run settles what no heuristic reliably derives.
func TestO4_ACrashThatNamesASlotPromotesThatSlot(t *testing.T) {
	draft, _ := detect.ApplyTrial(
		detect.Draft{Slots: envSlots(), Volumes: []spec.Volume{{Name: "data"}}},
		nil,
		detect.Trial{
			Ran: true, Started: true, Crashed: true,
			Log: "FATAL: could not connect to server: DATABASE_URL is not set\n",
		})

	require.True(t, draft.Slots[0].Required, "the app said this one was missing")
	require.Contains(t, draft.Slots[0].Evidence[1], "DATABASE_URL",
		"the evidence says what was observed, so a user can see why it became required")
}

// The restraint is the point: this is the false-block rate the phase plan says
// to measure rather than assume away.
func TestO4_ACrashThatNamesNothingPromotesNothing(t *testing.T) {
	draft, _ := detect.ApplyTrial(
		detect.Draft{Slots: envSlots(), Volumes: []spec.Volume{{Name: "data"}}},
		nil,
		detect.Trial{
			Ran: true, Started: true, Crashed: true,
			Log: "SyntaxError: Unexpected token '}' at server.js:12\n",
		})

	for _, slot := range draft.Slots {
		require.False(t, slot.Required,
			"one syntax error must not become three required slots and three deploy blockers")
	}
}

// R-107: the log is the answer, and Pando does not invent what was never said.
func TestR107_ACrashDoesNotInventASlotTheRepoNeverDeclared(t *testing.T) {
	draft, _ := detect.ApplyTrial(
		detect.Draft{Volumes: []spec.Volume{{Name: "data"}}},
		nil,
		detect.Trial{
			Ran: true, Started: true, Crashed: true,
			Log: "could not connect to postgres at localhost:5432: connection refused\n",
		})

	require.Empty(t, draft.Slots,
		"a repository that never mentioned Postgres does not acquire a Postgres slot because "+
			"its log did — showing the log and stopping is the correct outcome")
}

// A slot the app names by service rather than by variable is still promoted.
func TestO4_AServiceNamedInTheLogPromotesItsSlot(t *testing.T) {
	draft, _ := detect.ApplyTrial(
		detect.Draft{
			Slots:   []spec.Slot{{Key: "CACHE_ENDPOINT", Type: spec.SlotRedis}},
			Volumes: []spec.Volume{{Name: "data"}},
		},
		nil,
		detect.Trial{Ran: true, Started: true, Crashed: true, Log: "Error: Redis connection to 127.0.0.1:6379 failed"},
	)
	require.True(t, draft.Slots[0].Required)
}

// An already-filled slot is not promoted; there is nothing to promote it for.
func TestAFilledSlotIsLeftAlone(t *testing.T) {
	filled := []spec.Slot{{
		Key: "DATABASE_URL", Type: spec.SlotPostgres,
		Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned},
	}}
	draft, _ := detect.ApplyTrial(
		detect.Draft{Slots: filled, Volumes: []spec.Volume{{Name: "data"}}}, nil,
		detect.Trial{Ran: true, Crashed: true, Log: "DATABASE_URL something went wrong"})
	require.False(t, draft.Slots[0].Required)
}

// --- R-201, R-202: persistence ----------------------------------------------

// R-202: the trial run improves the warning by naming the directory.
func TestR202_TheWarningNamesTheDirectoryTheAppActuallyWrote(t *testing.T) {
	draft, _ := detect.ApplyTrial(
		detect.Draft{Volumes: []spec.Volume{{Name: "declared"}}}, nil,
		detect.Trial{Ran: true, Started: true, ObservedWrites: []string{"/app/uploads"}})

	require.True(t, hasWarning(draft.Warnings, spec.WarnNoPersistentVolume, "/app/uploads"))
	require.True(t, hasWarning(draft.Warnings, spec.WarnNoPersistentVolume, "reporting itself healthy"),
		"R-203: the reason this earns a warning is that the failure looks like success")
}

// R-201: with no trial observation, the generic warning still fires.
func TestR201_NoDeclaredVolumeWarnsEvenWithNothingObserved(t *testing.T) {
	draft, _ := detect.ApplyTrial(detect.Draft{}, nil,
		detect.Trial{Ran: true, Started: true})

	require.True(t, hasWarning(draft.Warnings, spec.WarnNoPersistentVolume, "you can ignore this"))
}

// A declared volume and nothing written outside it is the quiet case.
func TestNoWarningWhenStorageIsDeclaredAndNothingElseIsWritten(t *testing.T) {
	draft, _ := detect.ApplyTrial(
		detect.Draft{Volumes: []spec.Volume{{Name: "data"}}}, nil,
		detect.Trial{Ran: true, Started: true})
	require.Empty(t, draft.Warnings)
}

// --- R-168: path routing ----------------------------------------------------

// R-168 is a warning, never a fix — R-028 forbids rewriting response bodies.
func TestR168_RootAbsoluteAssetsRaiseADismissibleWarning(t *testing.T) {
	warnings := detect.PathRoutingWarning(memSource{
		"index.html": `<html><head><script src="/static/app.js"></script></head></html>`,
	}, detect.Draft{})

	require.Len(t, warnings, 1)
	require.Equal(t, spec.WarnPathRoutingIncompatible, warnings[0].Code)
	require.Contains(t, warnings[0].Message, "/static/app.js", "it names what it found")
	require.Contains(t, warnings[0].Message, "does not rewrite",
		"the warning has to say Pando will not fix this, because R-028 says it must not")
	require.False(t, warnings[0].Dismissed)
}

func TestR168_RelativeAssetsAreFine(t *testing.T) {
	require.Empty(t, detect.PathRoutingWarning(memSource{
		"index.html": `<html><script src="./app.js"></script><a href="about.html">a</a></html>`,
	}, detect.Draft{}))
}

func TestR168_NothingToCheckIsNotAWarning(t *testing.T) {
	require.Empty(t, detect.PathRoutingWarning(memSource{"main.go": "package main"}, detect.Draft{}))
}

// An app bound only to loopback is healthy, unreachable, and silent about it.
func TestAnAppListeningOnlyOnLoopbackIsToldSoExplicitly(t *testing.T) {
	result, _ := auction().Run(context.Background(), noExposeSource())

	_, questions := detect.ApplyTrial(result.Winner.Draft, result.Questions,
		detect.FromTrialResult(fullyObservant(), api.TrialResult{
			Started: true, LoopbackPorts: []int{3000},
		}))

	var port detect.Question
	for _, q := range questions {
		if q.Kind == api.QuestionPort {
			port = q
		}
	}
	require.NotEmpty(t, port.Key)
	require.Contains(t, port.Prompt, "3000")
	require.Contains(t, port.Prompt, "0.0.0.0",
		"the question should say what to change, not just that something is wrong")
	require.NotContains(t, port.Prompt, "no network port at all",
		"it did open a port — saying otherwise is both wrong and less useful")
	require.NoError(t, port.Validate())
}

// An app that never started says so, rather than claiming it opened no port.
func TestAnAppThatNeverStartedSaysThatRatherThanBlamingThePort(t *testing.T) {
	result, _ := auction().Run(context.Background(), noExposeSource())

	_, questions := detect.ApplyTrial(result.Winner.Draft, result.Questions,
		detect.FromTrialResult(fullyObservant(), api.TrialResult{Started: false}))

	for _, q := range questions {
		if q.Kind == api.QuestionPort {
			require.Contains(t, q.Prompt, "did not start")
		}
	}
}

// TestR168_ThePathRoutingWarningReachesEveryCandidate asserts R-168 survives
// the user answering the build-strategy question.
//
// Whether an app writes absolute addresses into its own HTML is a fact about
// the app, not about how it is built — but the warning was attached to the
// winning draft alone, and R-103 offers the runners-up as answers. Choosing one
// adopted a draft that had never been told, so the app deployed clean, came up
// blank under its path prefix, and the one thing Pando knew about why had been
// dropped at the moment the user made a choice it had offered them.
func TestR168_ThePathRoutingWarningReachesEveryCandidate(t *testing.T) {
	// A repository two detectors both recognize, with a frontend that loads its
	// assets from the root.
	source := memSource{
		"index.html":         `<html><head><script src="/assets/app.js"></script></head></html>`,
		"Dockerfile":         "FROM node:20\nCMD [\"node\", \"server.js\"]\n",
		"docker-compose.yml": "services:\n  web:\n    build: .\n    ports:\n      - \"3001:3001\"\n",
	}

	job := &detect.Job{Auction: auction()}
	proposal, err := job.Run(context.Background(), "app_1",
		spec.Source{Type: "git", URL: "https://example.test/app"}, source)
	require.NoError(t, err)

	require.True(t, hasWarning(proposal.Winner.Draft.Warnings, spec.WarnPathRoutingIncompatible, "/assets/app.js"),
		"the winner is warned")

	require.NotEmpty(t, proposal.RunnersUp, "this repository has more than one answer")
	for _, candidate := range proposal.RunnersUp {
		require.True(t,
			hasWarning(candidate.Draft.Warnings, spec.WarnPathRoutingIncompatible, "/assets/app.js"),
			"and so is every candidate the user could choose instead, here %q", candidate.Detector)
	}
}

// R-201: a repository that declares no volume is warned at setup.
//
// The trial run only improves that warning (R-202) by naming the directory it
// watched being written to. It is not what creates it — and attaching the
// warning inside the trial's branch made it conditional on a trial that, for
// every strategy built from source, never runs. macscout is the case: a Go
// server keeping a SQLite database, planned as a buildpack build, deployed with
// nothing told to persist and no warning that anything was missing.
//
// R-203 calls this the worst failure mode in the system, because it works
// perfectly until the second deploy and then discards everything while
// reporting healthy.
func TestR201_TheStorageWarningSurvivesATrialThatCannotRun(t *testing.T) {
	draft := detect.Draft{
		Workloads: []spec.Workload{{Name: "web", Primary: true, Exposed: true}},
	}

	out, _ := detect.ApplyTrial(draft, nil, detect.Trial{Ran: false})

	require.Len(t, out.Warnings, 1,
		"no volumes declared and no trial to observe writes still means no persistence")
	require.Equal(t, spec.WarnNoPersistentVolume, out.Warnings[0].Code)
	require.Contains(t, out.Warnings[0].Message, "No persistent volume found")
}

// And a repository that did declare one is not nagged about it.
func TestADeclaredVolumeIsNotWarnedAboutWithoutATrial(t *testing.T) {
	draft := detect.Draft{
		Workloads: []spec.Workload{{Name: "web", Primary: true}},
		Volumes:   []spec.Volume{{Name: "data"}},
	}
	out, _ := detect.ApplyTrial(draft, nil, detect.Trial{Ran: false})
	require.Empty(t, out.Warnings)
}

// TestR200_StorageAnImageDeclaresIsGiven asserts R-200. Vaultwarden declares
// /data and refuses to start without storage there (issue #55).
func TestR200_StorageAnImageDeclaresIsGiven(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true,
		Mounts: []spec.Mount{{VolumeID: "config", Path: "/config"}}}},
		Volumes: []spec.Volume{{ID: "config", Name: "config"}}}
	out, _ := detect.ApplyTrial(draft, nil, detect.Trial{Ran: true, ImageVolumes: []string{"/config", "/data"}})

	require.Len(t, out.Volumes, 2, "one new volume; the path already mounted is left alone")
	require.Equal(t, spec.VolumeFromImage, out.Volumes[1].Declared)
	require.Contains(t, out.Workloads[0].Mounts, spec.Mount{VolumeID: out.Volumes[1].ID, Path: "/data"})
}

// TestR200_StorageAnAppAsksForInItsOutputIsGiven asserts R-200. Nginx Proxy
// Manager declares /data with VOLUME and checks at start that /etc/letsencrypt
// is mounted too, stopping on every start until it is (issue #90).
func TestR200_StorageAnAppAsksForInItsOutputIsGiven(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}}
	out, _ := detect.ApplyTrial(draft, nil, detect.Trial{Ran: true, ImageVolumes: []string{"/data"},
		Log: "❯ Checking paths ...\n\x1b[1;31mERROR: /etc/letsencrypt is not mounted! Check your docker configuration.\x1b[0m\n" +
			"ERROR: /etc/letsencrypt is not mounted!\nwarning: '/srv/media' must be mounted as a volume\n"})

	var paths []string
	for _, m := range out.Workloads[0].Mounts {
		paths = append(paths, m.Path)
	}
	require.ElementsMatch(t, []string{"/data", "/etc/letsencrypt", "/srv/media"}, paths)
	require.Len(t, out.Volumes, 3)
}

// Traffic goes to the web port an image listens on, not its SSH or SMTP port
// (issue #55).
func TestR097_AnImagesWebPortIsTheOneRoutedTo(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}}
	out, _ := detect.ApplyTrial(draft, nil, detect.Trial{Ran: true, ObservedPorts: []int{22, 3000}})
	require.Equal(t, 3000, out.Workloads[0].Ports[0].Number)
}

// An image that serves only a non-HTTP protocol is refused, not deployed.
func TestR021_AnImageWithNothingForABrowserIsRefused(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: fixedTrial{ports: []int{6379}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "redis:7"}, nil)
	require.NoError(t, err)
	require.Equal(t, detect.StatusBlocked, p.Status)
	require.NotNil(t, p.Blocked)
	require.Contains(t, p.Blocked.Message, "6379")
}

type fixedTrial struct{ ports []int }

func (fixedTrial) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsTrialRun: true, SupportsPortObservation: true}, nil
}

func (f fixedTrial) Trial(context.Context, api.TrialRequest) (api.TrialResult, error) {
	return api.TrialResult{Started: true, ObservedPorts: f.ports}, nil
}

// A trial log with NUL bytes in it can still be stored. The proposal is jsonb,
// which refuses the \u0000 a NUL encodes to; Grafana writes them, and its
// detection could not be saved and stayed running for good (issue #55).
func TestATrialLogIsStoredWithoutNULBytes(t *testing.T) {
	trial := detect.FromTrialResult(api.RuntimeCapabilities{}, api.TrialResult{Log: "start\x00ed\x00"})
	require.Equal(t, "started", trial.Log)
}
