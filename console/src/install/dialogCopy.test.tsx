// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ChangePassword, Login } from '../auth/Login';
import { answering, mount, reply, type Mounted, type Seed } from '../ui/testing';
import { AccessAI } from './AccessAI';
import { AccountPage } from './Account';
import { Events } from './Events';
import { Identity } from './Identity';
import { PolicyAI } from './PolicyAI';

// What a screen says once somebody opens a dialog in it, types in it, or gets
// an answer to what they asked.

let screen: Mounted | undefined;
const open = (...args: Parameters<typeof mount>) => (screen = mount(...args));

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

/** One page of a paged list, as the cache holds it. */
const page = (rows: object) => ({ pages: [rows], pageParams: [''] });

describe('a group’s dialogs', () => {
  const groups = (list: object[]) => {
    vi.stubGlobal('fetch', answering({}));
    return open(<Identity canEdit />, [
      [['groups', 'list', '', {}], page({ groups: list })],
      [['roles'], { roles: [] }],
      [['roles', 'all'], { roles: [] }],
    ]);
  };

  it('titles adding a group, and editing one’s members by its name', async () => {
    const s = groups([{ id: 'grp_1', name: 'ops', member_count: 2 }]);
    await s.press('Add group');
    // The button, and the dialog's title.
    expect(s.text().split('Add group')).toHaveLength(3);
    await s.press('Cancel');
    await s.press('Edit members');
    expect(s.text()).toContain('Members of ops');
  });

  it.each([
    [1, 'The one member keep'],
    [3, 'The 3 members keep'],
  ])('counts %i member(s) kept on deleting a group', async (count, said) => {
    const s = groups([{ id: 'grp_1', name: 'ops', member_count: count }]);
    await s.press('Delete');
    expect(s.text()).toContain(said);
  });

  it('describes each provider group by its source and size', async () => {
    const s = groups([
      { id: 'grp_a', name: 'eng', source: 'idp_okta', source_name: 'Okta', member_count: 4 },
      { id: 'grp_b', name: 'sales', source: 'idp_okta', members: ['usr_1'] },
      { id: 'grp_c', name: 'legal', source: 'idp_okta' },
      { id: 'grp_1', name: 'ops', member_count: 0 },
    ]);
    await s.press('Provider groups');
    expect(s.text()).toContain('Okta, 4 members');
    expect(s.text()).toContain('idp_okta, 1 members');
    expect(s.text()).toContain('idp_okta, 0 members');
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
