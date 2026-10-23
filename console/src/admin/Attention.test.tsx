// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { Notice } from '@api/types.gen';
import { Attention, noticesKey } from './Attention';
import { PanelOpener } from './AppPanel';
import { AppVerb } from './verbs';
import { mount, reply, serverRender, unanswered, type Mounted } from '../ui/testing';

const notice = (key: string, message: string, ignored = false): Notice => ({ key, message, fingerprint: `fp_${key}`, ignored });

const notices: Notice[] = [
  notice('config.unshipped', 'The configuration has changed since this app was last deployed. Deploy to apply it.'),
  notice('variables.unset', 'API_KEY is declared with no value.'),
  notice('backup.not_taken', 'The last backup was not taken.', true),
  notice('security.below_threshold', 'This app’s security score is 58 and this host requires 70.'),
  notice('warning.WARN_NO_PERSISTENT_VOLUME', 'Nothing this app writes is kept.'),
];

const seed = (list: Notice[]): Array<[unknown[], unknown]> => [[noticesKey('app_1'), { notices: list }]];

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

// R-412: what the Overview asks of somebody, with each one ignorable.
describe('needs your attention', () => {
  it('lists the notices not ignored, and counts them', () => {
    const html = serverRender(<Attention appID="app_1" onGo={() => {}} onFindings={() => {}} />, seed(notices), [AppVerb.SpecEdit]);
    expect(html).toContain('Needs your attention');
    expect(html).toContain('API_KEY is declared with no value.');
    expect(html).not.toContain('The last backup was not taken.');
    expect(html).toContain('>4<');
  });

  it('is not drawn at all when everything is ignored', () => {
    const html = serverRender(
      <Attention appID="app_1" onGo={() => {}} onFindings={() => {}} />,
      seed(notices.map((n) => ({ ...n, ignored: true }))),
      [AppVerb.SpecEdit],
    );
    expect(html).toBe('');
  });

  it('ignores a notice on the server and drops its row', async () => {
    const calls: Array<[string, string]> = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push([init?.method ?? 'GET', String(url)]);
      return init?.method === 'PUT' ? reply({ ...notices[1], ignored: true }) : unanswered();
    });
    screen = mount(<Attention appID="app_1" onGo={() => {}} onFindings={() => {}} />, seed(notices.slice(1, 2)), [AppVerb.SpecEdit]);
    expect(screen.text()).toContain('API_KEY is declared with no value.');
    await screen.press('Ignore');
    expect(calls).toContainEqual(['PUT', '/api/v1/apps/app_1/notices/variables.unset/ignore']);
    expect(screen.text()).not.toContain('API_KEY');
    expect(screen.text()).not.toContain('Needs your attention');
  });

  it('offers the fix for each, and takes somebody to it', async () => {
    vi.stubGlobal('fetch', unanswered);
    const open = vi.fn();
    const go = vi.fn();
    const findings = vi.fn();
    screen = mount(
      <PanelOpener value={open}>
        <Attention appID="app_1" onGo={go} onFindings={findings} />
      </PanelOpener>,
      seed([...notices.slice(0, 2), notice('backup.not_taken', 'Not taken.'), ...notices.slice(3)]),
      [AppVerb.SpecEdit, AppVerb.LogsRead],
    );
    await screen.press('Review changes');
    expect(open).toHaveBeenCalledWith({ view: 'deployments', deployment: 'unshipped' });
    await screen.press('Fill them in');
    expect(go).toHaveBeenCalledWith('settings', 'variables');
    await screen.press('Open storage');
    expect(go).toHaveBeenCalledWith('settings', 'storage');
    await screen.press('Add storage');
    expect(go).toHaveBeenCalledWith('settings', 'storage');
    await screen.press('See findings');
    expect(findings).toHaveBeenCalled();
  });

  it('offers no fix somebody could not make', () => {
    const html = serverRender(
      <PanelOpener value={() => {}}>
        <Attention appID="app_1" onGo={() => {}} onFindings={() => {}} />
      </PanelOpener>,
      seed(notices),
      [AppVerb.View],
    );
    expect(html).not.toContain('Fill them in');
    expect(html).not.toContain('Review changes');
    expect(html).not.toContain('Add storage');
    // Reading the findings changes nothing.
    expect(html).toContain('See findings');
    expect(html).toContain('Ignore');
  });
});
