import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { Usage, refreshUsageIfMoved } from './Usage';

const app = { id: 'app_x', name: 'crewmate', pinned_spec_id: 'spec_x' } as unknown as App;

function render(running: boolean, layout: 'stack' | 'table') {
  const client = new QueryClient();
  client.setQueryData(['apps', app.id, 'usage'], {
    supported: true,
    reported_at: '2026-09-26T22:00:00Z',
    host_cpu_millis: 4000,
    host_memory_bytes: 8 << 30,
    workloads: [
      {
        name: 'web',
        primary: true,
        running,
        cpu_millis: running ? 150 : 0,
        cpu_limit_millis: 0,
        memory_bytes: running ? 64 << 20 : 0,
        memory_limit_bytes: 0,
        disk_bytes: 76 << 10,
        volumes: [],
      },
    ],
  });
  return renderToString(
    <QueryClientProvider client={client}>
      <Usage app={app} layout={layout} />
    </QueryClientProvider>,
  );
}

const count = (html: string, s: string) => html.split(s).length - 1;

describe('In use, for a part that is not running (#86)', () => {
  it('says so once beside the part, and still gives its disk', () => {
    const html = render(false, 'stack');
    expect(count(html, 'Not running')).toBe(1);
    expect(html).not.toContain('>CPU<');
    expect(html).not.toContain('>Memory<');
    expect(html).toContain('>Disk<');
    expect(html).toContain('76 KB');
  });

  it('stacks every label above its value rather than beside it', () => {
    const html = render(true, 'stack');
    for (const label of ['CPU', 'Memory', 'Disk']) {
      expect(html).toMatch(new RegExp(`flex-direction:column[^>]*><span[^>]*>${label}</span>`));
    }
  });

  it('gives a stopped reading and the disk the same type in the table', () => {
    const html = render(false, 'table');
    expect(html).toMatch(/font:var\(--type-body-ui\)[^>]*>Not running</);
    expect(html).toMatch(/font:var\(--type-body-ui\)[^>]*>76 KB</);
  });
});

describe('In use, after a deploy finishes', () => {
  const invalidated = (client: QueryClient) => client.getQueryState(['apps', app.id, 'usage'])?.isInvalidated;

  function cached() {
    const client = new QueryClient();
    client.setQueryData(['apps', app.id, 'usage'], { supported: true, workloads: [] });
    return client;
  }

  it('reads again when the app goes from deploying to running', () => {
    const client = cached();
    const moved = refreshUsageIfMoved(
      client,
      app.id,
      { state: 'deploying', pinned_spec_id: 'spec_x' },
      { state: 'running', pinned_spec_id: 'spec_x' },
    );
    expect(moved).toBe(true);
    expect(invalidated(client)).toBe(true);
  });

  it('reads again when a new deploy pins another spec', () => {
    const client = cached();
    expect(
      refreshUsageIfMoved(client, app.id, { state: 'running', pinned_spec_id: 'spec_x' }, { state: 'running', pinned_spec_id: 'spec_y' }),
    ).toBe(true);
    expect(invalidated(client)).toBe(true);
  });

  it('leaves the reading alone while nothing has changed', () => {
    const client = cached();
    expect(
      refreshUsageIfMoved(client, app.id, { state: 'running', pinned_spec_id: 'spec_x' }, { state: 'running', pinned_spec_id: 'spec_x' }),
    ).toBe(false);
    expect(invalidated(client)).toBe(false);
  });
});
