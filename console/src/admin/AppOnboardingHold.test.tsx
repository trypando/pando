// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AppOnboarding } from './AppOnboarding';
import type { AppWithVerbs } from './verbs';

// Rendered into a document, so effects and timers run: the hold between
// "Plan ready" and the review is an effect and a timer, and that is where this
// went wrong.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const app = {
  id: 'app_x',
  name: '2048',
  source: { type: 'git', url: 'https://github.com/gabrielecirulli/2048', ref: 'master' },
  verbs: ['app.view', 'app.spec.edit', 'app.deploy', 'app.delete', 'app.secrets.write'],
} as unknown as AppWithVerbs;

const detection = {
  commit: '3f9a2c1d0000',
  winning_bid: { detector: 'static', strategy: 'static', confidence: 0.9, evidence: ['index.html at the root'] },
  questions: [],
  draft_spec: {
    build: { strategy: 'static' },
    workloads: [{ name: 'web', primary: true, exposed: true }],
  },
  trial: {},
};

const report = {
  standing: { verdict: 'secure' },
  scan: { score: 100 },
  counts: { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 },
  worst: [],
  ignoring_unfixable: false,
};

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

let root: Root;
let container: HTMLDivElement;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.stubGlobal(
    'matchMedia',
    (query: string) =>
      ({
        matches: false,
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }) as unknown as MediaQueryList,
  );
  container = document.createElement('div');
  document.body.append(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('AppOnboarding, watching detection finish', () => {
  // The security report is asked for only once detection has stopped running,
  // so on a quick repository it arrives during the "Plan ready" hold and adds
  // the scan step then. The page stayed on "Plan ready" with no way to accept
  // the plan until it was reloaded.
  it('shows the plan’s actions once the hold ends, even when the scan arrives during it', async () => {
    let sendReport: () => void = () => {};
    const scanned = new Promise<Response>((resolve) => {
      sendReport = () => resolve(json(report));
    });
    vi.stubGlobal('fetch', (input: string) => {
      if (input.endsWith('/security')) return scanned;
      return Promise.resolve(
        json({ status: 'needs_answers', answers: {}, commit: detection.commit, updated_at: new Date().toISOString(), detection }),
      );
    });

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    root = createRoot(container);
    await act(async () => {
      root.render(
        <QueryClientProvider client={client}>
          <AppOnboarding app={app} onBack={() => {}} onDeployRefused={() => {}} />
        </QueryClientProvider>,
      );
    });

    // Play the steps through until the hold begins.
    for (let i = 0; i < 600 && !container.textContent?.includes('Plan ready'); i++) {
      await act(() => vi.advanceTimersByTimeAsync(100));
    }
    expect(container.textContent).toContain('Plan ready');
    expect(container.textContent).not.toContain('Accept and deploy');

    // The scan lands mid-hold, then everything has time to settle.
    await act(async () => sendReport());
    for (let i = 0; i < 300; i++) await act(() => vi.advanceTimersByTimeAsync(100));

    expect(container.textContent).toContain('Accept and deploy');
  });
});
