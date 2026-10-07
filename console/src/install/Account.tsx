// One account: who it is, what it can do here, the actions on it, and its
// history — every audit event where it is the actor or the target — with a
// link to the Audit log narrowed to it, for anything deeper.
//
// The actions are the ones Accounts used to put on every row. They are here
// now so that the list reads as a list, and changing someone's role or
// suspending them is something done on purpose, looking at who they are.
// Resetting a local account's password is here for the same reason.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, EmptyState, Icon, IconButton, Input, Select, StatusIndicator, Tag } from '@design';

import { api } from '@api/client';
import { InstallVerb, useInstallVerb, usePrincipal } from '../app/principal';
import { Sheet } from '../ui/Sheet';
import { FieldSkeleton, HeadingSkeleton, LineSkeleton } from '../ui/Loading';
import { BesideField } from '../ui/BesideField';
import { withParams } from '../ui/paged';
import { Identities } from './Identities';
import { AccountApps } from './AccountApps';
import { GeneratedPassword, PasswordToCopy } from './GeneratedPassword';
import type { Account, Role } from './Accounts';
import { Quiet, RoleLabel, RolePicker, StatusToggle, messageOf, refusal, sentence } from './Accounts';
import { NO_FILTERS, WHEN, linkQuery } from './audit';
import type { AuditFilters } from './audit';
import { AuditTable, LoadOlder, useAuditLog, usePeople } from './Installation';

/** Groups read for the account page: the most one request returns. */
const GROUPS_LIMIT = 500;

interface Group {
  id: string;
  name: string;
  /** Set when an identity provider owns the membership (R-078). */
  source?: string;
  source_name?: string;
  members?: string[];
  install_role_id?: string;
}

export function AccountPage({
  userID,
  onBack,
  onAudit,
  onGroups,
}: {
  userID: string;
  onBack: () => void;
  /** Opens the Audit log with these filters. */
  onAudit: (query: string) => void;
  /** Opens Groups and roles, where groups are made. */
  onGroups?: () => void;
}) {
  const manage = useInstallVerb(InstallVerb.UsersManage);
  const canReadAudit = useInstallVerb(InstallVerb.AuditRead);
  const me = usePrincipal();
  const isSelf = userID === me.data?.user_id;

  const account = useQuery({
    queryKey: ['users', userID],
    queryFn: () => api.get<Account>(`/users/${userID}`),
  });
  const roles = useQuery({
    queryKey: ['roles'],
    queryFn: () => api.get<{ roles: Role[] }>('/roles'),
  });
  // The groups this account is in, asked of the server (`member`), and the
  // groups it could be added to. The list counts each group's members rather
  // than carrying them (issue #72), so membership is marked here from the
  // first answer: `members` holds this account alone when it is in the group.
  const groups = useQuery({
    queryKey: ['groups', 'of', userID],
    queryFn: async () => {
      const [mine, every] = await Promise.all([
        api.get<{ groups: Group[] | null }>(withParams('/groups', { member: userID, limit: GROUPS_LIMIT })),
        api.get<{ groups: Group[] | null }>(withParams('/groups', { limit: GROUPS_LIMIT })),
      ]);
      const inGroup = new Set((mine.groups ?? []).map((g) => g.id));
      const mark = (g: Group): Group => ({ ...g, members: inGroup.has(g.id) ? [userID] : [] });
      const rest = (every.groups ?? []).filter((g) => !inGroup.has(g.id));
      return { groups: [...(mine.groups ?? []), ...rest].map(mark) };
    },
  });

  const back = (
    <Button variant="ghost" icon={<Icon name="arrow-left" />} onClick={onBack} style={{ alignSelf: 'flex-start' }}>
      Accounts
    </Button>
  );

  if (account.isError) {
    return (
      <Sheet heading={back}>
        <Banner tone="failed">{messageOf(account.error)}</Banner>
      </Sheet>
    );
  }

  // The page's own shape while the account loads: its name and status in the
  // header, and the details with their labels and no values. The labels are
  // the same for every account, so they are real text now rather than later.
  if (!account.data) {
    return (
      <Sheet
        heading={
          <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
            {back}
            <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3) var(--space-4)' }}>
              <HeadingSkeleton />
              <LineSkeleton width="8ch" />
            </div>
          </div>
        }
      >
        <section role="status" aria-label="Loading">
          <Heading>Details</Heading>
          <Details>
            {['Username', 'Name', 'Email', 'Identity source', 'Created', 'Groups', 'Installation role'].map((term) => (
              <Detail key={term} term={term}>
                <LineSkeleton width={term === 'Created' ? '22ch' : '16ch'} />
              </Detail>
            ))}
          </Details>
        </section>
      </Sheet>
    );
  }

  const a = account.data;
  const active = a.status === 'active';
  const memberOf = (groups.data?.groups ?? []).filter((g) => g.members?.includes(a.id));
  const roleName = (id: string) => {
    const r = roles.data?.roles.find((x) => x.id === id);
    return r ? sentence(r.name) : id;
  };

  return (
    <Sheet
      heading={
        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
          {back}
          <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3) var(--space-4)' }}>
            <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>{a.display_name || a.external_id}</h3>
            <StatusIndicator
              // Suspended is a stopped account, not a failed one (see Accounts).
              status={active ? 'running' : 'stopped'}
              label={active ? 'Active' : 'Suspended'}
            />
            {manage && !isSelf && (
              <div style={{ display: 'flex', gap: 'var(--space-3)' }}>
                <StatusToggle account={a} />
                {/* An external provider holds its accounts' passwords; Pando has
                    nothing to reset. */}
                {a.adapter_id === LOCAL && <ResetPassword account={a} />}
                <DeleteAccount account={a} onDeleted={onBack} />
              </div>
            )}
          </div>
        </div>
      }
      note={a.id}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)' }}>
        <section>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 'var(--space-3)' }}>
            <Heading>Details</Heading>
            {/* Your own name and email need no permission; everything else
                here takes install.users.manage, and the dialog says which. */}
            {(manage || isSelf) && <EditDetails account={a} manage={manage} isSelf={isSelf} />}
          </div>
          <Details>
            {/* An external account's external_id is the provider's ID for the
                person, not a name anyone signs in with. */}
            <Detail term={a.adapter_id === LOCAL ? 'Username' : 'Identity at provider'} mono>
              {a.external_id}
            </Detail>
            <Detail term="Name">{a.display_name || '—'}</Detail>
            <Detail term="Email">{a.email || '—'}</Detail>
            <Detail term="Identity source" mono={a.adapter_id !== LOCAL}>
              {a.adapter_id === LOCAL ? 'Local account' : a.adapter_id}
            </Detail>
            {a.alias_of && (
              <Detail term="Replaced by" mono>
                {a.alias_of}
              </Detail>
            )}
            {a.adapter_id === LOCAL && (
              <Detail term="Password">
                {a.must_change_password ? 'Must be changed at next sign-in' : 'Set by the account holder'}
              </Detail>
            )}
            <Detail term="Created">{a.created_at ? new Date(a.created_at).toLocaleString() : '—'}</Detail>
            <Detail term="Groups">
              {/* Not "None" before the groups have arrived. */}
              {groups.isPending ? (
                <LineSkeleton width="16ch" />
              ) : manage ? (
                <GroupMembership account={a} groups={groups.data?.groups ?? []} isSelf={isSelf} onGroups={onGroups} />
              ) : memberOf.length > 0 ? (
                memberOf.map((g) => g.name).join(', ')
              ) : (
                'None'
              )}
            </Detail>
            <Detail term="Installation role">
              <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
                {/* A picker with no roles in it yet would show the role's id. */}
                {roles.isPending ? (
                  manage ? (
                    <FieldSkeleton width="32ch" />
                  ) : (
                    <LineSkeleton width="12ch" />
                  )
                ) : manage ? (
                  <div style={{ maxWidth: '32ch' }}>
                    <RolePicker account={a} roles={roles.data?.roles ?? []} isSelf={isSelf} />
                  </div>
                ) : (
                  <span>
                    <RoleLabel roleID={a.install_role_id} roles={roles.data?.roles ?? []} />
                  </span>
                )}
                {/* A group's installation role is held by everyone in it. It is
                    changed on the group, not here, so it is said rather than
                    offered. */}
                {memberOf
                  .filter((g) => g.install_role_id)
                  .map((g) => (
                    <span key={g.id} style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
                      {roleName(g.install_role_id!)} through {g.name}
                    </span>
                  ))}
              </div>
            </Detail>
          </Details>
        </section>

        <Identities userID={a.id} manage={manage} />

        <AccountApps principal={{ kind: 'user', id: a.id, name: a.display_name || a.external_id }} />

        {canReadAudit && <Activity userID={a.id} onAudit={onAudit} />}
      </div>
    </Sheet>
  );
}

const LOCAL = 'idp_local';

/** The account's audit history: events where it is the actor or the target. */
function Activity({ userID, onAudit }: { userID: string; onAudit: (query: string) => void }) {
  const [when, setWhen] = useState('7d');
  // A custom range is the Audit log's job; the presets are enough to look.
  const filters: AuditFilters = { ...NO_FILTERS, involving: userID, when };
  const { log, events } = useAuditLog(filters);
  const people = usePeople();

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
          <Heading>Activity</Heading>
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            Audit events where this account is the actor or the target.
          </p>
        </div>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-end', gap: 'var(--space-3)' }}>
          <div style={{ minWidth: '18ch' }}>
            <Select
              label="Time range"
              value={when}
              options={WHEN.filter((w) => w.value !== 'custom').map((w) => ({ value: w.value, label: w.label }))}
              onChange={(e) => setWhen(e.target.value)}
            />
          </div>
          <BesideField>
            <Button variant="secondary" onClick={() => onAudit(linkQuery(filters))}>
              Open in audit log
            </Button>
          </BesideField>
        </div>
      </div>

      {log.isError && <Quiet>{messageOf(log.error)}</Quiet>}

      <AuditTable
        events={events}
        people={people}
        loading={log.isPending}
        empty={
          <EmptyState heading="No events in this range">
            Choose a longer time range, or open the audit log for more filters.
          </EmptyState>
        }
      />
      <LoadOlder log={log} />
    </section>
  );
}

/**
 * Username, name and email. An identity provider's account takes its username
 * and email from the provider, which would put them back at the next sign-in,
 * so those two are shown but not editable. A username is how someone signs in,
 * so changing one — even your own — takes install.users.manage.
 */
function EditDetails({ account, manage, isSelf }: { account: Account; manage: boolean; isSelf: boolean }) {
  const [open, setOpen] = useState(false);
  const [username, setUsername] = useState(account.external_id);
  const [name, setName] = useState(account.display_name ?? '');
  const [email, setEmail] = useState(account.email ?? '');
  const queries = useQueryClient();
  const local = account.adapter_id === LOCAL;

  // Only what changed: a PATCH naming the username of an IdP account is
  // refused even when the value is the same.
  const changes: Record<string, string> = {};
  if (username !== account.external_id) changes.username = username;
  if (name !== (account.display_name ?? '')) changes.display_name = name;
  if (email !== (account.email ?? '')) changes.email = email;

  const save = useMutation({
    mutationFn: () => api.patch<unknown>(`/users/${account.id}`, changes),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['users'] });
      void queries.invalidateQueries({ queryKey: ['users', account.id] });
      if (isSelf) void queries.invalidateQueries({ queryKey: ['me'] });
      setOpen(false);
    },
  });

  const start = () => {
    setUsername(account.external_id);
    setName(account.display_name ?? '');
    setEmail(account.email ?? '');
    save.reset();
    setOpen(true);
  };
  // A refusal belongs to the value that caused it.
  const edit = (set: (v: string) => void) => (e: React.ChangeEvent<HTMLInputElement>) => {
    if (save.isError) save.reset();
    set(e.target.value);
  };
  const atProvider = `Set by ${account.adapter_id}, the identity provider this account signs in with.`;

  return (
    <>
      <Button variant="secondary" onClick={start}>
        Edit
      </Button>
      {open && (
        <Dialog
          open
          title={`Edit ${account.external_id}`}
          onClose={() => setOpen(false)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                disabled={save.isPending || Object.keys(changes).length === 0}
                onClick={() => save.mutate()}
              >
                {save.isPending ? 'Saving' : 'Save'}
              </Button>
            </>
          }
        >
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
            <Input
              label="Username"
              mono
              value={username}
              disabled={!local || !manage}
              helper={
                !local
                  ? atProvider
                  : !manage
                    ? 'Changing a username takes permission to manage accounts.'
                    : 'What the account signs in with.'
              }
              onChange={edit(setUsername)}
            />
            <Input label="Name" value={name} onChange={edit(setName)} />
            <Input
              label="Email"
              type="email"
              value={email}
              disabled={!local}
              helper={local ? undefined : atProvider}
              onChange={edit(setEmail)}
            />
            {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
          </div>
        </Dialog>
      )}
    </>
  );
}

/**
 * The groups an account is in, for someone who manages accounts: remove it from
 * one, or add it to another. A group an identity provider syncs is the
 * provider's to change (R-078), so it is listed without a remove and is not
 * offered to add to.
 */
function GroupMembership({
  account,
  groups,
  isSelf,
  onGroups,
}: {
  account: Account;
  groups: Group[];
  isSelf: boolean;
  /** Groups and roles, where a group is made. */
  onGroups?: () => void;
}) {
  const queries = useQueryClient();
  const [error, setError] = useState<string>();
  const memberOf = groups.filter((g) => g.members?.includes(account.id));
  const offered = groups.filter((g) => !g.source && !g.members?.includes(account.id));

  const change = useMutation({
    mutationFn: ({ group, add }: { group: string; add: boolean }) => {
      const path = `/groups/${group}/members/${account.id}`;
      return add ? api.put<unknown>(path) : api.del<unknown>(path);
    },
    onSuccess: () => setError(undefined),
    // Shown as written: removing the last person who can manage accounts is
    // refused (R-088), and the remedy says what to do instead.
    onError: (e) => setError(refusal(e)),
    onSettled: () => {
      void queries.invalidateQueries({ queryKey: ['groups'] });
      // A group carries app access and possibly an installation role.
      void queries.invalidateQueries({ queryKey: ['users', account.id, 'apps'] });
      if (isSelf) void queries.invalidateQueries({ queryKey: ['me'] });
    },
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', maxWidth: '48ch' }}>
      {memberOf.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2) var(--space-3)' }}>
          {memberOf.map((g) => (
            <span key={g.id} style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)' }}>
              {/* A provider's group says where it comes from: its members are
                  changed there, not here (R-078). */}
              <Tag>{g.source_name ? `${g.name} · ${g.source_name}` : g.name}</Tag>
              {!g.source && (
                <IconButton
                  label={`Remove from ${g.name}`}
                  size={24}
                  disabled={change.isPending}
                  onClick={() => change.mutate({ group: g.id, add: false })}
                >
                  <Icon name="x" size={14} />
                </IconButton>
              )}
            </span>
          ))}
        </div>
      )}
      {offered.length > 0 ? (
        <Select
          aria-label="Add to group"
          value=""
          disabled={change.isPending}
          onChange={(e) => e.target.value && change.mutate({ group: e.target.value, add: true })}
          options={[{ value: '', label: 'Add to group' }, ...offered.map((g) => ({ value: g.id, label: g.name }))]}
        />
      ) : (
        // Said, rather than an empty row: with no groups to offer, a bare
        // "None" read as a row that could not be edited.
        <span style={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2) var(--space-3)' }}>
          <span style={{ color: 'var(--ink-secondary)' }}>
            {groups.some((g) => !g.source)
              ? 'In every group there is.'
              : memberOf.length > 0
                ? 'No Pando groups to add to yet.'
                : 'No groups yet.'}
          </span>
          {onGroups && (
            <Button variant="secondary" onClick={onGroups}>
              {groups.length === 0 ? 'Create a group' : 'Groups and roles'}
            </Button>
          )}
        </span>
      )}
      {error && <span style={{ font: 'var(--type-caption)', color: 'var(--marker-deep)' }}>{error}</span>}
    </div>
  );
}

/**
 * A new password for somebody else's local account, generated rather than typed
 * (see GeneratedPassword). Not offered on your own page: the API refuses it
 * there, because changing your own password takes your current one
 * (POST /me/password).
 */
function ResetPassword({ account }: { account: Account }) {
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState('');
  const [mustChange, setMustChange] = useState(true);
  const queries = useQueryClient();

  const reset = useMutation({
    mutationFn: () =>
      api.post<void>(`/users/${account.id}/password`, { password, must_change_password: mustChange }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['users', account.id] });
      void queries.invalidateQueries({ queryKey: ['users'] });
      void queries.invalidateQueries({ queryKey: ['audit'] });
    },
  });

  const close = () => {
    setOpen(false);
    // The next reset starts from a fresh password, not this one.
    setPassword('');
    setMustChange(true);
    reset.reset();
  };

  return (
    <>
      <Button variant="secondary" onClick={() => setOpen(true)}>
        Reset password
      </Button>
      {open &&
        (reset.isSuccess ? (
          <Dialog
            open
            title={`Password reset for ${account.external_id}`}
            // Last sight of it: Pando cannot show it again once this closes.
            description="Copy the password and give it to them separately. It is not shown again after you close this."
            onClose={close}
            footer={
              <Button variant="primary" onClick={close}>
                Done
              </Button>
            }
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
              <PasswordToCopy value={password} />
              <Quiet>
                Every session {account.external_id} had has ended.
                {mustChange ? ' They choose a new password the next time they sign in.' : ''}
              </Quiet>
            </div>
          </Dialog>
        ) : (
          <Dialog
            open
            title={`Reset password for ${account.external_id}`}
            // What happens to the sessions is said before, not after (R-282).
            description="Pando replaces the password with the one below and signs the account out everywhere. Copy it and give it to them separately."
            onClose={close}
            footer={
              <>
                <Button variant="ghost" onClick={close}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  disabled={reset.isPending || password === ''}
                  onClick={() => reset.mutate()}
                >
                  {reset.isPending ? 'Resetting' : 'Reset password'}
                </Button>
              </>
            }
          >
            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
              <GeneratedPassword
                value={password}
                onChange={(p) => {
                  if (reset.isError) reset.reset();
                  setPassword(p);
                }}
                mustChange={mustChange}
                onMustChange={setMustChange}
              />
              {reset.isError && <Banner tone="failed">{messageOf(reset.error)}</Banner>}
            </div>
          </Dialog>
        ))}
    </>
  );
}

function DeleteAccount({ account, onDeleted }: { account: Account; onDeleted: () => void }) {
  const [open, setOpen] = useState(false);
  const queries = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.del<void>(`/users/${account.id}`),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['users'] });
      void queries.invalidateQueries({ queryKey: ['groups'] });
      onDeleted();
    },
  });

  return (
    <>
      <Button variant="destructive" onClick={() => setOpen(true)}>
        Delete
      </Button>
      {open && (
        <Dialog
          open
          title={`Delete ${account.external_id}`}
          // What goes with it, before it goes (R-282). Suspending is the
          // reversible one, and the dialog says so.
          description="The account signs out everywhere, loses its installation role, group memberships and access to every app, and its tokens stop working. This cannot be undone. To stop someone signing in but keep the account, suspend it instead."
          onClose={() => setOpen(false)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {remove.isPending ? 'Deleting' : 'Delete account'}
              </Button>
            </>
          }
        >
          {remove.isError && <Banner tone="failed">{messageOf(remove.error)}</Banner>}
        </Dialog>
      )}
    </>
  );
}

function Heading({ children }: { children: React.ReactNode }) {
  return <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>{children}</h4>;
}

function Details({ children }: { children: React.ReactNode }) {
  return (
    <dl
      style={{
        display: 'grid',
        gridTemplateColumns: 'minmax(12ch, max-content) 1fr',
        gap: 'var(--space-3) var(--space-6)',
        margin: 0,
        font: 'var(--type-body-ui)',
      }}
    >
      {children}
    </dl>
  );
}

function Detail({ term, mono, children }: { term: string; mono?: boolean; children: React.ReactNode }) {
  return (
    <>
      <dt style={{ color: 'var(--ink-secondary)' }}>{term}</dt>
      <dd style={{ margin: 0, minWidth: 0, overflowWrap: 'anywhere', font: mono ? 'var(--type-code)' : undefined }}>
        {children}
      </dd>
    </>
  );
}
