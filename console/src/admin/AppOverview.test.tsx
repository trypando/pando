import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { AppOverview } from './AppOverview';
import { AppVerb, AppVerbs } from './verbs';

const app = { id: 'app_01FAILED', name: 'Skyjo Game', state: 'failed' } as unknown as App;

function render(verbs: string[]) {
  const client = new QueryClient();
  // No deploys, so the only "Open logs" on the screen is the banner's.
  client.setQueryData(['apps', app.id, 'deployments'], { deployments: [] });
  return renderToString(
    <QueryClientProvider client={client}>
      <AppVerbs.Provider value={verbs}>
        <AppOverview app={app} onGo={() => {}} />
      </AppVerbs.Provider>
    </QueryClientProvider>,
  );
}

describe('the overview of a failed app', () => {
  it('offers the logs beside the failure, to somebody who may read them', () => {
    const html = render([AppVerb.LogsRead]);
    const banner = html.slice(html.indexOf('role="alert"'));
    expect(banner).toContain('Pando has stopped trying to start this app.');
    expect(banner).toContain('Open logs');
  });

  it('offers no logs to somebody who may not read them', () => {
    const html = render([AppVerb.View]);
    expect(html).toContain('Pando has stopped trying to start this app.');
    expect(html).not.toContain('Open logs');
  });
});
