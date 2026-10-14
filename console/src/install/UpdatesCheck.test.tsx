// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { Status } from '@api/types.gen';
import { Updates } from './Updates';

// Rendered into a document so the button can be pressed: the check is a
// mutation, and what it answers replaces what the screen shows (R-349).
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const before: Status = {
  current: '0.4.0',
  development: false,
  enabled: true,
  channel: 'stable',
  checked_at: '2026-10-13T18:00:00Z',
  latest: '0.4.0',
  available: false,
  releases: [],
  security: false,
  breaking: false,
};

const after: Status = {
  ...before,
  checked_at: '2026-10-13T23:40:00Z',
  latest: '0.5.0',
  available: true,
  releases: [{ version: '0.5.0', prerelease: false, published_at: '2026-10-13T23:38:00Z', url: '', notes: '- New.\n', security: false, breaking: true }],
  breaking: true,
};

let root: Root;
let container: HTMLDivElement;
let queries: QueryClient;

beforeEach(() => {
  container = document.createElement('div');
  document.body.append(container);
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queries.setQueryData(['me'], { verbs: ['install.view'] });
  queries.setQueryData(['updates'], before);
  queries.setQueryData(['upgrade-last'], { upgrade: null });
  queries.setQueryData(['upgrade-plan', '0.5.0'], { current: '0.4.0', target: '0.5.0', possible: false, reasons: [], breaking: [] });
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
});

async function pressCheck() {
  root = createRoot(container);
  act(() =>
    root.render(
      <QueryClientProvider client={queries}>
        <Updates />
      </QueryClientProvider>,
    ),
  );
  const button = [...container.querySelectorAll('button')].find((b) => b.textContent === 'Check for updates');
  expect(button).toBeDefined();
  await act(async () => {
    button!.click();
    await new Promise((r) => setTimeout(r, 0));
  });
}

describe('Check for updates (R-349)', () => {
  it('asks the server now and shows what it found', async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify(after), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetch);

    expect(container.textContent).not.toContain('0.5.0');
    await pressCheck();

    expect(fetch).toHaveBeenCalledWith(expect.stringMatching(/\/api\/v1\/updates\/check$/), expect.objectContaining({ method: 'POST' }));
    expect(container.textContent).toContain('Update available');
    expect(container.textContent).toContain('0.5.0');
  });

  it('shows why a check failed', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        new Response(JSON.stringify({ code: 'INTERNAL', message: 'Pando could not read host policy.' }), { status: 500 }),
      ),
    );

    await pressCheck();

    expect(container.textContent).toContain('Pando could not read host policy.');
  });
});
