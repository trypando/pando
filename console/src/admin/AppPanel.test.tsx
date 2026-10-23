// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';

import type { App } from '@api/types.gen';
import { AppPanel, PanelOpener, panelViews, useOpenPanel, type PanelTarget } from './AppPanel';
import { AdminConsole } from './AdminConsole';
import { EXEC_WARNING } from './Terminal';
import { AppVerb } from './verbs';
import { InstallVerb } from '../app/principal';
import { mount, serverRender, unanswered } from '../ui/testing';

const app = { id: 'app_1', name: 'crewmate', state: 'running', pinned_spec_id: 'spec_1' } as App;

// What every view asks for, so the panel renders without the network.
const seed: Array<[unknown[], unknown]> = [
  [['apps', 'app_1', 'status'], { state: 'running', desired_state: 'running', workloads: [] }],
  [['apps', 'app_1', 'deployments'], { deployments: [] }],
  [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
  [['apps', 'app_1', 'spec', 1], { body: { workloads: [{ name: 'web', primary: true }, { name: 'worker' }] } }],
];

/** The panel's tab labels, in order. */
const tabs = () => [...document.querySelectorAll('[role="tab"]')].map((t) => t.textContent);

function panel(target: PanelTarget, verbs: string[], on: Partial<{ onChange: () => void; onClose: () => void }> = {}) {
  return mount(
    <AppPanel
      app={app}
      verbs={verbs}
      target={target}
      onChange={on.onChange ?? (() => {})}
      onClose={on.onClose ?? (() => {})}
      hrefOf={(t) => `/admin/apps/app_1?panel=${t.view}`}
    />,
    seed,
    verbs,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('which views the panel offers', () => {
  it('offers output and deployments on app.logs.read, and a terminal on app.exec (R-084)', () => {
    expect(panelViews([AppVerb.LogsRead])).toEqual(['output', 'deployments']);
    expect(panelViews([AppVerb.Exec])).toEqual(['terminal']);
    expect(panelViews([AppVerb.LogsRead, AppVerb.Exec])).toEqual(['output', 'deployments', 'terminal']);
    expect(panelViews([AppVerb.View])).toEqual([]);
    expect(panelViews(undefined)).toEqual([]);
  });

  it('hides the terminal from somebody who may only read logs', () => {
    vi.stubGlobal('fetch', unanswered);
    vi.stubGlobal('EventSource', class { addEventListener() {} close() {} });
    const screen = panel({ view: 'output' }, [AppVerb.LogsRead]);
    expect(tabs()).toEqual(['Output', 'Deployments']);
    screen.unmount();
  });

  it('lands a link to a view somebody cannot use on one they can', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = panel({ view: 'output' }, [AppVerb.Exec]);
    expect(tabs()).toEqual(['Terminal']);
    expect(screen.text()).toContain(EXEC_WARNING);
    screen.unmount();
  });

  it('draws nothing for somebody who may use none of them', () => {
    const html = serverRender(
      <AppPanel app={app} verbs={[AppVerb.View]} target={{ view: 'output' }} onChange={() => {}} onClose={() => {}} />,
      seed,
    );
    expect(html).toBe('');
  });
});

describe('the panel itself', () => {
  it('closes from Close and from Escape, but not from Escape in the terminal', async () => {
    vi.stubGlobal('fetch', unanswered);
    const onClose = vi.fn();
    const screen = panel({ view: 'terminal' }, [AppVerb.Exec], { onClose });

    await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="Close"]')!.click());
    expect(onClose).toHaveBeenCalledTimes(1);

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    expect(onClose).toHaveBeenCalledTimes(2);

    // A key the shell is waiting for, typed into xterm's own element.
    const xterm = document.createElement('div');
    xterm.className = 'xterm';
    document.body.append(xterm);
    xterm.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(onClose).toHaveBeenCalledTimes(2);
    screen.unmount();
  });

  it('goes full screen and back', async () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = panel({ view: 'terminal' }, [AppVerb.Exec]);
    const sheet = () => document.querySelector<HTMLElement>('[role="dialog"]')!;
    expect(sheet().style.width).toBe('75%');
    await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="Full screen"]')!.click());
    expect(sheet().style.width).toBe('100%');
    expect(document.querySelector('button[aria-label="Exit full screen"]')).not.toBeNull();
    screen.unmount();
  });

  it('switches views through the address, keeping the workload', async () => {
    vi.stubGlobal('fetch', unanswered);
    vi.stubGlobal('EventSource', class { addEventListener() {} close() {} });
    const onChange = vi.fn();
    const screen = panel({ view: 'output', workload: 'worker' }, [AppVerb.LogsRead, AppVerb.Exec], { onChange });
    expect(document.querySelector('a[role="tab"]')?.getAttribute('href')).toBe('/admin/apps/app_1?panel=output');
    await screen.click('Terminal');
    expect(onChange).toHaveBeenCalledWith({ view: 'terminal', workload: 'worker' });
    screen.unmount();
  });
});

describe('opening the panel from the app screen', () => {
  function Probe({ view }: { view?: 'output' | 'terminal' }) {
    const open = useOpenPanel(view);
    return <span>{open ? 'can open' : 'cannot open'}</span>;
  }

  it('is offered only inside an app screen, and only for a view the person may use', () => {
    expect(serverRender(<Probe />, [], [AppVerb.LogsRead])).toContain('cannot open');
    const inside = (view: 'output' | 'terminal', verbs: string[]) =>
      serverRender(
        <PanelOpener value={() => {}}>
          <Probe view={view} />
        </PanelOpener>,
        [],
        verbs,
      );
    expect(inside('output', [AppVerb.LogsRead])).toContain('can open');
    expect(inside('terminal', [AppVerb.LogsRead])).toContain('cannot open');
    expect(inside('terminal', [AppVerb.Exec])).toContain('can open');
  });
});

describe('the panel in the address', () => {
  const me: [unknown[], unknown] = [['me'], { verbs: [InstallVerb.View] }];
  const render = (verbs: string[], panel?: 'output' | 'deployments' | 'terminal', tab = 'overview') =>
    serverRender(
      <AdminConsole
        route={{ view: 'admin', section: 'apps', appID: 'app_1', tab, panel }}
        go={() => {}}
        onLeave={() => {}}
        onSettings={() => {}}
        administrative
      />,
      [me, [['apps', 'app_1'], { ...app, verbs }], ...seed],
    );

  it('opens on the view the address names', () => {
    const html = render([AppVerb.View, AppVerb.LogsRead], 'deployments');
    expect(html).toContain('role="dialog"');
    expect(html).toContain('aria-label="Deployments"');
  });

  it('is closed when the address names none', () => {
    expect(render([AppVerb.View, AppVerb.LogsRead])).not.toContain('role="dialog"');
  });

  it('opens a terminal with R-086’s warning and nothing about recording', () => {
    const html = render([AppVerb.View, AppVerb.Exec], 'terminal');
    expect(html).toContain('A terminal has everything the app has, including its database and its secrets.');
    expect(html).toContain('Open terminal');
    expect(html).not.toContain('records the session');
  });

  it('opens Settings, which holds what Configuration did', () => {
    const html = render([AppVerb.View], undefined, 'settings');
    expect(html).toMatch(/aria-selected="true"[^>]*>Settings</);
    expect(html).not.toContain('>Configuration<');
  });

  it('stays closed for somebody who may not use what it names', () => {
    expect(render([AppVerb.View], 'terminal')).not.toContain('role="dialog"');
  });
});
