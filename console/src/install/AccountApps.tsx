// The apps an account or a group has something on: its role for managing each,
// whether it can open each, and — where the viewer may change that — the
// controls to change it. One component for both, because a grant to a group is
// the same grant as one to a person (R-078); only who holds it differs.
//
// Editable only where the server says so. `can_manage` on each row is the
// authorizer's own answer for app.grants.manage on that app (GET
// /users/{id}/apps, GET /groups/{id}/apps), so a row the viewer cannot change
// is shown as text rather than as a control that would be refused. On an
// account's page, access through a group is always text: it belongs to the
// group, and changing it would change it for everyone in it.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, Dialog, EmptyState, Select, Tag } from '@design';

import { api } from '@api/client';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { Table } from '../ui/Table';
import { FieldSkeleton, LineSkeleton, Loading } from '../ui/Loading';
import { SearchField } from '../ui/SearchField';
import { ShowMore, usePaged, useSettled, withParams, type PageOf } from '../ui/paged';
import type { Role } from './Accounts';
import { Quiet, messageOf, sentence } from './Accounts';

/** Who the grants are for. */
export interface Principal {
  kind: 'user' | 'group';
  id: string;
  /** How the principal is named in sentences. */
  name: string;
}

interface Grant {
  grant_id: string;
  plane: 'control' | 'data';
  role_id?: string;
  role_name?: string;
  via: 'user' | 'group';
  group_id?: string;
  group_name?: string;
}

interface AppAccess {
  app_id: string;
  app_name: string;
  owner: boolean;
  can_manage: boolean;
  control: Grant[];
  data: Grant[];
}

const listKey = (p: Principal) => [p.kind === 'user' ? 'users' : 'groups', p.id, 'apps'];

/** The principal's own grant, as opposed to one an account holds through a
 *  group. Every grant in a group's list is the group's own. */
/** Apps offered at once in the picker; a search finds the rest. */
const PICK_LIMIT = 50;

const isOwn = (p: Principal) => (g: Grant) => p.kind === 'group' || g.via === 'user';

export function AccountApps({
  principal,
  heading = 'Apps',
  action,
}: {
  principal: Principal;
  heading?: string;
  /** Beside "Give access to an app", for the surface the list sits in. */
  action?: React.ReactNode;
}) {
  const noun = principal.kind === 'user' ? 'account' : 'group';
  // A page of apps at a time (issue #72): a group everyone is in can be
  // granted every app in the install.
  const paged = usePaged<{ apps: AppAccess[] | null } & PageOf, AppAccess>({
    key: listKey(principal),
    path: `/${principal.kind === 'user' ? 'users' : 'groups'}/${principal.id}/apps`,
    rows: (p) => p.apps,
  });
  const access = paged.query;
  // The roles an app can be granted with: the three built-ins and any custom
  // app role.
  const roles = useQuery({
    queryKey: ['roles', 'app'],
    queryFn: () => api.get<{ roles: Role[] }>('/roles?scope=app'),
  });
  // Someone who sees every app and manages access to every app
  // (install.apps.view and install.apps.grants.manage) can be offered an app
  // the principal does not have yet. Anyone else manages the apps they were
  // granted, from each app's Sharing tab.
  const seesEveryApp = useInstallVerb(InstallVerb.AppsView);
  const sharesEveryApp = useInstallVerb(InstallVerb.AppsGrantsManage);
  const everyApp = seesEveryApp && sharesEveryApp;
  const [adding, setAdding] = useState(false);

  const rows = paged.rows;

  return (
    <section>
      <div
        style={{
          display: 'flex',
          flexWrap: 'wrap',
          alignItems: 'flex-end',
          justifyContent: 'space-between',
          gap: 'var(--space-3) var(--space-4)',
          marginBottom: 'var(--space-4)',
        }}
      >
        <div>
          <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>{heading}</h4>
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            {principal.kind === 'user'
              ? 'The role this account manages each app with, and whether it can open it.'
              : 'The role everyone in this group manages each app with, and whether they can open it.'}
          </p>
        </div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
          {everyApp && (
            <Button variant="secondary" onClick={() => setAdding(true)}>
              Give access to an app
            </Button>
          )}
          {action}
        </div>
      </div>

      {access.isError && <Quiet>{messageOf(access.error)}</Quiet>}

      <Table
        loading={access.isPending}
        skeletonRows={3}
        columns={[
          {
            key: 'app_name',
            header: 'App',
            width: 'minmax(0,1fr)',
            filter: 'text',
            render: (row: AppAccess) => (
              <span style={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2)' }}>
                {row.app_name}
                {row.owner && <Tag>Owner of record</Tag>}
              </span>
            ),
          },
          {
            key: 'role',
            header: 'Role',
            width: 'minmax(0,26ch)',
            render: (row: AppAccess) => (
              <div style={{ padding: 'var(--space-2) 0' }}>
                <RoleCell principal={principal} row={row} roles={roles.data?.roles ?? []} />
              </div>
            ),
          },
          {
            key: 'use',
            header: 'Can open',
            width: '16ch',
            render: (row: AppAccess) => <UseCell principal={principal} row={row} />,
          },
        ]}
        rows={rows.map((r) => ({ ...r, id: r.app_id }))}
        empty={
          <EmptyState heading="No apps">
            {everyApp
              ? `This ${noun} has no access to any app. Give it access to one above.`
              : `This ${noun} has no access to any app you can see.`}
          </EmptyState>
        }
      />
      <ShowMore query={access} label="Show more apps" />

      {adding && (
        <GiveAccess
          principal={principal}
          has={new Set(rows.map((r) => r.app_id))}
          roles={roles.data?.roles ?? []}
          onClose={() => setAdding(false)}
        />
      )}
    </section>
  );
}

/** The principal's own grant for managing the app, editable if the viewer may;
 *  and, for an account, any role it holds through a group, as text. */
function RoleCell({ principal, row, roles }: { principal: Principal; row: AppAccess; roles: Role[] }) {
  const own = row.control.find(isOwn(principal));
  const viaGroups = principal.kind === 'user' ? row.control.filter((g) => g.via === 'group') : [];
  const change = useGrantChange(principal, row.app_id);
  const nameOf = (g: Grant) => sentence(g.role_name || roles.find((r) => r.id === g.role_id)?.name || g.role_id || '');

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      {row.can_manage ? (
        <Select
          aria-label={`Role on ${row.app_name}`}
          value={own?.role_id ?? ''}
          disabled={change.isPending}
          onChange={(e) => {
            const role = e.target.value;
            if (!role && own) change.mutate({ method: 'delete', grant: own.grant_id });
            else if (role && own) change.mutate({ method: 'patch', grant: own.grant_id, role });
            else if (role) change.mutate({ method: 'post', plane: 'control', role });
          }}
          options={[{ value: '', label: 'None' }, ...roles.map((r) => ({ value: r.id, label: sentence(r.name) }))]}
          helper={change.isError ? messageOf(change.error) : undefined}
        />
      ) : (
        <span>{own ? nameOf(own) : viaGroups.length > 0 ? '' : 'None'}</span>
      )}
      {viaGroups.map((g) => (
        <span key={g.grant_id} style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          {nameOf(g)} through {g.group_name || g.group_id}
        </span>
      ))}
    </div>
  );
}

function UseCell({ principal, row }: { principal: Principal; row: AppAccess }) {
  const own = row.data.find(isOwn(principal));
  const viaGroup = principal.kind === 'user' ? row.data.find((g) => g.via === 'group') : undefined;
  const change = useGrantChange(principal, row.app_id);
  // An owner opens the app by owning it (R-072), grant or no grant. A group is
  // never an owner.
  const opens = Boolean(own || viaGroup || row.owner);

  if (!row.can_manage || row.owner) {
    return (
      <span style={{ color: opens ? undefined : 'var(--ink-tertiary)' }}>
        {opens ? (row.owner && !own ? 'Yes, as owner' : 'Yes') : 'No'}
      </span>
    );
  }
  return (
    <Checkbox
      aria-label={`Can open ${row.app_name}`}
      label={viaGroup && !own ? 'Through a group' : undefined}
      checked={Boolean(own || viaGroup)}
      disabled={change.isPending || Boolean(viaGroup && !own)}
      onChange={(e) => {
        if (e.target.checked) change.mutate({ method: 'post', plane: 'data' });
        else if (own) change.mutate({ method: 'delete', grant: own.grant_id });
      }}
    />
  );
}

type Change =
  | { method: 'post'; plane: 'control' | 'data'; role?: string }
  | { method: 'patch'; grant: string; role: string }
  | { method: 'delete'; grant: string };

function useGrantChange(principal: Principal, appID: string) {
  const invalidate = useInvalidate(principal, appID);
  return useMutation({
    mutationFn: (c: Change) => {
      const base = `/apps/${appID}/grants`;
      if (c.method === 'post') {
        return api.post<unknown>(base, {
          plane: c.plane,
          principal_kind: principal.kind,
          principal_id: principal.id,
          ...(c.role ? { role_id: c.role } : {}),
        });
      }
      if (c.method === 'patch') return api.patch<unknown>(`${base}/${c.grant}`, { role_id: c.role });
      return api.del<unknown>(`${base}/${c.grant}`);
    },
    onSettled: invalidate,
  });
}

function useInvalidate(principal: Principal, appID?: string) {
  const queries = useQueryClient();
  return () => {
    void queries.invalidateQueries({ queryKey: listKey(principal) });
    // A group's grant is every member's access, so each member's own list of
    // apps is stale too.
    if (principal.kind === 'group') void queries.invalidateQueries({ queryKey: ['users'] });
    if (appID) void queries.invalidateQueries({ queryKey: ['apps', appID] });
  };
}

/** Access to an app the principal has nothing on yet — for someone who manages
 *  every app, so every app offered is one they can grant. */
function GiveAccess({
  principal,
  has,
  roles,
  onClose,
}: {
  principal: Principal;
  has: Set<string>;
  roles: Role[];
  onClose: () => void;
}) {
  // Searched by the server, a screenful at a time: the install may hold
  // twenty thousand apps, and a picker of all of them is not one (issue #72).
  const [search, setSearch] = useState('');
  const settled = useSettled(search.trim());
  const apps = useQuery({
    queryKey: ['apps', 'pick', settled],
    queryFn: () =>
      api.get<{ apps: { id: string; name: string }[] | null; next_cursor?: string }>(
        withParams('/apps', { q: settled, limit: PICK_LIMIT }),
      ),
    placeholderData: (previous) => previous,
  });
  const offered = (apps.data?.apps ?? []).filter((a) => !has.has(a.id));
  const more = Boolean(apps.data?.next_cursor);
  const [appID, setAppID] = useState('');
  const [role, setRole] = useState('');
  const [opens, setOpens] = useState(true);
  const invalidate = useInvalidate(principal);
  const noun = principal.kind === 'user' ? 'account' : 'group';

  const give = useMutation({
    mutationFn: async () => {
      const base = `/apps/${appID}/grants`;
      const who = { principal_kind: principal.kind, principal_id: principal.id };
      if (role) await api.post<unknown>(base, { plane: 'control', role_id: role, ...who });
      if (opens) await api.post<unknown>(base, { plane: 'data', ...who });
    },
    onSettled: invalidate,
    onSuccess: onClose,
  });

  return (
    <Dialog
      open
      title="Give access to an app"
      description={
        principal.kind === 'user'
          ? `What ${principal.name} can do on one more app.`
          : `What everyone in ${principal.name} can do on one more app.`
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={give.isPending || !appID || (!role && !opens)}
            onClick={() => give.mutate()}
          >
            {give.isPending ? 'Giving access' : 'Give access'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {apps.isPending ? (
          // The field in outline: a picker with only "Choose an app" in it
          // reads as a list of no apps.
          <Loading gap="var(--space-2)">
            <LineSkeleton width="6ch" font="var(--type-label)" />
            <FieldSkeleton />
          </Loading>
        ) : offered.length === 0 && settled === '' && !more ? (
          <Quiet>This {noun} already has access to every app.</Quiet>
        ) : (
          <>
            <SearchField value={search} onChange={setSearch} placeholder="Search apps" width="100%" />
            {offered.length === 0 ? (
              <Quiet>No apps match &ldquo;{settled}&rdquo; that this {noun} doesn&rsquo;t already have.</Quiet>
            ) : (
              <Select
                label="App"
                value={appID}
                onChange={(e) => setAppID(e.target.value)}
                helper={more ? `The first ${PICK_LIMIT} apps. Search to find another.` : undefined}
                options={[{ value: '', label: 'Choose an app' }, ...offered.map((a) => ({ value: a.id, label: a.name }))]}
              />
            )}
          </>
        )}
        <Select
          label="Role"
          value={role}
          onChange={(e) => setRole(e.target.value)}
          helper={`What the ${noun} can change on the app. None to only let it open the app.`}
          options={[{ value: '', label: 'None' }, ...roles.map((r) => ({ value: r.id, label: sentence(r.name) }))]}
        />
        <Checkbox label="Can open the app" checked={opens} onChange={(e) => setOpens(e.target.checked)} />
        {give.isError && <Banner tone="failed">{messageOf(give.error)}</Banner>}
      </div>
    </Dialog>
  );
}
