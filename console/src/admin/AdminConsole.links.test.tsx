// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { AdminConsole } from './AdminConsole';
import { Launcher } from '../launcher/Launcher';
import { InstallVerb } from '../app/principal';
import { serverRender } from '../ui/testing';

// The console's navigation is links to the pages' own addresses, so each can be
// opened in a new tab: the sidebar, an app's tabs, the logo and the launcher's
// Admin.
const noop = () => {};
const me: [unknown[], unknown] = [['me'], { verbs: [InstallVerb.View, InstallVerb.AuditRead] }];

const hrefs = (html: string) => Array.from(html.matchAll(/<a [^>]*href="([^"]+)"/g), (m) => m[1]);

describe('console links', () => {
  it('gives the sidebar and the logo their addresses', () => {
    const html = serverRender(
      <AdminConsole route={{ view: 'admin', section: 'apps' }} go={noop} onLeave={noop} onSettings={noop} administrative />,
      [me],
    );
    expect(hrefs(html)).toEqual(
      expect.arrayContaining(['/', '/admin', '/admin/accounts', '/admin/system', '/admin/audit', '/admin/api']),
    );
  });

  it("gives an app's tabs their addresses", () => {
    const html = serverRender(
      <AdminConsole
        route={{ view: 'admin', section: 'apps', appID: 'app_1', tab: 'overview' }}
        go={noop}
        onLeave={noop}
        onSettings={noop}
        administrative
      />,
      [me, [['apps', 'app_1'], { id: 'app_1', name: 'shop', status: 'running', pinned_spec_id: 'spec_1', verbs: [] }]],
    );
    expect(hrefs(html)).toEqual(
      expect.arrayContaining([
        '/admin/apps/app_1/overview',
        '/admin/apps/app_1/events',
        '/admin/apps/app_1/sharing',
        '/admin/apps/app_1/settings',
      ]),
    );
    // Logs, Terminal and Configuration are no longer tabs.
    for (const gone of ['logs', 'terminal', 'detection', 'resources']) {
      expect(hrefs(html)).not.toContain(`/admin/apps/app_1/${gone}`);
    }
  });

  it("gives the launcher's Admin its address", () => {
    const html = serverRender(<Launcher onAdmin={noop} onSettings={noop} />, [me]);
    expect(hrefs(html)).toContain('/admin');
  });
});
