// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { routeBody, WebhookRoutesCard } from './WebhookRoutes';
import { AppVerb } from './verbs';
import { mount, reply, unanswered, type Mounted } from '../ui/testing';

const app = { id: 'app_1' } as unknown as App;
const route = {
  name: 'noodle',
  method: 'POST',
  path: '/api/v1/webhook/noodle',
  verification: { kind: 'hmac_sha256', header: 'X-Hub-Signature-256' },
  secret_set: true,
  deployed: false,
};
const verbs = [AppVerb.SpecEdit, AppVerb.SecretsWrite];
const key = ['apps', 'app_1', 'webhook-routes'];

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('the route a draft writes', () => {
  it('sends a prefix and allow_repeats only for HMAC', () => {
    const draft = {
      name: ' noodle ',
      method: 'POST',
      path: ' /hook ',
      kind: 'header',
      header: ' X-Token ',
      prefix: 'sha256=',
      secret: '',
      allow_repeats: true,
    };
    expect(routeBody(draft)).toEqual({
      name: 'noodle',
      method: 'POST',
      path: '/hook',
      verification: { kind: 'header', header: 'X-Token', prefix: '' },
      allow_repeats: false,
      secret: '',
    });
    expect(routeBody({ ...draft, kind: 'hmac_sha256' }).allow_repeats).toBe(true);
  });
});

describe('webhook routes', () => {
  it('TestR432_ListsRoutesAndWhatIsNotDeployedYet', () => {
    vi.stubGlobal('fetch', unanswered);
    screen = mount(<WebhookRoutesCard app={app} />, [[key, { routes: [route] }]], verbs);
    expect(screen.text()).toContain('POST /api/v1/webhook/noodle');
    expect(screen.text()).toContain('Next deploy');
    expect(screen.text()).not.toContain('No secret');
  });

  it('says when there are none, and offers nothing without the verbs', () => {
    vi.stubGlobal('fetch', unanswered);
    screen = mount(<WebhookRoutesCard app={app} />, [[key, { routes: [] }]]);
    expect(screen.text()).toContain('No webhook routes');
    expect(screen.text()).not.toContain('Add route');
  });

  it('TestR427_ShowsAGeneratedSecretOnce', async () => {
    const posted: unknown[] = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && String(url).endsWith('/webhook-routes')) {
        posted.push(JSON.parse(String(init.body)));
        return reply({ revision: 4, secret: 'pdwr_shown_once' }, 201);
      }
      return reply({ routes: [route] });
    });
    screen = mount(<WebhookRoutesCard app={app} />, [[key, { routes: [] }]], verbs);
    await screen.press('Add route');
    await screen.fill('Name', 'noodle');
    await screen.fill('Path', '/hook');
    await screen.fill('Header', 'X-Sig');
    await screen.press('Add route');
    expect(posted).toHaveLength(1);
    expect(document.body.innerHTML).toContain('pdwr_shown_once');
  });

  it('replaces a secret and removes a route', async () => {
    const calls: string[] = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push(`${init?.method ?? 'GET'} ${String(url)}`);
      if (String(url).endsWith('/secret')) return reply({ secret: 'pdwr_new' }, 201);
      return reply({ routes: [route] });
    });
    screen = mount(<WebhookRoutesCard app={app} />, [[key, { routes: [route] }]], verbs);
    await screen.press('Replace secret');
    expect(screen.text()).toContain('The old secret stops working at once.');
    await screen.press('Replace secret');
    expect(document.body.innerHTML).toContain('pdwr_new');
    await screen.press('Remove');
    expect(calls.some((c) => c.startsWith('DELETE') && c.endsWith('/webhook-routes/noodle'))).toBe(true);
  });

  it('shows the server’s refusal (R-105)', async () => {
    vi.stubGlobal('fetch', async () =>
      reply({ code: 'VALID_INVALID', message: 'Webhook route "noodle" has path "x", which cannot be matched exactly.' }, 400),
    );
    screen = mount(<WebhookRoutesCard app={app} />, [[key, { routes: [] }]], verbs);
    await screen.press('Add route');
    await screen.fill('Name', 'noodle');
    await screen.fill('Path', 'x');
    await screen.fill('Header', 'X-Sig');
    await screen.press('Add route');
    expect(document.body.textContent).toContain('cannot be matched exactly');
  });
});
