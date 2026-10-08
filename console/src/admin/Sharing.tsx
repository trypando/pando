// Sharing (R-076, R-077).
//
// Two rules, both about wording rather than mechanics:
//
// **The anonymous grant never stands on the bare word "public."** R-077 [P],
// and the override is in the heading only: the action is called "Make it
// public", because that is what it is called everywhere else and a heading
// nobody recognizes is its own kind of unclear. What the requirement is
// actually protecting is kept — the consequence, *anyone on the internet can
// open this, without signing in*, sits directly under the heading, beside each
// of the two ways to do it, and in the confirmation, so the word is never doing
// the work alone. "Public" is a word people skim past; the sentence is not.
// R-075 makes it a real grant row rather than a flag, and this is the interface
// to that row.
//
// **When host policy forbids it, the option is visible but disabled, with an
// explanation of who to ask** — not hidden. A hidden option produces a support
// ticket instead of understanding, and a person who cannot find the setting
// cannot tell whether it exists.
//
// **Without app.grants.manage, the list is read-only.** Anyone who can view
// the app can read who has access to it (the endpoint is app.view); changing
// it is app.grants.manage. So the table stays, without its Remove column, and
// the two sections that exist only to change it say what is true instead.
//
// Sharing with everyone comes in two kinds: plainly, or behind a passcode. A
// passcode is still the anonymous grant — nobody signs in — with one thing
// asked of the visitor first. The proxy sends a visitor without it to a
// passcode page instead of the sign-in page (auth/Login.tsx).

import { useContext, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Dialog, Input, Radio, Select, Skeleton, Tag } from '@design';

import { api, RequestFailed } from '@api/client';
import type { Role } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { LineSkeleton } from '../ui/Loading';
import { Table } from '../ui/Table';
import { ShowMore, usePaged, type PageOf } from '../ui/paged';
import { RecipientField, type Chosen } from './RecipientField';
import { BesideField } from '../ui/BesideField';
import {
  BUILT_IN_APP_ROLES,
  describeChoice,
  everyone,
  orderRoles,
  PASSCODE_MIN,
  roleChoice,
  shareRequests,
  USE_ONLY,
  type Grant,
} from './share-access';
import { AppVerb, AppVerbs, changesAnything, useCan } from './verbs';

interface GrantsResponse extends PageOf {
  grants: Grant[] | null;
  /**
   * What host policy allows in sharing with everyone (R-076): both ways, only
   * behind a passcode, or not at all.
   *
   * From the API, not inferred: the console must not decide what policy allows.
   * Absent is treated as allowed — the server refuses what policy forbids
   * either way, and the refusal carries the explanation.
   */
  public_sharing?: PublicSharing;
}

type PublicSharing = 'allowed' | 'passcode_only' | 'none';

/** The dialog open over the "everyone" section, if any. */
type Asking = 'public' | 'add-passcode' | 'change-passcode' | 'remove-passcode' | null;

/** A change to the anonymous grant. */
type EveryoneChange =
  | { method: 'post'; passcode?: string }
  | { method: 'patch'; grant: string; passcode: string };

export function Sharing({ appID, appName }: { appID: string; appName: string }) {
  const queries = useQueryClient();
  const canManage = useCan(AppVerb.GrantsManage);
  // Whether the screen's own "view but not change" note already covers this.
  const viewOnly = !changesAnything(useContext(AppVerbs));

  const [kind, setKind] = useState<'user' | 'group'>('user');
  const [recipient, setRecipient] = useState<Chosen | null>(null);
  // The least access, not the second-least. Sharing an app most often means
  // "you can use this"; anything more is a deliberate choice, and a default
  // that quietly hands over the app's settings is the wrong way round.
  const [role, setRole] = useState('');

  // Which way to open it up. The passcode is the default for the same reason
  // "can open" is above: of two ways to share with strangers, the narrower one.
  const [withPasscode, setWithPasscode] = useState(true);
  const [asking, setAsking] = useState<Asking>(null);
  const [passcode, setPasscode] = useState('');

  const grantsKey = ['apps', appID, 'grants'];
  // A page at a time, by principal (issue #72): an app shared with every
  // person by name holds a grant per person. The grant to everyone, and each
  // person's grants on both planes, arrive together on one page.
  const paged = usePaged<GrantsResponse, Grant>({
    key: grantsKey,
    path: `/apps/${appID}/grants`,
    rows: (p) => p.grants,
  });
  const grants = { ...paged.query, data: paged.query.data?.pages[0] };

  // The roles an app can be granted with, custom ones included. GET /roles is
  // an install-level read that some people who manage an app's sharing are
  // refused, and the built-ins exist on every installation (R-081), so a
  // refusal offers those rather than no choice at all.
  const roles = useQuery({
    queryKey: ['roles', 'app'],
    queryFn: () => api.get<{ roles: Role[] | null }>('/roles?scope=app'),
    enabled: canManage,
    retry: false,
  });
  const appRoles = orderRoles(roles.data?.roles ?? (roles.isError ? BUILT_IN_APP_ROLES : []));

  const rows = paged.rows;

  const share = useMutation({
    mutationFn: async (who: Chosen) => {
      // In order: a failure part way leaves what succeeded in place, and the
      // table, refreshed either way, shows exactly how far it got.
      for (const r of shareRequests(rows, who, role || null)) {
        if (r.method === 'post') await api.post<unknown>(`/apps/${appID}/grants`, r.body);
        else await api.patch<unknown>(`/apps/${appID}/grants/${r.grant}`, r.body);
      }
    },
    onSuccess: () => {
      setRecipient(null);
      setRole('');
    },
    onSettled: () => queries.invalidateQueries({ queryKey: grantsKey }),
  });

  const revoke = useMutation({
    mutationFn: (grantID: string) => api.del(`/apps/${appID}/grants/${grantID}`),
    onSettled: () => queries.invalidateQueries({ queryKey: grantsKey }),
  });

  const open = useMutation({
    mutationFn: (c: EveryoneChange) =>
      c.method === 'post'
        ? api.post<unknown>(`/apps/${appID}/grants`, {
            plane: 'data',
            principal_kind: 'anonymous',
            ...(c.passcode ? { passcode: c.passcode } : {}),
          })
        : api.patch<unknown>(`/apps/${appID}/grants/${c.grant}`, { passcode: c.passcode }),
    onSuccess: () => close(),
    onSettled: () => queries.invalidateQueries({ queryKey: grantsKey }),
  });

  const anonymous = rows.find((g) => g.principal_kind === 'anonymous');
  const state = everyone(rows);
  const policy: PublicSharing = grants.data?.public_sharing ?? 'allowed';
  // Whether a passcode may be added or changed, and whether one may be done
  // without. The second is narrower: a passcode-only policy keeps the first.
  const allowed = policy !== 'none';
  const plainAllowed = policy === 'allowed';
  // What is actually offered when opening it up: the passcode, when policy
  // leaves no choice.
  const passcodeChosen = withPasscode || !plainAllowed;

  const ask = (what: Asking) => {
    open.reset();
    setPasscode('');
    setAsking(what);
  };
  const close = () => {
    setAsking(null);
    setPasscode('');
  };

  const sayPublic = `Anyone on the internet can open ${appName}, without signing in.`;
  const sayPasscode = `Anyone on the internet who has the passcode can open ${appName}, without signing in.`;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)', maxWidth: MEASURE }}>
      <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        {/* Both planes are in this table, so the heading cannot claim only one
            of them. "Who can open this app" over a row that says `owner` reads
            as a mistake — and the R-070/071 distinction is the thing this
            screen exists to make visible, not to paper over. */}
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Who has access</h4>
        <Table
          loading={grants.isPending}
          skeletonRows={2}
          columns={[
            { key: 'who', header: 'Who', width: 'minmax(0,44ch)', render: who },
            { key: 'access', header: 'Access', width: 'minmax(0,28ch)', render: access },
            ...(canManage
              ? [
                  {
                    key: 'actions',
                    header: '',
                    width: '12ch',
                    align: 'right' as const,
                    render: (row: Access) => (
                      <Button
                        variant="secondary"
                        disabled={revoke.isPending}
                        onClick={() => row.grantIDs.forEach((id) => revoke.mutate(id))}
                      >
                        Remove
                      </Button>
                    ),
                  },
                ]
              : []),
          ]}
          rows={byPrincipal(rows)}
        />
        <ShowMore query={paged.query} label="Show more people" />
        {revoke.isError && <Failure error={revoke.error} />}
      </section>

      {/* Whether it is open to everyone is the one fact the sections below
          carry that the table does not spell out, so it is said here instead —
          once the grants are in. Before then either sentence would be a guess. */}
      {!canManage && grants.isPending && <LineSkeleton width="44ch" />}
      {!canManage && !grants.isPending && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          {state === 'public'
            ? sayPublic
            : state === 'passcode'
              ? sayPasscode
              : `Only people given access can open ${appName}.`}
          {!viewOnly && ' You can see who has access but not change it.'}
        </p>
      )}

      {canManage && (
        <Section>
          <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Share with someone</h4>
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            They&rsquo;ll see {appName} the next time they sign in. Pando doesn&rsquo;t send them a
            message.
          </p>
          {/* An account or a group, then which one. Two kinds, so a pair of
              radios rather than a select: both choices stay in view. */}
          <div role="radiogroup" aria-label="Share with" style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-5)' }}>
            <Radio
              name="share-kind"
              value="user"
              label="A person"
              checked={kind === 'user'}
              onChange={() => {
                setKind('user');
                setRecipient(null);
                share.reset();
              }}
            />
            <Radio
              name="share-kind"
              value="group"
              label="A group"
              checked={kind === 'group'}
              onChange={() => {
                setKind('group');
                setRecipient(null);
                share.reset();
              }}
            />
          </div>
          {/* The row wraps rather than overlapping when there is not enough of
              it; the select is sized from its content, not from a spacing
              token. */}
          <div
            style={{
              display: 'flex',
              flexWrap: 'wrap',
              gap: 'var(--space-3)',
              alignItems: 'flex-end',
            }}
          >
            <div style={{ flex: '2 1 24ch' }}>
              <RecipientField
                // A fresh field per kind: text typed while looking for a
                // person is not a group's name.
                key={kind}
                appID={appID}
                kind={kind}
                value={recipient}
                onChange={(c) => {
                  if (share.isError) share.reset();
                  setRecipient(c);
                }}
              />
            </div>
            {roles.isPending ? (
              <Skeleton width="28ch" height="var(--control-input)" />
            ) : (
              <Select
                label="What they can do"
                options={[
                  { value: '', label: USE_ONLY },
                  ...appRoles.map((r) => ({ value: r.id, label: roleChoice(r) })),
                ]}
                value={role}
                onChange={(e) => setRole(e.target.value)}
                style={{ flex: '1 1 28ch' }}
              />
            )}
            <BesideField>
              <Button
                variant="primary"
                onClick={() => recipient && share.mutate(recipient)}
                disabled={!recipient || share.isPending || grants.isPending}
              >
                {share.isPending ? 'Sharing' : 'Share app'}
              </Button>
            </BesideField>
          </div>
          {/* Under the row rather than under the dropdown: as the dropdown's
              helper it made that one field taller, and the row, aligned to
              the bottom, lifted the dropdown off the line the others sit on. */}
          {!roles.isPending && (
            <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
              {describeChoice(appRoles.find((r) => r.id === role))}
            </p>
          )}
          {share.isError && <Failure error={share.error} />}
        </Section>
      )}

      {canManage && (
        <Section>
          {/* R-077 [P], overridden in the heading and kept everywhere else. The
              requirement forbids presenting this as the *bare* word "public" —
              a toggle labeled "Public" and nothing else, which people skim past
              without registering what it does. "Make it public" is what the
              action is called everywhere else in the world, and the consequence
              beside each way of doing it is what the requirement is actually
              protecting. Once it is done, the heading says what is true
              without the word at all. */}
          <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>
            {state === 'private' || grants.isPending ? 'Make it public' : 'Open to everyone'}
          </h4>

          {grants.isPending ? (
            <LineSkeleton width="44ch" />
          ) : state === 'private' && policy !== 'passcode_only' ? (
            // Both ways, as a choice — or, when policy allows neither, both
            // still visible and disabled, so the setting can be found.
            <div
              role="radiogroup"
              aria-label="How to make it public"
              style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}
            >
              <Radio
                name="everyone"
                value="passcode"
                label="With a passcode"
                description={`${sayPasscode} You choose the passcode and give it to them.`}
                checked={withPasscode}
                disabled={!allowed}
                onChange={() => setWithPasscode(true)}
              />
              <Radio
                name="everyone"
                value="public"
                label="Without a passcode"
                description={sayPublic}
                checked={!withPasscode}
                disabled={!allowed}
                onChange={() => setWithPasscode(false)}
              />
            </div>
          ) : (
            // Private under a passcode-only policy offers the one way there
            // is; a choice of one is not a choice.
            <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>
              {state === 'public' ? sayPublic : sayPasscode}
            </p>
          )}

          {!grants.isPending && policy !== 'allowed' && (
            // Visible and disabled, with who to ask. Hiding it would produce a
            // support ticket instead of understanding.
            <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
              {policy === 'none'
                ? 'This installation doesn’t allow sharing with everyone. An administrator can change that in host policy.'
                : 'This installation allows sharing with everyone only behind a passcode. An administrator can change that in host policy.'}
            </p>
          )}

          {/* Which buttons depends on the grants, so none until they arrive:
              "Make it public" on an app that already is would offer the wrong
              thing. */}
          {grants.isPending ? (
            <Skeleton width="16ch" height="var(--control-console)" />
          ) : state === 'private' ? (
            <Button
              variant="secondary"
              disabled={!allowed}
              onClick={() => ask('public')}
              style={{ alignSelf: 'flex-start' }}
            >
              {passcodeChosen ? 'Make it public with a passcode' : 'Make it public'}
            </Button>
          ) : (
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
              {state === 'passcode' ? (
                <>
                  <Button variant="secondary" onClick={() => ask('change-passcode')}>
                    Change passcode
                  </Button>
                  {/* Taking the passcode away is plain public, which only an
                      "allowed" policy permits; the server refuses it otherwise. */}
                  {plainAllowed && (
                    <Button variant="secondary" onClick={() => ask('remove-passcode')}>
                      Remove passcode
                    </Button>
                  )}
                </>
              ) : (
                <Button variant="secondary" disabled={!allowed} onClick={() => ask('add-passcode')}>
                  Add a passcode
                </Button>
              )}
              <Button
                variant="destructive"
                disabled={revoke.isPending}
                onClick={() => anonymous && revoke.mutate(anonymous.id)}
              >
                Make it private again
              </Button>
            </div>
          )}
        </Section>
      )}

      {asking && (
        <EveryoneDialog
          asking={asking}
          withPasscode={passcodeChosen}
          appName={appName}
          passcode={passcode}
          onPasscode={(v) => {
            if (open.isError) open.reset();
            setPasscode(v);
          }}
          pending={open.isPending}
          error={open.isError ? open.error : null}
          onClose={close}
          onConfirm={() => {
            if (asking === 'public') {
              open.mutate({ method: 'post', ...(passcodeChosen ? { passcode } : {}) });
            } else if (anonymous) {
              open.mutate({ method: 'patch', grant: anonymous.id, passcode: asking === 'remove-passcode' ? '' : passcode });
            }
          }}
        />
      )}
    </div>
  );
}

/**
 * The confirmation for every change to who, beyond the people named, can open
 * the app — each one asks, because each one changes who on the internet can
 * reach it. Removing the passcode widens that, so it is confirmed like making
 * it public; changing it shuts out everyone let in by the old one, so that is
 * said before it happens.
 */
function EveryoneDialog({
  asking,
  withPasscode,
  appName,
  passcode,
  onPasscode,
  pending,
  error,
  onClose,
  onConfirm,
}: {
  asking: Exclude<Asking, null>;
  withPasscode: boolean;
  appName: string;
  passcode: string;
  onPasscode: (v: string) => void;
  pending: boolean;
  error: unknown;
  onClose: () => void;
  onConfirm: () => void;
}) {
  const needsPasscode = asking === 'add-passcode' || asking === 'change-passcode' || (asking === 'public' && withPasscode);

  const text: Record<Exclude<Asking, null>, { title: string; description: string; action: string }> = {
    public: withPasscode
      ? {
          title: 'Make this app public with a passcode?',
          description: `Anyone who has the link and the passcode will be able to open ${appName} without signing in. You can undo this at any time.`,
          action: 'Make it public',
        }
      : {
          title: 'Make this app public?',
          description: `Anyone who has the link will be able to open ${appName} without signing in. You can undo this at any time.`,
          action: 'Make it public',
        },
    'add-passcode': {
      title: 'Add a passcode',
      description: `Anyone opening ${appName} will be asked for it first, without signing in.`,
      action: 'Add passcode',
    },
    'change-passcode': {
      title: 'Change the passcode',
      description: `Everyone who entered the old passcode will be asked for the new one the next time they open ${appName}.`,
      action: 'Change passcode',
    },
    'remove-passcode': {
      title: 'Remove the passcode?',
      description: `Anyone who has the link will be able to open ${appName} without signing in or entering a passcode.`,
      action: 'Remove passcode',
    },
  };
  const t = text[asking];
  const short = passcode.length < PASSCODE_MIN;

  return (
    <Dialog
      open
      title={t.title}
      description={t.description}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={pending || (needsPasscode && short)} onClick={onConfirm}>
            {t.action}
          </Button>
        </>
      }
    >
      {needsPasscode ? (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (!pending && !short) onConfirm();
          }}
        >
          {/* Shown as typed, not masked: the person choosing it has to give it
              to somebody, and Pando keeps only a hash, so this is the one time
              it can be read back. */}
          <Input
            label="Passcode"
            mono
            autoFocus
            autoComplete="off"
            spellCheck={false}
            value={passcode}
            onChange={(e) => onPasscode(e.target.value)}
            helper={`At least ${PASSCODE_MIN} characters. Give it to the people you want to let in; Pando doesn’t show it again.`}
            error={error ? messageOf(error) : undefined}
          />
        </form>
      ) : (
        error != null && <Failure error={error} />
      )}
    </Dialog>
  );
}

function Section({ children }: { children: React.ReactNode }) {
  return (
    <section
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-3)',
        paddingTop: 'var(--space-5)',
        borderTop: 'var(--border-width) solid var(--rule)',
      }}
    >
      {children}
    </section>
  );
}

/** One person, group or everyone, and everything they can do with this app. */
interface Access {
  key: string;
  principalKind: string;
  name: string;
  canOpen: boolean;
  /** For everyone: whether a passcode is asked for first. */
  passcode: boolean;
  roles: string[];
  grantIDs: string[];
}

/**
 * One row per person, not one per grant.
 *
 * Owning an app is two grants — control to administer it, data to open it —
 * and the table listed both, so the owner appeared twice with no indication
 * that the two lines were the same person. R-070/071 keeps the planes separate
 * in the model, which is right; it does not follow that somebody reading a list
 * of who has access should be shown the model.
 *
 * Everyone is a row too, when the app is open to them: a list of who has
 * access that leaves out "anyone on the internet" understates it.
 */
function byPrincipal(rows: Grant[]): Access[] {
  const out = new Map<string, Access>();

  for (const row of rows) {
    const key = `${row.principal_kind}:${row.principal_id ?? ''}`;
    const anonymous = row.principal_kind === 'anonymous';
    const entry = out.get(key) ?? {
      key,
      principalKind: row.principal_kind,
      // The name the server resolved, falling back to the identifier only when
      // there is nothing else — a principal deleted since the grant was made.
      name: anonymous
        ? row.passcode
          ? 'Everyone with the passcode'
          : 'Everyone'
        : row.principal_name || row.principal_id || '',
      canOpen: false,
      passcode: Boolean(row.passcode),
      roles: [],
      grantIDs: [],
    };

    entry.grantIDs.push(row.id);
    if (row.plane === 'data') {
      entry.canOpen = true;
    } else {
      entry.roles.push(sentence(row.role_name || row.role_id || 'control'));
    }
    out.set(key, entry);
  }

  return [...out.values()];
}

function who(row: Access): React.ReactNode {
  if (row.principalKind !== 'group') return <span>{row.name}</span>;
  // A group's grant is every member's (R-078), which is worth seeing at a
  // glance beside a list of people.
  return (
    <span style={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'baseline', gap: 'var(--space-2)' }}>
      {row.name}
      <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>Group</span>
    </span>
  );
}

function access(row: Access): React.ReactNode {
  // Everyone opens it without signing in, which is the whole of what that
  // row means; said as the consequence rather than as a role (R-077).
  if (row.principalKind === 'anonymous') {
    return <span>{row.passcode ? 'Can open it with the passcode' : 'Can open it, without signing in'}</span>;
  }
  // Everything this person can do, in one cell. "Can open it" is the data
  // plane; the roles are control (R-070, R-071). Someone who owns the app has
  // both, and that now reads as one person with two capabilities rather than
  // as two people who happen to share a name.
  const parts = [...row.roles, ...(row.canOpen ? ['Can open it'] : [])];
  return (
    <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap', alignItems: 'center' }}>
      {row.roles.map((r) => (
        <Tag key={r}>{r}</Tag>
      ))}
      {row.canOpen && <span>Can open it</span>}
      {parts.length === 0 && <span>No access</span>}
    </div>
  );
}

/** Role names are stored lowercase; the design system sets everything in
 *  sentence case. */
function sentence(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function messageOf(error: unknown): string {
  if (error instanceof RequestFailed) return error.remedy ? `${error.message} ${error.remedy}` : error.message;
  return 'Pando could not reach the server. Check that it is running and try again.';
}

function Failure({ error }: { error: unknown }) {
  const failed = error instanceof RequestFailed ? error : null;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--marker-deep)', margin: 0 }}>
        {failed?.message ?? 'Something went wrong. Try again.'}
      </p>
      {failed?.remedy && (
        <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
          {failed.remedy}
        </p>
      )}
    </div>
  );
}
