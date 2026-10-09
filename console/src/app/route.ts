// Where you are, in the address bar rather than in a variable.
//
// The console kept its position in component state, so a reload dropped
// whoever was looking at it back on the launcher — and the back button did
// nothing, and there was no way to send somebody a link to an app.
//
// The paths here are exactly the ones the server reserves for the console
// (`/`, `/login`, `/admin`, `/admin/*` — see consoleRoutes in httpapi). It
// serves index.html for any of them, which is what makes a reload on a deep
// link work, and nothing else may be added here without reserving it there:
// every console route is a slug an app cannot have, and that is what keeps the
// two namespaces apart (R-023).

import { useEffect, useState } from 'react';

export type Section =
  | 'apps'
  | 'api'
  | 'accounts'
  | 'identity'
  | 'sign-in'
  | 'system'
  | 'audit'
  | 'approvals'
  | 'events';

/** The tabs of System (issue #154): how the installation itself is set up and
 *  kept, as opposed to its apps or its people. Each was a sidebar item of its
 *  own, and its old address still opens it. */
type SystemTab = 'adapters' | 'policy' | 'backups' | 'updates';

const SYSTEM_TABS: SystemTab[] = ['adapters', 'policy', 'backups', 'updates'];

export interface Route {
  /** Settings is its own page, not a section of the admin console: it is
   *  about the person, and everyone reaches it. It still lives under /admin,
   *  because that prefix is already reserved against app slugs (R-023). */
  view: 'launcher' | 'admin' | 'settings';
  section: Section;
  appID?: string;
  /** An app's tab, or System's. */
  tab?: string;
  /** An account's own page, under Accounts. */
  userID?: string;
  /** The Audit log's filters, as a query string without `?` — how an
   *  account's page links to the whole log already narrowed to it. On the
   *  Sign-in screen, the provider and test sign-in to show. */
  query?: string;
}

const SECTIONS: Section[] = [
  'apps',
  'api',
  'accounts',
  'identity',
  'sign-in',
  'system',
  'audit',
  'approvals',
  'events',
];

/** Reads a route out of a path. Anything unrecognized is the launcher. */
export function parse(pathname: string, search = ''): Route {
  const parts = pathname.split('/').filter(Boolean);

  if (parts[0] !== 'admin') return { view: 'launcher', section: 'apps' };
  if (parts[1] === 'settings') return { view: 'settings', section: 'apps' };

  // /admin/apps/{id}[/{tab}]
  if (parts[1] === 'apps' && parts[2]) {
    return { view: 'admin', section: 'apps', appID: parts[2], tab: parts[3] };
  }

  // /admin/accounts/{id}
  if (parts[1] === 'accounts' && parts[2]) {
    return { view: 'admin', section: 'accounts', userID: parts[2] };
  }

  // /admin/audit?… — filters carried in from a link. /admin/sign-in?… — the
  // provider a test sign-in came back from, and its report (issue #51).
  const query = search.replace(/^\?/, '');
  if (parts[1] === 'audit' && query) return { view: 'admin', section: 'audit', query };
  if (parts[1] === 'sign-in' && query) return { view: 'admin', section: 'sign-in', query };

  // /admin/system/{tab}. The bare /admin/system is its first tab, and which
  // one that is depends on what the person may see, so it carries no tab.
  // /admin/system/adapters?… is the outcome of a source connection's browser
  // authorization (issue #127).
  // Each tab was a section of its own at /admin/{tab}, and the adapters
  // screen before that was called Installation; a link may still say either.
  const tab =
    parts[1] === 'system'
      ? SYSTEM_TABS.find((t) => t === parts[2])
      : parts[1] === 'installation'
        ? 'adapters'
        : SYSTEM_TABS.find((t) => t === parts[1]);
  if (parts[1] === 'system' || tab) {
    const route: Route = { view: 'admin', section: 'system' };
    if (tab) route.tab = tab;
    if (tab === 'adapters' && query) route.query = query;
    return route;
  }

  const section = SECTIONS.find((s) => s === parts[1]);
  return { view: 'admin', section: section ?? 'apps' };
}

/** The path for a route. The inverse of parse, and tested as such. */
export function format(route: Route): string {
  if (route.view === 'launcher') return '/';
  if (route.view === 'settings') return '/admin/settings';
  if (route.section === 'apps' && route.appID) {
    return `/admin/apps/${route.appID}${route.tab ? `/${route.tab}` : ''}`;
  }
  if (route.section === 'accounts' && route.userID) return `/admin/accounts/${route.userID}`;
  if (route.section === 'system' && route.tab) {
    return `/admin/system/${route.tab}${route.query ? `?${route.query}` : ''}`;
  }
  if ((route.section === 'audit' || route.section === 'sign-in') && route.query) {
    return `/admin/${route.section}?${route.query}`;
  }
  return route.section === 'apps' ? '/admin' : `/admin/${route.section}`;
}

/**
 * The current route, and a way to change it.
 *
 * popstate is what makes the browser's own back and forward buttons work. It
 * is easy to leave out and easy not to notice: without it the address bar
 * changes and the page does not, which is worse than having no routing at all.
 */
export function useRoute(): [Route, (next: Route, replace?: boolean) => void] {
  const [route, setRoute] = useState<Route>(() => parse(window.location.pathname, window.location.search));

  useEffect(() => {
    const onPop = () => setRoute(parse(window.location.pathname, window.location.search));
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  const go = (next: Route, replace = false) => {
    const path = format(next);
    if (path !== window.location.pathname + window.location.search) {
      window.history[replace ? 'replaceState' : 'pushState']({}, '', path);
    }
    setRoute(next);
  };

  return [route, go];
}
