// Sharing (R-070, R-071, R-075 – R-077, R-264).
//
// Two sections. **Public access** is the anonymous grant (R-075), as two
// switches: Make public, and under it while on, Require passcode. It is a
// switch and not a choice among private, shared and public because making an
// app public adds to the people it is shared with — it never replaces them
// (R-264). **People and groups** lists everybody else, with opening the app and
// managing it as two columns, because they are two planes (R-070, R-071).
//
// **The anonymous grant never stands on the bare word "public"** (R-077): the
// switch's own line says the consequence, *accessible to anyone on the
// internet*, and turning it on asks first.
//
// **The passcode is shown only while it is being set or changed.** Pando keeps
// its digest and nothing else (R-075a), so afterwards there is nothing to show
// but that one is set.
//
// **When host policy forbids it, the switch is visible but disabled, with a few
// words of why** (R-076) — not hidden. A hidden option produces a support
// ticket instead of understanding.
//
// **Without app.grants.manage, it is read-only.** Anyone who can view the app
// can read who has access to it (the endpoint is app.view); changing it is
// app.grants.manage.

import { useContext, useState, type CSSProperties, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient, type UseMutationResult } from '@tanstack/react-query';
import { Button, Card, Checkbox, Dialog, Icon, IconButton, Input, Select, Skeleton, Switch } from '@design';

import { api, RequestFailed } from '@api/client';
import { format } from '../app/route';
import { InstallVerb, useInstallVerb, usePrincipal } from '../app/principal';
import type { Role } from '../install/Accounts';
import { BesideField } from '../ui/BesideField';
import { copyOrSelect } from '../ui/CopyField';
import { MEASURE } from '../ui/layout';
import { LineSkeleton } from '../ui/Loading';
import { RouteLink, followInPlace } from '../ui/RouteLink';
import { Table } from '../ui/Table';
import { ShowMore, usePaged, type PageOf } from '../ui/paged';
import { RecipientField, type Chosen } from './RecipientField';
import {
  BUILT_IN_APP_ROLES,
  CAN_OPEN,
  everyone,
  initialOf,
  NO_ROLE,
  openRequests,
  orderRoles,
  PASSCODE_MIN,
  people,
  roleChoice,
  roleRequests,
  shareRequests,
  type Grant,
  type GrantRequest,
  type Person,
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

/** The dialog open over Public access, if any. */
type Asking = 'public' | 'add-passcode' | 'change-passcode' | 'remove-passcode' | null;

/** A change to the anonymous grant. */
type EveryoneChange =
  | { method: 'post'; passcode?: string }
  | { method: 'patch'; grant: string; passcode: string };

const heading: CSSProperties = { font: 'var(--type-h4)', color: 'var(--ink)', margin: 0 };
const caption: CSSProperties = { font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 };

export function Sharing({ appID, appName }: { appID: string; appName: string }) {
  const queries = useQueryClient();
  const canManage = useCan(AppVerb.GrantsManage);
  // Whether the screen's own "view but not change" note already covers this.
  const viewOnly = !changesAnything(useContext(AppVerbs));

  const grantsKey = ['apps', appID, 'grants'];
  // A page at a time, by principal (issue #72): an app shared with every
  // person by name holds a grant per person. The grant to everyone, and each
  // person's grants on both planes, arrive together on one page.
  const paged = usePaged<GrantsResponse, Grant>({
    key: grantsKey,
    path: `/apps/${appID}/grants`,
    rows: (p) => p.grants,
  });
  const loading = paged.query.isPending;
  const rows = paged.rows;
  const policy: PublicSharing = paged.query.data?.pages[0]?.public_sharing ?? 'allowed';

  // In order: a failure part way leaves what succeeded in place, and the
  // table, refreshed either way, shows exactly how far it got.
  const send = useMutation({
    mutationFn: async (requests: GrantRequest[]) => {
      for (const r of requests) {
        if (r.method === 'post') await api.post<unknown>(`/apps/${appID}/grants`, r.body);
        else if (r.method === 'patch') await api.patch<unknown>(`/apps/${appID}/grants/${r.grant}`, r.body);
        else await api.del(`/apps/${appID}/grants/${r.grant}`);
      }
    },
    onSettled: () => queries.invalidateQueries({ queryKey: grantsKey }),
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)', maxWidth: MEASURE }}>
      <PublicAccess
        appID={appID}
        appName={appName}
        grants={rows}
        loading={loading}
        policy={policy}
        canManage={canManage}
        onChanged={() => queries.invalidateQueries({ queryKey: grantsKey })}
      />
      <People
        appID={appID}
        grants={rows}
        loading={loading}
        canManage={canManage}
        send={send}
        more={<ShowMore query={paged.query} label="Show more people" />}
      />
      {!canManage && !viewOnly && <p style={caption}>You can see who has access but not change it.</p>}
    </div>
  );
}

/** The anonymous grant, as two switches. */
function PublicAccess({
  appID,
  appName,
  grants,
  loading,
  policy,
  canManage,
  onChanged,
}: {
  appID: string;
  appName: string;
  grants: Grant[];
  loading: boolean;
  policy: PublicSharing;
  canManage: boolean;
  onChanged: () => void;
}) {
  const [asking, setAsking] = useState<Asking>(null);
  const [passcode, setPasscode] = useState('');

  const anonymous = grants.find((g) => g.principal_kind === 'anonymous');
  const state = everyone(grants);

  const close = () => {
    setAsking(null);
    setPasscode('');
  };
  const change = useMutation({
    mutationFn: (c: EveryoneChange) =>
      c.method === 'post'
        ? api.post<unknown>(`/apps/${appID}/grants`, {
            plane: 'data',
            principal_kind: 'anonymous',
            ...(c.passcode ? { passcode: c.passcode } : {}),
          })
        : api.patch<unknown>(`/apps/${appID}/grants/${c.grant}`, { passcode: c.passcode }),
    onSuccess: close,
    onSettled: onChanged,
  });
  // Turning it off narrows who can reach the app, so it is not asked about.
  const makePrivate = useMutation({
    mutationFn: (grant: string) => api.del(`/apps/${appID}/grants/${grant}`),
    onSettled: onChanged,
  });

  const ask = (what: Asking) => {
    change.reset();
    setPasscode('');
    setAsking(what);
  };

  // A passcode-only host makes the passcode part of making it public.
  const passcodeRequired = policy === 'passcode_only';
  const busy = change.isPending || makePrivate.isPending;

  return (
    <section aria-labelledby="public-access" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <h4 id="public-access" style={heading}>
        Public access
      </h4>
      <Card padding="none">
        {loading ? (
          <div style={{ padding: 'var(--space-4)' }}>
            <LineSkeleton width="44ch" />
          </div>
        ) : (
          <>
            <SwitchRow
              label="Make public"
              line="Accessible to anyone on the internet"
              note={policy === 'none' && state === 'private' ? 'Not allowed on this host' : undefined}
              checked={state !== 'private'}
              disabled={!canManage || busy || (policy === 'none' && state === 'private')}
              onChange={(on) => {
                if (on) ask('public');
                else if (anonymous) makePrivate.mutate(anonymous.id);
              }}
            />
            {state !== 'private' && (
              <SwitchRow
                divided
                label="Require passcode"
                line="Visitors need the passcode to open it"
                note={passcodeRequired ? 'Required on this host' : undefined}
                checked={state === 'passcode'}
                // Taking the passcode away is plain public, which only an
                // "allowed" policy permits; the server refuses it otherwise.
                disabled={!canManage || busy || (passcodeRequired && state === 'passcode') || policy === 'none'}
                onChange={(on) => ask(on ? 'add-passcode' : 'remove-passcode')}
                extra={
                  state === 'passcode' && (
                    <>
                      {/* Pando keeps a digest, not the passcode (R-075a):
                          there is nothing to show but that one is set. */}
                      <span aria-label="Passcode set" style={{ font: 'var(--type-code)', color: 'var(--ink-secondary)' }}>
                        ••••••••
                      </span>
                      {canManage && (
                        <Button variant="ghost" onClick={() => ask('change-passcode')}>
                          Change
                        </Button>
                      )}
                    </>
                  )
                }
              />
            )}
          </>
        )}
      </Card>
      {makePrivate.isError && <Failure error={makePrivate.error} />}

      {asking && (
        <EveryoneDialog
          asking={asking}
          withPasscode={passcodeRequired}
          appName={appName}
          passcode={passcode}
          onPasscode={(v) => {
            if (change.isError) change.reset();
            setPasscode(v);
          }}
          pending={change.isPending}
          error={change.isError ? change.error : null}
          onClose={close}
          onConfirm={() => {
            if (asking === 'public') {
              change.mutate({ method: 'post', ...(passcodeRequired ? { passcode } : {}) });
            } else if (anonymous) {
              change.mutate({ method: 'patch', grant: anonymous.id, passcode: asking === 'remove-passcode' ? '' : passcode });
            }
          }}
        />
      )}
    </section>
  );
}

/** A setting and its switch, on one line of the Public access card. */
function SwitchRow({
  label,
  line,
  note,
  checked,
  disabled,
  divided = false,
  extra,
  onChange,
}: {
  label: string;
  line: string;
  /** Why it cannot be changed here, beside the switch. */
  note?: string;
  checked: boolean;
  disabled: boolean;
  divided?: boolean;
  extra?: ReactNode;
  onChange: (on: boolean) => void;
}) {
  return (
    <div
      style={{
        display: 'flex',
        flexWrap: 'wrap',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: 'var(--space-3)',
        padding: 'var(--space-4)',
        borderTop: divided ? 'var(--border-width) solid var(--rule)' : undefined,
      }}
    >
      <span style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>{label}</span>
        <span style={caption}>{line}</span>
      </span>
      <span style={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
        {extra}
        {note && <span style={caption}>{note}</span>}
        <Switch aria-label={label} checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      </span>
    </div>
  );
}

/** People, groups and tokens: the share bar, then who has what. */
function People({
  appID,
  grants,
  loading,
  canManage,
  send,
  more,
}: {
  appID: string;
  grants: Grant[];
  loading: boolean;
  canManage: boolean;
  send: UseMutationResult<void, Error, GrantRequest[]>;
  more: ReactNode;
}) {
  const me = usePrincipal().data;
  // Groups and roles is shown to holders of either verb (AdminConsole).
  const canView = useInstallVerb(InstallVerb.View);
  const canManageUsers = useInstallVerb(InstallVerb.UsersManage);
  const canSeeRoles = canView || canManageUsers;
  const [recipient, setRecipient] = useState<Chosen | null>(null);
  // The least access, not the second-least: sharing an app most often means
  // "you can use this"; anything more is a deliberate choice.
  const [access, setAccess] = useState('');

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
  const roleOptions = appRoles.map((r) => ({ value: r.id, label: roleChoice(r) }));

  const rows = people(grants, me);
  const identityHref = format({ view: 'admin', section: 'identity' });

  const share = () => {
    if (!recipient) return;
    send.mutate(shareRequests(grants, recipient, access || null), {
      onSuccess: () => {
        setRecipient(null);
        setAccess('');
      },
    });
  };

  return (
    <section aria-labelledby="people-groups" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <h4 id="people-groups" style={heading}>
        People and groups
      </h4>

      {canManage && (
        <Card padding="sm">
          <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-end', gap: 'var(--space-3)' }}>
            <div style={{ flex: '2 1 22rem', minWidth: 0 }}>
              <RecipientField
                appID={appID}
                kind="any"
                value={recipient}
                onChange={(c) => {
                  if (send.isError) send.reset();
                  setRecipient(c);
                }}
              />
            </div>
            {roles.isPending ? (
              <Skeleton width="16rem" height="var(--control-input)" />
            ) : (
              <Select
                label="Access"
                options={[{ value: '', label: CAN_OPEN }, ...roleOptions]}
                value={access}
                onChange={(e) => setAccess(e.target.value)}
                style={{ flex: '0 1 16rem' }}
              />
            )}
            <BesideField>
              <Button variant="primary" onClick={share} disabled={!recipient || send.isPending || loading}>
                {send.isPending ? 'Sharing' : 'Share'}
              </Button>
            </BesideField>
          </div>
        </Card>
      )}
      {send.isError && <Failure error={send.error} />}

      <Table
        loading={loading}
        skeletonRows={2}
        empty={<p style={{ ...caption, padding: 'var(--space-3)' }}>Not shared with anybody yet.</p>}
        columns={[
          { key: 'who', header: 'Who', width: 'minmax(0,1fr)', render: (p: Person) => <Who person={p} /> },
          {
            key: 'opens',
            header: 'Opens it',
            width: '10ch',
            render: (p: Person) =>
              canManage && !p.you ? (
                <Checkbox
                  aria-label={`${p.name} can open it`}
                  checked={p.opens}
                  disabled={send.isPending}
                  onChange={(e) => send.mutate(openRequests(grants, p, e.target.checked))}
                />
              ) : (
                <Opens opens={p.opens} />
              ),
          },
          {
            key: 'manages',
            header: 'Manages it',
            width: '20ch',
            render: (p: Person) =>
              canManage && !p.you ? (
                <Select
                  aria-label={`${p.name}’s role`}
                  options={[
                    { value: '', label: NO_ROLE },
                    // A role the list does not offer — it could not be read —
                    // is still the one they hold.
                    ...(p.role && !roleOptions.some((o) => o.value === p.role?.id)
                      ? [{ value: p.role.id, label: p.role.name }]
                      : []),
                    ...roleOptions,
                  ]}
                  value={p.role?.id ?? ''}
                  disabled={send.isPending}
                  onChange={(e) => send.mutate(roleRequests(grants, p, e.target.value))}
                />
              ) : (
                <span style={{ color: p.role ? 'var(--ink)' : 'var(--ink-secondary)' }}>{p.role?.name ?? NO_ROLE}</span>
              ),
          },
          ...(canManage
            ? [
                {
                  key: 'remove',
                  header: '',
                  width: 'var(--space-6)',
                  align: 'right' as const,
                  // Not on your own row: taking your own access away is how
                  // an app ends up with nobody to manage it.
                  render: (p: Person) =>
                    p.you ? null : (
                      <IconButton
                        label={`Remove ${p.name}`}
                        disabled={send.isPending}
                        onClick={() => send.mutate(p.grantIDs.map((grant) => ({ method: 'delete' as const, grant })))}
                      >
                        <Icon name="x" />
                      </IconButton>
                    ),
                },
              ]
            : []),
        ]}
        // Keyed by `id`, the principal's: prefixed (usr_, grp_, tok_), so unique across kinds.
        rows={rows}
      />
      {more}

      <p style={caption}>
        People with the Administrator or App manager role can also manage every app on this host.{' '}
        {canSeeRoles ? (
          <RouteLink
            href={identityHref}
            onFollow={() => followInPlace(identityHref)}
            style={{ color: 'var(--ink-secondary)', textDecoration: 'underline' }}
          >
            Groups and roles
          </RouteLink>
        ) : null}
      </p>
    </section>
  );
}

/** Opening the app, said where it cannot be changed. */
function Opens({ opens }: { opens: boolean }) {
  if (!opens) return <span style={{ color: 'var(--ink-secondary)' }}>No</span>;
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)' }}>
      <Icon name="check" color="var(--ink)" />
      Yes
    </span>
  );
}

/** Icons for what is not a person. A group and a token have no initial. */
const TILE: Record<'group' | 'token', string> = { group: 'users', token: 'key-round' };

function Who({ person }: { person: Person }) {
  const mark: CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    flex: '0 0 auto',
    width: 'var(--space-5)',
    height: 'var(--space-5)',
    background: 'var(--paper-sunken)',
    border: 'var(--border-width) solid var(--rule)',
    font: 'var(--type-label)',
    color: 'var(--ink-secondary)',
  };
  // "You" needs no second line; a group or token says what it is.
  const second = person.you ? '' : person.kind === 'group' ? 'Group' : person.kind === 'token' ? 'Service token' : '';
  return (
    <span style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)', minWidth: 0 }}>
      {person.kind === 'user' ? (
        <span aria-hidden="true" data-avatar style={{ ...mark, borderRadius: 'var(--radius-pill)' }}>
          {initialOf(person.name)}
        </span>
      ) : (
        <span aria-hidden="true" style={{ ...mark, borderRadius: 'var(--radius-sm)' }}>
          <Icon name={TILE[person.kind]} />
        </span>
      )}
      <span style={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
        <span
          style={{
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            font: person.kind === 'token' ? 'var(--type-code-sm)' : 'var(--type-body-ui)',
          }}
        >
          {person.you ? 'You' : person.name}
        </span>
        {second && <span style={caption}>{second}</span>}
      </span>
    </span>
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
  /** Making it public needs a passcode too: host policy asks for one. */
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
          description: `Anyone on the internet with the passcode will be able to open ${appName} without signing in.`,
          action: 'Make public',
        }
      : {
          title: 'Make this app public?',
          description: `Anyone on the internet will be able to open ${appName} without signing in.`,
          action: 'Make public',
        },
    'add-passcode': {
      title: 'Require a passcode',
      description: `Visitors will be asked for it before ${appName} opens.`,
      action: 'Require passcode',
    },
    'change-passcode': {
      title: 'Change the passcode',
      description: `Everyone who entered the old passcode will be asked for the new one.`,
      action: 'Change passcode',
    },
    'remove-passcode': {
      title: 'Stop requiring a passcode?',
      description: `Anyone on the internet will be able to open ${appName} without one.`,
      action: 'Stop requiring it',
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
          <PasscodeField value={passcode} onChange={onPasscode} error={error ? messageOf(error) : undefined} />
        </form>
      ) : (
        error != null && <Failure error={error} />
      )}
    </Dialog>
  );
}

/**
 * The passcode, while it is being typed: shown by default, because the person
 * choosing it has to give it to somebody, with a toggle to hide it and Copy.
 * This is the one time it can be read: Pando keeps only a digest (R-075a).
 */
export function PasscodeField({ value, onChange, error }: { value: string; onChange: (v: string) => void; error?: string }) {
  const [hidden, setHidden] = useState(false);
  const [copied, setCopied] = useState(false);
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <div style={{ display: 'flex', alignItems: 'flex-end', gap: 'var(--space-2)' }}>
        <Input
          label="Passcode"
          type={hidden ? 'password' : 'text'}
          mono
          autoFocus
          autoComplete="off"
          spellCheck={false}
          value={value}
          aria-invalid={error ? true : undefined}
          onChange={(e) => {
            setCopied(false);
            onChange(e.target.value);
          }}
          style={{ flex: '1 1 auto' }}
        />
        <BesideField>
          <IconButton label={hidden ? 'Show passcode' : 'Hide passcode'} onClick={() => setHidden(!hidden)}>
            <Icon name={hidden ? 'eye' : 'eye-off'} />
          </IconButton>
          <Button
            variant="secondary"
            disabled={value === ''}
            onClick={() => void copyOrSelect(value, null).then((how) => setCopied(how === 'copied'))}
          >
            {copied ? 'Copied' : 'Copy'}
          </Button>
        </BesideField>
      </div>
      <span style={{ ...caption, color: error ? 'var(--marker-deep)' : 'var(--ink-secondary)' }}>
        {error ?? `At least ${PASSCODE_MIN} characters. Not shown again.`}
      </span>
    </div>
  );
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
      {failed?.remedy && <p style={caption}>{failed.remedy}</p>}
    </div>
  );
}
