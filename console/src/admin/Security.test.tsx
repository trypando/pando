import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { Security } from './Security';

function render(report: unknown): string {
  const client = new QueryClient();
  client.setQueryData(['apps', 'app_x', 'security'], report);
  return renderToString(
    <QueryClientProvider client={client}>
      <Security appID="app_x" />
    </QueryClientProvider>,
  );
}

const counts = { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 };

describe('the overview’s security section (R-311)', () => {
  it('during the first deploy, shows the source’s findings and no score until the image is scanned', () => {
    const html = render({
      standing: { verdict: 'ok', score: null, threshold: 0 },
      scan: { id: 'scn_1', app_id: 'app_x', commit: '3f9a2c1', scanner_ref: 'scn', score: 100, findings: [], ran_at: '' },
      counts,
      scope: 'source',
      awaiting_image: true,
      ignoring_unfixable: false,
    });
    expect(html).toContain('Scored once the image is built.');
    expect(html).toContain('from the source, before this configuration');
    expect(html).not.toContain('100 / 100');
    expect(html).not.toContain('has not been scanned yet');
  });

  it('shows the score once a scan has read the image', () => {
    const html = render({
      standing: { verdict: 'ok', score: 61, threshold: 0 },
      scan: {
        id: 'scn_2',
        app_id: 'app_x',
        spec_id: 'spec_1',
        commit: '3f9a2c1',
        image: 'sha256:' + 'ab'.repeat(32),
        scanner_ref: 'scn',
        score: 61,
        findings: [],
        ran_at: '',
      },
      counts: { ...counts, critical: 1, high: 1, medium: 1, low: 1 },
      scope: 'image_and_source',
      ignoring_unfixable: false,
    });
    expect(html).toContain('61 / 100');
    expect(html).toContain('of the image it runs and its source');
    expect(html).not.toContain('Scored once the image is built');
  });
});
