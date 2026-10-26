package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// helperCalls records each helper a standby operation runs, answering with
// out for the helper's stdout.
type helperCalls struct {
	mu   sync.Mutex
	runs []helperRun
	in   []string
	out  string
	err  error
}

func (h *helperCalls) hook(_ context.Context, run helperRun) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var in []byte
	if run.Stdin != nil {
		in, _ = io.ReadAll(run.Stdin)
	}
	h.runs = append(h.runs, run)
	h.in = append(h.in, string(in))
	_, _ = io.WriteString(run.Stdout, h.out)
	return h.err
}

// standbyDaemon is a fake daemon that creates and lists volumes, with every
// helper answered by the returned recorder.
func standbyDaemon(t *testing.T) (*fakeDaemon, *Adapter, *helperCalls) {
	t.Helper()
	f, a := newFakeDaemon(t, nil)
	var mu sync.Mutex
	volumes := map[string]map[string]string{}
	f.on("POST /volumes/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string
			Labels map[string]string
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		volumes[body.Name] = body.Labels
		mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"Name": body.Name})
	})
	f.on("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		var filters map[string]map[string]bool
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		mu.Lock()
		defer mu.Unlock()
		var items []map[string]any
		for name, labels := range volumes {
			if matchesLabels(labels, filters["label"]) {
				items = append(items, map[string]any{"Name": name, "Labels": labels})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"Volumes": items})
	})
	f.on("DELETE /volumes/*", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(volumes, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	calls := &helperCalls{}
	a.helperHook = calls.hook
	return f, a, calls
}

func matchesLabels(labels map[string]string, want map[string]bool) bool {
	for w := range want {
		k, v, hasValue := strings.Cut(w, "=")
		got, ok := labels[k]
		if !ok || (hasValue && got != v) {
			return false
		}
	}
	return true
}

func TestR420_AVolumeIsListedReadAndCopiedByHelpersThatOnlyReadIt(t *testing.T) {
	ctx := context.Background()
	_, a, calls := standbyDaemon(t)
	calls.out = "T 100\n41ed 4096 90 .\n81a4 3 95 ./a b\n"

	listing, err := a.ListVolume(ctx, "app_a", "vol_1")
	require.NoError(t, err)
	require.Equal(t, int64(100), listing.TakenAt)
	require.Equal(t, []VolumeEntry{{Path: ".", Mode: 0o40755, Size: 4096, MTime: 90}, {Path: "./a b", Mode: 0o100644, Size: 3, MTime: 95}}, listing.Entries)
	require.True(t, listing.Entries[0].IsDir())
	run := calls.runs[0]
	require.True(t, run.Mounts[0].ReadOnly)
	require.Equal(t, "pando-app_a-vol_1", run.Mounts[0].Source)
	require.Equal(t, snapshotCaps, run.Caps)

	require.NoError(t, a.ReadVolumeFiles(ctx, "app_a", "vol_1", []string{"./a b"}, io.Discard))
	require.Equal(t, "./a b\n", calls.in[1])
	require.True(t, calls.runs[1].Mounts[0].ReadOnly)
	require.Contains(t, calls.runs[1].Cmd[2], "--no-recursion")
}

func TestR420_ACopyIsStagedFromTheNewestAndCommittedWithItsListing(t *testing.T) {
	ctx := context.Background()
	f, a, calls := standbyDaemon(t)

	require.NoError(t, a.StageStandbyCopy(ctx, "app_a", "vol_1", []string{"./old"}, strings.NewReader("tar")))
	require.Equal(t, 1, f.called("POST /volumes/create"))
	require.Contains(t, calls.runs[0].Cmd[2], "cp -a -l", "a new copy shares unchanged files with the newest")
	require.Equal(t, "./old\n", calls.in[0])
	require.Equal(t, "tar", calls.in[1])
	require.Equal(t, "pando-standby-app_a-vol_1", calls.runs[1].Mounts[0].Source)
	require.False(t, calls.runs[1].Mounts[0].ReadOnly)

	listing := VolumeListing{TakenAt: 7, Entries: []VolumeEntry{{Path: "./x", Mode: 0o100644, Size: 1, MTime: 5}}}
	require.NoError(t, a.CommitStandbyCopy(ctx, "app_a", "vol_1", 4, listing, []int{1}))
	commit := calls.runs[2]
	require.Equal(t, []string{"sh", "0000000004", "0000000001"}, commit.Cmd[3:])
	require.Equal(t, "T 7\n81a4 1 5 ./x\n", calls.in[2])

	// Nothing to extract: only the removals.
	require.NoError(t, a.StageStandbyCopy(ctx, "app_a", "vol_1", nil, nil))
	require.Len(t, calls.runs, 4)

	err := a.StageStandbyCopy(ctx, "app_a", "vol_1", []string{"./../etc"}, nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
}

func TestR420_TheCopiesAreReadWithTheNewestListing(t *testing.T) {
	ctx := context.Background()
	_, a, calls := standbyDaemon(t)
	calls.out = "0000000003\n0000000002\nlost+found\n\nT 9\n81a4 1 5 ./x\n"
	got, err := a.StandbyCopies(ctx, "app_a", "vol_1")
	require.NoError(t, err)
	require.Equal(t, []int{2, 3}, got.Seqs)
	require.Equal(t, int64(9), got.Newest.TakenAt)

	calls.out = ""
	got, err = a.StandbyCopies(ctx, "app_a", "vol_1")
	require.NoError(t, err)
	require.Empty(t, got.Seqs)

	calls.err = errs.New(errs.AdapterFailed, "helper failed")
	_, err = a.StandbyCopies(ctx, "app_a", "vol_1")
	require.Error(t, err)
}

func TestR420_ACopyIsRestoredIntoTheAppsVolume(t *testing.T) {
	ctx := context.Background()
	f, a, calls := standbyDaemon(t)
	require.NoError(t, a.RestoreStandbyCopy(ctx, "app_a", "vol_1", 2))
	require.Equal(t, 1, f.called("POST /volumes/create"))
	run := calls.runs[0]
	require.Equal(t, "0000000002", run.Cmd[4])
	require.True(t, run.Mounts[0].ReadOnly, "the copy is only read")
	require.Equal(t, "pando-app_a-vol_1", run.Mounts[1].Source)
	require.False(t, run.Mounts[1].ReadOnly)
}

func TestR419_HomeMarksAndStandbyStorageAreLabeledApartFromTheBundle(t *testing.T) {
	ctx := context.Background()
	f, a, _ := standbyDaemon(t)
	f.on("GET /networks", respond(http.StatusOK, []any{}))
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	require.NoError(t, a.MarkHome(ctx, "app_a", at))
	marks, err := a.HomeMarks(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]time.Time{"app_a": at}, marks)

	require.NoError(t, a.ensureStaging(ctx, "app_a", "vol_1"))
	bundles, err := a.Bundles(ctx)
	require.NoError(t, err)
	require.Empty(t, bundles, "neither makes this host the app's (placement reads io.pando.bundle)")

	require.NoError(t, a.RemoveStandbyStorage(ctx, "app_a"))
	require.NoError(t, a.ClearHome(ctx, "app_a"))
	marks, err = a.HomeMarks(ctx)
	require.NoError(t, err)
	require.Empty(t, marks)
}

func TestAVolumeListingRoundTripsAndRefusesWhatItCannotRead(t *testing.T) {
	l := VolumeListing{TakenAt: 3, Entries: []VolumeEntry{{Path: ".", Mode: 0o40700, Size: 1, MTime: 2}}}
	back, err := ParseVolumeListing(l.Format())
	require.NoError(t, err)
	require.Equal(t, l, back)

	for _, bad := range []string{"", "nonsense\n", "T 1\n81a4 1 2\n", "T 1\nzz 1 2 ./a\n", "T 1\n81a4 1 2 ../a\n", "T 1\n81a4 1 2 ./a//b\n"} {
		_, err := ParseVolumeListing([]byte(bad))
		require.Error(t, err, bad)
	}
	_, err = ParseVolumeListing([]byte("T 1\n81a4 1 2 ./a\nb\n"))
	require.Contains(t, errs.As(err).Message, "cannot copy to its standby")
	require.Contains(t, errs.As(err).Remedy, "availability.standby")
}

func TestR419_AStandbyIsCreatedAndNeverStarted(t *testing.T) {
	require.False(t, createOnly(context.Background()))
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusInternalServerError, map[string]string{"message": "stop here"}))
	p := api.BundlePlan{BundleID: "app_a-standby", Network: api.NetworkPlan{Private: true}}
	err := a.ApplyStopped(context.Background(), p)
	require.Error(t, err)
	require.Zero(t, f.called("POST /containers"), "nothing is created past the failure, and nothing is started")
}

func TestRunHelperWritesToNowhereWhenNoStdoutIsGiven(t *testing.T) {
	_, a, calls := standbyDaemon(t)
	calls.out = "ignored"
	require.NoError(t, a.runHelper(context.Background(), helperRun{Verb: "test"}))
	var buf bytes.Buffer
	require.NoError(t, a.runHelper(context.Background(), helperRun{Verb: "test", Stdout: &buf}))
	require.Equal(t, "ignored", buf.String())
}
