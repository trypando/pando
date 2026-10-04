import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { Capacity } from './Capacity';
import type { RuntimeCapacity } from './Capacity';

function render(runtimes: RuntimeCapacity[]) {
  const client = new QueryClient();
  client.setQueryData(['capacity'], { runtimes });
  // Without the markers React puts between adjacent pieces of text, so a
  // sentence can be looked for as it reads.
  return renderToString(
    <QueryClientProvider client={client}>
      <Capacity names={{ rt_docker: 'Docker' }} />
    </QueryClientProvider>,
  ).replace(/<!-- -->/g, '');
}

const docker: RuntimeCapacity = {
  adapter_ref: 'rt_docker',
  status: 'ok',
  total_cpu_millis: 8000,
  total_memory_bytes: 16 * 2 ** 30,
  total_disk_bytes: 0,
  allocated_cpu_millis: 6000,
  allocated_memory_bytes: 12 * 2 ** 30,
  allocated_disk_bytes: 0,
  in_use_cpu_millis: 1250,
  in_use_memory_bytes: 2 * 2 ** 30,
  running_workloads: 14,
  details: { server_version: '27.0' },
  reported: '2026-10-04T12:00:00Z',
};

// R-243: the common readings every runtime reports are read as what they mean,
// not printed as the adapter's JSON — which stays, behind Details.
describe('Capacity (R-242, R-243)', () => {
  it('answers how much is left, per runtime, by its name', () => {
    const html = render([docker]);
    expect(html).toContain('Docker');
    expect(html).toContain('rt_docker');
    expect(html).toContain('2.00 cores left');
    expect(html).toContain('4.0 GB left');
    expect(html).toContain('6.00 cores committed');
    expect(html).toContain('1.25 cores in use now');
    expect(html).toContain('14 workloads running');
  });

  it('keeps the runtime’s own details behind a disclosure', () => {
    const html = render([docker]);
    expect(html).toContain('Show details');
    expect(html).not.toContain('server_version');
  });

  it('says a reading is not reported rather than drawing an empty meter', () => {
    const html = render([docker]);
    expect(html).toContain('Not reported, so Pando does not check disk before a deploy.');
    expect(html.split('role="meter"').length - 1).toBe(2);
  });

  it('shows no in-use figure for a runtime that does not report usage', () => {
    const { in_use_cpu_millis: _c, in_use_memory_bytes: _m, ...quiet } = docker;
    expect(render([quiet])).not.toContain('in use now');
  });

  it('marks committed near the total', () => {
    expect(render([{ ...docker, allocated_cpu_millis: 7600 }])).toContain('var(--marker)');
    expect(render([docker])).not.toContain('var(--marker)');
  });

  it('says plainly when a runtime reports nothing, or cannot be reached', () => {
    const silent: RuntimeCapacity = { adapter_ref: 'rt_other', status: 'ok', running_workloads: -1 };
    expect(render([silent])).toContain('This runtime does not report its capacity');
    expect(render([silent])).not.toContain('workloads running');
    expect(render([{ adapter_ref: 'rt_down', status: 'unreachable' }])).toContain('Pando could not reach this runtime');
  });

  it('has an empty state when no runtime is set up', () => {
    expect(render([])).toContain('No runtime is set up');
  });
});
