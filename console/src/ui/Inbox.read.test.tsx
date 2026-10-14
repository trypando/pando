// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { InboxButton } from './Inbox';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

interface Row {
  id: string;
  kind: string;
  subject: string;
  link?: string;
  read_at?: string;
  created_at: string;
}

let root: Root;
let host: HTMLElement;
let rows: Row[];
let calls: string[];
// Resolves the next mark-read request; held open so a test can see what the
// dialog does while it is in flight.
let releaseRead: (() => void) | undefined;

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  calls = [];
  releaseRead = undefined;
  rows = [
    { id: 'ntf_2', kind: 'deploy_failed', subject: 'notes failed to deploy', created_at: '2026-10-14T10:00:00Z' },
    {
      id: 'ntf_1',
      kind: 'backup_failed',
      subject: 'Backup of wiki failed',
      created_at: '2026-10-13T10:00:00Z',
      read_at: '2026-10-13T11:00:00Z',
    },
  ];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), 'http://pando.test');
      const method = init?.method ?? 'GET';
      calls.push(`${method} ${url.pathname}${url.search}`);
      const unread = rows.filter((r) => !r.read_at);
      if (method === 'POST') {
        const id = url.pathname.match(/notifications\/([^/]+)\/read$/)?.[1];
        await new Promise<void>((r) => (releaseRead = r));
        for (const r of rows) if (!id || r.id === id) r.read_at ??= '2026-10-14T12:00:00Z';
        return json({});
      }
      if (url.pathname.endsWith('/unread')) return json({ unread: unread.length });
      const list = url.searchParams.get('unread') === 'true' ? unread : rows;
      return json({ notifications: list, unread: unread.length });
    }),
  );
});

afterEach(() => {
  act(() => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});

const settle = () => act(async () => void (await new Promise((r) => setTimeout(r, 0))));
const dialog = () => document.querySelector('[role="dialog"]') as HTMLElement;
const button = (text: string) =>
  [...dialog().querySelectorAll('button')].find((b) => b.textContent?.includes(text)) as HTMLButtonElement;

async function openInbox() {
  root = createRoot(host);
  act(() =>
    root.render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <InboxButton />
      </QueryClientProvider>,
    ),
  );
  await settle();
  await act(async () => host.querySelector('button')!.click());
  await settle();
}

describe('InboxDialog', () => {
  // TestR377: one notification marked read leaves the unread list as it is
  // opened, and stays reachable under All.
  it('clears a notification from Unread when it is read, and keeps it under All', async () => {
    await openInbox();
    expect(calls).toContain('GET /api/v1/me/notifications?unread=true');
    expect(dialog().textContent).toContain('notes failed to deploy');
    expect(dialog().textContent).not.toContain('Backup of wiki failed');

    await act(async () => button('notes failed to deploy').click());
    await settle();
    // Gone while the request is still in flight.
    expect(dialog().textContent).not.toContain('notes failed to deploy');
    expect(dialog().textContent).toContain('Nothing unread');
    await act(async () => releaseRead!());
    await settle();
    expect(calls).toContain('POST /api/v1/me/notifications/ntf_2/read');

    await act(async () => button('All').click());
    await settle();
    expect(dialog().textContent).toContain('notes failed to deploy');
    expect(dialog().textContent).toContain('Backup of wiki failed');
    expect(dialog().textContent).not.toContain('New');
  });

  // R-377: all can be marked read, and the unread list empties.
  it('empties Unread when all are marked read', async () => {
    await openInbox();
    await act(async () => button('Mark all read').click());
    await settle();
    expect(calls).toContain('POST /api/v1/me/notifications/read');
    expect(dialog().textContent).toContain('Nothing unread');
    // The bell's own count, read with the dialog closed, while the request is
    // still in flight.
    await act(async () => button('Close').click());
    expect(host.querySelector('button')!.getAttribute('aria-label')).toBe('Notifications');
    await act(async () => releaseRead!());
    await settle();
  });

  // Navigating first cancelled the request: the notification came back unread.
  it('marks a notification read before following its link', async () => {
    rows[0]!.link = '/apps/app_1';
    const assign = vi.fn();
    vi.stubGlobal('location', { ...window.location, origin: window.location.origin, assign });
    await openInbox();

    await act(async () => button('notes failed to deploy').click());
    expect(assign).not.toHaveBeenCalled();
    await act(async () => releaseRead!());
    await settle();
    expect(assign).toHaveBeenCalledWith('/apps/app_1');
  });

  // Opened during the click, while the browser still allows a new tab, rather
  // than after the request: this page stays, so the request is not cancelled.
  it('opens a link elsewhere at once, without waiting for the request', async () => {
    rows[0]!.link = 'https://status.example.com/incident';
    const opened = vi.fn();
    vi.stubGlobal('open', opened);
    await openInbox();

    await act(async () => button('notes failed to deploy').click());
    expect(opened).toHaveBeenCalledWith('https://status.example.com/incident', '_blank', 'noopener');
    await act(async () => releaseRead!());
    await settle();
    expect(calls).toContain('POST /api/v1/me/notifications/ntf_2/read');
  });
});
