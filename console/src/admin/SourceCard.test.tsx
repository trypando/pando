// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { PanelOpener } from './AppPanel';
import { START_FIRST_WARNING, hasHealthCheck } from './DeploySettings';
import { SourceCard, short, tagOf } from './SourceCard';
import { AppVerb } from './verbs';
import { mount, reply, serverRender, unanswered, type Mounted, type Seed } from '../ui/testing';

const git = {
  id: 'app_1',
  name: 'crewmate',
  state: 'running',
  pinned_spec_id: 'spec_1',
  source: { type: 'git', url: 'github.com/acme/crewmate', ref: 'main', commit: 'a3f9c21aaaaaaa' },
} as unknown as App;
const image = { ...git, source: { type: 'image', image: 'ghcr.io/acme/crewmate:1.4', digest: 'sha256:9f2c07e1ffff' } } as unknown as App;
const upload = { ...git, source: { type: 'upload', upload_id: 'upl_1' } } as unknown as App;

const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();

/** The newest revision, the deploys, and what auto-deploy last found. */
function seed(app: App, deploy: object = {}, extra: object = {}): Seed {
  return [
    [['apps', app.id, 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
    [['apps', app.id, 'spec', 1], { id: 'spec_1', revision: 1, body: { source: app.source, deploy: { strategy: 'recreate', auto_deploy: { enabled: false }, ...deploy }, ...extra } }],
    [['apps', app.id, 'deployments'], { deployments: [{ id: 'dep_1', status: 'succeeded', started_at: ago(130), finished_at: ago(120) }] }],
    [['apps', app.id, 'status'], { state: 'running', workloads: [] }],
    [['apps', app.id, 'auto-deploy'], { settings: { enabled: false }, deployed: { enabled: false }, pending: false, paused: false, webhook_secret_set: false, webhook_url: '' }],
  ];
}

const everything = [AppVerb.SpecEdit, AppVerb.Deploy, AppVerb.LogsRead];
const card = (app: App) => <SourceCard app={app} onRefused={() => {}} onWaiting={() => {}} />;

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('the source card', () => {
  it('reads a repository, its branch and what is running from it', () => {
    const html = serverRender(card(git), seed(git), everything);
    expect(html).toContain('github.com/acme/crewmate');
    expect(html).toContain('>main<');
    expect(html).toContain('>a3f9c21<');
    expect(html).toContain('deployed 2 hours ago');
    expect(html).toContain('Deploy automatically');
    expect(html).toContain('When main changes');
    expect(html).toContain('Change source');
  });

  it('reads an image and the build it runs, without automatic deploys', () => {
    const html = serverRender(card(image), seed(image), everything);
    expect(html).toContain('ghcr.io/acme/crewmate:1.4');
    expect(html).toContain('sha256:9f2c07e1');
    expect(html).toContain('Check for update');
    expect(html).toContain('Upgrades included');
    expect(html).not.toContain('Deploy automatically');
  });

  it('reads uploaded files, and takes new ones', () => {
    const html = serverRender(card(upload), seed(upload), everything);
    expect(html).toContain('Uploaded files');
    expect(html).toContain('Drop the app’s folder here to send a new version.');
    expect(html).toContain('Choose files');
    expect(html).not.toContain('Deploy automatically');
  });

  it('offers no change to somebody who may only look', () => {
    const html = serverRender(card(upload), seed(upload), [AppVerb.View]);
    expect(html).not.toContain('Change source');
    expect(html).not.toContain('Choose files');
    expect(html).not.toContain('Deployment log');
  });

  it('opens the deploy log and the history in the side panel', async () => {
    vi.stubGlobal('fetch', unanswered);
    const open = vi.fn();
    screen = mount(<PanelOpener value={open}>{card(git)}</PanelOpener>, seed(git), everything);
    await screen.press('Deployment log');
    expect(open).toHaveBeenCalledWith({ view: 'deployments', deployment: 'dep_1' });
    await screen.press('History');
    expect(open).toHaveBeenCalledWith({ view: 'deployments' });
  });

  it('shortens commits and digests, and finds an image’s tag', () => {
    expect(short('a3f9c21aaaa')).toBe('a3f9c21');
    expect(short('sha256:41ab9d02cafe')).toBe('sha256:41ab9d02');
    expect(short(undefined)).toBe('');
    expect(tagOf('ghcr.io/acme/web:1.4')).toBe('1.4');
    expect(tagOf('localhost:5000/web')).toBe('latest');
    expect(tagOf('web:2@sha256:abc')).toBe('2');
  });
});

describe('a newer commit', () => {
  const found = (commit: string): Seed => [
    ...seed(git).slice(0, 4),
    [
      ['apps', 'app_1', 'auto-deploy'],
      { settings: { enabled: true }, deployed: { enabled: true }, paused: false, last_check: { app_id: 'app_1', checked_at: ago(3), found_ref: 'refs/heads/main', found_commit: commit }, webhook_url: '' },
    ],
  ];

  it('is offered only when it is not what is running', () => {
    expect(serverRender(card(git), found('a3f9c21aaaaaaa'), everything)).not.toContain('has a newer commit');
    const html = serverRender(card(git), found('e81b4d0bbbbbbb'), everything);
    expect(html).toContain('has a newer commit');
    expect(html).toContain('Deploy e81b4d0');
    expect(html).toContain('Checked 3 min ago');
    expect(serverRender(card(git), found('e81b4d0bbbbbbb'), [AppVerb.SpecEdit])).not.toContain('Deploy e81b4d0');
  });

  it('is deployed by writing a revision naming it, then deploying that', async () => {
    const calls: Array<[string, string, unknown]> = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push([init?.method ?? 'GET', String(url), init?.body ? JSON.parse(String(init.body)) : undefined]);
      if (init?.method === 'POST' && String(url).endsWith('/specs')) return reply({ revision: 2 }, 201);
      if (init?.method === 'POST' && String(url).endsWith('/deployments')) return reply({ status: 'awaiting_approval' }, 202);
      return unanswered();
    });
    const waiting = vi.fn();
    screen = mount(<SourceCard app={git} onRefused={() => {}} onWaiting={waiting} />, found('e81b4d0bbbbbbb'), everything);
    await screen.press('Deploy e81b4d0');
    const posts = calls.filter(([m]) => m === 'POST');
    expect((posts[0]?.[2] as { source: { commit: string } }).source.commit).toBe('e81b4d0bbbbbbb');
    expect(posts[1]).toEqual(['POST', '/api/v1/apps/app_1/deployments', { spec_revision: 2 }]);
    expect(waiting).toHaveBeenCalled();
  });

  it('says why a deploy was refused', async () => {
    vi.stubGlobal('fetch', async () => reply({ code: 'PLAN_SLOT_UNFILLED', message: 'Fill DATABASE_URL first.' }, 422));
    const refused = vi.fn();
    screen = mount(<SourceCard app={git} onRefused={refused} onWaiting={() => {}} />, found('e81b4d0bbbbbbb'), everything);
    await screen.press('Deploy e81b4d0');
    expect(refused).toHaveBeenCalledWith('Fill DATABASE_URL first.', undefined, 'PLAN_SLOT_UNFILLED');
  });
});

describe('a newer build of the tag (R-143a)', () => {
  it('is looked for when asked, then offered', async () => {
    const upgraded: unknown[] = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      if (init?.method === 'POST') upgraded.push(JSON.parse(String(init.body)));
      if (String(url).endsWith('/image-update') && (init?.method ?? 'GET') === 'GET') {
        return reply({ image: 'ghcr.io/acme/crewmate:1.4', current_digest: 'sha256:9f2c07e1', latest_digest: 'sha256:41ab9d02eeee', update_available: true, revision: 1 });
      }
      if (init?.method === 'POST') return reply({ status: 'pending' }, 202);
      return unanswered();
    });
    screen = mount(card(image), seed(image), everything);
    await screen.press('Check for update');
    expect(screen.text()).toContain('Tag 1.4 now names a newer build, sha256:41ab9d02.');
    await screen.press('Upgrade');
    // The build the check found, and no other.
    expect(upgraded).toEqual([{ digest: 'sha256:41ab9d02eeee' }]);
  });

  it('says so when there is nothing newer', async () => {
    vi.stubGlobal('fetch', async () => reply({ image: 'x:1', current_digest: 'a', latest_digest: 'a', update_available: false, revision: 1 }));
    screen = mount(card(image), seed(image), everything);
    await screen.press('Check for update');
    expect(screen.text()).toContain('Up to date.');
  });
});

describe('the deploy switches', () => {
  it('asks before starting new versions first, in R-145’s words', async () => {
    const posted: unknown[] = [];
    vi.stubGlobal('fetch', async (_: string, init?: RequestInit) => {
      if (init?.method === 'POST') {
        posted.push(JSON.parse(String(init.body)));
        return reply({ revision: 2 }, 201);
      }
      return unanswered();
    });
    screen = mount(card(git), seed(git), everything);
    await screen.click('Start new version first');
    expect(screen.text()).toContain(START_FIRST_WARNING);
    expect(posted).toHaveLength(0);
    await screen.press('Cancel');
    expect(posted).toHaveLength(0);
    await screen.click('Start new version first');
    await screen.press('Turn on');
    expect((posted[0] as { deploy: { strategy: string } }).deploy.strategy).toBe('start_then_swap');
  });

  it('turns approval on and automatic deploys off in one revision (R-158)', async () => {
    const posted: unknown[] = [];
    vi.stubGlobal('fetch', async (_: string, init?: RequestInit) => {
      if (init?.method === 'POST') posted.push(JSON.parse(String(init.body)));
      return init?.method && init.method !== 'GET' ? reply({ revision: 2 }, 201) : unanswered();
    });
    screen = mount(card(git), seed(git, { auto_deploy: { enabled: true } }), everything);
    await screen.click('Require approval');
    expect((posted[0] as { deploy: unknown }).deploy).toMatchObject({ require_approval: true, auto_deploy: { enabled: false } });
  });

  it('turns automatic deploys on through the auto-deploy endpoint', async () => {
    const put: Array<[string, unknown]> = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') put.push([String(url), JSON.parse(String(init.body))]);
      return init?.method && init.method !== 'GET' ? reply({ changed: true }) : unanswered();
    });
    screen = mount(card(git), seed(git), everything);
    await screen.click('Deploy automatically');
    expect(put[0]).toEqual(['/api/v1/apps/app_1/auto-deploy', { enabled: true }]);
  });

  it('holds automatic deploys while approval is required', () => {
    const html = serverRender(card(git), seed(git, { require_approval: true }), everything);
    expect(html).toContain('Not while deploys need approval');
  });

  it('needs a health check to roll back (R-147)', () => {
    expect(serverRender(card(git), seed(git), everything)).toContain('Needs a health check');
    const checked = serverRender(card(git), seed(git, {}, { health: { source: 'http', path: '/healthz' } }), everything);
    expect(checked).toContain('When the health check fails');
    expect(hasHealthCheck({ workloads: [{ name: 'web', healthcheck: { command: ['true'] } }] } as never)).toBe(true);
    expect(hasHealthCheck({ health: { source: 'http' } } as never)).toBe(false);
  });

  it('are shown, and cannot be changed, without the spec edit', () => {
    const html = serverRender(card(git), seed(git), [AppVerb.View]);
    expect(html).toContain('Require approval');
    expect(html.match(/role="switch"[^>]*disabled/g)?.length ?? html.match(/disabled=""[^>]*role="switch"/g)?.length).toBe(4);
  });
});

describe('changing the source', () => {
  it('writes a revision with the new repository and no commit', async () => {
    const posted: unknown[] = [];
    vi.stubGlobal('fetch', async (_: string, init?: RequestInit) => {
      if (init?.method === 'POST') posted.push(JSON.parse(String(init.body)));
      return init?.method && init.method !== 'GET' ? reply({ revision: 2 }, 201) : unanswered();
    });
    screen = mount(card(git), seed(git), everything);
    await screen.press('Change source');
    await screen.fill('Repository', 'github.com/acme/other');
    await screen.fill('Branch', 'trunk');
    await screen.press('Save');
    expect((posted[0] as { source: unknown }).source).toEqual({ type: 'git', url: 'github.com/acme/other', ref: 'trunk', commit: '' });
  });

  it('writes a revision with the new image and no digest', async () => {
    const posted: unknown[] = [];
    vi.stubGlobal('fetch', async (_: string, init?: RequestInit) => {
      if (init?.method === 'POST') posted.push(JSON.parse(String(init.body)));
      return init?.method && init.method !== 'GET' ? reply({ revision: 2 }, 201) : unanswered();
    });
    screen = mount(card(image), seed(image), everything);
    await screen.press('Change source');
    await screen.fill('Image', 'ghcr.io/acme/crewmate:1.5');
    await screen.press('Save');
    expect((posted[0] as { source: unknown }).source).toEqual({ type: 'image', image: 'ghcr.io/acme/crewmate:1.5', digest: '' });
  });
});
