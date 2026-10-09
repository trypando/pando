// Mapping Pando's app states onto the design system's status symbols.
//
// The state machine has eight states (design 05 §1.1) and the design system has
// five symbols: filled circle running, hollow ring building, filled triangle
// failed, short dash stopped, filled circle info. So this is a real mapping and
// not a rename, and two parts of it are worth arguing about.
//
// **`degraded` has no symbol of its own.** It is the reconciler's most
// load-bearing distinction — degraded is recoverable and still being worked,
// `failed` is terminal and needs a person (R-151) — and the brand spec predates
// it. Mapping it to `failed` would be wrong twice: it would put marker red on an
// app that is being recovered, breaking both R-151's distinction and the "under
// 3% red" rule. It maps to `building`'s hollow ring instead, which already means
// "in flux, being worked on" — which is exactly what degraded is — and the word
// beside the symbol says "Degraded". Status is a symbol *plus a word*, and here
// the word carries the difference.
//
// The system should grow a sixth symbol for it. That belongs in the design
// project, which PROVENANCE.md names as the source of truth, not here.
//
// **`draft`, `proposed` and `archived` are not deploy states at all.** They get
// `info`, because a symbol implying activity would be a lie about an app that
// has never run.

import type { StatusIndicatorProps } from '@design';

type AppState =
  | 'draft'
  | 'proposed'
  | 'deploying'
  | 'running'
  | 'degraded'
  | 'stopped'
  | 'failed'
  | 'archived';

type Symbol = NonNullable<StatusIndicatorProps['status']>;

const symbols: Record<AppState, Symbol> = {
  draft: 'info',
  proposed: 'info',
  archived: 'info',

  deploying: 'building',

  // See the note above. Not `failed`, deliberately.
  degraded: 'building',

  running: 'running',
  stopped: 'stopped',
  failed: 'failed',
};

// Sentence case, and named for what the person is looking at rather than for
// the state machine's internals.
const labels: Record<AppState, string> = {
  draft: 'Draft',
  proposed: 'Not deployed',
  deploying: 'Deploying',
  running: 'Running',
  degraded: 'Degraded',
  stopped: 'Stopped',
  failed: 'Failed',
  archived: 'Deleted',
};

export function statusSymbol(state: string): Symbol {
  return symbols[state as AppState] ?? 'info';
}

/**
 * stoppedForIdle says Pando stopped the app because nobody used it (R-396),
 * which is said in the word: the symbol is the same dash, and a stop nobody
 * chose is not mistaken for one somebody did, or for a failure.
 */
export function statusLabel(state: string, stoppedForIdle?: boolean): string {
  if (stoppedForIdle && state === 'stopped') return 'Stopped for inactivity';
  return labels[state as AppState] ?? state;
}
