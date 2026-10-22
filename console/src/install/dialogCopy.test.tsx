// @vitest-environment jsdom
import { act } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ChangePassword, Login } from '../auth/Login';
import { answering, mount, reply, serverRender, type Mounted, type Seed } from '../ui/testing';
import { AccessAI } from './AccessAI';
import { AccountPage } from './Account';
import { Events } from './Events';
import { Identity } from './Identity';
import { PolicyAI } from './PolicyAI';

// What a screen says as it opens over the data it reads, and once somebody
// opens a dialog in it, types in it, or gets an answer to what they asked.

let screen: Mounted | undefined;
const open = (...args: Parameters<typeof mount>) => (screen = mount(...args));

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

/** One page of a paged list, as the cache holds it. */
const page = (rows: object) => ({ pages: [rows], pageParams: [''] });

/** The groups and roles screen's data, with these groups and AI drafting on
 *  or off. */
const identity = (list: object[], drafting = false): Seed => [
  [['groups', 'list', '', {}], page({ groups: list })],
  [['roles'], { roles: [] }],
  [['roles', 'all'], { roles: [] }],
  [['ai-functions'], { functions: [{ function: 'draft_access', on: drafting }] }],
];

describe('groups and roles as they open', () => {
  const synced = { id: 'grp_a', name: 'eng', source: 'idp_okta', source_name: 'Okta', member_count: 4 };
  const own = { id: 'grp_1', name: 'ops', member_count: 1 };
  const cases: Array<[string, object[], boolean, string, string]> = [
    ['a synced group by its provider', [synced], false, '>Okta<', '>Pando<'],
    ['a Pando group, with no AI', [own, synced], false, '>Pando<', 'Ask AI'],
    ['drafting with AI where it is on', [own], true, 'Ask AI', 'Okta'],
  ];
  it.each(cases)('shows %s', (_, list, drafting, said, unsaid) => {
    const html = serverRender(<Identity canEdit />, identity(list, drafting));
    expect(html).toContain(said);
    expect(html).not.toContain(unsaid);
  });
});

describe('a group’s dialogs', () => {
  const groups = (list: object[]) => {
    vi.stubGlobal('fetch', answering({}));
    return open(<Identity canEdit />, identity(list));
  };
  /** Opens a group's options and chooses one; the menu is portaled to the body. */
  const choose = async (group: string, item: string) => {
    const button = document.querySelector<HTMLButtonElement>(`button[aria-label="Options for ${group}"]`)!;
    await act(async () => button.click());
    const found = [...document.querySelectorAll<HTMLElement>('[role="menuitem"]')].find((el) => el.textContent === item)!;
    await act(async () => found.click());
  };

  it.each([
    ['a synced group: its app limit only', 'eng', ['App limit']],
    ['a Pando group, with provider groups to link', 'ops', ['Edit members', 'Provider groups', 'App limit', 'Delete']],
  ])('offers %s', async (_, group, items) => {
    groups([
      { id: 'grp_a', name: 'eng', source: 'idp_okta', source_name: 'Okta', member_count: 4 },
      { id: 'grp_1', name: 'ops', member_count: 1 },
    ]);
    await act(async () => document.querySelector<HTMLButtonElement>(`button[aria-label="Options for ${group}"]`)!.click());
    expect([...document.querySelectorAll('[role="menuitem"]')].map((el) => el.textContent)).toEqual(items);
  });

  it('offers no provider groups to link when there are none', async () => {
    groups([{ id: 'grp_1', name: 'ops', member_count: 1 }]);
    await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="Options for ops"]')!.click());
    expect(document.body.textContent).not.toContain('Provider groups');
  });

  it('opens a group’s apps in a dialog, and adding one takes the dialog over', async () => {
    vi.stubGlobal('fetch', answering({}));
    const s = open(<Identity canEdit />, [
      ...identity([{ id: 'grp_1', name: 'ops', member_count: 1 }]),
      [['me'], { verbs: ['install.apps.view', 'install.apps.grants.manage'] }],
      [['groups', 'grp_1', 'apps', '', {}], page({ apps: [] })],
      [['roles', 'app'], { roles: [] }],
    ]);
    const dialogs = () => [...document.querySelectorAll('[role="dialog"]')];
    await s.press('Apps');
    expect(dialogs()).toHaveLength(1);
    expect(s.text()).toContain('Apps for ops');
    await s.press('Add app');
    expect(dialogs()).toHaveLength(1);
    expect(dialogs()[0]?.querySelector('h3')?.textContent).toBe('Add app');
    await s.press('Cancel');
    expect(dialogs()).toHaveLength(1);
    expect(s.text()).toContain('Apps for ops');
  });

  it('titles adding a group, and editing one’s members by its name', async () => {
    const s = groups([{ id: 'grp_1', name: 'ops', member_count: 2 }]);
    await s.press('Add group');
    // The button, and the dialog's title.
    expect(s.text().split('Add group')).toHaveLength(3);
    await s.press('Cancel');
    await choose('ops', 'Edit members');
    expect(s.text()).toContain('Members of ops');
  });

  it.each([
    [1, 'The one member keeps their account and'],
    [3, 'The 3 members keep their accounts and'],
  ])('counts %i member(s) kept on deleting a group', async (count, said) => {
    const s = groups([{ id: 'grp_1', name: 'ops', member_count: count }]);
    await choose('ops', 'Delete');
    expect(s.text()).toContain(said);
  });

  it('describes each provider group by its source and size', async () => {
    const s = groups([
      { id: 'grp_a', name: 'eng', source: 'idp_okta', source_name: 'Okta', member_count: 4 },
      { id: 'grp_b', name: 'sales', source: 'idp_okta', members: ['usr_1'] },
      { id: 'grp_c', name: 'legal', source: 'idp_okta' },
      { id: 'grp_1', name: 'ops', member_count: 0 },
    ]);
    await choose('ops', 'Provider groups');
    expect(s.text()).toContain('Okta, 4 members');
    expect(s.text()).toContain('idp_okta, 1 member');
    expect(s.text()).toContain('idp_okta, 0 members');
  });
});

describe('a role’s permissions', () => {
  const roles = [
    { id: 'role_ops', name: 'ops', scope: 'install', builtin: false, verbs: ['install.view', 'install.audit.read'] },
    { id: 'role_viewer', name: 'viewer', scope: 'app', builtin: true, verbs: ['app.view'] },
  ];

  it('counts them on the row and lists them once the role is opened', async () => {
    vi.stubGlobal('fetch', answering({}));
    const s = open(<Identity canEdit />, [
      [['groups', 'list', '', {}], page({ groups: [] })],
      [['roles'], { roles: [] }],
      [['roles', 'all'], { roles }],
      [['ai-functions'], { functions: [] }],
    ]);
    expect(s.text()).not.toContain('install.audit.read');
    // press matches a button's whole text, so this is the viewer row's.
    await s.press('1 permission');
    expect(s.text()).toContain('The viewer role');
    await s.press('Close');
    await s.press('2 permissions');
    expect(s.text()).toContain('The ops role');
    expect(s.text()).toContain('Installation role, made on this installation. 2 permissions.');
    expect(s.text()).toContain('install.audit.read');
    expect(s.text()).toContain('Read the audit log.');
  });
});

describe('a new subscription', () => {
  it.each([
    [true, 1],
    [false, 0],
  ])('offers the installation as its scope to somebody who manages all: %s', async (canManageAll, times) => {
    const s = open(<Events canManageAll={canManageAll} apps={[]} />, [
      [['subscriptions', '', {}], page({ subscriptions: [] })],
      [['events'], { events: [], destinations: [] }],
    ]);
    await s.press('New subscription');
    expect(s.text().match(/Installation/g) ?? []).toHaveLength(times);
  });
});

describe('a subscription to Slack, R-367', () => {
  it('chooses the channel it posts to from the workspace’s', async () => {
    const slack = { id: 'ntf_slack', name: 'Slack', kind: 'slack', audience: 'channel', channels: true };
    const s = open(<Events canManageAll apps={[]} />, [
      [['subscriptions', '', {}], page({ subscriptions: [] })],
      [['events'], { events: [], destinations: [slack] }],
      [['notify-channels', 'ntf_slack'], { channels: [{ id: 'C1', name: 'deploys' }, { id: 'G1', name: 'ops', private: true }] }],
    ]);
    await s.press('New subscription');
    expect(s.text()).not.toContain('Slack channel');
    await s.click('Notification channel');
    expect(s.text()).toContain('Slack channel');
    expect(s.text()).toContain('#deploys');
    expect(s.text()).toContain('/invite @Pando');
    await s.fill('Slack channel', 'G1');
  });
});

describe('a subscription’s request', () => {
  it.each([
    ['{"text":"{{.Type}}"}', 'Custom body'],
    ['', 'Default body'],
  ])('says whether its body is its own', async (template, said) => {
    vi.stubGlobal('fetch', answering({}));
    const sub = { id: 'sub_1', events: ['app.deployed'], destination: 'webhook', url: 'https://hooks.example.com', enabled: true, consecutive_failures: 0, created_at: '2026-10-01T00:00:00Z', payload_template: template };
    const s = open(<Events canManageAll={false} apps={[]} />, [
      [['subscriptions', '', {}], page({ subscriptions: [sub] })],
      [['subscription', 'sub_1'], sub],
      [['deliveries', 'sub_1'], { deliveries: [] }],
    ]);
    await s.click('https://hooks.example.com');
    expect(s.text()).toContain(said);
  });
});

describe('a password reset', () => {
  it.each([
    [false, 'They choose a new password at their next sign-in.'],
    [true, ''],
  ])('says whether they choose a new one, unticked: %s', async (untick, said) => {
    vi.stubGlobal('fetch', answering({ '/passwords/generate': { password: 'correct-horse-battery' }, '/password': {} }));
    const account = { id: 'usr_1', adapter_id: 'idp_local', external_id: 'dana', status: 'active', must_change_password: false, install_role_id: '' };
    const s = open(<AccountPage userID="usr_1" onBack={() => {}} onAudit={() => {}} />, [
      [['me'], { verbs: ['install.users.manage'] }],
      [['users', 'usr_1'], account],
    ]);
    await s.press('Reset password');
    if (untick) await s.click('Must change password at next sign-in');
    await s.press('Reset password');
    expect(s.text()).toContain('dana is signed out everywhere.' + (said && ` ${said}`));
    if (untick) expect(s.text()).not.toContain('They choose a new password');
  });
});

describe('asking AI to draft', () => {
  const cases: Array<[string, React.ReactNode, string, unknown, string]> = [
    ['a policy change', <PolicyAI onClose={() => {}} />, 'Rule', { proposed: {}, changes: [], reply: 'Nothing to change.' }, 'Change the proposal'],
    ['access', <AccessAI onClose={() => {}} />, 'Access', { reply: 'Nothing to draft.' }, 'Change the draft'],
  ];
  it.each(cases)('asks to change %s once there is one', async (_, dialog, label, answer, again) => {
    vi.stubGlobal('fetch', async () => reply(answer));
    const s = open(dialog, [[['verbs'], { verbs: [] }]] satisfies Seed);
    await s.fill(label, 'Nobody may open a shell');
    await s.submit(label);
    expect(s.text()).toContain(again);
  });
});

describe('a new password typed twice', () => {
  const forms: Array<[string, React.ReactNode, Seed, string, string]> = [
    ['setting up', <Login />, [[['setup'], { needed: true }]], 'Password', 'Password again'],
    ['changing one', <ChangePassword />, [], 'New password', 'New password again'],
  ];
  it.each(forms)('says when the two differ, %s', async (_, form, data, first, second) => {
    vi.stubGlobal('matchMedia', () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    const s = open(form, data);
    await s.fill(first, 'one-long-password');
    expect(s.text()).not.toContain('Passwords don’t match.');
    await s.fill(second, 'another-password');
    expect(s.text()).toContain('Passwords don’t match.');
  });
});
