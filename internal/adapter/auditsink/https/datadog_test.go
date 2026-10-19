package https

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// recorder is a TLS destination keeping every body it is posted, answering
// status for the nth request (1-based) in fail and 202 otherwise.
type recorder struct {
	mu     sync.Mutex
	bodies []string
	types  []string
}

func record(t *testing.T, fail int) (*httptest.Server, string, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, string(b))
		rec.types = append(rec.types, r.Header.Get("Content-Type"))
		n := len(rec.bodies)
		rec.mu.Unlock()
		if n == fail {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return srv, ca, rec
}

var ddNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func datadogSink(t *testing.T, url, ca string, extra map[string]any) *Adapter {
	t.Helper()
	settings := map[string]any{"url": url, "body": BodyDatadog, "auth_header": "DD-API-KEY", "auth_scheme": SchemeNone, "ca_certificate": ca}
	for k, v := range extra {
		settings[k] = v
	}
	a, err := configure(t, settings)
	require.NoError(t, err)
	a.now = func() time.Time { return ddNow }
	return a
}

func logs(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	require.NoError(t, d.Decode(&out), body)
	return out
}

// TestR382_DatadogBodySendsEachEventAsALog asserts R-382 for Datadog: each
// event is a Logs API item with the message Datadog requires, ddsource and
// service in the body, the event's time as date, and the event's own fields
// as attributes.
func TestR382_DatadogBodySendsEachEventAsALog(t *testing.T) {
	srv, ca, rec := record(t, 0)
	a := datadogSink(t, srv.URL+"/api/v2/logs", ca, nil)
	require.Equal(t, DatadogMaxBody, a.cfg.MaxBodyBytes, "the Datadog body is capped at Datadog's payload limit by default")

	native := `{"id":9007199254740993,"occurred_at":"2026-10-18T11:00:00.123456Z","principal_kind":"user","principal_id":"usr_1","actor_name":"Ada","action":"app.deploy","target_kind":"app","target_id":"app_1","detail":{"n":1}}`
	system := `{"id":2,"occurred_at":"2026-10-18T11:30:00Z","principal_kind":"system","principal_id":null,"action":"install.update","app_id":null}`
	ocsf := `{"time":1792321200000,"message":"app.use by usr_2 on app app_2","status":"Success","metadata":{"uid":"3"}}`
	require.NoError(t, a.Send(context.Background(), api.AuditBatch{
		Events: []json.RawMessage{json.RawMessage(native), json.RawMessage(system), json.RawMessage(ocsf)},
		IDs:    []int64{1, 2, 3},
	}))
	require.Len(t, rec.bodies, 1)
	require.Equal(t, "application/json", rec.types[0])
	got := logs(t, rec.bodies[0])
	require.Len(t, got, 3)

	require.Equal(t, "app.deploy by Ada on app app_1", got[0]["message"])
	require.Equal(t, "pando", got[0]["ddsource"])
	require.Equal(t, "pando", got[0]["service"])
	require.Equal(t, "2026-10-18T11:00:00.123456Z", got[0]["date"])
	require.Equal(t, json.Number("9007199254740993"), got[0]["id"], "a large id keeps its precision")
	require.Equal(t, "usr_1", got[0]["principal_id"])
	require.Equal(t, map[string]any{"n": json.Number("1")}, got[0]["detail"])

	require.Equal(t, "install.update by system on install", got[1]["message"])

	require.Equal(t, "app.use by usr_2 on app app_2", got[2]["message"], "an OCSF event's own message is kept")
	require.Equal(t, time.UnixMilli(1792321200000).UTC().Format(time.RFC3339Nano), got[2]["date"])
	require.Equal(t, "Success", got[2]["status"])
}

// TestR382_DatadogOldEventsAreSentUndated asserts an event older than
// Datadog accepts as a log's date (18 hours) is sent without date, so Datadog
// dates it on arrival rather than rejecting it, and keeps its own time in
// occurred_at.
func TestR382_DatadogOldEventsAreSentUndated(t *testing.T) {
	srv, ca, rec := record(t, 0)
	a := datadogSink(t, srv.URL, ca, nil)
	at := func(age time.Duration) json.RawMessage {
		return json.RawMessage(`{"id":1,"action":"app.deploy","occurred_at":"` + ddNow.Add(-age).Format(time.RFC3339) + `"}`)
	}
	require.NoError(t, a.Send(context.Background(), api.AuditBatch{
		Events: []json.RawMessage{at(datadogMaxAge), at(datadogMaxAge + time.Second), at(30 * 24 * time.Hour), json.RawMessage(`{"id":4,"action":"app.use"}`)},
	}))
	got := logs(t, rec.bodies[0])
	require.Equal(t, ddNow.Add(-datadogMaxAge).Format(time.RFC3339Nano), got[0]["date"], "an event within the window keeps its date")
	for i := 1; i < 4; i++ {
		require.NotContains(t, got[i], "date", "event %d", i)
		require.NotEmpty(t, got[i]["message"])
	}
	require.Equal(t, ddNow.Add(-30*24*time.Hour).Format(time.RFC3339), got[2]["occurred_at"])
}

// TestR382_DatadogRefusesAnEventThatIsNotAnObject asserts an event that
// cannot carry Datadog's fields fails the batch before anything is posted.
func TestR382_DatadogRefusesAnEventThatIsNotAnObject(t *testing.T) {
	srv, ca, rec := record(t, 0)
	a := datadogSink(t, srv.URL, ca, nil)
	for _, ev := range []string{`[1]`, `null`, `{not json`} {
		err := a.Send(context.Background(), api.AuditBatch{Events: []json.RawMessage{json.RawMessage(ev)}})
		require.ErrorContains(t, err, "could not be wrapped", ev)
	}
	require.Empty(t, rec.bodies)
}

// TestR382_BatchesAreCutToMaxBodyBytes asserts a batch whose body would pass
// max_body_bytes goes as several POSTs, in order, each within the limit, for
// the array and line shapes alike.
func TestR382_BatchesAreCutToMaxBodyBytes(t *testing.T) {
	events := make([]json.RawMessage, 10)
	ids := make([]int64, 10)
	for i := range events {
		events[i] = json.RawMessage(`{"id":` + string(rune('0'+i)) + `,"action":"app.deploy","pad":"` + strings.Repeat("x", 40) + `"}`)
		ids[i] = int64(i)
	}
	for _, body := range []string{BodyDatadog, BodyJSONArray, BodyNDJSON, BodySplunkHEC, BodyElasticBulk} {
		t.Run(body, func(t *testing.T) {
			srv, ca, rec := record(t, 0)
			a, err := configure(t, map[string]any{"url": srv.URL, "body": body, "ca_certificate": ca, "max_body_bytes": 400})
			require.NoError(t, err)
			require.NoError(t, a.Send(context.Background(), api.AuditBatch{Events: events, IDs: ids}))
			require.Greater(t, len(rec.bodies), 1, "the batch was split")
			var ids []string
			for _, b := range rec.bodies {
				require.LessOrEqual(t, len(b), 400)
				for _, part := range strings.Split(b, `"id":`)[1:] {
					ids = append(ids, part[:1])
				}
			}
			require.Equal(t, strings.Split("0123456789", ""), ids, "every event, once, in order")
		})
	}

	t.Run("unlimited by default", func(t *testing.T) {
		srv, ca, rec := record(t, 0)
		a, err := configure(t, map[string]any{"url": srv.URL, "body": BodyNDJSON, "ca_certificate": ca})
		require.NoError(t, err)
		require.NoError(t, a.Send(context.Background(), api.AuditBatch{Events: events}))
		require.Len(t, rec.bodies, 1)
	})

	t.Run("an event too large alone", func(t *testing.T) {
		srv, ca, rec := record(t, 0)
		a, err := configure(t, map[string]any{"url": srv.URL, "body": BodyJSONArray, "ca_certificate": ca, "max_body_bytes": 50})
		require.NoError(t, err)
		err = a.Send(context.Background(), api.AuditBatch{Events: events[:1], IDs: []int64{42}})
		require.ErrorContains(t, err, "Audit event 42 is 81 bytes")
		require.ErrorContains(t, err, "Raise max_body_bytes")
		require.Empty(t, rec.bodies)
		err = a.Send(context.Background(), api.AuditBatch{Events: events[:1]})
		require.ErrorContains(t, err, "Audit event at position 1 is")
	})

	t.Run("a refused body fails the batch", func(t *testing.T) {
		srv, ca, rec := record(t, 2)
		a, err := configure(t, map[string]any{"url": srv.URL, "body": BodyNDJSON, "ca_certificate": ca, "max_body_bytes": 200})
		require.NoError(t, err)
		err = a.Send(context.Background(), api.AuditBatch{Events: events})
		require.ErrorContains(t, err, "answered 413")
		require.Len(t, rec.bodies, 2, "nothing is posted after the refusal")
	})

	t.Run("negative is refused", func(t *testing.T) {
		_, err := configure(t, map[string]any{"url": "https://siem.example.com", "max_body_bytes": -1})
		require.ErrorContains(t, err, "max_body_bytes is -1")
	})
}

// TestR382_DatadogBatchesHoldAtMostAThousandLogs asserts that a Datadog body
// holds no more logs than its intake takes in one array, and that other
// bodies have no such cap.
func TestR382_DatadogBatchesHoldAtMostAThousandLogs(t *testing.T) {
	events := make([]json.RawMessage, datadogMaxItems+1)
	for i := range events {
		events[i] = json.RawMessage(`{"action":"app.deploy","occurred_at":"2026-10-18T11:00:00Z"}`)
	}
	srv, ca, rec := record(t, 0)
	a := datadogSink(t, srv.URL, ca, nil)
	require.NoError(t, a.Send(context.Background(), api.AuditBatch{Events: events}))
	require.Len(t, rec.bodies, 2)
	require.Len(t, logs(t, rec.bodies[0]), datadogMaxItems)
	require.Len(t, logs(t, rec.bodies[1]), 1)

	srv, ca, rec = record(t, 0)
	b, err := configure(t, map[string]any{"url": srv.URL, "body": BodyJSONArray, "ca_certificate": ca})
	require.NoError(t, err)
	require.NoError(t, b.Send(context.Background(), api.AuditBatch{Events: events}))
	require.Len(t, rec.bodies, 1, "other bodies are not counted")
}

// TestR382_DatadogPresetUsesTheDatadogBody asserts the Datadog preset sends
// log items to the v2 intake with no query parameters, which Datadog does not
// read for a JSON body.
func TestR382_DatadogPresetUsesTheDatadogBody(t *testing.T) {
	for _, p := range Info().Presets {
		if p.ID == "datadog" {
			require.Equal(t, BodyDatadog, p.Values["body"])
			require.Equal(t, "https://http-intake.logs.datadoghq.com/api/v2/logs", p.Values["url"])
			return
		}
	}
	t.Fatal("no datadog preset")
}
