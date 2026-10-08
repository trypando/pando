import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AuditSinks } from './AuditSinks';
import type { AuditSink } from './AuditSinks';
import { AuditTable } from './Installation';
import type { AuditRecord } from './Installation';

function render(sinks: AuditSink[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(['audit-sinks'], { audit_sinks: sinks });
  // Without the markers React puts between adjacent pieces of text, so a
  // sentence can be looked for as it reads.
  return renderToString(
    <QueryClientProvider client={client}>
      <AuditSinks onAdapters={() => {}} />
    </QueryClientProvider>,
  ).replace(/<!-- -->/g, '');
}

const splunk: AuditSink = {
  id: 'audit_sink/https',
  kind: 'https',
  name: 'Splunk',
  enabled: true,
  transport: 'https',
  endpoint: 'splunk.example.com:8088',
  format: 'native',
  backlog: 12,
  adapter_id: 'ad_splunk',
  delivered_at: '2026-10-08T11:58:00Z',
  cursor_at: '2026-10-08T11:57:00Z',
  delivered_count: 4031,
  attempts: 0,
  disclosure: 'Every audit event is sent to splunk.example.com:8088 over HTTPS.',
};

describe('Audit sinks on the Audit log screen (R-385)', () => {
  it('says that a copy of the log leaves, and where', () => {
    const html = render([splunk]);
    expect(html).toContain('Every audit event is sent to splunk.example.com:8088 over HTTPS.');
    expect(html).toContain('Delivering');
    expect(html).toContain('Last delivered');
    expect(html).toContain('12 events behind.');
    expect(html).toContain('Open adapters');
  });

  it('says at least, when the backlog is too long to count', () => {
    const html = render([{ ...splunk, backlog: 100_000, backlog_capped: true }]);
    expect(html).toContain('At least 100,000 events behind.');
  });

  it('shows a failing sink’s error and since when', () => {
    const html = render([
      {
        ...splunk,
        failing_since: '2026-10-08T09:00:00Z',
        last_error: 'POST https://splunk.example.com:8088/services/collector: 503 Service Unavailable',
        last_error_at: '2026-10-08T11:00:00Z',
        attempts: 4,
        next_attempt_at: '2026-10-08T13:00:00Z',
      },
    ]);
    expect(html).toContain('Every audit event is sent to splunk.example.com:8088 over HTTPS.');
    expect(html).toContain('Failing');
    expect(html).toContain('4 attempts');
    expect(html).toContain('503 Service Unavailable');
    expect(html).toContain('Next attempt at');
  });

  it('shows what a sink Pando turned off missed, and points to the archive', () => {
    const html = render([
      {
        ...splunk,
        enabled: false,
        disabled_at: '2026-09-01T00:00:00Z',
        disabled_reason: 'It failed for 24 hours',
        gap_from: '2026-06-01T00:00:00Z',
        gap_to: '2026-07-01T00:00:00Z',
      },
    ]);
    expect(html).toContain('Turned off by Pando');
    expect(html).toContain('It failed for 24 hours.');
    expect(html).toContain('removed from the live log while it was off');
    expect(html).toContain('The archive for those months is the backfill');
    expect(html).toContain('href="#archived-months"');
    // It is not sending, so it is not described as sending.
    expect(html).not.toContain('Every audit event is sent to');
  });

  it('shows a sink an administrator turned off as one line', () => {
    const html = render([{ ...splunk, enabled: false }]);
    expect(html).toContain('is turned off and sends nothing.');
    expect(html).not.toContain('Every audit event is sent to');
  });

  it('shows nothing when no sink exists', () => {
    expect(render([])).toBe('');
  });
});

describe('Audit log rows (R-379)', () => {
  const row = (r: Partial<AuditRecord>): AuditRecord => ({
    id: 1,
    occurred_at: '2026-10-08T12:00:00Z',
    principal_kind: 'user',
    principal_id: 'usr_01',
    action: 'app.delete',
    ...r,
  });
  const table = (events: AuditRecord[]) =>
    renderToString(<AuditTable events={events} people={[]} empty={null} />).replace(/<!-- -->/g, '');

  it('names the actor as recorded, and tells denied from failed', () => {
    const html = table([
      row({ actor_name: 'Dana Reyes', outcome: 'denied', source_ip: '203.0.113.7' }),
      row({ id: 2, outcome: 'failed' }),
    ]);
    expect(html).toContain('Dana Reyes');
    expect(html).toContain('Denied');
    expect(html).toContain('Failed');
    expect(html).toContain('203.0.113.7');
  });

  it('reads an event from before R-379 as it did', () => {
    const html = table([row({})]);
    expect(html).toContain('usr_01');
    expect(html).not.toContain('Denied');
    expect(html).not.toContain('Failed');
  });
});
