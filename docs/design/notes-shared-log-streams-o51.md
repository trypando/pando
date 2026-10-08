# Shared live log streams (O-51)

O-51 decided that the console's Logs tab reads one shared live log stream per app part, fanned out
to every open viewer over server-sent events, so that the runtime's load grows with the number of
parts being watched and not with the number of people watching them. This note records how that is
built and the defaults it chose.

## What it replaced

The Logs tab asked `GET /apps/{id}/logs?tail=500` every five seconds while an app was running. Each
request called `RuntimeAdapter.Logs` with no follow and no cache: a container log read through the
Docker API, a pod log request to the Kubernetes API server, or both through a host's agent on the
multi-host Docker runtime. Fifty people with one app open made ten log reads a second against the
runtime, each for 500 lines. `tail` had no upper bound, so one request could ask for an app's whole
retained history (up to the R-223 cap).

`pando logs --follow` sent `follow=true`, which the handler ignored: it printed the last 200 lines
and exited.

## Design

`internal/core/logstream` holds a `Hub`, one per replica, keyed by (runtime adapter ref, app,
workload).

- **One runtime stream per key.** The first viewer opens `RuntimeAdapter.Logs` with `Follow: true`
  and `Tail` set to the backfill size. Later viewers on the same replica join it. The call runs on a
  context detached from any viewer, so one viewer leaving does not end it for the rest. All three
  runtime adapters (Docker, Kubernetes, multi-host Docker) already honored `Follow`; the adapter
  interface did not change.
- **Backfill.** The stream keeps its most recent lines in a ring. A viewer joining later receives
  that ring first, then live lines. The first viewer receives the runtime's tail as live lines.
- **Grace period.** When the last viewer leaves, the runtime stream stays open for a grace period
  and then closes. A reload or switching parts and back reuses it.
- **Restart.** A followed Docker log ends when the container stops, and a Kubernetes log ends when
  the pod goes. While viewers remain, the hub tells them once (`notice`), reopens with
  `Since` set to when the stream ended, and tells them again when lines arrive. Reopening backs off
  from 1 s to 30 s while the part stays down; a stream that produced lines resets the backoff.
  Docker's `since` has one-second resolution, so a line printed in the second the stream ended can
  appear twice.
- **Slow viewers.** Lines are handed to each viewer through a bounded buffer without waiting. A viewer
  whose buffer is full is disconnected with `event: lagged` and a message saying why; the browser
  reconnects on its own and starts again from the backfill. Nothing a viewer does can make the shared
  stream wait.
- **Authorization.** Each viewer is authorized on connect with `app.logs.read`, exactly as the
  one-shot endpoint is. While connected, the request's credentials are authenticated again and the
  verb checked again every `assertion.Lifetime` (120 s), the same interval and the same rule as the
  proxy's long-lived connections (design 06 §3.1, §4.2, O-13): a revoked session, revoked token,
  suspended user (R-048, R-049) or removed grant ends the stream with `event: revoked`, after which
  the console does not reconnect. An error looking the credential up also ends it. No decision is
  cached in the hub; it shares only what the runtime printed.
- **Long lines** are cut at 16 KiB and end with an ellipsis.

The handlers in `internal/httpapi/log_handlers.go` resolve the app's runtime and part, then hand the
viewer to `Hub.Serve`, which owns the loop. They only format output.

## Surfaces (R-261)

| Surface | Reads |
|---|---|
| Console Logs tab | `GET /apps/{id}/logs/stream` (server-sent events), reconnecting on its own |
| `pando logs --follow` | `GET /apps/{id}/logs?follow=true`, the same shared stream as plain text, Pando's own notices prefixed `[pando]` |
| `pando logs`, `pando logs -n N` | `GET /apps/{id}/logs?tail=N`, one-shot |
| MCP `pando_get_logs` | `GET /apps/{id}/logs`, one-shot, optional `tail` |

The one-shot read still calls the runtime per request. It serves the CLI and agents, which ask once,
and is the console's fallback when the stream cannot be opened (the fallback is also how the console
learns the error's text, since `EventSource` does not expose a refused response).

## Defaults [P]

| Setting | Value | Why |
|---|---|---|
| One-shot `tail` default | 200 | Unchanged. |
| One-shot `tail` cap | 5000 | A larger value is read as 5000 rather than refused. Five thousand lines is more than a screen can usefully show and bounds the work one request asks of the runtime. |
| Backfill (ring size, and the tail the stream opens with) | 1000 lines | Twice what the console used to ask for, so switching to the stream shows no less. |
| Grace period | 30 s | Covers a reload or a quick switch between parts. |
| Viewer buffer | 2048 events | Takes a full backfill arriving at once with room to spare. |
| Re-authorization interval | `assertion.Lifetime` (120 s) | Design 06 §3.1: one revocation window for every long-lived path. |
| Reconnect backoff | 1 s doubling to 30 s | A part that stays down is not asked about every second. |
| Line length | 16 KiB | Bounds memory per buffered event. |
| Console lines kept | 2000 | Older lines drop off the top of the box. |
| SSE `retry` | 3 s | How long a browser waits before reconnecting after `lagged`. |

## Multiple replicas

Each replica holds its own streams. N replicas with viewers of one part are at most N runtime
streams, which is the bound O-51 asks for: load grows with watched parts per replica, never with
viewers. No relay between replicas is needed, because every replica can reach the runtime.

## Not changed

The deploy-log stream (`/deployments/{id}/logs`) is in-process memory, not a runtime read, and was
already one source per deploy. It does not re-check authorization while connected; it ends when the
deploy does, and a running deploy's log is bounded by the build timeout (R-119).
