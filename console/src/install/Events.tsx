// Events — subscriptions to what happens in Pando, and where each one sends
// (issue #50, R-364 – R-374).
//
// A subscription is a filter and a destination: a webhook Pando signs, or a
// notification channel an administrator set up (Slack, Teams, Discord, email,
// ntfy). Anybody who administers an app may subscribe to it; an install-wide
// subscription, and seeing everybody's, needs install.events.manage.
//
// The signing key is shown once, at creation, in the dialog that made it, the
// way a token is. Pando cannot show it again, and the screen says so where the
// key is rather than in a help page.

import { useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Banner,
  Button,
  Checkbox,
  CodeBlock,
  Dialog,
  EmptyState,
  InlineCode,
  Input,
  Radio,
  Select,
  StatusIndicator,
  Tag,
} from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { Quiet, Screen, messageOf } from './Accounts';
import { Table } from '../ui/Table';
import { ShowMore, usePaged, type PageOf } from '../ui/paged';
import { relative } from '../ui/time';
import { Disclosure } from '../ui/Disclosure';
import { deliveriesInterval } from '../ui/polling';

interface EventDef {
  name: string;
  scope: 'app' | 'install';
  summary: string;
  fields: { name: string; description: string }[];
}

interface Destination {
  id: string;
  kind: string;
  audience: 'people' | 'channel';
}

interface Subscription {
  id: string;
  owner_id: string;
  owner_name?: string;
  app_id?: string;
  app_name?: string;
  events: string[];
  destination: 'webhook' | 'notify';
  url?: string;
  adapter_id?: string;
  description: string;
  enabled: boolean;
  disabled_reason?: string;
  consecutive_failures: number;
  created_at: string;
  signing_key?: string;
  owner_token_id?: string;
  method?: string;
  content_type?: string;
  payload_template?: string;
  header_names?: string[];
}

interface Delivery {
  id: string;
  event_id: string;
  event: string;
  status: 'pending' | 'succeeded' | 'failed';
  attempts: number;
  next_attempt_at?: string;
  last_status_code?: number;
  last_error?: string;
  created_at: string;
  attempt_log?: {
    attempt: number;
    attempted_at: string;
    status_code?: number;
    error?: string;
    duration_ms: number;
  }[];
  payload?: unknown;
}

const KIND_NAMES: Record<string, string> = {
  console: 'Console',
  smtp: 'Email',
  slack: 'Slack',
  teams: 'Microsoft Teams',
  discord: 'Discord',
  ntfy: 'ntfy',
};

function destinationName(d: Destination | undefined, id: string): string {
  if (!d) return id;
  return `${KIND_NAMES[d.kind] ?? d.kind} (${d.id})`;
}

function useCatalog() {
  return useQuery({
    queryKey: ['events'],
    queryFn: () => api.get<{ events: EventDef[]; destinations: Destination[] }>('/events'),
    staleTime: 60_000,
  });
}

export function Events({ canManageAll, apps }: { canManageAll: boolean; apps: App[] }) {
  const [creating, setCreating] = useState(false);
  return (
    <Screen
      heading="Events"
      action={
        <Button variant="primary" onClick={() => setCreating(true)}>
          New subscription
        </Button>
      }
    >
      <Quiet>
        Send what happens in Pando — deploys, failures, sign-ins, backups — to a webhook, or to Slack,
        Teams, Discord, email or ntfy. Which of Pando&rsquo;s own notifications reach you is under
        Settings. Each app&rsquo;s own events are on its Events tab.
      </Quiet>
      <Subscriptions
        apps={apps}
        canManageAll={canManageAll}
        creating={creating}
        onCreating={setCreating}
      />
    </Screen>
  );
}

/**
 * Subscriptions, as a table with the dialogs that make, show and change one.
 * The Events screen shows the caller's across the installation; an app's
 * Events tab passes `app` and shows that app's, and makes new ones about it.
 */
export function Subscriptions({
  apps,
  app,
  canManageAll,
  creating,
  onCreating,
}: {
  apps: App[];
  app?: App;
  canManageAll: boolean;
  creating: boolean;
  onCreating: (open: boolean) => void;
}) {
  const [everyone, setEveryone] = useState(false);
  const [created, setCreated] = useState<Subscription | null>(null);
  const [open, setOpen] = useState<string | null>(null);

  const catalog = useCatalog();
  // Newest first, a page at a time (issue #72).
  const paged = usePaged<{ subscriptions: Subscription[] | null } & PageOf, Subscription>({
    key: ['subscriptions'],
    path: '/subscriptions',
    rows: (p) => p.subscriptions,
    params: { everyone: everyone ? 'true' : undefined, app_id: app?.id },
  });
  const subs = paged.query;

  const destinations = catalog.data?.destinations ?? [];
  const rows = paged.rows;

  return (
    <>
      {canManageAll && (
        <Checkbox
          label="Show everybody's subscriptions"
          checked={everyone}
          onChange={(e) => setEveryone(e.target.checked)}
        />
      )}

      {subs.isError && <Quiet>{messageOf(subs.error)}</Quiet>}

      <Table
        loading={subs.isPending}
        onRowClick={(row: Subscription) => setOpen(row.id)}
        empty={
          <EmptyState
            heading="No subscriptions yet"
            action={
              <Button variant="primary" onClick={() => onCreating(true)}>
                New subscription
              </Button>
            }
          >
            A subscription sends the events you choose, as they happen, to somewhere you will see them.
          </EmptyState>
        }
        columns={[
          ...(app
            ? []
            : [
                {
                  key: 'about',
                  header: 'About',
                  width: 'minmax(0,20ch)',
                  render: (row: Subscription) => (row.app_id ? row.app_name || row.app_id : 'Whole installation'),
                },
              ]),
          {
            key: 'events',
            header: 'Events',
            width: 'minmax(0,1fr)',
            mono: true,
            render: (row: Subscription) => row.events.join(', '),
          },
          {
            key: 'to',
            header: 'Sent to',
            width: 'minmax(0,28ch)',
            muted: true,
            render: (row: Subscription) =>
              row.destination === 'webhook'
                ? row.url
                : destinationName(
                    destinations.find((d) => d.id === row.adapter_id),
                    row.adapter_id ?? '',
                  ),
          },
          {
            key: 'status',
            header: 'Status',
            width: '16ch',
            render: (row: Subscription) => <SubscriptionStatus sub={row} />,
          },
          ...(everyone
            ? [{ key: 'owner_name', header: 'Owner', width: 'minmax(0,16ch)', muted: true }]
            : []),
        ]}
        rows={rows}
      />
      <ShowMore query={subs} label="Show more subscriptions" />

      {creating && (
        <CreateSubscription
          apps={app ? [app] : apps}
          fixedApp={app}
          canManageAll={canManageAll}
          catalog={catalog.data?.events ?? []}
          destinations={destinations}
          onClose={() => onCreating(false)}
          onCreated={(s) => {
            onCreating(false);
            if (s.signing_key) setCreated(s);
          }}
        />
      )}
      {created && <SigningKey sub={created} onClose={() => setCreated(null)} />}
      {open && (
        <SubscriptionDetail
          id={open}
          destinations={destinations}
          onClose={() => setOpen(null)}
          onKey={(s) => setCreated(s)}
        />
      )}
    </>
  );
}

function SubscriptionStatus({ sub }: { sub: Subscription }) {
  if (!sub.enabled) return <StatusIndicator status="stopped" label="Off" />;
  if (sub.consecutive_failures > 0) return <StatusIndicator status="failed" label="Failing" />;
  return <StatusIndicator status="running" label="On" />;
}

/** Event names grouped by what they are about, in catalog order. */
function groupsOf(catalog: EventDef[]): [string, EventDef[]][] {
  const groups = new Map<string, EventDef[]>();
  for (const e of catalog) {
    const prefix = e.name.split('.')[0] ?? e.name;
    groups.set(prefix, [...(groups.get(prefix) ?? []), e]);
  }
  return [...groups.entries()];
}

function CreateSubscription({
  apps,
  fixedApp,
  canManageAll,
  catalog,
  destinations,
  onClose,
  onCreated,
}: {
  apps: App[];
  /** Made from an app's own Events tab: about that app, without asking. */
  fixedApp?: App;
  canManageAll: boolean;
  catalog: EventDef[];
  destinations: Destination[];
  onClose: () => void;
  onCreated: (s: Subscription) => void;
}) {
  const queries = useQueryClient();
  const [appID, setAppID] = useState(fixedApp?.id ?? (canManageAll ? '' : (apps[0]?.id ?? '')));
  const [request, setRequest] = useState<RequestOptions>(DEFAULT_REQUEST);
  const [chosen, setChosen] = useState<string[]>([]);
  const [kind, setKind] = useState<'webhook' | 'notify'>('webhook');
  const [url, setURL] = useState('');
  const [adapterID, setAdapterID] = useState(destinations[0]?.id ?? '');
  const [description, setDescription] = useState('');

  // An install event never reaches an app subscription, so it is not offered
  // for one: a box that can be ticked and never fires is a broken promise.
  const offered = useMemo(
    () => catalog.filter((e) => !appID || e.scope === 'app'),
    [catalog, appID],
  );
  const everything = chosen.includes('*');

  const toggle = (pattern: string, on: boolean) =>
    setChosen((cur) => (on ? [...cur.filter((c) => c !== pattern), pattern] : cur.filter((c) => c !== pattern)));

  const create = useMutation({
    mutationFn: () =>
      api.post<Subscription>('/subscriptions', {
        app_id: appID || undefined,
        events: chosen,
        destination: kind,
        url: kind === 'webhook' ? url : undefined,
        adapter_id: kind === 'notify' ? adapterID : undefined,
        description,
        ...(kind === 'webhook' ? requestBody(request, true) : {}),
      }),
    onSuccess: (s) => {
      void queries.invalidateQueries({ queryKey: ['subscriptions'] });
      onCreated(s);
    },
  });

  const aboutOptions = [
    ...(canManageAll ? [{ value: '', label: 'Whole installation' }] : []),
    ...apps.map((a) => ({ value: a.id, label: a.name })),
  ];

  const ready =
    chosen.length > 0 &&
    (kind === 'webhook' ? url.trim() !== '' && !parseHeaders(request.headers).problem : adapterID !== '') &&
    (Boolean(fixedApp) || aboutOptions.length > 0);

  return (
    <Dialog
      open
      width={720}
      title="New subscription"
      description="Choose what to hear about and where Pando sends it."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!ready || create.isPending} onClick={() => create.mutate()}>
            {create.isPending ? 'Subscribing' : 'Subscribe'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        {create.isError && <Banner tone="failed">{messageOf(create.error)}</Banner>}

        {!fixedApp && (
          <Select
            label="About"
            value={appID}
            options={aboutOptions}
            helper={
              appID
                ? 'Events about this app. You hear about them while you can see the app.'
                : 'Every app, and the installation itself: sign-ins, policy, adapters, upgrades.'
            }
            onChange={(e) => {
              setAppID(e.target.value);
              setChosen([]);
            }}
          />
        )}

        <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
            Events
          </legend>
          <Checkbox
            label="Everything"
            description="Every event, including ones a later Pando adds."
            checked={everything}
            onChange={(e) => setChosen(e.target.checked ? ['*'] : [])}
          />
          {!everything &&
            groupsOf(offered).map(([prefix, defs]) => (
              <div key={prefix} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
                <Checkbox
                  label={`All ${prefix} events`}
                  checked={chosen.includes(`${prefix}.*`)}
                  onChange={(e) => toggle(`${prefix}.*`, e.target.checked)}
                />
                {!chosen.includes(`${prefix}.*`) && (
                  <div
                    style={{
                      display: 'grid',
                      gridTemplateColumns: 'repeat(auto-fill, minmax(32ch, 1fr))',
                      gap: 'var(--space-2)',
                      paddingLeft: 'var(--space-5)',
                    }}
                  >
                    {defs.map((d) => (
                      <Checkbox
                        key={d.name}
                        label={d.name}
                        description={d.summary}
                        checked={chosen.includes(d.name)}
                        onChange={(e) => toggle(d.name, e.target.checked)}
                      />
                    ))}
                  </div>
                )}
              </div>
            ))}
        </fieldset>

        <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
            Send to
          </legend>
          <Radio
            name="destination"
            label="A webhook"
            description="Pando posts each event as JSON to a URL, signed so the receiver can tell it came from Pando."
            checked={kind === 'webhook'}
            onChange={() => setKind('webhook')}
          />
          <Radio
            name="destination"
            label="A notification channel"
            description={
              destinations.length > 0
                ? 'Slack, Teams, Discord, email or ntfy, as set up under Adapters. Email goes to you.'
                : 'None is set up yet. An administrator adds one under Adapters.'
            }
            disabled={destinations.length === 0}
            checked={kind === 'notify'}
            onChange={() => setKind('notify')}
          />
          {kind === 'webhook' ? (
            <Input
              label="Webhook URL"
              value={url}
              placeholder="https://example.com/hooks/pando"
              helper="Pando retries for about a day if it does not answer with a 2xx status."
              onChange={(e) => setURL(e.target.value)}
            />
          ) : null}
          {kind === 'webhook' ? (
            <Disclosure show="Change the request" hide="Hide the request">
              <RequestFields value={request} onChange={setRequest} />
            </Disclosure>
          ) : (
            <Select
              label="Channel"
              value={adapterID}
              options={destinations.map((d) => ({ value: d.id, label: destinationName(d, d.id) }))}
              onChange={(e) => setAdapterID(e.target.value)}
            />
          )}
        </fieldset>

        <Input
          label="Description"
          value={description}
          helper="Optional. What this subscription is for, for whoever finds it later."
          onChange={(e) => setDescription(e.target.value)}
        />
      </div>
    </Dialog>
  );
}

/** The signing key, the one time Pando can show it. */
function SigningKey({ sub, onClose }: { sub: Subscription; onClose: () => void }) {
  return (
    <Dialog
      open
      width={640}
      title="Save the signing key"
      description={`Pando signs every delivery to ${sub.url} with this key.`}
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={onClose}>
          I&rsquo;ve saved it
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <Banner tone="info">
          This is the only time Pando shows this key. If you lose it, rotate it from the subscription to
          get a new one.
        </Banner>
        <CodeBlock copyable lines={sub.signing_key ?? ''} />
        <Quiet>
          Check each delivery&rsquo;s <InlineCode>Pando-Signature</InlineCode> header against it. The
          reference under API and tools shows how.
        </Quiet>
      </div>
    </Dialog>
  );
}

function SubscriptionDetail({
  id,
  destinations,
  onClose,
  onKey,
}: {
  id: string;
  destinations: Destination[];
  onClose: () => void;
  onKey: (s: Subscription) => void;
}) {
  const queries = useQueryClient();
  const [openDelivery, setOpenDelivery] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [editing, setEditing] = useState(false);

  const sub = useQuery({
    queryKey: ['subscription', id],
    queryFn: () => api.get<Subscription>(`/subscriptions/${id}`),
  });
  const deliveries = useQuery({
    queryKey: ['deliveries', id],
    queryFn: () => api.get<{ deliveries: Delivery[] }>(`/subscriptions/${id}/deliveries`),
    // Pending deliveries move on their own; the list follows them, as often
    // as they can move (deliveriesInterval) rather than every three seconds
    // through hours of retries.
    refetchInterval: (q) => deliveriesInterval(q.state.data?.deliveries),
  });

  const refresh = () => {
    void queries.invalidateQueries({ queryKey: ['subscriptions'] });
    void queries.invalidateQueries({ queryKey: ['subscription', id] });
    void queries.invalidateQueries({ queryKey: ['deliveries', id] });
  };

  const setEnabled = useMutation({
    mutationFn: (enabled: boolean) => api.patch<Subscription>(`/subscriptions/${id}`, { enabled }),
    onSuccess: refresh,
  });
  const test = useMutation({
    mutationFn: () => api.post<Delivery>(`/subscriptions/${id}/test`, {}),
    onSuccess: refresh,
  });
  const rotate = useMutation({
    mutationFn: () => api.post<Subscription>(`/subscriptions/${id}/signing-key`, {}),
    onSuccess: (s) => onKey(s),
  });
  const remove = useMutation({
    mutationFn: () => api.del<unknown>(`/subscriptions/${id}`),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['subscriptions'] });
      onClose();
    },
  });

  const s = sub.data;
  const failure = setEnabled.error ?? test.error ?? rotate.error ?? remove.error;

  return (
    <Dialog
      open
      width={880}
      title={s ? (s.app_id ? `${s.app_name || s.app_id} events` : 'Installation events') : 'Subscription'}
      description={s?.description || undefined}
      onClose={onClose}
      footer={
        s && (
          <>
            {confirmDelete ? (
              <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {remove.isPending ? 'Deleting' : 'Delete subscription and its log'}
              </Button>
            ) : (
              <Button variant="ghost" onClick={() => setConfirmDelete(true)}>
                Delete
              </Button>
            )}
            {s.destination === 'webhook' && (
              <>
                <Button variant="secondary" onClick={() => setEditing(true)}>
                  Change the request
                </Button>
                <Button variant="secondary" disabled={rotate.isPending} onClick={() => rotate.mutate()}>
                  Rotate signing key
                </Button>
              </>
            )}
            <Button variant="secondary" disabled={setEnabled.isPending} onClick={() => setEnabled.mutate(!s.enabled)}>
              {s.enabled ? 'Turn off' : 'Turn on'}
            </Button>
            <Button variant="primary" disabled={!s.enabled || test.isPending} onClick={() => test.mutate()}>
              {test.isPending ? 'Sending test' : 'Send test'}
            </Button>
          </>
        )
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {sub.isError && <Quiet>{messageOf(sub.error)}</Quiet>}
        {failure && <Banner tone="failed">{messageOf(failure)}</Banner>}
        {s && !s.enabled && s.disabled_reason && <Banner tone="failed">{s.disabled_reason}</Banner>}

        {s && (
          <dl
            style={{
              display: 'grid',
              gridTemplateColumns: 'max-content 1fr',
              gap: 'var(--space-2) var(--space-4)',
              margin: 0,
              font: 'var(--type-body-ui)',
            }}
          >
            <dt style={{ color: 'var(--ink-secondary)' }}>Status</dt>
            <dd style={{ margin: 0 }}>
              <SubscriptionStatus sub={s} />
            </dd>
            <dt style={{ color: 'var(--ink-secondary)' }}>Events</dt>
            <dd style={{ margin: 0, display: 'flex', flexWrap: 'wrap', gap: 'var(--space-1)' }}>
              {s.events.map((e) => (
                <Tag key={e} mono>
                  {e}
                </Tag>
              ))}
            </dd>
            <dt style={{ color: 'var(--ink-secondary)' }}>Sent to</dt>
            <dd style={{ margin: 0, overflowWrap: 'anywhere' }}>
              {s.destination === 'webhook'
                ? s.url
                : destinationName(
                    destinations.find((d) => d.id === s.adapter_id),
                    s.adapter_id ?? '',
                  )}
            </dd>
            {s.destination === 'webhook' && (
              <>
                <dt style={{ color: 'var(--ink-secondary)' }}>Request</dt>
                <dd style={{ margin: 0, display: 'flex', flexWrap: 'wrap', gap: 'var(--space-1)' }}>
                  <Tag mono>{s.method || 'POST'}</Tag>
                  <Tag mono>{s.content_type || 'application/json'}</Tag>
                  {(s.header_names ?? []).map((h) => (
                    <Tag key={h} mono>
                      {h}
                    </Tag>
                  ))}
                  {s.payload_template ? <Tag>Own body</Tag> : <Tag>Pando&rsquo;s event</Tag>}
                </dd>
              </>
            )}
            <dt style={{ color: 'var(--ink-secondary)' }}>Owner</dt>
            <dd style={{ margin: 0 }}>{s.owner_name || s.owner_id}</dd>
            <dt style={{ color: 'var(--ink-secondary)' }}>ID</dt>
            <dd style={{ margin: 0, font: 'var(--type-mono)' }}>{s.id}</dd>
          </dl>
        )}

        <h4 style={{ font: 'var(--type-h4)', margin: 'var(--space-2) 0 0' }}>Recent deliveries</h4>
        <Table
          loading={deliveries.isPending}
          onRowClick={(row: Delivery) => setOpenDelivery(openDelivery === row.id ? null : row.id)}
          empty={<Quiet>Nothing has been sent yet. Send a test to check the endpoint.</Quiet>}
          columns={[
            { key: 'event', header: 'Event', width: 'minmax(0,22ch)', mono: true },
            {
              key: 'status',
              header: 'Status',
              width: '18ch',
              render: (row: Delivery) => <DeliveryStatus d={row} />,
            },
            {
              key: 'attempts',
              header: 'Attempts',
              width: '10ch',
              align: 'right',
              muted: true,
            },
            {
              key: 'last_error',
              header: 'Last answer',
              width: 'minmax(0,1fr)',
              muted: true,
              render: (row: Delivery) =>
                row.last_error || (row.last_status_code ? `${row.last_status_code}` : '—'),
            },
            {
              key: 'created_at',
              header: 'Happened',
              width: '16ch',
              muted: true,
              render: (row: Delivery) => relative(row.created_at),
            },
          ]}
          rows={deliveries.data?.deliveries ?? []}
        />
        {openDelivery && <DeliveryDetail subscriptionID={id} deliveryID={openDelivery} onChange={refresh} />}
        {editing && s && <EditRequest sub={s} onClose={() => setEditing(false)} onSaved={refresh} />}
      </div>
    </Dialog>
  );
}

function DeliveryStatus({ d }: { d: Delivery }) {
  switch (d.status) {
    case 'succeeded':
      return <StatusIndicator status="running" label="Delivered" />;
    case 'failed':
      return <StatusIndicator status="failed" label="Failed" />;
    default:
      return <StatusIndicator status="building" label={d.attempts > 0 ? 'Retrying' : 'Sending'} />;
  }
}

function DeliveryDetail({
  subscriptionID,
  deliveryID,
  onChange,
}: {
  subscriptionID: string;
  deliveryID: string;
  onChange: () => void;
}) {
  const path = `/subscriptions/${subscriptionID}/deliveries/${deliveryID}`;
  const d = useQuery({ queryKey: ['delivery', deliveryID], queryFn: () => api.get<Delivery>(path) });
  const redeliver = useMutation({
    mutationFn: () => api.post<Delivery>(`${path}/redeliver`, {}),
    onSuccess: () => {
      onChange();
      void d.refetch();
    },
  });

  if (d.isPending) return <Quiet>Reading the delivery.</Quiet>;
  if (d.isError) return <Quiet>{messageOf(d.error)}</Quiet>;
  const delivery = d.data;

  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 'var(--space-3)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>
          {delivery.event} <span style={{ font: 'var(--type-mono)', color: 'var(--ink-secondary)' }}>{delivery.id}</span>
        </h4>
        <Button variant="secondary" disabled={redeliver.isPending} onClick={() => redeliver.mutate()}>
          {redeliver.isPending ? 'Sending again' : 'Send again'}
        </Button>
      </div>
      {redeliver.isError && <Banner tone="failed">{messageOf(redeliver.error)}</Banner>}
      {delivery.status === 'pending' && delivery.next_attempt_at && (
        <Quiet>Next attempt {relative(delivery.next_attempt_at).toLowerCase()}.</Quiet>
      )}
      <Table
        dense
        empty={<Quiet>Not attempted yet.</Quiet>}
        columns={[
          { key: 'attempt', header: 'Attempt', width: '8ch', align: 'right' },
          {
            key: 'attempted_at',
            header: 'When',
            width: '22ch',
            muted: true,
            render: (row: NonNullable<Delivery['attempt_log']>[number]) =>
              new Date(row.attempted_at).toLocaleString(),
          },
          {
            key: 'status_code',
            header: 'Answer',
            width: '8ch',
            mono: true,
            render: (row: NonNullable<Delivery['attempt_log']>[number]) => row.status_code ?? '—',
          },
          {
            key: 'duration_ms',
            header: 'Took',
            width: '10ch',
            align: 'right',
            muted: true,
            render: (row: NonNullable<Delivery['attempt_log']>[number]) => `${row.duration_ms} ms`,
          },
          {
            key: 'error',
            header: 'Error',
            width: 'minmax(0,1fr)',
            muted: true,
            render: (row: NonNullable<Delivery['attempt_log']>[number]) => row.error || '—',
          },
        ]}
        rows={(delivery.attempt_log ?? []).map((a) => ({ ...a, id: String(a.attempt) }))}
      />
      <CodeBlock title="Payload" dense copyable lines={JSON.stringify(delivery.payload, null, 2)} />
    </section>
  );
}

/** How a webhook is sent (R-375), as a form edits it. */
interface RequestOptions {
  method: string;
  contentType: string;
  /** "Name: value" lines. */
  headers: string;
  template: string;
}

const DEFAULT_REQUEST: RequestOptions = { method: 'POST', contentType: 'application/json', headers: '', template: '' };

/** Reads "Name: value" lines into headers, or says which line is wrong. */
function parseHeaders(text: string): { headers: Record<string, string>; problem?: string } {
  const headers: Record<string, string> = {};
  for (const line of text.split('\n')) {
    if (line.trim() === '') continue;
    const at = line.indexOf(':');
    const name = at < 0 ? '' : line.slice(0, at).trim();
    if (!name) return { headers, problem: `"${line.trim()}" is not a header. Write one per line as Name: value.` };
    headers[name] = line.slice(at + 1).trim();
  }
  return { headers };
}

/** The request body fields for these options. Headers only when asked to send them. */
function requestBody(opts: RequestOptions, withHeaders: boolean): Record<string, unknown> {
  const body: Record<string, unknown> = {
    method: opts.method,
    content_type: opts.contentType,
    payload_template: opts.template,
  };
  if (withHeaders) body.headers = parseHeaders(opts.headers).headers;
  return body;
}

function RequestFields({
  value,
  onChange,
  headerNote,
}: {
  value: RequestOptions;
  onChange: (next: RequestOptions) => void;
  headerNote?: string;
}) {
  const problem = parseHeaders(value.headers).problem;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0,12ch) minmax(0,1fr)', gap: 'var(--space-3)' }}>
        <Select
          label="Method"
          value={value.method}
          options={['POST', 'PUT', 'PATCH']}
          onChange={(e) => onChange({ ...value, method: e.target.value })}
        />
        <Input
          label="Content type"
          value={value.contentType}
          mono
          onChange={(e) => onChange({ ...value, contentType: e.target.value })}
        />
      </div>
      <Input
        as="textarea"
        rows={3}
        mono
        spellCheck={false}
        autoComplete="off"
        label="Headers"
        value={value.headers}
        placeholder="Authorization: Bearer …"
        helper={
          headerNote ??
          'One per line, as Name: value — for a receiver that needs a key. Stored encrypted and never shown again.'
        }
        error={problem}
        onChange={(e) => onChange({ ...value, headers: e.target.value })}
      />
      <Input
        as="textarea"
        rows={5}
        mono
        spellCheck={false}
        label="Body"
        value={value.template}
        placeholder={'{"text": {{json .Subject}}, "link": {{json .Link}}}'}
        helper="Leave empty to send Pando's event as JSON. Otherwise a Go template, given .Type, .Subject, .Body, .Link, .App.Name, .Data and .Envelope; use json to put a value in JSON. Checked when you save."
        onChange={(e) => onChange({ ...value, template: e.target.value })}
      />
    </div>
  );
}

/** Changes how a webhook is sent. Header values are never shown back, so
 *  the headers field starts empty and replaces every header only if it is
 *  changed. */
function EditRequest({ sub, onClose, onSaved }: { sub: Subscription; onClose: () => void; onSaved: () => void }) {
  const [value, setValue] = useState<RequestOptions>({
    method: sub.method || 'POST',
    contentType: sub.content_type || 'application/json',
    headers: '',
    template: sub.payload_template ?? '',
  });
  const [removeHeaders, setRemoveHeaders] = useState(false);
  const replaceHeaders = removeHeaders || value.headers.trim() !== '';
  const save = useMutation({
    mutationFn: () =>
      api.patch<Subscription>(
        `/subscriptions/${sub.id}`,
        requestBody(removeHeaders ? { ...value, headers: '' } : value, replaceHeaders),
      ),
    onSuccess: () => {
      onSaved();
      onClose();
    },
  });
  const names = sub.header_names ?? [];
  return (
    <Dialog
      open
      width={720}
      title="Change the request"
      description={`How Pando sends each event to ${sub.url}.`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={save.isPending || Boolean(parseHeaders(value.headers).problem)}
            onClick={() => save.mutate()}
          >
            {save.isPending ? 'Saving' : 'Save'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
        <RequestFields
          value={value}
          onChange={setValue}
          headerNote={
            names.length > 0
              ? `Sent now: ${names.join(', ')}. Their values are not shown. Type here to replace every header; leave it empty to keep them.`
              : undefined
          }
        />
        {names.length > 0 && (
          <Checkbox
            label="Remove every header"
            checked={removeHeaders}
            onChange={(e) => setRemoveHeaders(e.target.checked)}
          />
        )}
      </div>
    </Dialog>
  );
}
