// @vitest-environment jsdom
// The app's header: its tile, its name, its score and its address.
import { act } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { AppAddress } from './AppAddress';
import { AppImage } from './AppImage';
import { AppName } from './AppName';
import { AppOverview, SECURITY_ID } from './AppOverview';
import { HeaderScore } from './HeaderScore';
import { AppVerb } from './verbs';
import { mount, reply, serverRender, unanswered, type Mounted } from '../ui/testing';

const app = { id: 'app_1', name: 'crewmate', slug: 'crewmate', state: 'running', pinned_spec_id: 'spec_1', address: 'https://crewmate.example.com/' } as unknown as App;

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('the launcher image', () => {
  it('opens to upload one, and says what it takes', async () => {
    vi.stubGlobal('fetch', unanswered);
    screen = mount(<AppImage app={app} />, [], [AppVerb.SpecEdit]);
    expect(screen.text()).not.toContain('up to 256 KB');
    await screen.press('');
    expect(screen.text()).toContain('Launcher image');
    expect(screen.text()).toContain('PNG, JPEG, WebP, GIF or SVG, up to 256 KB.');
    expect(screen.text()).toContain('Upload');
    expect(screen.text()).not.toContain('Remove');
  });

  it('offers to remove one that is set, and says why one was refused', async () => {
    vi.stubGlobal('fetch', async () => reply({ code: 'VALID_INVALID', message: 'This image is larger than 256 KB.' }, 400));
    screen = mount(<AppImage app={{ ...app, icon_updated_at: '2026-10-01T00:00:00Z' }} />, [], [AppVerb.SpecEdit]);
    await screen.press('');
    expect(screen.text()).toContain('Replace');
    await screen.press('Remove');
    expect(screen.text()).toContain('This image is larger than 256 KB.');
  });
});

describe('the name', () => {
  it('is a heading with Rename beside it, for somebody who can rename', async () => {
    const patched: unknown[] = [];
    vi.stubGlobal('fetch', async (_: string, init?: RequestInit) => {
      if (init?.method === 'PATCH') patched.push(JSON.parse(String(init.body)));
      return reply({ ...app, name: 'crew' });
    });
    screen = mount(<AppName app={app} />, [], [AppVerb.SpecEdit]);
    await screen.press('Rename');
    const input = document.querySelector<HTMLInputElement>('input[aria-label="App name"]')!;
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, 'crew');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await screen.press('Save');
    expect(patched).toEqual([{ name: 'crew' }]);
  });

  it('is a heading alone otherwise', () => {
    const html = serverRender(<AppName app={app} />);
    expect(html).toContain('crewmate</h3>');
    expect(html).not.toContain('Rename');
  });
});

describe('the address', () => {
  it('copies the address', async () => {
    const write = vi.fn(async () => undefined);
    vi.stubGlobal('navigator', { clipboard: { writeText: write } });
    vi.stubGlobal('fetch', unanswered);
    screen = mount(<AppAddress app={app} />);
    expect(screen.text()).toContain('crewmate.example.com');
    const copy = document.querySelector<HTMLButtonElement>('button[aria-label="Copy address"]')!;
    copy.click();
    await Promise.resolve();
    expect(write).toHaveBeenCalledWith('https://crewmate.example.com/');
  });
});

describe('the score in the header', () => {
  const report = (standing: object, scanner = 'trivy') => [[['apps', 'app_1', 'security'], { standing, scanner, counts: {}, ignoring_unfixable: false }]] as Array<[unknown[], unknown]>;

  it('shows the shield and the number, never out of a hundred', () => {
    const html = serverRender(<HeaderScore appID="app_1" onOpen={() => {}} />, report({ verdict: 'insecure', score: 58, threshold: 70 }));
    expect(html).toContain('>58<');
    expect(html).not.toContain('/ 100');
    expect(html).toContain('Security score 58, below this installation&#x27;s requirement of 70');
  });

  it('is absent with no score and no scanner', () => {
    expect(serverRender(<HeaderScore appID="app_1" onOpen={() => {}} />, report({ verdict: 'unscanned', threshold: 0 }, ''))).toBe('');
  });

  it('takes somebody to the Security section', async () => {
    const opened = vi.fn();
    vi.stubGlobal('fetch', unanswered);
    screen = mount(<HeaderScore appID="app_1" onOpen={opened} />, report({ verdict: 'ok', score: 91, threshold: 0 }));
    await screen.press('91');
    expect(opened).toHaveBeenCalled();
  });

  it('lands on the Security section when the overview is opened at it', () => {
    vi.stubGlobal('fetch', unanswered);
    const scrolled = vi.fn();
    Element.prototype.scrollIntoView = scrolled;
    screen = mount(<AppOverview app={app} onGo={() => {}} focus="security" />);
    expect(document.getElementById(SECURITY_ID)).not.toBeNull();
    expect(scrolled).toHaveBeenCalled();
  });
});
