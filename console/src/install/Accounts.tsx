// Accounts — the screen behind install.users.manage and install.view.
//
// This is where an administrator hands over. Until it existed, the only account
// that could administer a Pando install was the one bootstrap created on first
// run: the verbs existed, the role existed, and nothing could write a second
// grant. An install had exactly one administrator forever.
//
// The list is read-only: who each account is, whether it can sign in, and its
// installation role. A row opens the account's own page (Account.tsx), which
// holds the actions — two separate ones, because they are two separate powers.
// Suspending an account stops it signing in (R-049); changing its role changes
// what it may do. The API keeps them on different routes and different verbs,
// and the console does not merge them back together.
//
// Nobody types a password for somebody else. Adding an account shows one Pando
// generated (GeneratedPassword.tsx), for the administrator to pass on.

import { useContext, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Input, Select, StatusIndicator, Tabs, Tag } from '@design';

import { api, RequestFailed } from '@api/client';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { GeneratedPassword } from './GeneratedPassword';
import { SystemTabs } from './systemTabs';
import { Sheet } from '../ui/Sheet';
import { SearchField } from '../ui/SearchField';
import { ShowMore, usePaged, useSettled, type PageOf } from '../ui/paged';
import { Table } from '../ui/Table';

export interface Account {
  id: string;
  adapter_id: string;
  external_id: string;
  email?: string;
  display_name?: string;
  status: string;
  must_change_password: boolean;
  install_role_id: string;
  created_at?: string;
  /** Set on an account whose sign-in identities were moved to another (O-1). */
  alias_of?: string;
}

export interface Role {
  id: string;
  name: string;
  builtin: boolean;
  verbs: string[];
}

export function Accounts({ onOpen }: { onOpen: (account: Account) => void }) {
  const manage = useInstallVerb(InstallVerb.UsersManage);
  const [adding, setAdding] = useState(false);
  const [query, setQuery] = useState('');

  // A page at a time, searched by the server — username, name or email — so
  // the account being looked for is found whichever page it would be on
  // (issue #72). Status and role narrow in the table's own column filters.
  const accounts = usePaged<{ users: Account[] | null } & PageOf, Account>({
    key: ['users', 'list'],
    path: '/users',
    rows: (p) => p.users,
    search: useSettled(query.trim()),
  });
  const roles = useQuery({
    queryKey: ['roles'],
    queryFn: () => api.get<{ roles: Role[] }>('/roles'),
  });

  const rows = accounts.rows;

  return (
    <Screen
      heading="Accounts"
      action={
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
          {manage && (
            <Button variant="primary" onClick={() => setAdding(true)}>
              Add account
            </Button>
          )}
          {(rows.length > 0 || query !== '') && <SearchField value={query} onChange={setQuery} placeholder="Search accounts" />}

        </div>
      }
    >
      {accounts.query.isError && <Quiet>{messageOf(accounts.query.error)}</Quiet>}

      <Table
        loading={accounts.query.isPending}
        onRowClick={onOpen}
        columns={[
          { key: 'external_id', header: 'Username', width: 'minmax(0,22ch)', mono: true, filter: 'text' },
          {
            key: 'display_name',
            header: 'Name',
            width: 'minmax(0,22ch)',
            filter: 'text',
            render: (row: Account) => row.display_name || '—',
          },
          {
            key: 'email',
            header: 'Email',
            width: 'minmax(0,28ch)',
            muted: true,
            filter: 'text',
            render: (row: Account) => row.email || '—',
          },
          {
            key: 'status',
            header: 'Status',
            width: '14ch',
            filter: 'values',
            filterValue: (row: Account) => (row.status === 'active' ? 'Active' : 'Suspended'),
            render: (row: Account) => (
              <StatusIndicator
                // Suspended is a stopped account, not a failed one. Marker red
                // is for failure and destruction, and an account somebody
                // deliberately suspended is neither.
                status={row.status === 'active' ? 'running' : 'stopped'}
                label={row.status === 'active' ? 'Active' : 'Suspended'}
              />
            ),
          },
          {
            key: 'install_role_id',
            header: 'Role',
            width: 'minmax(0,20ch)',
            filter: 'values',
            filterValue: (row: Account) => {
              const r = roles.data?.roles.find((x) => x.id === row.install_role_id);
              return r ? sentence(r.name) : 'None';
            },
            // Read-only here; it changes on the account's page.
            render: (row: Account) => <RoleLabel roleID={row.install_role_id} roles={roles.data?.roles ?? []} />,
          },
        ]}
        rows={rows}
        empty={query.trim() !== '' ? <Quiet>No accounts match &ldquo;{query.trim()}&rdquo;.</Quiet> : undefined}
      />
      <ShowMore query={accounts.query} label="Show more accounts" />

      {adding && <AddAccount onClose={() => setAdding(false)} />}
    </Screen>
  );
}

export function RoleLabel({ roleID, roles }: { roleID: string; roles: Role[] }) {
  if (!roleID) return <span style={{ color: 'var(--ink-tertiary)' }}>None</span>;
  const role = roles.find((r) => r.id === roleID);
  return <Tag>{role ? sentence(role.name) : roleID}</Tag>;
}

export function RolePicker({
  account,
  roles,
  isSelf,
}: {
  account: Account;
  roles: Role[];
  isSelf: boolean;
}) {
  const queries = useQueryClient();
  const [error, setError] = useState<string>();

  const change = useMutation({
    mutationFn: (roleID: string) =>
      roleID === ''
        ? api.del<void>(`/users/${account.id}/role`)
        : api.put<unknown>(`/users/${account.id}/role`, { role_id: roleID }),
    onSuccess: () => {
      setError(undefined);
      void queries.invalidateQueries({ queryKey: ['users'] });
      // Your own verbs may have just changed, and the sidebar reads them.
      void queries.invalidateQueries({ queryKey: ['me'] });
    },
    // The server's refusal, shown as written — "this is the only account that
    // can manage accounts" is exactly what the person needs to read.
    onError: (e) => setError(messageOf(e)),
  });

  return (
    <Select
      value={account.install_role_id}
      disabled={change.isPending}
      onChange={(e) => change.mutate(e.target.value)}
      options={[
        { value: '', label: isSelf ? 'None — give up admin' : 'None' },
        ...roles.map((r) => ({ value: r.id, label: sentence(r.name) })),
      ]}
      helper={error}
    />
  );
}

export function StatusToggle({ account }: { account: Account }) {
  const queries = useQueryClient();
  const suspended = account.status !== 'active';

  const set = useMutation({
    mutationFn: (status: string) => api.patch<void>(`/users/${account.id}`, { status }),
    onSuccess: () => void queries.invalidateQueries({ queryKey: ['users'] }),
  });

  // Suspending ends every session the account holds, immediately (R-048). That
  // is worth saying before it happens rather than after.
  return (
    <Button
      variant="secondary"
      disabled={set.isPending}
      onClick={() => set.mutate(suspended ? 'active' : 'suspended')}
    >
      {suspended ? 'Reinstate' : 'Suspend'}
    </Button>
  );
}

function AddAccount({ onClose }: { onClose: () => void }) {
  const [username, setUsername] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [password, setPassword] = useState('');
  const [mustChange, setMustChange] = useState(true);
  const queries = useQueryClient();

  const create = useMutation({
    mutationFn: () =>
      api.post<unknown>('/users', {
        username,
        password,
        display_name: displayName,
        must_change_password: mustChange,
      }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['users'] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      title="Add account"
      // The password is only ever on this screen: Pando cannot show it again
      // and sends it nowhere, so handing it over is the administrator's job.
      description="A local account with a password Pando generates. Copy the password and give it to the person separately; Pando does not send it to them."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={create.isPending || username === '' || password === ''}
            onClick={() => create.mutate()}
          >
            {create.isPending ? 'Adding' : 'Add account'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <Input
          label="Username"
          value={username}
          onChange={(e) => {
            // A refusal ("that username is taken") belongs to the value that
            // caused it.
            if (create.isError) create.reset();
            setUsername(e.target.value);
          }}
        />
        <Input
          label="Name"
          value={displayName}
          helper="Shown in the console and in the audit log."
          onChange={(e) => setDisplayName(e.target.value)}
        />
        <GeneratedPassword
          value={password}
          onChange={setPassword}
          mustChange={mustChange}
          onMustChange={setMustChange}
        />
        {create.isError && <Banner tone="failed">{messageOf(create.error)}</Banner>}
      </div>
    </Dialog>
  );
}

/** Every install screen is one sheet. The frame lives in Sheet. */
export function Screen({
  heading,
  action,
  children,
}: {
  heading: string;
  action?: React.ReactNode;
  children: React.ReactNode;
}) {
  // Left-aligned and uncapped: the rules under a table run to the window's
  // edge, and the screen's action sits beside its heading rather than at the
  // far end of a measure. Sheet keeps both and adds the neatline.
  const system = useContext(SystemTabs);
  if (system) {
    // A tab of System: System's heading, this screen's action, then the tabs
    // — spaced as an app's tabs are.
    return (
      <Sheet heading="System" action={action}>
        <Tabs value={system.value} onChange={system.onChange} items={system.items} />
        <div style={{ paddingTop: 'var(--space-5)' }}>{children}</div>
      </Sheet>
    );
  }
  return (
    <Sheet heading={heading} action={action}>
      {children}
    </Sheet>
  );
}

export function Quiet({ children }: { children: React.ReactNode }) {
  return <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{children}</p>;
}

export function messageOf(error: unknown): string {
  if (error instanceof RequestFailed) return error.message;
  return 'Pando could not reach the server. Reload the page to try again.';
}

/** The server's message and, when it gave one, its remedy (R-105). Most
 *  refusals on these screens are the last-administrator rule (R-088), and the
 *  remedy is the way through it. */
export function refusal(error: unknown): string {
  const remedy = error instanceof RequestFailed ? error.remedy : undefined;
  return remedy ? `${messageOf(error)} ${remedy}` : messageOf(error);
}

/** Role names are stored lowercase; the design system sets everything in
 *  sentence case. */
export function sentence(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
