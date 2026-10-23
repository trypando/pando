// What the Sharing tab sends for each choice on its form (R-070, R-071), and
// how it reads the anonymous grant (R-075, R-077).

import { describe, expect, it } from 'vitest';

import {
  describeChoice,
  everyone,
  initialOf,
  openRequests,
  orderRoles,
  people,
  roleChoice,
  roleRequests,
  shareRequests,
  type Grant,
} from './share-access';

const ada = { kind: 'user' as const, id: 'usr_ada' };

const grant = (g: Partial<Grant>): Grant => ({
  id: 'gr_x',
  app_id: 'app_1',
  plane: 'data',
  principal_kind: 'user',
  ...g,
});

describe('shareRequests', () => {
  it('asks only to open the app by default', () => {
    expect(shareRequests([], ada, null)).toEqual([
      { method: 'post', body: { plane: 'data', principal_kind: 'user', principal_id: 'usr_ada' } },
    ]);
  });

  it('gives a role and the ability to open the app together', () => {
    expect(shareRequests([], { kind: 'group', id: 'grp_ops' }, 'role_operator')).toEqual([
      {
        method: 'post',
        body: { plane: 'control', principal_kind: 'group', principal_id: 'grp_ops', role_id: 'role_operator' },
      },
      { method: 'post', body: { plane: 'data', principal_kind: 'group', principal_id: 'grp_ops' } },
    ]);
  });

  it('does not ask again for what is already there', () => {
    const has = [
      grant({ id: 'gr_d', principal_id: 'usr_ada' }),
      grant({ id: 'gr_c', plane: 'control', principal_id: 'usr_ada', role_id: 'role_viewer' }),
    ];
    expect(shareRequests(has, ada, null)).toEqual([]);
    expect(shareRequests(has, ada, 'role_viewer')).toEqual([]);
  });

  it('moves an existing role rather than adding a second', () => {
    const has = [grant({ id: 'gr_c', plane: 'control', principal_id: 'usr_ada', role_id: 'role_viewer' })];
    expect(shareRequests(has, ada, 'role_owner')).toEqual([
      { method: 'patch', grant: 'gr_c', body: { role_id: 'role_owner' } },
      { method: 'post', body: { plane: 'data', principal_kind: 'user', principal_id: 'usr_ada' } },
    ]);
  });

  it('does not mistake a group for an account with the same ID', () => {
    const has = [grant({ principal_kind: 'group', principal_id: 'usr_ada' })];
    expect(shareRequests(has, ada, null)).toHaveLength(1);
  });
});

describe('everyone', () => {
  it('reads private, public and passcode', () => {
    expect(everyone([grant({ principal_id: 'usr_ada' })])).toBe('private');
    expect(everyone([grant({ principal_kind: 'anonymous' })])).toBe('public');
    expect(everyone([grant({ principal_kind: 'anonymous', passcode: true })])).toBe('passcode');
  });
});

describe('roleChoice', () => {
  it('names a custom role, since there is no description to give', () => {
    expect(roleChoice({ id: 'role_01H', name: 'support' })).toBe('Support');
    expect(roleChoice({ id: 'role_viewer', name: 'viewer' })).toBe('Viewer');
  });
});

describe('the access dropdown', () => {
  it('lists the built-ins by what they allow, then custom roles by name', () => {
    const ordered = orderRoles([
      { id: 'role_owner', name: 'owner' },
      { id: 'role_02', name: 'support' },
      { id: 'role_viewer', name: 'viewer' },
      { id: 'role_01', name: 'auditor' },
      { id: 'role_operator', name: 'operator' },
    ]);
    expect(ordered.map((r) => r.name)).toEqual(['viewer', 'operator', 'owner', 'auditor', 'support']);
  });

  it('describes a custom role by what it holds', () => {
    expect(describeChoice(undefined)).toContain('No access to its settings');
    expect(describeChoice({ id: 'role_02', name: 'support', verbs: ['app.view', 'app.logs.read'] })).toBe(
      'Open the app, plus: view, logs.read.',
    );
    expect(describeChoice({ id: 'role_03', name: 'empty' })).toBe('Open the app, with the Empty role.');
  });

  it('describes each built-in role in a sentence', () => {
    expect(describeChoice({ id: 'role_viewer', name: 'viewer' })).toBe('Open the app and view its settings and logs.');
    expect(describeChoice({ id: 'role_operator', name: 'operator' })).toBe(
      'Open, deploy and restart the app, and change its settings.',
    );
    expect(describeChoice({ id: 'role_owner', name: 'owner' })).toBe('Everything, including sharing and deleting the app.');
  });
});

// R-070, R-071: the two columns of the People and groups table, each one plane.
describe('one plane at a time', () => {
  const sam = { kind: 'user', id: 'usr_sam' };
  const grants: Grant[] = [
    { id: 'gr_c', app_id: 'app_1', plane: 'control', principal_kind: 'user', principal_id: 'usr_sam', role_id: 'role_viewer' },
    { id: 'gr_d', app_id: 'app_1', plane: 'data', principal_kind: 'user', principal_id: 'usr_sam' },
  ];

  it('lets somebody open the app, or stops them, without touching their role', () => {
    expect(openRequests(grants, sam, true)).toEqual([]);
    expect(openRequests(grants, sam, false)).toEqual([{ method: 'delete', grant: 'gr_d' }]);
    expect(openRequests([grants[0]!], sam, true)).toEqual([
      { method: 'post', body: { plane: 'data', principal_kind: 'user', principal_id: 'usr_sam' } },
    ]);
  });

  it('gives, moves and takes away a role, without touching opening it', () => {
    expect(roleRequests(grants, sam, 'role_viewer')).toEqual([]);
    expect(roleRequests(grants, sam, 'role_owner')).toEqual([{ method: 'patch', grant: 'gr_c', body: { role_id: 'role_owner' } }]);
    expect(roleRequests(grants, sam, '')).toEqual([{ method: 'delete', grant: 'gr_c' }]);
    expect(roleRequests([grants[1]!], sam, 'role_owner')).toEqual([
      { method: 'post', body: { plane: 'control', principal_kind: 'user', principal_id: 'usr_sam', role_id: 'role_owner' } },
    ]);
    expect(roleRequests([grants[1]!], sam, '')).toEqual([]);
  });
});

describe('the people an app is shared with', () => {
  const grants: Grant[] = [
    { id: 'gr_a', app_id: 'app_1', plane: 'data', principal_kind: 'anonymous' },
    { id: 'gr_1', app_id: 'app_1', plane: 'data', principal_kind: 'group', principal_id: 'grp_1', principal_name: 'Design' },
    { id: 'gr_2', app_id: 'app_1', plane: 'control', principal_kind: 'user', principal_id: 'usr_me', principal_name: 'ben', role_id: 'role_owner', role_name: 'owner' },
    { id: 'gr_3', app_id: 'app_1', plane: 'data', principal_kind: 'user', principal_id: 'usr_me', principal_name: 'ben' },
    { id: 'gr_4', app_id: 'app_1', plane: 'control', principal_kind: 'token', principal_id: 'tok_1', role_id: 'role_x' },
  ];

  it('is one row each, you first, and never everyone', () => {
    const rows = people(grants, { id: 'usr_me' });
    expect(rows.map((p) => [p.kind, p.name, p.you, p.opens, p.role?.name])).toEqual([
      ['user', 'ben', true, true, 'Owner'],
      ['group', 'Design', false, true, undefined],
      ['token', 'tok_1', false, false, 'Role_x'],
    ]);
    expect(rows[0]?.grantIDs).toEqual(['gr_2', 'gr_3']);
  });

  it('marks nobody as you when nobody is signed in', () => {
    expect(people(grants).some((p) => p.you)).toBe(false);
  });

  it('draws a person as the first letter of their name', () => {
    expect(initialOf('sam ortiz')).toBe('S');
    expect(initialOf('  Émile')).toBe('É');
    expect(initialOf('')).toBe('?');
  });
});
