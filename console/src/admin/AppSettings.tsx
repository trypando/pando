// An app's Settings tab: a list of pages on the left that is real navigation,
// and one page at a time, each at an address of its own
// (/admin/apps/{id}/settings/{page}). At phone width the list stacks above
// the page.
//
// Every page is readable with app.view; each control on it asks for exactly
// the verb its endpoint checks, so somebody who can only look sees the same
// pages with nothing on them that would be refused.

import type { App } from '@api/types.gen';
import { RouteLink } from '../ui/RouteLink';
import type { SettingsPage } from '../app/route';
import { Egress } from './Egress';
import { SettingsDependencies } from './SettingsDependencies';
import { SettingsGeneral } from './SettingsGeneral';
import { SettingsStorage } from './SettingsStorage';
import { SettingsVariables } from './SettingsVariables';
import './settings.css';

const PAGES: Array<{ page: SettingsPage; label: string }> = [
  { page: 'general', label: 'General' },
  { page: 'variables', label: 'Variables' },
  { page: 'dependencies', label: 'Dependencies' },
  { page: 'storage', label: 'Storage' },
  { page: 'egress', label: 'Egress' },
];

export function AppSettings({
  app,
  page,
  onPage,
  hrefOf,
}: Readonly<{
  app: App;
  page: SettingsPage;
  onPage: (page: SettingsPage) => void;
  /** Each page's address, so a page opens in a new tab like any link. */
  hrefOf?: (page: SettingsPage) => string;
}>) {
  return (
    <div className="pando-settings">
      <nav aria-label="Settings" className="pando-settings-nav">
        {PAGES.map((p) => (
          <RouteLink
            key={p.page}
            href={hrefOf?.(p.page) ?? `#${p.page}`}
            aria-current={p.page === page ? 'page' : undefined}
            onFollow={() => onPage(p.page)}
          >
            {p.label}
          </RouteLink>
        ))}
      </nav>
      <div style={{ minWidth: 0 }}>
        {page === 'general' && <SettingsGeneral app={app} />}
        {page === 'variables' && <SettingsVariables appID={app.id} />}
        {page === 'dependencies' && <SettingsDependencies appID={app.id} />}
        {page === 'storage' && <SettingsStorage app={app} />}
        {page === 'egress' && <Egress appID={app.id} />}
      </div>
    </div>
  );
}
