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
