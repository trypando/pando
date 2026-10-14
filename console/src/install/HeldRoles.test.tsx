import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { InstallRole } from './Account';
import { GroupHeldRoles, HeldRoles, heldRoleNames, heldThrough, refreshMembership } from './Accounts';
import type { Account, HeldRole, Role } from './Accounts';

const roles: Role[] = [
  { id: 'role_administrator', name: 'administrator', builtin: true, verbs: [] },
  { id: 'role_auditor', name: 'audit readers', builtin: false, verbs: [] },
];

const direct: HeldRole = { role_id: 'role_auditor', via: { kind: 'direct' } };
const okta: HeldRole = {
  role_id: 'role_administrator',
  via: { kind: 'group', group_id: 'grp_01', name: 'pando-administrator', source: 'idp_okta', source_name: 'Okta' },
};
const ops: HeldRole = { role_id: 'role_auditor', via: { kind: 'group', group_id: 'grp_02', name: 'Ops' } };

// Issue #221: a role held through a group is a role the account holds.
describe('heldThrough', () => {
  it('names the group, and the provider of a synced one', () => {
    expect(heldThrough(okta)).toBe('through pando-administrator (Okta)');
    expect(heldThrough(ops)).toBe('through Ops');
  });

  it('falls back to the provider ID and the group ID', () => {
    expect(heldThrough({ role_id: 'r', via: { kind: 'group', group_id: 'grp_03', source: 'idp_x' } })).toBe(
      'through grp_03 (idp_x)',
    );
  });
});

describe('heldRoleNames', () => {
  it('is each role once, alphabetically, each a value of its own for the column filter', () => {
    expect(heldRoleNames([okta, direct, ops], roles)).toEqual(['Administrator', 'Audit readers']);
  });

  it('is None only for an account holding nothing by any route', () => {
    expect(heldRoleNames([], roles)).toEqual(['None']);
    expect(heldRoleNames([ops], roles)).toEqual(['Audit readers']);
  });

  it('names a role from the account when the roles list cannot be read, else shows its ID', () => {
    expect(heldRoleNames([{ ...direct, role_name: 'audit readers' }], [])).toEqual(['Audit readers']);
    expect(heldRoleNames([direct], [])).toEqual(['role_auditor']);
  });
});

describe('the Role column', () => {
  it('shows every role, saying where a group-held one comes from', () => {
    const html = renderToString(<HeldRoles held={[direct, okta]} roles={roles} />);
    expect(html).toContain('Audit readers');
    expect(html).toContain('Administrator');
    expect(html).toContain('through pando-administrator (Okta)');
    expect(html).not.toContain('None');
  });

  it('says None for an account holding nothing', () => {
    expect(renderToString(<HeldRoles held={[]} roles={roles} />)).toContain('None');
  });
});

const account: Account = {
  id: 'usr_01',
  adapter_id: 'idp_okta',
  external_id: '00ueve',
  status: 'active',
  must_change_password: false,
  install_role_id: '',
  install_roles: [okta],
};

function installRole(a: Account, manage: boolean, known?: Role[]) {
  return renderToString(
    <QueryClientProvider client={new QueryClient()}>
      <InstallRole account={a} roles={known} manage={manage} isSelf={false} />
    </QueryClientProvider>,
  );
}

describe('refreshMembership', () => {
  it('refreshes the groups and every account view, which show what a group carries', () => {
    const client = new QueryClient();
    const refreshed: unknown[] = [];
    client.invalidateQueries = ((filters?: { queryKey?: unknown }) => {
      refreshed.push(filters?.queryKey);
      return Promise.resolve();
    }) as QueryClient['invalidateQueries'];
    refreshMembership(client);
    expect(refreshed).toEqual([['groups'], ['users']]);
  });
});

describe('the account page role', () => {
  it('does not say None above a role held through a group', () => {
    expect(installRole(account, false, roles)).toBe('');
  });

  it('says None when the account holds nothing, and names a direct role', () => {
    expect(installRole({ ...account, install_roles: [] }, false, roles)).toContain('None');
    expect(installRole({ ...account, install_role_id: 'role_auditor', install_roles: [direct] }, false, roles)).toContain(
      'Audit readers',
    );
  });

  it('offers the picker to somebody who manages accounts', () => {
    expect(installRole(account, true, roles)).toContain('<select');
  });

  it('names your own direct role without the roles list, which needs install.view', () => {
    const own = { ...account, install_role_id: 'role_auditor', install_roles: [{ ...direct, role_name: 'audit readers' }] };
    expect(installRole(own, false, undefined)).toContain('Audit readers');
    expect(installRole({ ...account, install_roles: [] }, false, undefined)).toContain('None');
  });

  it('holds the place of the picker until the roles arrive', () => {
    expect(installRole(account, true, undefined)).not.toContain('<select');
  });
});

describe('the account page', () => {
  it('lists the roles held through groups, and not the direct one', () => {
    const html = renderToString(<GroupHeldRoles held={[direct, okta, ops]} roles={roles} />);
    expect(html).toContain('Administrator<!-- --> <!-- -->through pando-administrator (Okta)');
    expect(html).toContain('through Ops');
    expect(html.match(/through/g)).toHaveLength(2);
  });

  it('names a group-held role from the account when the roles list cannot be read', () => {
    const html = renderToString(<GroupHeldRoles held={[{ ...okta, role_name: 'administrator' }]} roles={[]} />);
    expect(html).toContain('Administrator');
    expect(html).not.toContain('role_administrator');
  });
});
