import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { serverRender as render } from '../ui/testing';
import { Audit, Installation } from './Installation';
import { Identity } from './Identity';
import { Events } from './Events';
import { AccountPage } from './Account';
import { AccountApps } from './AccountApps';
import { RolePicker } from './Accounts';
import { AdapterDialog } from './AdapterDialog';
import { Capacity } from './Capacity';
import { Identities } from './Identities';
import { NO_FILTERS } from './audit';

const kind = (category: string) => ({ category, kind: 'x', name: 'X', description: '', id_prefix: '', fields: null });

describe('the screens’ copy', () => {
  it('offers to add each category nothing is set up for, by its label', () => {
    const html = render(<Installation />, [
      [['me'], { verbs: ['install.adapters.manage'] }],
      [['adapters'], { adapters: [] }],
      [['adapter-kinds'], { kinds: [kind('ai'), kind('image_registry'), kind('routing')] }],
      [['sources'], { sources: [] }],
    ]);
    expect(html).toContain('Add an AI adapter');
    expect(html).toContain('Add an image registry adapter');
    expect(html).toContain('Add a routing adapter');
  });

  it('names where an archived month went, Pando when no adapter took it', () => {
    const month = { id: 'arc_1', month: '2026-01', row_count: 12, size_bytes: 1, sha256: 'a'.repeat(64) };
    const html = render(<Audit />, [
      [['audit-archives'], { archives: [month, { ...month, id: 'arc_2', month: '2026-02', adapter_ref: 's3/archive' }] }],
    ]);
    expect(html).toContain('>Pando<');
    expect(html).toContain('s3/archive');
  });

  it('says which scope a role applies to', () => {
    const roles = [
      { id: 'role_admin', name: 'admin', scope: 'install', builtin: true, verbs: [] },
      { id: 'role_viewer', name: 'viewer', scope: 'app', builtin: true, verbs: [] },
    ];
    const html = render(<Identity canEdit={false} />, [[['roles', 'all'], { roles }]]);
    expect(html).toContain('>Installation<');
    expect(html).toContain('>App<');
  });

  it('says what a subscription is about: an app by name, or the installation', () => {
    const sub = { id: 'sub_1', owner_id: 'usr_1', events: ['app.deployed'], destination: 'webhook', url: 'https://hooks.example.com', description: '', enabled: true, consecutive_failures: 0, created_at: '2026-10-01T00:00:00Z' };
    const html = render(<Events canManageAll={false} apps={[]} />, [
      [['subscriptions', '', {}], { pages: [{ subscriptions: [sub, { ...sub, id: 'sub_2', app_id: 'app_1', app_name: 'inventory' }, { ...sub, id: 'sub_3', app_id: 'app_2' }] }], pageParams: [''] }],
    ]);
    expect(html).toContain('>Installation<');
    expect(html).toContain('>inventory<');
    expect(html).toContain('>app_2<');
  });

  it('offers to edit an account an identity provider holds', () => {
    const account = { id: 'usr_1', adapter_id: 'idp_okta', external_id: 'dana', status: 'active', must_change_password: false, install_role_id: '' };
    const html = render(<AccountPage userID="usr_1" onBack={() => {}} onAudit={() => {}} />, [
      [['me'], { verbs: ['install.users.manage'] }],
      [['users', 'usr_1'], account],
    ]);
    expect(html).toContain('>Edit<');
  });
});

describe('the adapters’ restart notice', () => {
  const adapters = (verbs: string[]) =>
    render(<Installation />, [
      [['me'], { verbs }],
      [['adapters'], { adapters: [], restart_needed: true }],
    ]);

  it('says changes wait on a restart, and who can restart when the reader can’t', () => {
    expect(adapters(['install.adapters.manage'])).toContain('Adapter changes are saved. Restart Pando to apply them.');
    expect(adapters(['install.adapters.manage'])).not.toContain('Someone who can manage adapters');
    expect(adapters([])).toContain('Someone who can manage adapters can restart it.');
  });
});

describe('an empty audit log', () => {
  const audit = (initial: typeof NO_FILTERS) =>
    render(<Audit initial={initial} />, [[['audit', initial], { pages: [{ events: [], next_before: '' }], pageParams: [''] }]]);

  it('tells nothing recorded apart from nothing matching', () => {
    expect(audit(NO_FILTERS)).toContain('No events recorded');
    const filtered = audit({ ...NO_FILTERS, action: 'app.' });
    expect(filtered).toContain('No matching events');
    expect(filtered).toContain('Action is a prefix match');
  });
});

describe('an account', () => {
  const account = { id: 'usr_1', external_id: 'dana', status: 'active', must_change_password: false, install_role_id: '' };

  it('names a local account’s username as one', () => {
    const html = render(<AccountPage userID="usr_1" onBack={() => {}} onAudit={() => {}} />, [
      [['me'], { verbs: [] }],
      [['users', 'usr_1'], { ...account, adapter_id: 'idp_local' }],
    ]);
    expect(html).toContain('>Username<');
  });

  it.each([
    [true, 'None (gives up your role)'],
    [false, '>None<'],
  ])('offers no role, as the account itself: %s', (isSelf, none) => {
    expect(render(<RolePicker account={account as never} roles={[]} isSelf={isSelf} />)).toContain(none);
  });

  it('tags the apps it owns', () => {
    const access = { owner: false, can_manage: false, control: [], data: [] };
    const html = render(<AccountApps principal={{ kind: 'user', id: 'usr_1', name: 'dana' }} />, [
      [['roles', 'app'], { roles: [] }],
      [['users', 'usr_1', 'apps', '', {}], { pages: [{ apps: [{ ...access, app_id: 'app_1', app_name: 'inventory', owner: true }, { ...access, app_id: 'app_2', app_name: 'ledger' }] }], pageParams: [''] }],
    ]);
    expect(html.match(/>Owner</g)).toHaveLength(1);
  });

  it('tags the identity it was made from', () => {
    const identity = { adapter_id: 'idp_okta', adapter_name: 'Okta', external_id: 'dana' };
    const html = render(<Identities userID="usr_1" manage={false} />, [
      [['users', 'usr_1', 'identities'], { identities: [{ ...identity, origin: true }, { ...identity, external_id: 'dana2' }] }],
    ]);
    expect(html.match(/>Origin</g)).toHaveLength(1);
  });
});

describe('capacity', () => {
  const capacity = (refresh?: number) =>
    render(<Capacity names={{}} />, [
      [['capacity'], { as_of: '2026-10-04T12:00:00Z', refresh_seconds: refresh, runtimes: [{ adapter_ref: 'rt_docker', status: 'ok', running_workloads: 0 }] }],
    ]);

  it('says how often it is read, when it is', () => {
    expect(capacity(30)).toContain(', refreshed every 30 seconds.');
    expect(capacity()).not.toContain('refreshed every');
  });
});

describe('adding a source connection', () => {
  // When it takes effect is said once, after it is saved, beside the
  // restart notice; the steps before it are about setting it up.
  it('does not interrupt the steps with when it takes effect', () => {
    const html = render(<AdapterDialog category="source" adapters={[]} onClose={() => {}} onSaved={() => {}} />, [
      [['adapter-kinds'], { kinds: [] }],
    ]);
    expect(html).toContain('Add source adapter');
    expect(html).not.toContain('Takes effect');
  });
});
