// Groups and roles — R-078 and R-082, which had a full API and no screen.
//
// The two are deliberately not one screen with two tabs pretending to be the
// same idea. A group is *who*: a set of accounts, named once and granted access
// as a unit. A role is *what*: a named set of verbs. Conflating them is how an
// authorization model becomes a list of people with special powers, which is
// the thing R-078 exists to avoid.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Badge, Banner, Button, Checkbox, Dialog, Input, Select, Tag } from '@design';

import { api } from '@api/client';
import { Quiet, RoleLabel, Screen, messageOf, refusal, sentence } from './Accounts';
import { VERB_NOTES } from './verbNotes';
import { AccountApps } from './AccountApps';
import { NoMatches, SearchField } from '../ui/SearchField';
import { matches } from '../ui/search';
import { ShowMore, usePaged, useSettled, withParams, type PageOf } from '../ui/paged';
import { Table } from '../ui/Table';
import { LineSkeleton, Loading } from '../ui/Loading';
import { AIButton } from '../ui/AskAI';
import { AccessAI } from './AccessAI';
import { useAIFunctionOn } from './AIFunctions';

interface Group {
  id: string;
  name: string;
  /** The identity provider that owns the membership, and its name. */
  source?: string;
  source_name?: string;
  /** On GET /groups/{id} only; the list carries member_count instead. */
  members?: string[];
  /** How many people are in the group directly; on the list. */
  member_count?: number;
  /** On a Pando group: the provider groups whose members count as its own. */
  linked_from?: string[] | null;
  /** The installation role everyone in the group holds; '' for none. */
  install_role_id?: string;
}

interface Role {
  id: string;
  name: string;
  scope?: string;
  builtin: boolean;
  verbs: string[];
}

interface VerbRow {
  verb: string;
  scope: string;
}

interface Account {
  id: string;
  display_name?: string;
  email?: string;
  external_id: string;
}

export function Identity({ canEdit }: { canEdit: boolean }) {
  // One search for both lists: they are one screen, and someone looking for
  // "engineering" should not have to know first whether it is a group or a role.
  const [query, setQuery] = useState('');
  // Drafting with AI (R-343), behind one button: shown to whoever can create
  // groups and roles, and only when access drafting is assigned to an adapter.
  const draftOn = useAIFunctionOn('draft_access');
  const [drafting, setDrafting] = useState(false);
  return (
    <Screen
      heading="Groups and roles"
      action={
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
          {canEdit && draftOn && <AIButton onClick={() => setDrafting(true)} />}
          <SearchField value={query} onChange={setQuery} placeholder="Search groups and roles" />
        </div>
      }
    >
      {drafting && <AccessAI onClose={() => setDrafting(false)} />}
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-7)',
        }}
      >
        <Groups canEdit={canEdit} query={query} />
        <Roles canEdit={canEdit} query={query} />
      </div>
    </Screen>
  );
}

// --- groups ----------------------------------------------------------------

function Groups({ canEdit, query }: { canEdit: boolean; query: string }) {
  const [editing, setEditing] = useState<Group | 'new' | null>(null);
  const [deleting, setDeleting] = useState<Group | null>(null);
  const [showing, setShowing] = useState<string | null>(null);
  const [linking, setLinking] = useState<Group | null>(null);

  // A page at a time, searched by name on the server (issue #72). Each row
  // counts its people rather than listing them; the editor reads the group.
  const groups = usePaged<{ groups: Group[] | null } & PageOf, Group>({
    key: ['groups', 'list'],
    path: '/groups',
    rows: (p) => p.groups,
    search: useSettled(query.trim()),
  });
  // Installation roles only: a group's role here applies across the
  // installation (R-080). What it can do on one app is set in its apps.
  const roles = useQuery({
    queryKey: ['roles'],
    queryFn: () => api.get<{ roles: Role[] }>('/roles'),
  });
  const installRoles = roles.data?.roles ?? [];

  const all = groups.rows;
  const rows = all;
  // Looked up rather than kept, so the panel follows a rename or a delete.
  const shown = all.find((g) => g.id === showing);

  return (
    <section>
      <div
        style={{
          display: 'flex',
          alignItems: 'baseline',
          justifyContent: 'space-between',
        }}
      >
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Groups</h4>
        {canEdit && (
          <Button variant="secondary" onClick={() => setEditing('new')}>
            Add group
          </Button>
        )}
      </div>

      {/* R-078: an identity provider says who is in a group; Pando decides what
          that group can do. Saying so here is what stops someone looking for
          permissions on this screen. */}
      <Quiet>
        Named sets of people, so access is given to a team once instead of to each person. Everyone in a group holds its
        role and its access to apps.
      </Quiet>

      {groups.query.isError && <Banner tone="failed">{messageOf(groups.query.error)}</Banner>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          loading={groups.query.isPending}
          skeletonRows={3}
          columns={[
            { key: 'name', header: 'Name', width: 'minmax(0,36ch)' },
            {
              key: 'members',
              header: 'People',
              width: '14ch',
              render: (row: Group) => <Badge count={row.member_count ?? row.members?.length ?? 0} />,
            },
            {
              key: 'source',
              header: 'Comes from',
              width: '20ch',
              muted: true,
              // A group synced from an identity provider is not Pando's to
              // edit: the provider is the source of truth for membership
              // (R-078), and an edit here would be overwritten at the next
              // sign-in without saying so.
              render: (row: Group) =>
                row.source
                  ? (row.source_name ?? row.source)
                  : row.linked_from && row.linked_from.length > 0
                    ? `Pando, and ${row.linked_from.length} provider group${row.linked_from.length === 1 ? '' : 's'}`
                    : 'Pando',
            },
            {
              key: 'role',
              header: 'Role',
              width: 'minmax(0,24ch)',
              // Allowed on a synced group too: the provider says who is in it,
              // Pando says what it can do (R-078).
              render: (row: Group) =>
                canEdit ? (
                  <div style={{ padding: 'var(--space-2) 0' }}>
                    <GroupRole group={row} roles={installRoles} />
                  </div>
                ) : (
                  <RoleLabel roleID={row.install_role_id ?? ''} roles={installRoles} />
                ),
            },
            {
              key: 'edit',
              header: '',
              width: '32ch',
              align: 'right',
              render: (row: Group) => (
                <span style={{ display: 'inline-flex', gap: 'var(--space-2)' }}>
                  <Button variant="secondary" onClick={() => setShowing(showing === row.id ? null : row.id)}>
                    Apps
                  </Button>
                  {/* Not for a synced group: the identity provider would make
                      it again at the next sign-in (R-078). */}
                  {canEdit && !row.source && (
                    <>
                      <Button variant="secondary" onClick={() => setEditing(row)}>
                        Change people
                      </Button>
                      {all.some((g) => g.source) && (
                        <Button variant="secondary" onClick={() => setLinking(row)}>
                          Provider groups
                        </Button>
                      )}
                      <Button variant="secondary" onClick={() => setDeleting(row)}>
                        Delete
                      </Button>
                    </>
                  )}
                </span>
              ),
            },
          ]}
          rows={rows}
          empty={
            query.trim() ? (
              <NoMatches what="groups" query={query} />
            ) : (
              <Quiet>No groups yet. Apps can still be shared with one person at a time.</Quiet>
            )
          }
        />
        <div style={{ marginTop: 'var(--space-3)' }}>
          <ShowMore query={groups.query} label="Show more groups" />
        </div>
      </div>

      {/* Below the table rather than in a dialog: giving access to an app
          opens a dialog of its own, and a dialog over a dialog is one too
          many. */}
      {shown && (
        <div style={{ marginTop: 'var(--space-6)' }}>
          <AccountApps
            key={shown.id}
            principal={{ kind: 'group', id: shown.id, name: shown.name }}
            heading={`Apps for ${shown.name}`}
            action={
              <Button variant="ghost" onClick={() => setShowing(null)}>
                Close
              </Button>
            }
          />
        </div>
      )}

      {editing && <EditGroup group={editing} onClose={() => setEditing(null)} />}
      {linking && (
        <LinkGroups group={all.find((g) => g.id === linking.id) ?? linking} synced={all.filter((g) => g.source)} onClose={() => setLinking(null)} />
      )}
      {deleting && <DeleteGroup group={deleting} onClose={() => setDeleting(null)} />}
    </section>
  );
}

/** Members listed by name in the group editor; a search finds the rest. */
const MEMBERS_NAMED = 100;
/** Accounts a search in the group editor offers. */
const PEOPLE_FOUND = 20;

function EditGroup({ group, onClose }: { group: Group | 'new'; onClose: () => void }) {
  const queries = useQueryClient();
  const creating = group === 'new';
  const [name, setName] = useState(creating ? '' : group.name);
  // The group's members come from the group itself — the list counts them
  // rather than carrying them (issue #72) — and are held here once changed.
  const detail = useQuery({
    queryKey: ['groups', creating ? 'new' : group.id],
    queryFn: () => api.get<Group>(`/groups/${creating ? '' : group.id}`),
    enabled: !creating,
  });
  const [changed, setMembers] = useState<string[] | null>(creating ? [] : null);
  const members = changed ?? detail.data?.members ?? [];

  // The people offered: the members, by name, and whoever a search finds.
  // Not every account in the install, which can be a hundred thousand.
  const [search, setSearch] = useState('');
  const settled = useSettled(search.trim());
  // The members as the group had them, so one taken out stays on the list
  // to be put back.
  const named = (detail.data?.members ?? []).slice(0, MEMBERS_NAMED);
  const accounts = useQuery({
    queryKey: ['users', 'pick', settled, named],
    queryFn: async () => {
      const [inGroup, found] = await Promise.all([
        named.length > 0
          ? api.get<{ users: Account[] | null }>(withParams('/users', { id: named, limit: named.length }))
          : Promise.resolve({ users: [] as Account[] }),
        api.get<{ users: Account[] | null }>(withParams('/users', { q: settled, limit: PEOPLE_FOUND })),
      ]);
      const first = inGroup.users ?? [];
      return [...first, ...(found.users ?? []).filter((a) => !first.some((m) => m.id === a.id))];
    },
    enabled: creating || detail.isSuccess,
    placeholderData: (previous) => previous,
  });

  const save = useMutation({
    mutationFn: () =>
      creating ? api.post('/groups', { name, members }) : api.put(`/groups/${group.id}/members`, { members }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['groups'] });
      onClose();
    },
  });

  const toggle = (id: string) =>
    setMembers((current) => {
      const base = current ?? detail.data?.members ?? [];
      return base.includes(id) ? base.filter((m) => m !== id) : [...base, id];
    });

  return (
    <Dialog
      open
      onClose={onClose}
      title={creating ? 'Add group' : `People in ${name}`}
      description="Changing who is in a group changes what they can reach, immediately."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={save.isPending || (creating && !name) || (!creating && !detail.isSuccess)} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving' : 'Save'}
          </Button>
        </>
      }
    >
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-5)',
        }}
      >
        {creating && (
          <Input
            label="Name"
            value={name}
            helper="What this set of people is called, for example platform."
            onChange={(e) => setName(e.target.value)}
          />
        )}

        <div
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-3)',
          }}
        >
          <SearchField value={search} onChange={setSearch} placeholder="Search accounts to add" width="100%" />
          {(detail.data?.members?.length ?? 0) > MEMBERS_NAMED && (
            <Quiet>
              The first {MEMBERS_NAMED} of {detail.data?.members?.length} people are listed. Search to find the others.
            </Quiet>
          )}
          {/* A checkbox's line each, until the accounts arrive. */}
          {accounts.isPending && (
            <Loading>
              {[0, 1, 2].map((n) => (
                <LineSkeleton key={n} width={`${24 - n * 4}ch`} />
              ))}
            </Loading>
          )}
          {(accounts.data ?? []).map((a) => (
            <Checkbox
              key={a.id}
              checked={members.includes(a.id)}
              label={a.display_name || a.email || a.external_id}
              onChange={() => toggle(a.id)}
            />
          ))}
        </div>

        {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}

/** The group's installation role. Everyone in the group holds it, so a new
 *  member of a team gets what the team has without being given it one by one. */
function GroupRole({ group, roles }: { group: Group; roles: Role[] }) {
  const queries = useQueryClient();
  const [error, setError] = useState<string>();

  const change = useMutation({
    mutationFn: (roleID: string) =>
      roleID === ''
        ? api.del<void>(`/groups/${group.id}/role`)
        : api.put<unknown>(`/groups/${group.id}/role`, { role_id: roleID }),
    onSuccess: () => {
      setError(undefined);
      void queries.invalidateQueries({ queryKey: ['groups'] });
      // Every member's verbs just changed, possibly your own.
      void queries.invalidateQueries({ queryKey: ['users'] });
      void queries.invalidateQueries({ queryKey: ['me'] });
    },
    // Taking the role away from the only group that lets anyone manage
    // accounts is refused (R-088); the remedy says what to do instead.
    onError: (e) => setError(refusal(e)),
  });

  return (
    <Select
      aria-label={`Role for ${group.name}`}
      value={group.install_role_id ?? ''}
      disabled={change.isPending}
      onChange={(e) => change.mutate(e.target.value)}
      options={[{ value: '', label: 'None' }, ...roles.map((r) => ({ value: r.id, label: sentence(r.name) }))]}
      helper={error}
    />
  );
}

// --- roles -----------------------------------------------------------------

function Roles({ canEdit, query }: { canEdit: boolean; query: string }) {
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<Role | null>(null);

  const roles = useQuery({
    // Both scopes. Without it this listed only the installation roles, and the
    // app roles — three of Pando's five, and any made here — were not on the
    // screen that says what every role is.
    queryKey: ['roles', 'all'],
    queryFn: () => api.get<{ roles: Role[] | null }>('/roles?scope=all'),
  });

  // By name, by what it applies to, and by any permission it holds — so
  // "secrets" finds every role that can touch them.
  const rows = (roles.data?.roles ?? []).filter((r) =>
    matches(
      query,
      r.name,
      r.scope === 'install' ? 'whole installation' : 'one app',
      r.builtin ? 'Pando' : 'This installation',
      ...r.verbs,
    ),
  );

  return (
    <section>
      <div
        style={{
          display: 'flex',
          alignItems: 'baseline',
          justifyContent: 'space-between',
        }}
      >
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Roles</h4>
        {canEdit && (
          <Button variant="secondary" onClick={() => setAdding(true)}>
            Add role
          </Button>
        )}
      </div>

      {/* R-081, said where someone would otherwise look for an edit button. */}
      <Quiet>
        Named sets of permissions. The five Pando ships can&rsquo;t be edited or deleted — an install that quietly
        redefined what &ldquo;viewer&rdquo; means is an install where nobody can answer what a viewer can do.
      </Quiet>

      {roles.isError && <Banner tone="failed">{messageOf(roles.error)}</Banner>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          loading={roles.isPending}
          skeletonRows={5}
          columns={[
            { key: 'name', header: 'Name', width: 'minmax(0,24ch)' },
            {
              key: 'scope',
              header: 'Applies to',
              width: '18ch',
              // R-080: the two scopes are never conflated. An install role and
              // an app role are different things and this column is where a
              // reader learns that.
              render: (row: Role) => (row.scope === 'install' ? 'The whole installation' : 'One app'),
            },
            {
              key: 'builtin',
              header: 'Source',
              width: '14ch',
              muted: true,
              render: (row: Role) => (row.builtin ? 'Pando' : 'This installation'),
            },
            {
              key: 'verbs',
              header: 'Permissions',
              width: 'minmax(0,28ch)',
              render: (row: Role) => (
                <div
                  style={{
                    display: 'flex',
                    gap: 'var(--space-2)',
                    flexWrap: 'wrap',
                    padding: 'var(--space-2) 0',
                  }}
                >
                  {row.verbs.map((v) => (
                    <Tag key={v} mono>
                      {v}
                    </Tag>
                  ))}
                </div>
              ),
            },
            {
              key: 'delete',
              header: '',
              width: '10ch',
              align: 'right',
              // R-081: built-ins are not deletable, so they are not offered.
              render: (row: Role) =>
                canEdit && !row.builtin ? (
                  <Button variant="secondary" onClick={() => setDeleting(row)}>
                    Delete
                  </Button>
                ) : null,
            },
          ]}
          rows={rows}
          empty={query.trim() ? <NoMatches what="roles" query={query} /> : undefined}
        />
      </div>

      {adding && <AddRole onClose={() => setAdding(false)} />}
      {deleting && <DeleteRole role={deleting} onClose={() => setDeleting(null)} />}
    </section>
  );
}

function AddRole({ onClose }: { onClose: () => void }) {
  const queries = useQueryClient();
  const [name, setName] = useState('');
  const [scope, setScope] = useState('app');
  const [verbs, setVerbs] = useState<string[]>([]);

  const catalog = useQuery({
    queryKey: ['verbs'],
    queryFn: () => api.get<{ verbs: VerbRow[] | null }>('/verbs'),
  });

  // Every role, the shipped ones included, so a name already taken is said
  // while it is typed. The server refuses it too, ignoring case and spaces:
  // the built-ins are stored lowercase and shown capitalized, and
  // "Administrator" beside the real one would be indistinguishable in a
  // picker.
  const existing = useQuery({
    queryKey: ['roles', 'all'],
    queryFn: () => api.get<{ roles: Role[] }>('/roles?scope=all'),
  });
  const clash = (existing.data?.roles ?? []).find((r) => r.name.trim().toLowerCase() === name.trim().toLowerCase());
  const taken =
    name.trim() && clash
      ? clash.builtin
        ? `Pando already has a built-in role called ${sentence(clash.name)}. Choose a different name.`
        : `There is already a role called ${clash.name}. Choose a different name.`
      : undefined;

  const save = useMutation({
    mutationFn: () => api.post('/roles', { name: name.trim(), scope, verbs }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['roles'] });
      onClose();
    },
  });

  // Only the verbs of the chosen scope. A role mixing the two is refused by the
  // server (R-080), and offering the choice that will be refused is worse than
  // not offering it: the scopes are different questions, not a filter.
  const available = (catalog.data?.verbs ?? []).filter((v) => v.scope === scope);

  const toggle = (verb: string) =>
    setVerbs((current) => (current.includes(verb) ? current.filter((v) => v !== verb) : [...current, verb]));

  return (
    <Dialog
      open
      onClose={onClose}
      title="Add role"
      description="A role you add can be changed later. The ones Pando ships cannot."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={save.isPending || !name.trim() || Boolean(taken) || verbs.length === 0}
            onClick={() => save.mutate()}
          >
            {save.isPending ? 'Saving' : 'Add role'}
          </Button>
        </>
      }
    >
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-5)',
        }}
      >
        <Input
          label="Name"
          value={name}
          helper="What this set of permissions is called, for example support or release manager."
          error={taken}
          onChange={(e) => setName(e.target.value)}
        />

        <Select
          label="Applies to"
          value={scope}
          options={[
            { value: 'app', label: 'One app it is granted on' },
            { value: 'install', label: 'The whole installation' },
          ]}
          helper="A role is one or the other. Holding permissions over the installation is not the same as holding them over an app."
          onChange={(e) => {
            setScope(e.target.value);
            setVerbs([]);
          }}
        />

        <div
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-3)',
          }}
        >
          {catalog.isPending && (
            <Loading>
              {[0, 1, 2, 3].map((n) => (
                <LineSkeleton key={n} width={`${22 - (n % 2) * 6}ch`} />
              ))}
            </Loading>
          )}
          {available.map((v) => (
            <Checkbox
              key={v.verb}
              checked={verbs.includes(v.verb)}
              label={v.verb}
              description={VERB_NOTES[v.verb]}
              onChange={() => toggle(v.verb)}
            />
          ))}
        </div>

        {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}

// --- deleting ----------------------------------------------------------------
//
// Both are allowed, and both take access away from people who may not know it
// is happening, so each says whose access goes and whose stays before anything
// is deleted. The server refuses the one case that would lock the installation
// (R-088) and says why; that message is shown as-is.

function DeleteGroup({ group, onClose }: { group: Group; onClose: () => void }) {
  const queries = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.del<void>(`/groups/${group.id}`),
    onSuccess: () => {
      void queries.invalidateQueries();
      onClose();
    },
  });
  const people = group.member_count ?? group.members?.length ?? 0;

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Delete the ${group.name} group`}
      description="Everyone in it loses what was shared with the group."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
            {remove.isPending ? 'Deleting' : 'Delete group'}
          </Button>
        </>
      }
    >
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-3)',
        }}
      >
        <p style={{ font: 'var(--type-body-ui)', margin: 0 }}>
          {people === 1 ? 'The one person' : `The ${people} people`} in {group.name} lose every app that was shared with
          the group — both opening it and any role the group had for managing it. Their accounts are kept, and so is
          anything shared with them directly or through another group.
        </p>
        <p
          style={{
            font: 'var(--type-body-ui)',
            color: 'var(--ink-secondary)',
            margin: 0,
          }}
        >
          This can&rsquo;t be undone. Making a group with the same name later does not bring the access back.
        </p>
        {remove.isError && <Banner tone="failed">{refusal(remove.error)}</Banner>}
      </div>
    </Dialog>
  );
}

function DeleteRole({ role, onClose }: { role: Role; onClose: () => void }) {
  const queries = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.del<void>(`/roles/${role.id}`),
    onSuccess: () => {
      void queries.invalidateQueries();
      onClose();
    },
  });

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Delete the ${role.name} role`}
      description="Anyone given this role loses what it allowed."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
            {remove.isPending ? 'Deleting' : 'Delete role'}
          </Button>
        </>
      }
    >
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-3)',
        }}
      >
        <p style={{ font: 'var(--type-body-ui)', margin: 0 }}>
          {role.scope === 'install'
            ? `Everyone given ${role.name} across the installation loses the permissions it gave them.`
            : `Everyone given ${role.name} on an app — directly or through a group — loses the access it gave them on that app.`}{' '}
          They keep anything they hold another way.
        </p>
        <p
          style={{
            font: 'var(--type-body-ui)',
            color: 'var(--ink-secondary)',
            margin: 0,
          }}
        >
          This can&rsquo;t be undone. Making a role with the same name later does not give it back to anyone.
        </p>
        {remove.isError && <Banner tone="failed">{refusal(remove.error)}</Banner>}
      </div>
    </Dialog>
  );
}

/**
 * Which identity provider groups count as members of a Pando group (R-078).
 * Everyone in a linked provider group holds what this group holds, for as long
 * as the provider keeps them in it (R-079).
 */
function LinkGroups({ group, synced, onClose }: { group: Group; synced: Group[]; onClose: () => void }) {
  const queries = useQueryClient();
  const linked = new Set(group.linked_from ?? []);
  const toggle = useMutation({
    mutationFn: ({ id, on }: { id: string; on: boolean }) =>
      on ? api.put(`/groups/${group.id}/links/${id}`) : api.del(`/groups/${group.id}/links/${id}`),
    onSettled: () => void queries.invalidateQueries({ queryKey: ['groups'] }),
  });
  return (
    <Dialog
      open
      title={`Provider groups in ${group.name}`}
      description="Everyone in a chosen group counts as a member of this one. The provider says who is in it; removing someone there removes them here within two minutes."
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        {synced.map((g) => (
          <Checkbox
            key={g.id}
            label={g.name}
            description={`From ${g.source_name ?? g.source}, ${g.member_count ?? g.members?.length ?? 0} people`}
            checked={linked.has(g.id)}
            disabled={toggle.isPending}
            onChange={(e) => toggle.mutate({ id: g.id, on: e.target.checked })}
          />
        ))}
        {toggle.isError && <Banner tone="failed">{refusal(toggle.error)}</Banner>}
      </div>
    </Dialog>
  );
}
