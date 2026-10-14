// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { InboxButton } from './Inbox';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let root: Root;
let header: HTMLElement;
let queries: QueryClient;

beforeEach(() => {
  // The launcher's header is its own stacking context, as it is in the console.
  header = document.createElement('header');
  header.style.isolation = 'isolate';
  document.body.append(header);
  queries = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify({ notifications: [], unread: 0 }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    ),
  );
});

afterEach(() => {
  act(() => root.unmount());
  header.remove();
  vi.unstubAllGlobals();
});

describe('InboxButton', () => {
  // A dialog inside the header was layered only within it, under the app
  // tiles. Open, it belongs to the body.
  it('opens the inbox outside the header it sits in', async () => {
    root = createRoot(header);
    act(() =>
      root.render(
        <QueryClientProvider client={queries}>
          <InboxButton />
        </QueryClientProvider>,
      ),
    );
    await act(async () => {
      header.querySelector('button')!.click();
      await new Promise((r) => setTimeout(r, 0));
    });

    const dialog = document.querySelector('[role="dialog"]');
    expect(dialog?.textContent).toContain('Notifications');
    expect(header.contains(dialog)).toBe(false);

    await act(async () => {
      [...dialog!.querySelectorAll('button')].find((b) => b.textContent === 'Close')!.click();
    });
    expect(document.querySelector('[role="dialog"]')).toBeNull();
  });
});
