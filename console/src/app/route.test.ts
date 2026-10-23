import { describe, expect, it } from 'vitest';

import { format, parse } from './route';
import type { Route } from './route';

describe('route', () => {
  const routes: Route[] = [
    { view: 'launcher', section: 'apps' },
    { view: 'settings', section: 'apps' },
    { view: 'cli', section: 'apps' },
    { view: 'admin', section: 'apps' },
    { view: 'admin', section: 'api' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'settings' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'overview', panel: 'output', workload: 'worker' },
    { view: 'admin', section: 'apps', appID: 'app_01', panel: 'deployments', deployment: 'dep_01' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'events', panel: 'terminal' },
    { view: 'admin', section: 'accounts' },
    { view: 'admin', section: 'accounts', userID: 'usr_01' },
    { view: 'admin', section: 'audit' },
    { view: 'admin', section: 'approvals' },
    { view: 'admin', section: 'system' },
    { view: 'admin', section: 'system', tab: 'updates' },
    { view: 'admin', section: 'system', tab: 'adapters', query: 'connection=src_01&outcome=connected' },
    { view: 'admin', section: 'events' },
    { view: 'admin', section: 'audit', query: 'involving=usr_01&when=7d' },
    { view: 'admin', section: 'sign-in' },
    { view: 'admin', section: 'sign-in', query: 'provider=idp_01&test=abc' },
  ];

  it.each(routes)('parse is the inverse of format for %o', (route) => {
    const [path, search] = format(route).split('?');
    expect(parse(path ?? '', search ? `?${search}` : '')).toEqual(route);
  });

  it('still opens the adapters screen at its old address', () => {
    expect(parse('/admin/installation')).toEqual({ view: 'admin', section: 'system', tab: 'adapters' });
  });

  it.each(['adapters', 'policy', 'backups', 'updates'])('opens /admin/%s, now a tab of System', (tab) => {
    expect(parse(`/admin/${tab}`)).toEqual({ view: 'admin', section: 'system', tab });
  });

  it('keeps a source authorization outcome on the old adapters address', () => {
    expect(parse('/admin/adapters', '?outcome=connected')).toEqual({
      view: 'admin',
      section: 'system',
      tab: 'adapters',
      query: 'outcome=connected',
    });
  });

  it('opens the side panel from the address', () => {
    expect(format({ view: 'admin', section: 'apps', appID: 'app_01', tab: 'overview', panel: 'output', workload: 'worker' })).toBe(
      '/admin/apps/app_01/overview?panel=output&workload=worker',
    );
    expect(parse('/admin/apps/app_01', '?panel=deployments&deployment=dep_01')).toEqual({
      view: 'admin',
      section: 'apps',
      appID: 'app_01',
      tab: undefined,
      panel: 'deployments',
      deployment: 'dep_01',
    });
  });

  it('ignores a panel it does not know, and what would go in one', () => {
    expect(parse('/admin/apps/app_01/overview', '?panel=shell&workload=worker')).toEqual({
      view: 'admin',
      section: 'apps',
      appID: 'app_01',
      tab: 'overview',
    });
  });

  it.each([
    ['logs', { tab: 'overview', panel: 'output' }],
    ['terminal', { tab: 'overview', panel: 'terminal' }],
    ['resources', { tab: 'settings' }],
    ['detection', { tab: 'settings' }],
  ])('lands an old link to the %s tab on its new home', (old, home) => {
    expect(parse(`/admin/apps/app_01/${old}`)).toEqual({ view: 'admin', section: 'apps', appID: 'app_01', ...home });
  });

  it('keeps the workload an old Logs link carried in the panel it now opens', () => {
    expect(parse('/admin/apps/app_01/logs', '?workload=worker')).toEqual({
      view: 'admin',
      section: 'apps',
      appID: 'app_01',
      tab: 'overview',
      panel: 'output',
      workload: 'worker',
    });
  });

  it('keeps settings under the reserved /admin prefix', () => {
    // A top-level /settings would be a slug no app could have (R-023).
    expect(format({ view: 'settings', section: 'apps' })).toBe('/admin/settings');
  });
});
