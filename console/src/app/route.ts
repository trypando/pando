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

/** The pages of System (issue #154): how the installation itself is set up
 *  and kept, as opposed to its apps or its people. Each was a sidebar item of
 *  its own, and its old address still opens it. */
export type SystemTab = 'overview' | 'adapters' | 'policy' | 'backups' | 'hosts';

const SYSTEM_TABS: SystemTab[] = ['overview', 'adapters', 'policy', 'backups', 'hosts'];

/** Where a System page that was folded into another lives now: Updates is
 *  the Pando card on Overview. */
const MOVED_SYSTEM_TABS: Record<string, SystemTab> = { updates: 'overview' };

function systemTab(name: string | undefined): SystemTab | undefined {
  return SYSTEM_TABS.find((t) => t === name) ?? (name ? MOVED_SYSTEM_TABS[name] : undefined);
}

/** Policy's pages, each at /admin/system/policy/{page}. The bare /policy is
 *  the first. */
export type PolicyPage = 'sources' | 'access' | 'egress' | 'deployments' | 'tokens' | 'apps' | 'audit';

export const POLICY_PAGES: PolicyPage[] = ['sources', 'access', 'egress', 'deployments', 'tokens', 'apps', 'audit'];

/** The side panel over an app's screen: what it is printing, its deploys, and
 *  a terminal in it. Each was a tab of its own. */
export type PanelView = 'output' | 'deployments' | 'terminal';

const PANEL_VIEWS: PanelView[] = ['output', 'deployments', 'terminal'];

/**
 * Where each of an app's old tabs lives now. Logs and Terminal became the side
 * panel and Configuration was folded into Settings, which was `resources`; a
 * link to any of them lands on its new home rather than on nothing.
 */
const MOVED_TABS: Record<string, { tab: string; panel?: PanelView; page?: SettingsPage }> = {
  logs: { tab: 'overview', panel: 'output' },
  terminal: { tab: 'overview', panel: 'terminal' },
  resources: { tab: 'settings' },
  detection: { tab: 'settings', page: 'general' },
};

/** The pages of an app's Settings tab, each at an address of its own:
 *  /admin/apps/{id}/settings/{page}. The bare /settings is General. */
export type SettingsPage = 'general' | 'variables' | 'dependencies' | 'storage' | 'egress';

export const SETTINGS_PAGES: SettingsPage[] = ['general', 'variables', 'dependencies', 'storage', 'egress'];

/**
 * The Settings page a link's `focus` means. Settings was one long page, and a
 * warning or a refusal sent somebody to a section of it by name; each section
 * is a page now, and those names still land on the right one.
 */
const FOCUS_PAGES: Record<string, SettingsPage> = {
  general: 'general',
  variables: 'variables',
  dependencies: 'dependencies',
  storage: 'storage',
  egress: 'egress',
  'auto-deploy': 'general',
  configuration: 'general',
  detection: 'general',
  idle: 'general',
};

export function settingsPageFor(focus: string | undefined): SettingsPage | undefined {
  return focus ? FOCUS_PAGES[focus] : undefined;
}

export interface Route {
  /** Settings is its own page, not a section of the admin console: it is
   *  about the person, and everyone reaches it. It still lives under /admin,
   *  because that prefix is already reserved against app slugs (R-023).
   *  So is the CLI's approval page (R-406), which `pando login` opens, and
   *  an MCP client's (R-413), OAuth's authorization endpoint. */
  view: 'launcher' | 'admin' | 'settings' | 'cli' | 'authorize';
  section: Section;
  appID?: string;
  /** An app's tab, or System's. */
  tab?: string;
  /** The page of an app's Settings tab. Absent is General. */
  page?: SettingsPage;
  /** The side panel open over an app's screen, and what it shows: which
   *  workload's output or terminal, which deployment's log. */
  panel?: PanelView;
  workload?: string;
  deployment?: string;
  /** Under System: a page of Policy, or the adapter category shown. */
  sub?: string;
  /** An account's own page, under Accounts. */
  userID?: string;
  /** A group's or a role's own page, under Groups and roles. */
  groupID?: string;
  roleID?: string;
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
  if (parts[1] === 'cli') return { view: 'cli', section: 'apps' };
  if (parts[1] === 'authorize') return { view: 'authorize', section: 'apps' };

  // /admin/apps/{id}[/{tab}[/{page}]][?panel=…&workload=…&deployment=…]
  if (parts[1] === 'apps' && parts[2]) return appRoute(parts[2], parts[3], search, parts[4]);

  // /admin/accounts/{id}
  if (parts[1] === 'accounts' && parts[2]) {
    return { view: 'admin', section: 'accounts', userID: parts[2] };
  }

  // /admin/identity/groups/{id}, /admin/identity/roles/{id}, and the Roles
  // tab at /admin/identity/roles.
  if (parts[1] === 'identity') return identityRoute(parts[2], parts[3]);

  // /admin/audit?… — filters carried in from a link. /admin/sign-in?… — the
  // provider a test sign-in came back from, and its report (issue #51).
  const query = search.replace(/^\?/, '');
  if (parts[1] === 'audit' && query) return { view: 'admin', section: 'audit', query };
  if (parts[1] === 'sign-in' && query) return { view: 'admin', section: 'sign-in', query };

  // /admin/system/{tab}[/{sub}]. The bare /admin/system is its first page,
  // and which one that is depends on what the person may see, so it carries
  // no tab. /admin/system/adapters?… is the outcome of a source connection's
  // browser authorization (issue #127).
  // Each page was a section of its own at /admin/{tab}, and the adapters
  // screen before that was called Installation; a link may still say either.
  const inSystem = parts[1] === 'system';
  let tab = inSystem ? systemTab(parts[2]) : systemTab(parts[1]);
  if (parts[1] === 'installation') tab = 'adapters';
  if (inSystem || tab) {
    const route: Route = { view: 'admin', section: 'system' };
    if (tab) route.tab = tab;
    const sub = inSystem ? parts[3] : undefined;
    if (tab === 'policy' && sub && (POLICY_PAGES as string[]).includes(sub)) route.sub = sub;
    // An adapter category: a word such as image_registry.
    if (tab === 'adapters' && sub && /^[a-z_]+$/.test(sub)) route.sub = sub;
    if (tab === 'adapters' && query) route.query = query;
    return route;
  }

  const section = SECTIONS.find((s) => s === parts[1]);
  return { view: 'admin', section: section ?? 'apps' };
}

/** Groups and roles: a group's page, a role's, or the list on either tab. */
function identityRoute(kind: string | undefined, id: string | undefined): Route {
  const route: Route = { view: 'admin', section: 'identity' };
  if (kind === 'groups' && id) route.groupID = id;
  else if (kind === 'roles' && id) route.roleID = id;
  else if (kind === 'roles') route.tab = 'roles';
  return route;
}

/** An app's screen: its tab, and the side panel when one is open. */
function appRoute(appID: string, tab: string | undefined, search: string, sub?: string): Route {
  const route: Route = { view: 'admin', section: 'apps', appID, tab };
  const moved = tab ? MOVED_TABS[tab] : undefined;
  if (moved) route.tab = moved.tab;
  const page = moved?.page ?? SETTINGS_PAGES.find((p) => p === sub);
  if (route.tab === 'settings' && page) route.page = page;

  const params = new URLSearchParams(search);
  const panel = PANEL_VIEWS.find((v) => v === params.get('panel')) ?? moved?.panel;
  if (!panel) return route;
  route.panel = panel;
  const workload = params.get('workload');
  const deployment = params.get('deployment');
  if (workload) route.workload = workload;
  if (deployment) route.deployment = deployment;
  return route;
}

/** The query string of an app's side panel, or nothing when it is closed. */
function panelQuery(route: Route): string {
  if (!route.panel) return '';
  const params = new URLSearchParams({ panel: route.panel });
  if (route.workload) params.set('workload', route.workload);
  if (route.deployment) params.set('deployment', route.deployment);
  return `?${params.toString()}`;
}

/** The views that are one page each, at one address. */
const FIXED_PATHS: Partial<Record<Route['view'], string>> = {
  launcher: '/',
  settings: '/admin/settings',
  cli: '/admin/cli',
  authorize: '/admin/authorize',
};

/** The path for a route. The inverse of parse, and tested as such. */
export function format(route: Route): string {
  const fixed = FIXED_PATHS[route.view];
  if (fixed) return fixed;
  if (route.section === 'apps' && route.appID) {
    const page = route.tab === 'settings' && route.page ? `/${route.page}` : '';
    return `/admin/apps/${route.appID}${route.tab ? `/${route.tab}` : ''}${page}${panelQuery(route)}`;
  }
  if (route.section === 'accounts' && route.userID) return `/admin/accounts/${route.userID}`;
  if (route.section === 'identity') {
    if (route.groupID) return `/admin/identity/groups/${route.groupID}`;
    if (route.roleID) return `/admin/identity/roles/${route.roleID}`;
    if (route.tab === 'roles') return '/admin/identity/roles';
  }
  if (route.section === 'system' && route.tab) {
    const sub = route.sub ? `/${route.sub}` : '';
    const query = route.query ? `?${route.query}` : '';
    return `/admin/system/${route.tab}${sub}${query}`;
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

  // An app's old tab is read as its new home; the address is made to say so,
  // so that what is copied from it is the address of what is on the screen.
  useEffect(() => {
    if (!route.appID) return;
    const path = format(route);
    if (path !== window.location.pathname + window.location.search) window.history.replaceState({}, '', path);
  }, []);

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
