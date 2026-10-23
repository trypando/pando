// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { AppOverview } from './AppOverview';
import { AppVerb } from './verbs';
import { PanelOpener } from './AppPanel';
import { mount, serverRender, unanswered } from '../ui/testing';

const app = { id: 'app_01FAILED', name: 'Skyjo Game', state: 'failed' } as unknown as App;

// No deploys, so the only "Open logs" on the screen is the banner's.
const seed: Array<[unknown[], unknown]> = [[['apps', app.id, 'deployments'], { deployments: [] }]];

afterEach(() => vi.unstubAllGlobals());

describe('the overview of a failed app', () => {
  it('opens the logs from the failure, for somebody who may read them', async () => {
    // The sections that ask for more than the seed have nothing to wait on.
    vi.stubGlobal('fetch', unanswered);
    const open = vi.fn();
    const screen = mount(
      <PanelOpener value={open}>
        <AppOverview app={app} onGo={() => {}} />
      </PanelOpener>,
      seed,
      [AppVerb.LogsRead],
    );
    expect(screen.text()).toContain('Pando has stopped trying to start this app.');
    await screen.press('Open logs');
    expect(open).toHaveBeenCalledWith({ view: 'output' });
    screen.unmount();
  });

  it('offers no logs to somebody who may not read them', () => {
    const html = serverRender(
      <PanelOpener value={() => {}}>
        <AppOverview app={app} onGo={() => {}} />
      </PanelOpener>,
      seed,
      [AppVerb.View],
    );
    expect(html).toContain('Pando has stopped trying to start this app.');
    expect(html).not.toContain('Open logs');
  });
});

describe('the overview of a running app', () => {
  it('has the sections in order: attention, source, workloads, security', () => {
    const running = { ...app, state: 'running', pinned_spec_id: 'spec_1', source: { type: 'git', url: 'github.com/acme/skyjo' } } as unknown as App;
    const html = serverRender(<AppOverview app={running} onGo={() => {}} />, [
      ...seed,
      [['apps', app.id, 'notices'], { notices: [{ key: 'config.unshipped', message: 'The configuration has changed.', fingerprint: 'f', ignored: false }] }],
      [['apps', app.id, 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
      [['apps', app.id, 'spec', 1], { id: 'spec_1', revision: 1, body: { source: running.source, deploy: {} } }],
    ]);
    const at = (s: string) => html.indexOf(s);
    expect(at('Needs your attention')).toBeGreaterThan(-1);
    expect(at('Needs your attention')).toBeLessThan(at('Source and deploys'));
    expect(at('Source and deploys')).toBeLessThan(at('Workloads'));
    expect(at('Workloads')).toBeLessThan(at('Security'));
    expect(html).not.toContain('Pando has stopped trying');
  });

  it('shows a deploy waiting for approval, which is state rather than a to-do', () => {
    const html = serverRender(<AppOverview app={{ ...app, state: 'running' } as App} onGo={() => {}} />, [
      [['apps', app.id, 'deployments'], { deployments: [{ id: 'dep_1', status: 'awaiting_approval', started_at: '2026-10-23T10:00:00Z' }] }],
    ]);
    expect(html).toContain('A deploy is waiting for approval');
  });
});
