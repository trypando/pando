package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

func probing() api.RuntimeCapabilities {
	return api.RuntimeCapabilities{
		SupportsTrialRun: true, SupportsPortObservation: true, SupportsHTTPProbe: true,
	}
}

func answers(pairs ...int) []api.HTTPAnswer {
	var out []api.HTTPAnswer
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, api.HTTPAnswer{Port: pairs[i], Status: pairs[i+1]})
	}
	return out
}

func routedPort(t *testing.T, draft detect.Draft) spec.Port {
	t.Helper()
	for _, w := range draft.Workloads {
		if w.Primary {
			require.NotEmpty(t, w.Ports)
			return w.Ports[0]
		}
	}
	t.Fatal("no primary workload")
	return spec.Port{}
}

// TestR097_TheWebPortIsChosenOverPortsThatOnlyListen asserts R-097 for an app
// that opens several ports. Traffic goes to the first, and the first used to
// be whichever socket was listed first — epmd, MQTT, Tomcat's shutdown port,
// BitTorrent (issue #90). What the trial watched answer HTTP decides it, then
// what the app's own log says.
func TestR097_TheWebPortIsChosenOverPortsThatOnlyListen(t *testing.T) {
	for name, tc := range map[string]struct {
		caps   api.RuntimeCapabilities
		result api.TrialResult
		want   int
	}{
		"tomcat's connector, not its shutdown port": {
			caps:   probing(),
			result: api.TrialResult{ObservedPorts: []int{8005, 8080}, HTTPAnswers: answers(8080, 404)},
			want:   8080,
		},
		"a web UI beside BitTorrent": {
			caps:   probing(),
			result: api.TrialResult{ObservedPorts: []int{6881, 8080}, HTTPAnswers: answers(8080, 200)},
			want:   8080,
		},
		"a management UI beside epmd, AMQP and Erlang distribution": {
			caps: probing(),
			result: api.TrialResult{ObservedPorts: []int{4369, 5672, 15672, 25672},
				HTTPAnswers: answers(15672, 200)},
			want: 15672,
		},
		"a server that opened an ephemeral port first": {
			caps:   probing(),
			result: api.TrialResult{ObservedPorts: []int{9080, 47373}, HTTPAnswers: answers(9080, 200)},
			want:   9080,
		},
		"a dashboard over a websocket listener that answers not found": {
			caps: probing(),
			result: api.TrialResult{ObservedPorts: []int{1883, 8083, 8883, 18083},
				HTTPAnswers: answers(8083, 404, 18083, 200)},
			want: 18083,
		},
		"the port the log gives a plain http address, not the https redirector": {
			caps: probing(),
			result: api.TrialResult{
				ObservedPorts: []int{8078, 8079, 8080, 8081},
				HTTPAnswers:   answers(8080, 200, 8081, 301),
				Log: "Fortio echo TCP server listening on tcp [::]:8078\n" +
					"Https redirector running on [::]:8081\n" +
					"UI started - visit: http://localhost:8080/fortio/\n",
			},
			want: 8080,
		},
		"the port the log says the application runs at": {
			caps: probing(),
			result: api.TrialResult{
				ObservedPorts: []int{4369, 8080, 8081, 13825},
				HTTPAnswers:   answers(8080, 302, 8081, 200),
				Log:           "[Livebook] Application running at http://localhost:8080/\n",
			},
			want: 8080,
		},
		"without a probe: the port the log says it listens on, not epmd or an ephemeral port": {
			caps: fullyObservant(),
			result: api.TrialResult{
				ObservedPorts: []int{4369, 8008, 36869},
				Log:           "hello_erl listening on 8008\n",
			},
			want: 8008,
		},
		"without a probe or a log: not epmd and not an ephemeral port": {
			caps:   fullyObservant(),
			result: api.TrialResult{ObservedPorts: []int{4369, 7000, 36869}},
			want:   7000,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.result.Started = true
			draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}}
			out, questions := detect.ApplyTrial(draft, nil, detect.FromTrialResult(tc.caps, tc.result))

			port := routedPort(t, out)
			require.Equal(t, tc.want, port.Number)
			require.Equal(t, "http", port.Protocol)
			require.Equal(t, spec.PortObserved, port.Source)
			require.Empty(t, detect.Asked(questions), "the trial settled it; nobody is asked")

			primary := out.Workloads[0]
			require.Len(t, primary.Ports, len(tc.result.ObservedPorts), "every port watched is kept for the review")
			for _, p := range primary.Ports[1:] {
				if p.Protocol == "http" {
					continue
				}
				require.Equal(t, "tcp", p.Protocol)
			}
		})
	}
}

// TestR102_WebPortsTheTrialCannotChooseBetweenAreAsked asserts R-102: two ports
// that both serve a page, and nothing saying which one people open, is a
// question rather than a guess — and the question names them (R-105).
func TestR102_WebPortsTheTrialCannotChooseBetweenAreAsked(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}}
	_, questions := detect.ApplyTrial(draft, nil, detect.FromTrialResult(probing(), api.TrialResult{
		Started: true, ObservedPorts: []int{8080, 9090}, HTTPAnswers: answers(8080, 200, 9090, 200),
	}))

	asked := detect.Asked(questions)
	require.Len(t, asked, 1)
	q := asked[0]
	require.Equal(t, detect.KeyPrimaryPort, q.Key)
	require.Equal(t, []string{"8080", "9090"}, q.Options)
	require.Contains(t, q.Prompt, "8080, 9090")
	require.NoError(t, q.Validate())

	// And the answer is the port the app is routed to.
	p := detect.Proposal{DraftSpec: spec.AppSpec{Workloads: []spec.Workload{{Name: "web", Primary: true,
		Ports: []spec.Port{{Number: 8080, Protocol: "http"}, {Number: 9090, Protocol: "http"}}}}},
		Questions: questions}
	got := p.WithAnswers(map[string]string{detect.KeyPrimaryPort: "9090"})
	port, ok := got.Workloads[0].HTTPPort()
	require.True(t, ok)
	require.Equal(t, 9090, port)
}

// An app whose trial answered on one port alone is never asked about it, even
// beside other ports that only listen.
func TestR103_OneWebPortAmongSeveralIsNotAQuestion(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true}}}
	_, questions := detect.ApplyTrial(draft, nil, detect.FromTrialResult(probing(), api.TrialResult{
		Started: true, ObservedPorts: []int{3000, 9229}, HTTPAnswers: answers(3000, 200),
	}))
	require.Empty(t, detect.Asked(questions))
}

type trialResults struct {
	caps    api.RuntimeCapabilities
	results func(api.TrialRequest) api.TrialResult
}

func (r trialResults) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return r.caps, nil
}

func (r trialResults) Trial(_ context.Context, req api.TrialRequest) (api.TrialResult, error) {
	return r.results(req), nil
}

func exitCode(n int) *int { return &n }

// TestR107_AProgramThatFinishesWithoutListeningIsRefused asserts R-107: the
// trial is the evidence, and Pando shows it and stops. A command-line tool
// prints its answer and exits 0, and it was deployed as a web app (issue #90).
func TestR107_AProgramThatFinishesWithoutListeningIsRefused(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
		results: func(api.TrialRequest) api.TrialResult {
			return api.TrialResult{Started: true, ExitCode: exitCode(0), Log: "0 0 0\n"}
		}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "example/wc:1"}, nil)
	require.NoError(t, err)
	require.Equal(t, detect.StatusBlocked, p.Status)
	require.Contains(t, p.Blocked.Message, "without ever listening")
	require.Equal(t, "0 0 0\n", p.TrialLog, "the log is shown with the refusal")
}

// A crash is not a clean exit: it is an app missing something, and the answer
// is its log rather than a refusal (R-107).
func TestR107_ACrashIsNotRefusedAsAProgramThatFinished(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
		results: func(api.TrialRequest) api.TrialResult {
			return api.TrialResult{Started: true, ExitCode: exitCode(1), Log: "DATABASE_URL is not set\n"}
		}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "example/web:1"}, nil)
	require.NoError(t, err)
	require.Nil(t, p.Blocked)
}

// TestR021_AnImageThatOnlyDeclaresADatabasePortIsRefused asserts R-021's
// reading in issue #55: an image with nothing for a browser is refused. MySQL
// and Postgres exit at once without a password, so the trial sees no port, and
// what the image declares is the evidence left (issue #90). MQTT is a broker's
// protocol, not a web page.
func TestR021_AnImageThatOnlyDeclaresADatabasePortIsRefused(t *testing.T) {
	crashes := trialResults{caps: probing(), results: func(api.TrialRequest) api.TrialResult {
		return api.TrialResult{Started: true, ExitCode: exitCode(1),
			Log: "Database is uninitialized and superuser password is not specified."}
	}}
	for name, exposed := range map[string][]int{"postgres": {5432}, "mysql": {3306, 33060}} {
		t.Run(name, func(t *testing.T) {
			images := &fakeImages{insp: amd64Image(&oci.Config{ExposedPorts: exposed})}
			job := &detect.Job{Auction: detect.NewAuction(), Runtime: crashes, Images: images}
			p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: name + ":8"}, nil)
			require.NoError(t, err)
			require.Equal(t, detect.StatusBlocked, p.Status)
			require.Contains(t, p.Blocked.Message, "declares only")
		})
	}

	t.Run("mosquitto", func(t *testing.T) {
		job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
			results: func(api.TrialRequest) api.TrialResult {
				return api.TrialResult{Started: true, ObservedPorts: []int{1883}}
			}}}
		p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "eclipse-mosquitto:2"}, nil)
		require.NoError(t, err)
		require.Equal(t, detect.StatusBlocked, p.Status)
		require.Contains(t, p.Blocked.Message, "1883")
	})
}

// An image whose web UI answered is not refused for the database port beside
// it, even with an Erlang node's ephemeral port open too.
func TestR021_AnImageWithAnAnsweringWebPortIsNotRefused(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
		results: func(api.TrialRequest) api.TrialResult {
			return api.TrialResult{Started: true, ObservedPorts: []int{4369, 5672, 15672, 41234},
				HTTPAnswers: answers(15672, 200)}
		}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "rabbitmq:4-management"}, nil)
	require.NoError(t, err)
	require.Nil(t, p.Blocked)
	primary, ok := p.DraftSpec.PrimaryWorkload()
	require.True(t, ok)
	port, ok := primary.HTTPPort()
	require.True(t, ok)
	require.Equal(t, 15672, port)
}

// TestR097_AnAppListeningOnLoopbackIsStartedWithHOST asserts R-097's port is
// one traffic can reach. An app that binds 127.0.0.1 unless HOST is set is
// tried again with HOST=0.0.0.0, and keeps it because it worked (issue #90).
func TestR097_AnAppListeningOnLoopbackIsStartedWithHOST(t *testing.T) {
	var tried []map[string]string
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
		results: func(req api.TrialRequest) api.TrialResult {
			env := map[string]string{}
			for k, v := range req.Env {
				env[k] = v.Reveal()
			}
			tried = append(tried, env)
			if env["HOST"] == "0.0.0.0" {
				return api.TrialResult{Started: true, ObservedPorts: []int{8080}, HTTPAnswers: answers(8080, 200)}
			}
			return api.TrialResult{Started: true, LoopbackPorts: []int{8080}}
		}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "example/web:1"}, nil)
	require.NoError(t, err)
	require.Len(t, tried, 2)
	require.Nil(t, p.Blocked)
	require.Empty(t, detect.Asked(p.Questions))

	primary, ok := p.DraftSpec.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, 8080, primary.Ports[0].Number)
	var host string
	for _, e := range primary.Env {
		if e.Key == "HOST" && e.Value != nil {
			host = *e.Value
		}
	}
	require.Equal(t, "0.0.0.0", host)
}

// And an app that stays on loopback whatever HOST says is asked for a command
// that moves it, with the reason, rather than proposed as ready.
func TestR105_AnAppThatStaysOnLoopbackIsAskedHowToStartIt(t *testing.T) {
	draft := detect.Draft{Workloads: []spec.Workload{{Name: "web", Primary: true,
		Ports: []spec.Port{{Number: 3000, Protocol: "http", Source: spec.PortFramework}}}}}
	_, questions := detect.ApplyTrial(draft, nil, detect.FromTrialResult(probing(), api.TrialResult{
		Started: true, LoopbackPorts: []int{3000},
	}))
	asked := detect.Asked(questions)
	require.Len(t, asked, 1)
	require.Equal(t, detect.KeyStartCommand, asked[0].Key)
	require.Contains(t, asked[0].Prompt, "127.0.0.1")
	require.NoError(t, asked[0].Validate())
}
