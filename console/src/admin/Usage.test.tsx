import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { Usage, usageKey } from './Usage';

const app = { id: 'app_x', name: 'crewmate', state: 'running', pinned_spec_id: 'spec_x' } as unknown as App;

function render(running: boolean, layout: 'stack' | 'table') {
  const client = new QueryClient();
  client.setQueryData(usageKey(app), {
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
  const deploying = { id: 'app_x', state: 'deploying', pinned_spec_id: 'spec_x' };
  const running = { ...deploying, state: 'running' };

  it('asks again when the app goes from deploying to running', () => {
    expect(usageKey(running)).not.toEqual(usageKey(deploying));
  });

  it('asks again when a deploy pins another spec', () => {
    expect(usageKey({ ...running, pinned_spec_id: 'spec_y' })).not.toEqual(usageKey(running));
  });

  it('keeps the reading while nothing has changed', () => {
    expect(usageKey({ ...running })).toEqual(usageKey(running));
  });

  it('is still reached by invalidating the app’s usage', async () => {
    const client = new QueryClient();
    client.setQueryData(usageKey(running), { supported: true, workloads: [] });
    await client.invalidateQueries({ queryKey: ['apps', 'app_x', 'usage'] });
    expect(client.getQueryState(usageKey(running))?.isInvalidated).toBe(true);
  });

  it('renders the reading for the app as it is now, not one taken mid-deploy', () => {
    const client = new QueryClient();
    const part = { name: 'web', primary: true, cpu_limit_millis: 0, memory_limit_bytes: 0, disk_bytes: 0, volumes: [] };
    client.setQueryData(usageKey(deploying), {
      supported: true,
      workloads: [{ ...part, running: false, cpu_millis: 0, memory_bytes: 0 }],
    });
    client.setQueryData(usageKey(running), {
      supported: true,
      workloads: [{ ...part, running: true, cpu_millis: 150, memory_bytes: 64 << 20 }],
    });
    const html = renderToString(
      <QueryClientProvider client={client}>
        <Usage app={{ ...app, ...running } as App} layout="stack" />
      </QueryClientProvider>,
    );
    expect(html).not.toContain('Not running');
    expect(html).toContain('>CPU<');
  });
});
