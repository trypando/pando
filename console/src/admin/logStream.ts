// The app's live output, from `GET /apps/{id}/logs/stream` (O-51).
//
// The Logs tab used to ask for the last 500 lines every five seconds, from
// every open tab, and each ask was a read from the runtime. Now every viewer of
// one part on a Pando server shares one runtime stream, and this follows it
// over server-sent events:
//
//   event: reset    — the stream (re)started; the recent lines follow
//   data: <line>    — one line the app printed
//   event: notice   — Pando reporting on the stream: it ended, it reconnected
//   event: lagged   — this viewer fell behind and was cut off; the browser
//                     reconnects on its own, and the server starts with reset
//   event: revoked  — access ended; reconnecting must not happen
//
// Kept apart from the component so the protocol can be tested without a DOM.

/** The most lines held on screen. Older lines drop off the top. */
export const KEEP = 2_000;

/** Appends `add` to `previous`, keeping at most `keep` lines. */
export function appendCapped(previous: string[], add: string[], keep = KEEP): string[] {
  if (add.length === 0) return previous;
  const joined = previous.concat(add);
  return joined.length > keep ? joined.slice(joined.length - keep) : joined;
}

/** How a notice reads in the log, set apart from what the app printed. */
export function noticeLine(text: string): string {
  return `[pando] ${text}`;
}

/** What the stream tells the screen. */
export interface LogStreamHandlers {
  /** The stream (re)started: replace what is shown. */
  reset(): void;
  /** Lines arrived, notices included, batched. */
  lines(lines: string[]): void;
  /** Pando ended the stream for good; the text says why. */
  revoked(message: string): void;
  /** The stream could not be opened, or closed and will not reopen. */
  failed(): void;
}

/** The part of EventSource this uses, so a test can supply its own. */
export interface EventSourceLike {
  readyState: number;
  onerror: ((this: EventSourceLike, ev: Event) => unknown) | null;
  onmessage: ((this: EventSourceLike, ev: MessageEvent) => unknown) | null;
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void;
  close(): void;
}

/** EventSource.CLOSED, which a test's stand-in cannot be relied on to carry. */
const CLOSED = 2;

/**
 * Follows one part's live log. Returns a function that stops following.
 *
 * Lines are handed over in batches, once per `schedule` tick: a backfill is a
 * thousand events arriving at once, and a render for each one is a thousand
 * renders.
 */
export function followLog(
  url: string,
  on: LogStreamHandlers,
  open: (url: string) => EventSourceLike = (u) => new EventSource(u) as unknown as EventSourceLike,
  schedule: (fn: () => void) => void = (fn) => window.setTimeout(fn, 50),
): () => void {
  const stream = open(url);
  let pending: string[] = [];
  let scheduled = false;
  let stopped = false;

  const flush = () => {
    scheduled = false;
    if (stopped || pending.length === 0) return;
    const batch = pending;
    pending = [];
    on.lines(batch);
  };
  const push = (line: string) => {
    pending.push(line);
    if (!scheduled) {
      scheduled = true;
      schedule(flush);
    }
  };

  stream.addEventListener('reset', () => {
    pending = [];
    on.reset();
  });
  stream.onmessage = (event) => push(String(event.data));
  stream.addEventListener('notice', (event) => push(noticeLine(String(event.data))));
  stream.addEventListener('revoked', (event) => {
    flush();
    stopped = true;
    stream.close();
    on.revoked(String(event.data));
  });
  // `lagged` needs nothing here: the server closes the connection after it,
  // the browser reconnects after the retry interval the server set, and the
  // new connection starts with reset. Showing why is enough.
  stream.addEventListener('lagged', (event) => push(noticeLine(String(event.data))));

  stream.onerror = () => {
    // A dropped connection is retried by the browser (readyState CONNECTING).
    // CLOSED means it will not be: the server answered with an error.
    if (stream.readyState === CLOSED && !stopped) {
      flush();
      stopped = true;
      on.failed();
    }
  };

  return () => {
    stopped = true;
    stream.close();
  };
}
