import { describe, expect, it } from 'vitest';

import { SETTINGS_PAGES, format, parse, settingsPageFor } from './route';
import type { Route } from './route';

describe('route', () => {
  const routes: Route[] = [
    { view: 'launcher', section: 'apps' },
    { view: 'settings', section: 'apps' },
    { view: 'cli', section: 'apps' },
    { view: 'authorize', section: 'apps' },
    { view: 'admin', section: 'apps' },
    { view: 'admin', section: 'api' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'settings' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'overview', panel: 'output', workload: 'worker' },
    { view: 'admin', section: 'apps', appID: 'app_01', panel: 'deployments', deployment: 'dep_01' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'events', panel: 'terminal' },
    { view: 'admin', section: 'accounts' },
    { view: 'admin', section: 'accounts', userID: 'usr_01' },
    { view: 'admin', section: 'identity' },
    { view: 'admin', section: 'identity', tab: 'roles' },
    { view: 'admin', section: 'identity', groupID: 'grp_01' },
    { view: 'admin', section: 'identity', roleID: 'role_01' },
    { view: 'admin', section: 'audit' },
    { view: 'admin', section: 'approvals' },
    { view: 'admin', section: 'system' },
    { view: 'admin', section: 'system', tab: 'overview' },
    { view: 'admin', section: 'system', tab: 'policy', sub: 'egress' },
    { view: 'admin', section: 'system', tab: 'adapters', sub: 'image_registry' },
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

  it.each(['adapters', 'policy', 'backups'])('opens /admin/%s, now a page of System', (tab) => {
    expect(parse(`/admin/${tab}`)).toEqual({ view: 'admin', section: 'system', tab });
  });

  it('opens Updates, at either old address, as Overview', () => {
    expect(parse('/admin/updates')).toEqual({ view: 'admin', section: 'system', tab: 'overview' });
    expect(parse('/admin/system/updates')).toEqual({ view: 'admin', section: 'system', tab: 'overview' });
  });

  it('opens a page of Policy, and ignores one that is not', () => {
    expect(parse('/admin/system/policy/access')).toEqual({ view: 'admin', section: 'system', tab: 'policy', sub: 'access' });
    expect(parse('/admin/system/policy/nothing')).toEqual({ view: 'admin', section: 'system', tab: 'policy' });
    expect(parse('/admin/system/adapters/Not-A-Category')).toEqual({ view: 'admin', section: 'system', tab: 'adapters' });
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
    ['detection', { tab: 'settings', page: 'general' }],
  ])('lands an old link to the %s tab on its new home', (old, home) => {
    expect(parse(`/admin/apps/app_01/${old}`)).toEqual({ view: 'admin', section: 'apps', appID: 'app_01', ...home });
  });

  it.each(SETTINGS_PAGES)('gives the %s page of Settings an address of its own', (page) => {
    const route: Route = { view: 'admin', section: 'apps', appID: 'app_01', tab: 'settings', page };
    expect(format(route)).toBe(`/admin/apps/app_01/settings/${page}`);
    expect(parse(`/admin/apps/app_01/settings/${page}`)).toEqual(route);
  });

  it('keeps a Settings page beside the side panel', () => {
    const route: Route = { view: 'admin', section: 'apps', appID: 'app_01', tab: 'settings', page: 'storage', panel: 'output' };
    expect(format(route)).toBe('/admin/apps/app_01/settings/storage?panel=output');
    expect(parse('/admin/apps/app_01/settings/storage', '?panel=output')).toEqual(route);
  });

  it('opens General for a page it does not know, and ignores a page on another tab', () => {
    expect(parse('/admin/apps/app_01/settings/network')).toEqual({ view: 'admin', section: 'apps', appID: 'app_01', tab: 'settings' });
    expect(parse('/admin/apps/app_01/overview/storage')).toEqual({ view: 'admin', section: 'apps', appID: 'app_01', tab: 'overview' });
    expect(format({ view: 'admin', section: 'apps', appID: 'app_01', tab: 'overview', page: 'storage' })).toBe('/admin/apps/app_01/overview');
  });

  it.each([
    ['variables', 'variables'],
    ['storage', 'storage'],
    ['dependencies', 'dependencies'],
    ['egress', 'egress'],
    ['auto-deploy', 'general'],
    ['configuration', 'general'],
    ['general', 'general'],
  ])('sends a link to the %s section to the %s page', (focus, page) => {
    expect(settingsPageFor(focus)).toBe(page);
  });

  it('sends a link with no section, or one it does not know, to no page in particular', () => {
    expect(settingsPageFor(undefined)).toBeUndefined();
    expect(settingsPageFor('security')).toBeUndefined();
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
