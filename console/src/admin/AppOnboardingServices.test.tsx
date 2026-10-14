// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AppOnboarding } from './AppOnboarding';
import type { AppWithVerbs } from './verbs';

// Rendered into a document, so the choice of who provides a service can be
// made and its effect on the variables and the plan read back.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const app = {
  id: 'app_x',
  name: 'yopass',
  source: { type: 'git', url: 'https://github.com/jhaals/yopass', ref: 'master' },
  verbs: ['app.view', 'app.spec.edit', 'app.deploy', 'app.delete', 'app.secrets.write'],
} as unknown as AppWithVerbs;

// Yopass: a Go server that reads its Memcached's address from MEMCACHED, and a
// PostgreSQL that is only reachable as its parts, to show both phrasings.
const detection = {
  commit: '3f9a2c1d0000',
  winning_bid: { detector: 'dockerfile', strategy: 'dockerfile', confidence: 0.9, evidence: ['Dockerfile at the root'] },
  questions: [],
  draft_spec: {
    build: { strategy: 'dockerfile', dockerfile: 'Dockerfile' },
    workloads: [
      {
        name: 'web',
        primary: true,
        exposed: true,
        ports: [{ number: 1337 }],
        env: [
          { key: 'MEMCACHED', slot_ref: 'MEMCACHED' },
          { key: 'DB_HOST', slot_ref: 'DB', slot_part: 'host' },
          { key: 'DB_PASSWORD', slot_ref: 'DB', slot_part: 'password' },
        ],
      },
    ],
    slots: [
      { key: 'MEMCACHED', type: 'memcached', required: true, resolution: { mode: 'provisioned' } },
      { key: 'DB', type: 'postgres', required: true },
    ],
  },
  trial: {},
};

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

let root: Root;
let container: HTMLDivElement;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.stubGlobal(
    'matchMedia',
    (query: string) =>
      ({ matches: false, media: query, addEventListener: () => {}, removeEventListener: () => {} }) as unknown as MediaQueryList,
  );
  vi.stubGlobal('fetch', (input: string) =>
    Promise.resolve(
      input.endsWith('/security')
        ? new Response('{}', { status: 404, headers: { 'Content-Type': 'application/json' } })
        : json({ status: 'ready', answers: {}, commit: detection.commit, updated_at: new Date().toISOString(), detection }),
    ),
  );
  container = document.createElement('div');
  document.body.append(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/** The services' subtitle while Pando creates some of them and not others. */
const MIXED = 'Pando creates the provisioned ones at the first deploy.';

/** The text of the page between two pieces of text on it. */
function between(from: string, to: string): string {
  const text = container.textContent ?? '';
  const start = text.indexOf(from);
  return text.slice(start, text.indexOf(to, start));
}

/** How many values carry a Required tag. */
function required(): number {
  return container.querySelectorAll('button[aria-label="Required. Mark not required."]').length;
}

function button(label: string): HTMLButtonElement {
  const found = [...container.querySelectorAll('button')].find((b) => b.textContent === label);
  if (!found) throw new Error(`no button "${label}"`);
  return found;
}

describe('AppOnboarding, the services an app needs', () => {
  it('lists a service apart from its variable, and moves both when you use your own (R-131)', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    root = createRoot(container);
    await act(async () => {
      root.render(
        <QueryClientProvider client={client}>
          <AppOnboarding app={app} onBack={() => {}} onDeployRefused={() => {}} />
        </QueryClientProvider>,
      );
    });
    for (let i = 0; i < 600 && !container.textContent?.includes(MIXED); i++) {
      await act(() => vi.advanceTimersByTimeAsync(100));
    }

    // Each service by what it is, with where the app reads it from. Pando
    // creates the Memcached; the PostgreSQL is the app's own.
    let services = between(MIXED, 'Used on every deploy');
    expect(services).toContain('Memcached');
    expect(services).toContain('Via MEMCACHED');
    expect(services).toContain('Provisioned');
    expect(services).toContain('PostgreSQL');
    expect(services).toContain('Via DB_HOST and DB_PASSWORD');

    // The variable says what it holds, with no field and no choice of its own.
    let variables = between('Used on every deploy', 'The plan');
    expect(variables).toContain('Address of the Memcached Pando creates');
    expect(variables).toContain('Filled in by Pando at deploy');
    expect(variables).toContain('Host of your PostgreSQL');
    expect(variables).toContain('Password of your PostgreSQL');
    expect(container.querySelector('input[placeholder="Such as host:11211"]')).toBeNull();
    expect(container.querySelector('input[placeholder="Your PostgreSQL’s host"]')).not.toBeNull();
    // Required is asked of the PostgreSQL's parts, not of what Pando creates.
    expect(required()).toBe(2);

    // Use your own: the service, its variable and the plan all say so.
    await act(async () => button('Use your own Memcached').click());
    services = between('Set each one’s address under Variables.', 'Used on every deploy');
    expect(services).toContain('External');
    variables = between('Used on every deploy', 'The plan');
    expect(variables).toContain('Address of your Memcached');
    expect(container.querySelector('input[placeholder="Such as host:11211"]')).not.toBeNull();
    expect(between('Processes and services', 'Ask AI')).not.toContain('Provisioned');
    // Your own Memcached's address is now yours to give.
    expect(required()).toBe(3);

    // And back: Pando creates it again, and the field is gone.
    await act(async () => button('Let Pando create the Memcached').click());
    expect(between(MIXED, 'Used on every deploy')).toContain('Provisioned');
    expect(between('Used on every deploy', 'The plan')).toContain('Filled in by Pando at deploy');
    expect(container.querySelector('input[placeholder="Such as host:11211"]')).toBeNull();
  });
});
