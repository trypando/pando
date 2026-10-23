// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';

import { Sharing } from './Sharing';
import { AppVerb } from './verbs';
import { mount, reply, unanswered, type Seed } from '../ui/testing';

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

const MANAGE = [AppVerb.View, AppVerb.GrantsManage];

const roles = [
  { id: 'role_viewer', name: 'viewer', builtin: true },
  { id: 'role_operator', name: 'operator', builtin: true },
  { id: 'role_owner', name: 'owner', builtin: true },
];

const named = [
  { id: 'gr_1', plane: 'control', principal_kind: 'user', principal_id: 'usr_me', principal_name: 'Ben', role_id: 'role_owner', role_name: 'owner' },
  { id: 'gr_2', plane: 'data', principal_kind: 'user', principal_id: 'usr_me', principal_name: 'Ben' },
  { id: 'gr_3', plane: 'data', principal_kind: 'group', principal_id: 'grp_1', principal_name: 'Design' },
  { id: 'gr_4', plane: 'control', principal_kind: 'user', principal_id: 'usr_sam', principal_name: 'Sam Ortiz', role_id: 'role_operator', role_name: 'operator' },
  { id: 'gr_5', plane: 'data', principal_kind: 'user', principal_id: 'usr_sam', principal_name: 'Sam Ortiz' },
  { id: 'gr_6', plane: 'control', principal_kind: 'user', principal_id: 'usr_jo', principal_name: 'jordan', role_id: 'role_viewer', role_name: 'viewer' },
  { id: 'gr_7', plane: 'control', principal_kind: 'token', principal_id: 'tok_1', principal_name: 'ci-deployer', role_id: 'role_operator', role_name: 'operator' },
];

const anonymous = (passcode = false) => ({ id: 'gr_anon', plane: 'data', principal_kind: 'anonymous', passcode });

function seed(grants: unknown[], policy = 'allowed', verbs: string[] = []): Seed {
  return [
    [['apps', 'app_1', 'grants', '', {}], { pages: [{ grants, public_sharing: policy }], pageParams: [''] }],
    [['roles', 'app'], { roles }],
    [['me'], { principal_kind: 'user', id: 'usr_me', user_id: 'usr_me', verbs }],
  ];
}

const sw = (label: string) => document.querySelector<HTMLInputElement>(`input[role="switch"][aria-label="${label}"]`);
const tick = () => act(async () => new Promise((r) => setTimeout(r, 20)));

type Call = { url: string; method: string; body?: string };
function recording(): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    calls.push({ url: String(url), method: init?.method ?? 'GET', body: init?.body as string | undefined });
    return init?.method && init.method !== 'GET' ? reply({}) : unanswered();
  });
  return calls;
}

// R-075, R-264: public is a switch that adds to the people named.
describe('public access', () => {
  it('offers Make public on a private app, and no passcode yet', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    expect(screen.text()).toContain('Accessible to anyone on the internet');
    expect(sw('Make public')?.checked).toBe(false);
    expect(sw('Make public')?.disabled).toBe(false);
    expect(sw('Require passcode')).toBeNull();
    screen.unmount();
  });

  it('asks before making it public, and keeps the people it is shared with', async () => {
    const calls = recording();
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    await act(async () => sw('Make public')!.click());
    expect(screen.text()).toContain('Make this app public?');
    expect(calls.filter((c) => c.method !== 'GET')).toHaveLength(0);
    await screen.press('Make public');
    const posts = calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(1);
    expect(JSON.parse(posts[0]!.body!)).toEqual({ plane: 'data', principal_kind: 'anonymous' });
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    screen.unmount();
  });

  it('shows Require passcode while public, and dots once one is set', () => {
    vi.stubGlobal('fetch', unanswered);
    const plain = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(), ...named]), MANAGE);
    expect(sw('Make public')?.checked).toBe(true);
    expect(sw('Require passcode')?.checked).toBe(false);
    expect(plain.text()).toContain('Visitors need the passcode to open it');
    expect(plain.text()).not.toContain('••••••••');
    plain.unmount();

    const locked = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(true), ...named]), MANAGE);
    expect(sw('Require passcode')?.checked).toBe(true);
    expect(locked.text()).toContain('••••••••');
    expect(locked.text()).toContain('Change');
    locked.unmount();
  });

  it('turns public off without asking', async () => {
    const calls = recording();
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(), ...named]), MANAGE);
    await act(async () => sw('Make public')!.click());
    expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/apps/app_1/grants/gr_anon'))).toBe(true);
    screen.unmount();
  });

  it('shows a switch the host forbids, disabled, with why', () => {
    vi.stubGlobal('fetch', unanswered);
    const none = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named, 'none'), MANAGE);
    expect(sw('Make public')?.disabled).toBe(true);
    expect(none.text()).toContain('Not allowed on this host');
    none.unmount();

    const required = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(true), ...named], 'passcode_only'), MANAGE);
    expect(sw('Require passcode')?.disabled).toBe(true);
    expect(required.text()).toContain('Required on this host');
    required.unmount();
  });

  it('asks for the passcode with the choice when the host requires one', async () => {
    const calls = recording();
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named, 'passcode_only'), MANAGE);
    await act(async () => sw('Make public')!.click());
    expect(screen.text()).toContain('Make this app public with a passcode?');
    await screen.fill('Passcode', 'harbor-light');
    await screen.press('Make public');
    const post = calls.find((c) => c.method === 'POST');
    expect(JSON.parse(post!.body!)).toEqual({ plane: 'data', principal_kind: 'anonymous', passcode: 'harbor-light' });
    screen.unmount();
  });
});

// R-075a: Pando keeps the passcode's digest, so it is readable only while typed.
describe('the passcode', () => {
  it('is shown while it is typed, can be hidden and copied, and is gone afterwards', async () => {
    const calls = recording();
    const writeText = vi.fn(async () => {});
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(), ...named]), MANAGE);
    await act(async () => sw('Require passcode')!.click());
    const field = () => document.getElementById('f-passcode') as HTMLInputElement;
    expect(field().type).toBe('text');
    await screen.fill('Passcode', 'harbor-light');
    expect(field().value).toBe('harbor-light');

    await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="Hide passcode"]')!.click());
    expect(field().type).toBe('password');
    await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="Show passcode"]')!.click());
    expect(field().type).toBe('text');

    await screen.press('Copy');
    expect(writeText).toHaveBeenCalledWith('harbor-light');
    expect(screen.text()).toContain('Copied');

    await screen.press('Require passcode');
    const patch = calls.find((c) => c.method === 'PATCH');
    expect(JSON.parse(patch!.body!)).toEqual({ passcode: 'harbor-light' });
    await tick();
    expect(document.getElementById('f-passcode')).toBeNull();
    expect(document.body.innerHTML).not.toContain('harbor-light');
    screen.unmount();
  });

  it('is changed and taken away through a dialog that asks first', async () => {
    const calls = recording();
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(true), ...named]), MANAGE);
    await screen.press('Change');
    expect(screen.text()).toContain('Change the passcode');
    await screen.press('Cancel');
    await act(async () => sw('Require passcode')!.click());
    expect(screen.text()).toContain('Stop requiring a passcode?');
    await screen.press('Stop requiring it');
    expect(JSON.parse(calls.find((c) => c.method === 'PATCH')!.body!)).toEqual({ passcode: '' });
    screen.unmount();
  });
});

// R-070, R-071: opening the app and managing it are two planes, two columns.
describe('people and groups', () => {
  it('shows each principal once, with an initial for a person and what a group or token is', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    const avatars = [...document.querySelectorAll('[data-avatar]')].map((a) => a.textContent);
    expect(avatars).toEqual(['B', 'S', 'J']);
    const text = screen.text();
    expect(text).toContain('Group');
    expect(text).toContain('Service token');
    expect(text).toContain('You');
    // Each of Sam's two grants is one row.
    expect(text.split('Sam Ortiz').length - 1).toBe(1);
    screen.unmount();
  });

  it('puts opening it and managing it in separate columns', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    const opens = (name: string) => document.querySelector<HTMLInputElement>(`input[aria-label="${name} can open it"]`)!;
    const role = (name: string) => document.querySelector<HTMLSelectElement>(`select[aria-label="${name}’s role"]`)!;
    expect(opens('Sam Ortiz').checked).toBe(true);
    expect(role('Sam Ortiz').value).toBe('role_operator');
    expect(opens('jordan').checked).toBe(false);
    expect(role('jordan').value).toBe('role_viewer');
    expect(opens('Design').checked).toBe(true);
    expect(role('Design').value).toBe('');
    // Your own row is not yours to change here.
    expect(document.querySelector('input[aria-label="Ben can open it"]')).toBeNull();
    expect(document.querySelector('button[aria-label="Remove Ben"]')).toBeNull();
    screen.unmount();
  });

  it('changes one plane at a time', async () => {
    const calls = recording();
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    await act(async () => document.querySelector<HTMLInputElement>('input[aria-label="jordan can open it"]')!.click());
    expect(JSON.parse(calls.find((c) => c.method === 'POST')!.body!)).toEqual({
      plane: 'data',
      principal_kind: 'user',
      principal_id: 'usr_jo',
    });

    const select = document.querySelector<HTMLSelectElement>('select[aria-label="Sam Ortiz’s role"]')!;
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value')!.set!.call(select, '');
      select.dispatchEvent(new Event('change', { bubbles: true }));
    });
    expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/grants/gr_4'))).toBe(true);
    expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/grants/gr_5'))).toBe(false);
    screen.unmount();
  });

  it('removes every grant a principal holds', async () => {
    const calls = recording();
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="Remove Sam Ortiz"]')!.click());
    const deleted = calls.filter((c) => c.method === 'DELETE').map((c) => c.url.split('/').at(-1));
    expect(deleted).toEqual(['gr_4', 'gr_5']);
    screen.unmount();
  });

  it('shares with Can open by default, from one field for people and groups', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named), MANAGE);
    const access = document.getElementById('s-access') as HTMLSelectElement;
    expect(access.value).toBe('');
    expect(access.options[0]?.textContent).toBe('Can open');
    expect([...access.options].map((o) => o.textContent)).toEqual(['Can open', 'Viewer', 'Operator', 'Owner']);
    expect(document.getElementById('f-person-or-group')).not.toBeNull();
    screen.unmount();
  });

  it('links Groups and roles for somebody who may see them', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed(named, 'allowed', ['install.view']), MANAGE);
    expect(screen.text()).toContain('People with the Administrator or App manager role can also manage every app on this host.');
    const link = [...document.querySelectorAll('a')].find((a) => a.textContent === 'Groups and roles');
    expect(link?.getAttribute('href')).toBe('/admin/identity');
    screen.unmount();
  });

  it('is read-only without app.grants.manage', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<Sharing appID="app_1" appName="crewmate" />, seed([anonymous(), ...named]), [AppVerb.View]);
    expect(document.querySelector('select')).toBeNull();
    expect(document.querySelector('input[type="checkbox"]:not([role="switch"])')).toBeNull();
    expect(document.getElementById('f-person-or-group')).toBeNull();
    expect(document.querySelector('button[aria-label^="Remove"]')).toBeNull();
    expect(sw('Make public')?.disabled).toBe(true);
    expect(sw('Require passcode')?.disabled).toBe(true);
    const text = screen.text();
    expect(text).toContain('Yes');
    expect(text).toContain('No role');
    expect(text).toContain('Operator');
    expect([...document.querySelectorAll('a')].some((a) => a.textContent === 'Groups and roles')).toBe(false);
    screen.unmount();
  });
});
