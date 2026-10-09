import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import type { ReactNode } from 'react';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { AppSpec } from '@api/types.gen';
import { BuildPlan } from './BuildPlan';
import { CarriedFiles } from './CarriedFiles';
import { Environment } from './Environment';

const appID = 'app_01NOTES';

// Revision 2 is newest and not the pinned one: every screen here edits on top
// of the newest revision, so what it shows has to come from revision 2.
const newest = {
  id: 'spec_2',
  revision: 2,
  body: {
    build: { dockerfile: '.nixpacks/Dockerfile', generated_files: { '.nixpacks/Dockerfile': 'FROM node:22-slim' } },
    workloads: [
      {
        name: 'web',
        env: [
          { key: 'GREETING', value: 'hello' },
          { key: 'API_KEY', value: '' },
        ],
        files: [{ path: '/etc/caddy/Caddyfile', content: ':80 { respond "hi" }' }],
      },
    ],
  } as unknown as AppSpec,
};

function render(node: ReactNode, listError = false) {
  // Kept from retrying on mount, so the failed list is what renders.
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } });
  if (listError) {
    client.getQueryCache().build(client, { queryKey: ['apps', appID, 'specs'] }).setState({
      status: 'error',
      error: new Error('connection refused'),
    });
  } else {
    client.setQueryData(['apps', appID, 'specs'], {
      revisions: [{ id: 'spec_1', revision: 1 }, { id: 'spec_2', revision: 2 }],
      pinned_spec_id: 'spec_1',
    });
    client.setQueryData(['apps', appID, 'spec', 2], newest);
  }
  return renderToString(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

describe('screens built on the newest spec revision', () => {
  it('Environment lists the newest revision’s variables and counts the empty ones', () => {
    const html = render(<Environment appID={appID} />);
    expect(html).toContain('GREETING');
    expect(html).toContain('hello');
    expect(html).toContain('API_KEY has no value yet.');
  });

  it('Environment says why when the revisions cannot be read', () => {
    const html = render(<Environment appID={appID} />, true);
    expect(html).toContain('Pando could not reach the server.');
    expect(html).not.toContain('no value yet');
  });

  it('CarriedFiles lists the newest revision’s files', () => {
    expect(render(<CarriedFiles appID={appID} />)).toContain('/etc/caddy/Caddyfile');
  });

  it('BuildPlan renders from the newest revision', () => {
    expect(render(<BuildPlan appID={appID} />)).toContain('FROM node:22-slim');
  });
});
