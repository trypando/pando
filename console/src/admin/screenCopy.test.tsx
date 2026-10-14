import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import type { ReactNode } from 'react';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { AppOverview } from './AppOverview';
import { DetectionReview } from './DetectionReview';
import { Resources } from './Resources';
import { Sharing } from './Sharing';

/** Renders a screen over a cache seeded with `data`, by query key. */
function render(screen: ReactNode, data: Array<[unknown[], unknown]>): string {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  for (const [key, value] of data) client.setQueryData(key, value);
  // React separates adjacent text with empty comments in server output.
  return renderToString(<QueryClientProvider client={client}>{screen}</QueryClientProvider>).replaceAll('<!-- -->', '');
}

const app = { id: 'app_1', name: 'inventory', pinned_spec_id: 'spec_1' } as App;

// One of each way a dependency is filled.
const slots = [
  { key: 'DATABASE_URL', type: 'postgres', required: true, resolution: { mode: 'provisioned' } },
  { key: 'REDIS_URL', type: 'redis', resolution: { mode: 'bound' } },
  { key: 'SMTP_URL', type: 'smtp', resolution: { mode: 'literal', secret_ref: 'sec_1' } },
];

describe('how a dependency is filled', () => {
  it('reads Provisioned, External or Value on the app’s settings', () => {
    const html = render(<Resources app={app} />, [[['slots', 'app_1'], { slots }]]);
    expect(html).toContain('>Provisioned<');
    expect(html).toContain('>External<');
    expect(html).toContain('>Value<');
  });

  it('reads the same words in the detection review', () => {
    const detection = {
      winning_bid: { detector: 'dockerfile', strategy: 'dockerfile', confidence: 0.9, evidence: [] },
      draft_spec: { workloads: [{ name: 'web' }], slots },
    };
    const html = render(<DetectionReview appID="app_1" reviewed={false} />, [
      [['apps', 'app_1', 'detection'], { status: 'ready', answers: {}, detection }],
    ]);
    expect(html).toContain('>Provisioned<');
    expect(html).toContain('>External<');
    expect(html).toContain('>Value<');
  });
});

describe('variables declared with no value', () => {
  const overview = (keys: string[]) =>
    render(<AppOverview app={app} onGo={() => {}} />, [
      [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
      [['apps', 'app_1', 'spec', 1], { id: 'spec_1', revision: 1, body: { workloads: [{ name: 'web', env: keys.map((key) => ({ key })) }] } }],
    ]);

  it('names one', () => {
    expect(overview(['API_KEY'])).toContain('API_KEY is declared with no value.');
  });

  it('names four, then counts the rest', () => {
    expect(overview(['A', 'B', 'C', 'D', 'E', 'F'])).toContain('6 variables are declared with no value: A, B, C, D, and 2 more.');
  });
});

describe('who has access', () => {
  const sharing = (grants: unknown[]) =>
    render(<Sharing appID="app_1" appName="inventory" />, [[['apps', 'app_1', 'grants', '', {}], { pages: [{ grants }], pageParams: [''] }]]);

  it('says what everyone can do, with or without a passcode', () => {
    expect(sharing([{ id: 'gr_1', plane: 'data', principal_kind: 'anonymous', passcode: true }])).toContain('Use, with the passcode');
    expect(sharing([{ id: 'gr_1', plane: 'data', principal_kind: 'anonymous' }])).toContain('Use, without signing in');
  });

  it('lists a person’s role and their use of the app in one cell', () => {
    const html = sharing([
      { id: 'gr_1', plane: 'control', principal_kind: 'user', principal_id: 'usr_1', principal_name: 'dana', role_name: 'owner' },
      { id: 'gr_2', plane: 'data', principal_kind: 'user', principal_id: 'usr_1', principal_name: 'dana' },
    ]);
    expect(html).toContain('>Owner<');
    expect(html).toContain('>Use<');
  });
});
