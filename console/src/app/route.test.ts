import { describe, expect, it } from 'vitest';

import { format, parse } from './route';
import type { Route } from './route';

describe('route', () => {
  const routes: Route[] = [
    { view: 'launcher', section: 'apps' },
    { view: 'settings', section: 'apps' },
    { view: 'admin', section: 'apps' },
    { view: 'admin', section: 'api' },
    { view: 'admin', section: 'apps', appID: 'app_01', tab: 'logs' },
    { view: 'admin', section: 'accounts' },
    { view: 'admin', section: 'accounts', userID: 'usr_01' },
    { view: 'admin', section: 'audit' },
    { view: 'admin', section: 'approvals' },
    { view: 'admin', section: 'updates' },
    { view: 'admin', section: 'audit', query: 'involving=usr_01&when=7d' },
    { view: 'admin', section: 'sign-in' },
    { view: 'admin', section: 'sign-in', query: 'provider=idp_01&test=abc' },
  ];

  it.each(routes)('parse is the inverse of format for %o', (route) => {
    const [path, search] = format(route).split('?');
    expect(parse(path ?? '', search ? `?${search}` : '')).toEqual(route);
  });

  it('still opens the adapters screen at its old address', () => {
    expect(parse('/admin/installation')).toEqual({ view: 'admin', section: 'adapters' });
  });

  it('keeps settings under the reserved /admin prefix', () => {
    // A top-level /settings would be a slug no app could have (R-023).
    expect(format({ view: 'settings', section: 'apps' })).toBe('/admin/settings');
  });
});
