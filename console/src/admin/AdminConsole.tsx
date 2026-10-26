// The management console.
//
// Reached only from the launcher, and only by someone holding an administrative
// verb (R-265). Everything in here is app administration, scoped by the server:
// `GET /apps` is control-plane scoped and returns the apps this person may
// manage and no others.
//
// Install-level administration — accounts, host policy, the installation's
// adapters and capacity, the audit log — sits beside Apps in the sidebar, each
// item shown on the verb it needs and not on "is an administrator". There is no
// implication graph between verbs (R-082), so a sidebar that assumed one would
// offer a screen whose every request comes back 403.

import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Badge,
  Banner,
  Button,
  EmptyState,
  Icon,
  IconButton,
  Logo,
  SidebarNav,
  Skeleton,
  StatusIndicator,
  Tabs,
} from '@design';
import type { SidebarItem, TabItem } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { COUNT_STALE_MS, InstallVerb, useInstallVerb, useManageableAppsTotal } from '../app/principal';
import { AccountPage } from '../install/Account';
import { Accounts, messageOf } from '../install/Accounts';
import { filtersFrom, linkQuery } from '../install/audit';
import { Identity } from '../install/Identity';
import { GroupPage } from '../install/GroupPage';
import { RolePage } from '../install/RolePage';
import { SignIn } from '../install/SignIn';
import { Events } from '../install/Events';
import { InboxButton } from '../ui/Inbox';
import { FeedbackButton } from '../ui/Feedback';
import { useUpdates } from '../install/Updates';
import { System, systemPages } from '../install/System';
import { Audit } from '../install/Installation';
import { statusLabel, statusSymbol } from '../ui/status';
import { AppOnboarding } from './AppOnboarding';
import { Sharing } from './Sharing';
import { AppOverview, scrollToSecurity } from './AppOverview';
import { useRecordFollowsStatus } from './appStatus';
import { AppEvents } from './AppEvents';
import { AppSettings } from './AppSettings';
import { AddApp } from './AddApp';
import { Reference } from './Reference';
import { Approvals, useApprovals } from './Approvals';
import { DeleteApp } from './DeleteApp';
import { DeployButton } from './DeployButton';
import { AppImage } from './AppImage';
import { AppName } from './AppName';
import { AppAddress } from './AppAddress';
import { HeaderScore } from './HeaderScore';
import { Lifecycle } from './Lifecycle';
import { AppVerb, AppVerbs, can, changesAnything, type AppWithVerbs } from './verbs';
import { AppPanel, PanelOpener, type PanelTarget } from './AppPanel';
import { format, settingsPageFor, type Route, type Section, type SettingsPage } from '../app/route';
import { MEASURE } from '../ui/layout';
import { Sheet } from '../ui/Sheet';
import { TopoBackground } from '../ui/TopoBackground';
import { SearchField } from '../ui/SearchField';
import { ShowMore, totalLabel, usePaged, useSettled, type PageOf } from '../ui/paged';
import { APP_LIST_KEY, useWatchedRows } from './appList';
import { NameCell } from './NameCell';
import { AccessCell, SecurityCell, StackedAppRow, StatusCell, UpdatedCell, rowStatus, securityFilterValue } from './AppListCells';
import { accessLabel } from './share-access';
import { useNarrow } from '../ui/narrow';
import { Table } from '../ui/Table';
import { RouteLink } from '../ui/RouteLink';
import { FieldSkeleton, HeadingSkeleton, LineSkeleton, Loading } from '../ui/Loading';
import { WithCode } from '../ui/ErrorCode';

/** An app's row at phone width, in place of the columns. */
const stackedRow = (row: App) => <StackedAppRow row={row} />;

export function AdminConsole({
  route,
  go,
  onLeave,
  onSettings,
  administrative,
}: {
  route: Route;
  go: (next: Route, replace?: boolean) => void;
  onLeave: () => void;
  /** The person's own settings — a page of its own, not a section here. */
  onSettings: () => void;
  /** Whether this person administers anything. False for somebody who came
   *  here for the API screen, which is open to everyone. */
  administrative: boolean;
}) {
  // Where we are comes from the address bar, so a reload lands back here and a
  // link to an app is a link to an app.
  //
  // The app is identified by id rather than held as a value. It used to hold
  // the App object captured when the row was clicked, and that object never
  // changed again — so accepting a proposal left this screen rendering the app
  // as it was before, with no way to deploy.
  const section = route.section;

  // Phone width: the sidebar becomes a menu (see the header below).
  const narrow = useNarrow();
  const [menuOpen, setMenuOpen] = useState(false);
  useEffect(() => {
    if (!menuOpen) return undefined;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setMenuOpen(false);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [menuOpen]);
  const selectedID = route.appID ?? null;
  const setSection = (next: Section) => go({ view: 'admin', section: next });
  const setSelectedID = (id: string | null) =>
    go(id ? { view: 'admin', section: 'apps', appID: id } : { view: 'admin', section: 'apps' });

  // One question per screen. `install.view` is not a master key: an account can
  // hold install.audit.read and nothing else, and for them the console is the
  // audit log.
  const canView = useInstallVerb(InstallVerb.View);
  const canManageUsers = useInstallVerb(InstallVerb.UsersManage);
  const canManageAdapters = useInstallVerb(InstallVerb.AdaptersManage);
  const canManagePolicy = useInstallVerb(InstallVerb.PolicyManage);
  const canReadAudit = useInstallVerb(InstallVerb.AuditRead);
  const canManageBackups = useInstallVerb(InstallVerb.BackupManage);
  const canManageCluster = useInstallVerb(InstallVerb.ClusterManage);
  const canApproveAll = useInstallVerb(InstallVerb.DeploysApprove);
  const canManageEvents = useInstallVerb(InstallVerb.EventsManage);

  // Deploys waiting for approval (R-154). Asked by anybody who administers an
  // app, not only by holders of install.deploys.approve: app.deploy.approve is
  // granted per app, through a custom role, and the sidebar has no other way
  // to know who holds it. The item is shown to an install-wide approver
  // always, and to anybody else while something is waiting that they may decide.
  const approvals = useApprovals(administrative || canApproveAll);
  const waiting = approvals.rows;

  // A page at a time, searched by the server (issue #72): an install can hold
  // twenty thousand apps, and neither the request nor the table should.
  const [appSearch, setAppSearch] = useState('');
  const apps = usePaged<{ apps: App[] | null } & PageOf, App>({
    key: APP_LIST_KEY,
    path: '/apps',
    rows: (p) => p.apps,
    search: useSettled(appSearch.trim()),
    enabled: administrative,
  });
  const rows = useWatchedRows(apps.rows, administrative);
  // Every app, whatever the search: the same count the Admin entry is decided
  // by, so it is already in the cache.
  const appCount = totalLabel(useManageableAppsTotal());

  // The count alone, for the sidebar: one row asked for, and `total` read.
  // Only asked for by somebody who may read it — the endpoint refuses the
  // rest, and a sidebar that fires a 403 on every load is a sidebar that fills
  // the log.
  const accounts = useQuery({
    queryKey: ['users', 'count'],
    queryFn: () => api.get<PageOf>('/users?limit=1'),
    enabled: canView || canManageUsers,
    // Counted to at most 10,000 (O-53); a change to accounts made here
    // invalidates ['users'] and asks again at once.
    staleTime: COUNT_STALE_MS,
  });

  // Whether a newer Pando is released (R-351), behind install.view like the
  // rest of the installation's state. The sidebar counts the versions behind.
  const updates = useUpdates(canView);
  const behind = updates.data?.available ? (updates.data.releases?.length ?? 0) : 0;

  // Apps only for somebody who administers one. A person who reached this
  // console for the API screen alone has no apps to manage, and a list of none
  // offering to add one they cannot create is a screen that answers 403.
  const items: SidebarItem[] = [];
  if (administrative) {
    items.push({ value: 'apps', label: 'Apps', trailing: <Badge>{appCount}</Badge> });
  }
  // Beside Apps: a request is about an app, and answering one is app work.
  if (canApproveAll || waiting.length > 0 || section === 'approvals') {
    items.push({ value: 'approvals', label: 'Approvals', trailing: <Badge count={waiting.length} /> });
  }
  // Reading accounts needs install.view; changing one needs
  // install.users.manage. Either is a reason to see the screen, and the screen
  // itself is read-only without the second.
  if (canView || canManageUsers) {
    items.push({
      value: 'accounts',
      label: 'Accounts',
      trailing: <Badge>{totalLabel(accounts.data)}</Badge>,
    });
  }
  // Groups and roles are the same verb pair as accounts, and a separate screen:
  // who someone is and what a role can do are different questions, and one
  // screen answering both is how an authorization model turns into a list of
  // people with special powers (R-078).
  if (canView || canManageUsers) items.push({ value: 'identity', label: 'Groups and roles' });
  // Identity providers are adapters (R-040): read with install.view, changed
  // with install.adapters.manage.
  if (canView) items.push({ value: 'sign-in', label: 'Sign-in' });
  // How the installation itself is set up and kept (issue #154). Each page
  // keeps the verb it had as a sidebar item of its own, and System is shown
  // only when one of them is, so nobody sees a screen here they could not see
  // before. The versions behind ride on System as well as on Overview, so
  // they are not hidden behind a click.
  const systemPagesShown = systemPages({
    view: canView,
    managePolicy: canManagePolicy,
    manageBackups: canManageBackups,
    manageCluster: canManageCluster,
  });
  if (systemPagesShown.length > 0) {
    items.push({ value: 'system', label: 'System', trailing: behind > 0 ? <Badge count={behind} /> : undefined });
  }
  if (canReadAudit) items.push({ value: 'audit', label: 'Audit log' });
  // Anybody who administers an app may subscribe to its events (R-368);
  // install.events.manage adds install-wide ones and everybody's.
  if (administrative || canManageEvents) items.push({ value: 'events', label: 'Events' });

  // Last, and for everyone. The API is the product (R-261) and an agent holding
  // a token is an ordinary principal (R-262), so the manual and the way to mint
  // a token are not administration — a developer with one app shared with them
  // needs both.
  items.push({ value: 'api', label: 'API and tools' });
  // Links, so each opens in a new tab as well as here.
  for (const item of items) item.href = format({ view: 'admin', section: item.value as Section });

  const nav = (
    <SidebarNav
      value={section}
      // One navigation, not two. setSection already drops the selected app,
      // and calling both pushed two history entries — so one Back went to a
      // URL that looked identical and nothing appeared to happen.
      onChange={(v) => {
        setMenuOpen(false);
        setSection(v as Section);
      }}
      // The logo goes home, as it does everywhere else. Home is the launcher
      // (R-264), the same place "Back to my apps" goes.
      header={
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <HomeLink onLeave={onLeave} />
          {/* Where the launcher has it: opposite the logo. Settings belong to
              the person, not to anything this sidebar administers, so they
              are not one of its items. The inbox is the person's too. */}
          <span style={{ display: 'inline-flex', gap: 'var(--space-1)' }}>
            <FeedbackButton />
            <InboxButton />
            <IconButton label="Settings" onClick={onSettings}>
              <Icon name="settings" size={16} />
            </IconButton>
          </span>
        </div>
      }
      items={items}
      footer={
        <Button variant="ghost" onClick={onLeave}>
          Back to my apps
        </Button>
      }
    />
  );

  return (
    // isolation makes this the stacking context, so the terrain's negative
    // z-index puts it above the paper and below everything else.
    <div
      style={{
        display: narrow ? 'block' : 'flex',
        minHeight: '100vh',
        background: 'var(--paper)',
        position: 'relative',
        isolation: 'isolate',
      }}
    >
      {/* A different map per screen; the tabs of one app share its map. */}
      <TopoBackground seed={`${section}/${selectedID ?? ''}`} />
      {narrow ? (
        <>
          {/* At phone width the sidebar would take most of the screen, so it
              becomes a menu behind a button, in a bar that keeps the two
              things the sidebar's header had: the way home and settings. */}
          <header
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              padding: 'var(--space-3) var(--console-padding)',
              borderBottom: 'var(--border-width) solid var(--rule)',
            }}
          >
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)' }}>
              <IconButton label="Menu" aria-expanded={menuOpen} onClick={() => setMenuOpen(true)}>
                <Icon name="menu" size={16} />
              </IconButton>
              <HomeLink onLeave={onLeave} />
            </span>
            <span style={{ display: 'inline-flex', gap: 'var(--space-1)' }}>
              <FeedbackButton />
              <InboxButton />
              <IconButton label="Settings" onClick={onSettings}>
                <Icon name="settings" size={16} />
              </IconButton>
            </span>
          </header>
          {menuOpen && (
            <div role="dialog" aria-modal="true" aria-label="Admin menu" style={{ position: 'fixed', inset: 0, zIndex: 20 }}>
              {/* The scrim: a click closes the menu, and Escape does for the keyboard. */}
              <div
                aria-hidden="true"
                onClick={() => setMenuOpen(false)}
                style={{ position: 'absolute', inset: 0, background: 'var(--scrim)' }}
              />
              <div
                style={{
                  position: 'absolute',
                  top: 0,
                  bottom: 0,
                  left: 0,
                  display: 'flex',
                  background: 'var(--paper)',
                  boxShadow: 'var(--shadow-popover)',
                }}
              >
                {nav}
              </div>
            </div>
          )}
        </>
      ) : (
        // The window's height, pinned while the page scrolls. Stretched to the
        // page's height instead, a long screen pushed "Back to my apps" at
        // the sidebar's foot below the fold. The items scroll within it if
        // they ever outgrow a short window.
        <div
          style={{
            position: 'sticky',
            top: 0,
            height: '100vh',
            flexShrink: 0,
            display: 'flex',
            overflowY: 'auto',
          }}
        >
          {nav}
        </div>
      )}

      <main style={{ flex: 1, minWidth: 0 }}>
        {section === 'accounts' &&
          (route.userID ? (
            <AccountPage
              userID={route.userID}
              onBack={() => go({ view: 'admin', section: 'accounts' })}
              onAudit={(query) => go({ view: 'admin', section: 'audit', query })}
              onGroups={() => go({ view: 'admin', section: 'identity' })}
              onOpenApp={(appID) => go({ view: 'admin', section: 'apps', appID })}
            />
          ) : (
            <Accounts onOpen={(a) => go({ view: 'admin', section: 'accounts', userID: a.id })} />
          ))}
        {section === 'identity' && route.groupID && <GroupPage groupID={route.groupID} canEdit={canManageUsers} go={go} />}
        {section === 'identity' && route.roleID && <RolePage roleID={route.roleID} canEdit={canManageUsers} go={go} />}
        {section === 'identity' && !route.groupID && !route.roleID && (
          <Identity
            canEdit={canManageUsers}
            tab={route.tab === 'roles' ? 'roles' : 'groups'}
            onTab={(tab) => go({ view: 'admin', section: 'identity', ...(tab === 'roles' ? { tab } : {}) }, true)}
            go={go}
          />
        )}
        {section === 'sign-in' && (
          <SignIn
            canEdit={canManageAdapters}
            canManagePolicy={canManagePolicy}
            query={route.query}
            onClearTest={() => go({ view: 'admin', section: 'sign-in' }, true)}
          />
        )}
        {section === 'system' && (
          <System route={route} go={go} pages={systemPagesShown} canManagePolicy={canManagePolicy} behind={behind} />
        )}
        {section === 'events' && <Events canManageAll={canManageEvents} apps={rows} />}
        {section === 'audit' && (
          <Audit
            // Filters carried in from a link, such as an account's page. Each
            // change writes the address back, so a reload or a copied link
            // shows the same events.
            initial={filtersFrom(route.query)}
            onFilters={(f) => {
              const query = linkQuery(f);
              go(query ? { view: 'admin', section: 'audit', query } : { view: 'admin', section: 'audit' }, true);
            }}
            onAdapters={canView ? () => go({ view: 'admin', section: 'system', tab: 'adapters', sub: 'audit_sink' }) : undefined}
            adaptersHref={format({ view: 'admin', section: 'system', tab: 'adapters', sub: 'audit_sink' })}
          />
        )}
        {section === 'api' && <Reference />}
        {section === 'approvals' && (
          <Approvals onReview={(appID, deployment) => go(panelRoute(appID, undefined, { view: 'deployments', deployment }))} />
        )}
        {section === 'apps' &&
          (selectedID ? (
            <AppScreen
              appID={selectedID}
              listed={rows.find((a) => a.id === selectedID)}
              tab={route.tab}
              page={route.page}
              onTab={(tab, page) => go({ view: 'admin', section: 'apps', appID: selectedID, tab, ...(page && { page }) }, true)}
              pageHref={(page) => format({ view: 'admin', section: 'apps', appID: selectedID, tab: 'settings', page })}
              panel={route.panel ? { view: route.panel, workload: route.workload, deployment: route.deployment } : undefined}
              onPanel={(target) => go(panelRoute(selectedID, route.tab, target, route.page), true)}
              panelHref={(target) => format(panelRoute(selectedID, route.tab, target, route.page))}
              onBack={() => setSelectedID(null)}
            />
          ) : (
            // Adding an app opens it. Detection is already running by the time
            // the request returns, and the next thing to do is look at what it
            // found — landing back on a list with a new row saying "draft"
            // leaves the person to work that out.
            <AppsList
              rows={rows}
              // Guarded by `administrative`: a query that is not enabled stays
              // pending forever, and would leave the list loading for good.
              loading={administrative && apps.query.isPending}
              query={appSearch}
              onQuery={setAppSearch}
              more={<ShowMore query={apps.query} label="Show more apps" />}
              onOpen={(app) => setSelectedID(app.id)}
              hrefOf={(app) => format({ view: 'admin', section: 'apps', appID: app.id })}
              onAdded={(app) => setSelectedID(app.id)}
            />
          ))}
      </main>
    </div>
  );
}

/** An app's screen with the side panel showing `target`, or closed. */
function panelRoute(appID: string, tab: string | undefined, target: PanelTarget | null, page?: SettingsPage): Route {
  const route: Route = { view: 'admin', section: 'apps', appID, tab, ...(page && { page }) };
  if (!target) return route;
  return { ...route, panel: target.view, workload: target.workload, deployment: target.deployment };
}

/** The logo, as a link home: the launcher (R-264). */
function HomeLink({ onLeave }: Readonly<{ onLeave: () => void }>) {
  return (
    <RouteLink href="/" onFollow={onLeave} title="Back to my apps" style={{ display: 'inline-flex' }}>
      <Logo size={20} />
    </RouteLink>
  );
}

function AppsList({
  rows,
  loading,
  query,
  onQuery,
  more,
  onOpen,
  hrefOf,
  onAdded,
}: {
  /** The pages read so far, already narrowed by the search. */
  rows: App[];
  /** The list has not arrived: placeholder rows, not "Add your first app". */
  loading: boolean;
  /** The search, by name or address slug. The server narrows the list:
   *  the app being looked for may not be on a page read yet. Narrowing by
   *  one column is the column filters' job, in the table's own headers. */
  query: string;
  onQuery: (query: string) => void;
  /** Reads the next page, while there is one. */
  more: React.ReactNode;
  onOpen: (app: App) => void;
  hrefOf: (app: App) => string;
  onAdded: (app: App) => void;
}) {
  const [adding, setAdding] = useState(false);
  const narrow = useNarrow();
  const shown = rows;

  // The page is not capped — a table's rows and rules run to the edge of the
  // window, which is what a wide display should look like. Its content is: the
  // columns are sized in `ch`, so Status and Updated sit next to the name
  // rather than two thousand pixels away from it.
  //
  // The action sits beside the heading rather than opposite it. Pushed to the
  // far end of a measure it is a long way from the word it belongs to, and on
  // a wide window the eye has to cross the whole page to find out what a screen
  // offers. Sheet does both, and frames the page as a survey sheet.
  return (
    <>
      <Sheet
        heading="Apps"
        action={
          <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
            <Button variant="primary" onClick={() => setAdding(true)}>
              Add app
            </Button>
            {(rows.length > 0 || query !== '') && <SearchField value={query} onChange={onQuery} placeholder="Search apps" />}
          </div>
        }
      >
        {/* Sideways rather than clipped, when the window is narrower than
            the columns' least widths: the table is as wide as those at the
            least, and this scrolls it. At phone width each row is a block
            instead, and nothing scrolls. */}
        <div style={narrow ? undefined : { overflowX: 'auto' }}>
        <Table
          style={narrow ? undefined : { minWidth: 'min-content' }}
          loading={loading}
          onRowClick={onOpen}
          rowHref={hrefOf}
          stacked={stackedRow}
          // A fresh install has no apps, and the list was a set of column
          // headers over nothing. R-002 is about the tenth app; the first one
          // is what makes the install anything at all.
          empty={
            query.trim() !== '' ? (
              <Quiet>No apps match &ldquo;{query.trim()}&rdquo;.</Quiet>
            ) : (
            <EmptyState
              heading="Add your first app"
              action={
                <Button variant="primary" onClick={() => setAdding(true)}>
                  Add app
                </Button>
              }
            >
              From a repository, an image or uploaded files.
            </EmptyState>
            )
          }
          columns={[
            {
              // The address under the name rather than in a column of its own:
              // an address is as long as a name, and two columns of them do not
              // fit beside the rest at a laptop's width (discussion #262).
              key: 'name',
              header: 'Name',
              width: 'minmax(24ch,1fr)',
              filter: 'text',
              // The search above the table narrows by name already.
              phoneFilter: false,
              render: (row: App) => <NameCell row={row} />,
            },
            {
              key: 'state',
              header: 'Status',
              width: '17ch',
              filter: 'values',
              filterValue: rowStatus,
              render: (row: App) => <StatusCell row={row} />,
            },
            {
              key: 'security_score',
              header: 'Security',
              width: '14ch',
              filter: 'values',
              filterValue: securityFilterValue,
              render: (row: App) => <SecurityCell row={row} />,
            },
            {
              // Who can open it without being given access by name — the
              // question an admin scans the list to answer (discussion #262).
              key: 'access',
              header: 'Access',
              width: '21ch',
              filter: 'values',
              filterValue: (row: App) => accessLabel(row.access),
              render: (row: App) => <AccessCell row={row} />,
            },
            { key: 'owner_name', header: 'Owner', width: 'minmax(12ch,18ch)', filter: 'values' },
            {
              key: 'updated_at',
              header: 'Updated',
              width: '12ch',
              muted: true,
              render: (row: App) => <UpdatedCell row={row} />,
            },
          ]}
          rows={shown}
        />
        </div>
        {more}
      </Sheet>

      {adding && (
        <AddApp
          onClose={() => setAdding(false)}
          onAdded={(app) => {
            setAdding(false);
            onAdded(app);
          }}
        />
      )}
    </>
  );
}

/**
 * Where each plan-time refusal is fixed.
 *
 * The same idea as the warnings on the overview: an error that says "choose how
 * to fill the DB_URL slot" on a screen with no slots on it is an error somebody
 * has to go looking for the answer to. A code with no entry keeps the plain
 * dismissal — nothing here claims to be actionable when it is not.
 */
const REFUSALS: Record<string, { label: string; tab: string; focus?: string }> = {
  PLAN_SLOT_UNFILLED: { label: 'Fill it in', tab: 'settings', focus: 'dependencies' },
  PLAN_CAPABILITY_UNSUPPORTED: { label: 'Open settings', tab: 'settings' },
  PLAN_SECURITY_BELOW_THRESHOLD: { label: 'See the findings', tab: 'overview', focus: 'security' },
  // R-183: the refusal names the entries; the app's egress section is where
  // they are taken out.
  PLAN_EGRESS_LOOSENING_FORBIDDEN: { label: 'Open egress', tab: 'settings', focus: 'egress' },
  VALID_PRIMARY_WORKLOAD: { label: 'Open configuration', tab: 'settings', focus: 'configuration' },
  VALID_DANGLING_MOUNT: { label: 'Open storage', tab: 'settings', focus: 'storage' },
  VALID_DANGLING_SLOT_REF: { label: 'Open dependencies', tab: 'settings', focus: 'dependencies' },
};

function AppScreen({
  appID,
  listed,
  tab: routeTab,
  page,
  onTab,
  pageHref,
  panel,
  onPanel,
  panelHref,
  onBack,
}: {
  appID: string;
  /** The row from the list, which is all that is left when the app's own
   *  record will not load. */
  listed?: App;
  tab?: string;
  /** The page of the Settings tab the address names. */
  page?: SettingsPage;
  onTab: (tab: string, page?: SettingsPage) => void;
  /** The address of a Settings page, for its link in the list. */
  pageHref?: (page: SettingsPage) => string;
  /** The side panel, when the address opens it. */
  panel?: PanelTarget;
  /** Opens the panel on a target, or closes it with null. */
  onPanel: (target: PanelTarget | null) => void;
  panelHref?: (target: PanelTarget) => string;
  onBack: () => void;
}) {
  // Read live, not handed down. Everything on this screen changes underneath
  // it: accepting a proposal pins a spec, deploying moves the app through
  // building to running. A snapshot taken when the row was clicked is wrong by
  // the time anything interesting has happened.
  //
  // It also carries what the caller may do here (`verbs`), which is what every
  // control below is shown or hidden on. Read once, for the whole screen.
  const app = useQuery({
    queryKey: ['apps', appID],
    queryFn: () => api.get<AppWithVerbs>(`/apps/${appID}`),
  });
  // The record is read again when the app's status moves on, rather than
  // polled: a deploy finishing is seen within a status poll on every tab.
  useRecordFollowsStatus(app.data, app.dataUpdatedAt);

  // An app with no pinned spec has never been through review, so detection is
  // the only thing worth showing it.
  const reviewed = Boolean(app.data?.pinned_spec_id);

  // The tab is in the address bar, so a reload comes back to it. Replace
  // rather than push when switching: flicking between tabs should not make the
  // back button walk them one at a time before leaving the app.
  const wanted = routeTab ?? 'overview';

  // Which section of a tab to open at, when something sent you there. A
  // warning about storage should land on storage, not on the top of a settings
  // tab with four sections above it. Cleared by any ordinary tab click, so it
  // only ever applies to the trip it was set for.
  const [focus, setFocus] = useState<string | undefined>(undefined);

  // A refused deploy, rendered under the header rather than inside it: the
  // message is a sentence or three, and a paragraph in a row of buttons moves
  // the buttons.
  const [refusal, setRefusal] = useState<{ message: string; remedy?: string; code?: string } | null>(
    null,
  );
  // A deploy that was accepted and is waiting for approval (R-154). Said where
  // it was started, because the button would otherwise go quiet and the app
  // would look as if nothing happened — or, worse, as if it were deploying.
  const [waiting, setWaiting] = useState(false);
  // On Settings, where somebody was sent is a page of its own, in the
  // address rather than a section to scroll to.
  const setTab = (next: string, at?: string) => {
    const settingsPage = next === 'settings' ? settingsPageFor(at) : undefined;
    setFocus(next === 'settings' ? undefined : at);
    onTab(next, settingsPage);
  };

  // Accepting a proposal is the moment `reviewed` flips, and the onboarding
  // page gives way to the tabs. Whatever tab an old link named, the thing
  // somebody came for next is on Overview: the deploy. Only on that
  // transition — an app that loaded already reviewed stays on the tab its
  // address names.
  const onboarding = useRef(false);
  useEffect(() => {
    if (app.data && !reviewed) onboarding.current = true;
    if (reviewed && onboarding.current) {
      onboarding.current = false;
      if (routeTab && routeTab !== 'overview') onTab('overview');
    }
  }, [reviewed, app.data]);

  // The header's shape while the record loads — the back button is real, since
  // it needs nothing from the app — rather than a blank page. The list's row
  // supplies the name when there is one, so the heading does not change as the
  // record arrives.
  if (app.isPending) {
    return (
      <Sheet
        heading={
          <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
            <Button variant="ghost" icon={<Icon name="arrow-left" />} onClick={onBack}>
              Apps
            </Button>
            <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
              {listed ? <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>{listed.name}</h3> : <HeadingSkeleton />}
              <LineSkeleton width="10ch" />
            </div>
          </div>
        }
      >
        {/* The tab row, then a card: what every tab opens with. */}
        <Loading gap="var(--space-5)">
          <FieldSkeleton width="40ch" />
          <div style={{ maxWidth: MEASURE }}>
            <Skeleton height="12rem" radius="md" />
          </div>
        </Loading>
      </Sheet>
    );
  }
  if (app.isError || !app.data) {
    // An app whose record will not load is exactly the app somebody is trying
    // to get rid of, and this screen used to offer them a back button and
    // nothing else — no name, no reason, no way out but the list they came
    // from. The list's own row carries the name, so the delete still knows what
    // it is about.
    //
    // Not the 404 contour figure. That figure means "the thing you came for is
    // not here", and this app is in the list — its record failed to load, and
    // the server's own message says why (R-105). A guessed "may have been
    // deleted" would be wrong about the one app it is shown for.
    return (
      <Sheet>
        <div
          style={{
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'flex-start',
            gap: 'var(--space-4)',
          }}
        >
          <Button variant="ghost" icon={<Icon name="arrow-left" />} onClick={onBack}>
            Apps
          </Button>
          {listed && <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>{listed.name}</h3>}
          <Banner tone="failed">{messageOf(app.error)}</Banner>
          <DeleteApp appID={appID} appName={listed?.name ?? 'this app'} onDeleted={onBack} />
        </div>
      </Sheet>
    );
  }

  // What the caller may do on this app, by the server's own answer. Every
  // control on this screen asks about exactly the verb its endpoint checks,
  // so a person who can only look at an app sees it read-only rather than as
  // a set of buttons that each come back refused (R-261).
  const verbs = app.data.verbs ?? [];

  // An app nobody has accepted yet gets the onboarding page instead of the
  // tabs: the review of what Pando found, and the choice to keep the app or
  // reject it. Same address. When it is accepted `reviewed` flips and this
  // screen takes over — carrying any refusal of the deploy that came with it,
  // which is why the refusal is set from there.
  if (!reviewed) {
    return (
      <AppVerbs.Provider value={verbs}>
        <AppOnboarding app={app.data} onBack={onBack} onDeployRefused={setRefusal} />
      </AppVerbs.Provider>
    );
  }

  // Every tab stays, read-only where this person cannot change anything:
  // Settings and Sharing are also where somebody finds out how the app is
  // configured and who can reach it. Logs and Terminal are not tabs; they are
  // the side panel (AppPanel), opened from the Overview.
  const tabs: TabItem[] = [
    { value: 'overview', label: 'Overview' },
    // Anyone who can see the app reads its events and may subscribe (R-378).
    { value: 'events', label: 'Events' },
    { value: 'sharing', label: 'Sharing' },
    { value: 'settings', label: 'Settings' },
  ];

  for (const t of tabs) t.href = format({ view: 'admin', section: 'apps', appID, tab: t.value });

  // A link to a tab that does not exist lands on the Overview.
  const tab = tabs.some((t) => t.value === wanted) ? wanted : 'overview';

  return (
    <AppVerbs.Provider value={verbs}>
      <PanelOpener value={onPanel}>
      <Sheet
        heading={
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              alignItems: 'flex-start',
              gap: 'var(--space-3)',
            }}
          >
            <Button variant="ghost" icon={<Icon name="arrow-left" />} onClick={onBack} style={{ alignSelf: 'flex-start' }}>
              Apps
            </Button>
            <div
              style={{
                display: 'flex',
                justifyContent: 'space-between',
                alignItems: 'flex-start',
                flexWrap: 'wrap',
                gap: 'var(--space-4)',
                width: '100%',
              }}
            >
              {/* What the app is: its tile, its name, how it is, its score,
                  and where it is reached. */}
              <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-4)', minWidth: 0 }}>
                <AppImage app={app.data} />
                <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', minWidth: 0 }}>
                  <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
                    <AppName app={app.data} />
                    <StatusIndicator
                      status={statusSymbol(app.data.state)}
                      label={statusLabel(app.data.state, app.data.stopped_for_idle, app.data.stopped_for_disk, app.data.stopped_for_orphan)}
                    />
                    <HeaderScore
                      appID={app.data.id}
                      onOpen={() => {
                        if (tab === 'overview') scrollToSecurity();
                        else setTab('overview', 'security');
                      }}
                    />
                  </div>
                  <AppAddress app={app.data} />
                </div>
              </div>
              <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
                {/* Deploy where somebody looking at an app can reach it, from any
                    tab, rather than under everything on the overview. Only for an
                    app that has a configuration to deploy: before that, the thing
                    to do is accept one. */}
                {app.data.pinned_spec_id && can(verbs, AppVerb.Deploy) && (
                  <DeployButton
                    app={app.data}
                    onRefused={(message, remedy, code) => {
                      setRefusal(message ? { message, remedy, code } : null);
                      if (message) setWaiting(false);
                    }}
                    onWaiting={() => setWaiting(true)}
                  />
                )}

                {/* Stopping is the thing somebody reaches for when they would
                    otherwise delete: it keeps the storage, the configuration and
                    the address, and starting brings back what was running. */}
                {can(verbs, AppVerb.Restart) && <Lifecycle app={app.data} />}

                {/* In the header rather than on Settings: an app whose source could
                    not be fetched has no pinned spec and therefore no Settings tab,
                    and that is the app most likely to be deleted. */}
                {can(verbs, AppVerb.Delete) && (
                  <DeleteApp appID={app.data.id} appName={app.data.name} onDeleted={onBack} />
                )}
              </div>
            </div>
          </div>
        }
        // The sheet's marginal data. An app's id is what the CLI and the API want
        // and the console had nowhere to show it, so it was a value you could
        // only get by reading the address bar.
        note={app.data.id}
      >
        {/* Sheet provides the page inset, so this carries only the measure and
            the gap above the tabs. */}
        {refusal && (
          <div style={{ paddingBottom: 'var(--space-4)', maxWidth: MEASURE }}>
            {/* The server's words, which are written to be acted on (R-105) —
                and, where the console has the screen that acts on them, the way
                there. A refusal that names a remedy on a page with no control
                for it is a remedy nobody can take. */}
            <Banner
              tone="failed"
              action={
                REFUSALS[refusal.code ?? ''] ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      const fix = REFUSALS[refusal.code ?? ''];
                      if (fix) setTab(fix.tab, fix.focus);
                      setRefusal(null);
                    }}
                  >
                    {REFUSALS[refusal.code ?? '']?.label}
                  </Button>
                ) : (
                  <Button variant="ghost" onClick={() => setRefusal(null)}>
                    Dismiss
                  </Button>
                )
              }
            >
              <WithCode code={refusal.code}>
                {refusal.message}
                {refusal.remedy ? ` ${refusal.remedy}` : ''}
              </WithCode>
            </Banner>
          </div>
        )}

        {waiting && (
          <div style={{ paddingBottom: 'var(--space-4)', maxWidth: MEASURE }}>
            <Banner
              tone="info"
              action={
                tab === 'overview' ? (
                  <Button variant="ghost" onClick={() => setWaiting(false)}>
                    Dismiss
                  </Button>
                ) : (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setTab('overview');
                      setWaiting(false);
                    }}
                  >
                    See the request
                  </Button>
                )
              }
            >
              This deploy is waiting for approval.
            </Banner>
          </div>
        )}

        {/* Once, rather than on every section: without a verb that changes
            anything, every control below is absent, and this says why. */}
        {!changesAnything(verbs) && (
          <div style={{ paddingBottom: 'var(--space-4)', maxWidth: MEASURE }}>
            <Quiet>You can view this app but not change it.</Quiet>
          </div>
        )}

        <Tabs value={tab} onChange={setTab} items={tabs} />

        <div style={{ paddingTop: 'var(--space-5)' }}>
          {tab === 'sharing' && <Sharing appID={app.data.id} appName={app.data.name} />}
          {tab === 'overview' && (
            <AppOverview
              app={app.data}
              onGo={setTab}
              focus={focus}
              onRefused={(message, remedy, code) => {
                setRefusal(message ? { message, remedy, code } : null);
                if (message) setWaiting(false);
              }}
              onWaiting={() => setWaiting(true)}
            />
          )}
          {tab === 'events' && <AppEvents app={app.data} />}
          {tab === 'settings' && (
            <AppSettings app={app.data} page={page ?? 'general'} onPage={(p) => onTab('settings', p)} hrefOf={pageHref} />
          )}
        </div>
      </Sheet>
      {panel && (
        <AppPanel
          app={app.data}
          verbs={verbs}
          target={panel}
          onChange={onPanel}
          onClose={() => onPanel(null)}
          hrefOf={panelHref}
        />
      )}
      </PanelOpener>
    </AppVerbs.Provider>
  );
}

function Quiet({ children }: { children: React.ReactNode }) {
  return <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>{children}</p>;
}
