// Slots and volumes — the two first-class objects R-030 names that had no
// screen.
//
// Both live on the app rather than the installation, because both are the app's:
// a slot is a hole the app declared (R-130) and a volume is data the app owns
// and that outlives it (R-204).
//
// Reading either is app.view; filling a slot and declaring a volume are
// app.spec.edit. Without that verb both tables stay, with nothing on them that
// offers a change the server would refuse.

import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Input, Select, StatusIndicator } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { Environment } from './Environment';
import { CarriedFiles } from './CarriedFiles';
import { BuildPlan } from './BuildPlan';
import { DeploySection } from './DeploySettings';
import { IdleSection } from './IdleSettings';
import { Egress } from './Egress';
import { SourceSection } from './UploadSource';
import { Table } from '../ui/Table';
import { AppVerb, useCan } from './verbs';

interface Slot {
  key: string;
  type: string;
  required?: boolean;
  resolution?: { mode: string; target?: string; secret_ref?: string };
}

interface Volume {
  id: string;
  adapter_ref: string;
  handle: string;
}

export function Resources({ app, focus }: { app: App; focus?: string }) {
  const appID = app.id;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)' }}>
      <SourceSection appID={appID} />
      <Slots appID={appID} focus={focus === 'dependencies'} />
      <Environment appID={appID} focus={focus === 'variables'} />
      <CarriedFiles appID={appID} />
      <BuildPlan appID={appID} />
      {/* `focus` is how the persistence warning on Overview lands somebody on
          the thing it is talking about rather than on this tab's first
          section. */}
      <Volumes appID={appID} focus={focus === 'storage'} />
      <Egress appID={appID} focus={focus === 'egress'} />
      <DeploySection app={app} />
      <IdleSection app={app} />
    </div>
  );
}

// --- slots -----------------------------------------------------------------

const TYPE_NAMES: Record<string, string> = {
  postgres: 'PostgreSQL',
  mysql: 'MySQL',
  redis: 'Redis',
  s3: 'object storage',
  smtp: 'email',

  // Apps pinned before detection stopped calling every variable a dependency
  // still carry these. Nothing about such a slot says what it connects to,
  // because there was never anything to connect it to — it is a value.
  unknown: 'a value',
};

/** Pando can stand these up itself. The rest are bound or given a value. */
const PROVISIONABLE = new Set(['postgres', 'mysql', 'redis']);

function Slots({ appID, focus }: { appID: string; focus?: boolean }) {
  const [editing, setEditing] = useState<Slot | null>(null);
  const canEdit = useCan(AppVerb.SpecEdit);
  const heading = useRef<HTMLElement>(null);

  useEffect(() => {
    if (focus) heading.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [focus]);

  const slots = useQuery({
    queryKey: ['slots', appID],
    queryFn: () => api.get<{ slots: Slot[] | null }>(`/apps/${appID}/slots`),
  });

  const rows = slots.data?.slots ?? [];

  return (
    <section ref={heading}>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Dependencies</h4>
      {/* R-132, said once here rather than repeated per row: an unfilled
          required dependency refuses the deploy instead of starting something
          that crashes on connection refused. */}
      <Quiet>
        Things this app needs. A required one that isn&rsquo;t filled in refuses the next deploy
        rather than starting an app that can&rsquo;t connect.
      </Quiet>

      {slots.isError && <Banner tone="failed">{messageOf(slots.error)}</Banner>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          loading={slots.isPending}
          skeletonRows={2}
          columns={[
            { key: 'key', header: 'Variable', width: 'minmax(0,26ch)', mono: true },
            {
              key: 'type',
              header: 'Kind',
              width: '18ch',
              render: (row: Slot) => TYPE_NAMES[row.type] ?? row.type,
            },
            {
              key: 'resolution',
              header: 'Filled by',
              width: 'minmax(0,28ch)',
              render: (row: Slot) => describe(row),
            },
            ...(canEdit
              ? [
                  {
                    key: 'edit',
                    header: '',
                    width: '12ch',
                    align: 'right' as const,
                    render: (row: Slot) => (
                      <Button variant="secondary" onClick={() => setEditing(row)}>
                        Change
                      </Button>
                    ),
                  },
                ]
              : []),
          ]}
          rows={rows}
          empty={<Quiet>This app declares nothing it depends on.</Quiet>}
        />
      </div>

      {editing && canEdit && (
        <FillSlot appID={appID} slot={editing} onClose={() => setEditing(null)} />
      )}
    </section>
  );
}

/** How a slot reads in a sentence, with the unfilled case said plainly. */
function describe(slot: Slot) {
  if (!slot.resolution) {
    // A status is always a symbol plus a word, never a colored chip.
    return slot.required ? (
      <StatusIndicator status="failed" label="Not filled in" />
    ) : (
      <StatusIndicator status="stopped" label="Not filled in, and optional" />
    );
  }
  switch (slot.resolution.mode) {
    case 'provisioned':
      return `Pando runs one inside this app`;
    case 'bound':
      return slot.resolution.target ?? 'Something already running';
    case 'literal':
      // The value itself is a stored secret and is not shown. Rendering it
      // here would put a database password in a table that anyone holding
      // app.view can read, which app.secrets.read exists to prevent (R-083).
      return 'A value you set';
    default:
      return slot.resolution.mode;
  }
}

function FillSlot({
  appID,
  slot,
  onClose,
}: {
  appID: string;
  slot: Slot;
  onClose: () => void;
}) {
  const queries = useQueryClient();
  const canProvision = PROVISIONABLE.has(slot.type);
  const isValue = slot.type === 'unknown';
  const [mode, setMode] = useState(
    slot.resolution?.mode ?? (canProvision ? 'provisioned' : isValue ? 'literal' : 'bound'),
  );
  const [target, setTarget] = useState(slot.resolution?.target ?? '');
  const [value, setValue] = useState('');

  const save = useMutation({
    mutationFn: () =>
      api.put(`/apps/${appID}/slots/${encodeURIComponent(slot.key)}`, { mode, target, value }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['slots', appID] });
      onClose();
    },
  });

  const options = [
    ...(canProvision
      ? [{ value: 'provisioned', label: `Pando runs a ${TYPE_NAMES[slot.type] ?? slot.type} inside this app` }]
      : []),
    { value: 'bound', label: 'Connect to one that already exists' },
    { value: 'literal', label: 'Paste a value' },
  ];

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Fill ${slot.key}`}
      // Filling a dependency and deploying are separate acts: this changes how
      // the app will run next time and does not deploy it now.
      description="This takes effect at the next deploy."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving' : 'Save'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        {/* R-010: Pando does not run infrastructure it cannot stand up in a
            container. Saying which kinds it can is better than offering the
            option and failing at deploy. */}
        {isValue ? (
          <Banner tone="info">
            Nothing about {slot.key} says what it connects to, so there is nothing for Pando to run
            or to point it at. Paste the value it should have.
          </Banner>
        ) : (
          !canProvision && (
            <Banner tone="info">
              Pando doesn&rsquo;t stand up {TYPE_NAMES[slot.type] ?? slot.type} itself. Connect this
              to something you already run, or paste a value.
            </Banner>
          )
        )}

        <Select
          label="How this is filled"
          value={mode}
          options={options}
          onChange={(e) => setMode(e.target.value)}
        />

        {mode === 'bound' && (
          <Input
            label="Where it is"
            mono
            value={target}
            helper="A connection string for something already running, on this host or elsewhere."
            onChange={(e) => setTarget(e.target.value)}
          />
        )}

        {mode === 'literal' && (
          <Input
            label="Value"
            mono
            type="password"
            value={value}
            helper="Stored as a secret. It is never shown again and never appears in an exported configuration."
            onChange={(e) => setValue(e.target.value)}
          />
        )}

        {mode === 'provisioned' && (
          <Quiet>
            Pando runs it beside this app, reachable only by this app, and backs its data up with the
            app&rsquo;s.
          </Quiet>
        )}

        {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}

// --- volumes ---------------------------------------------------------------

function Volumes({ appID, focus }: { appID: string; focus?: boolean }) {
  const queries = useQueryClient();
  const [adding, setAdding] = useState(false);
  const canEdit = useCan(AppVerb.SpecEdit);
  const heading = useRef<HTMLElement>(null);

  useEffect(() => {
    if (focus) heading.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [focus]);
  const [name, setName] = useState('');
  const [path, setPath] = useState('');

  const volumes = useQuery({
    queryKey: ['volumes', appID],
    queryFn: () => api.get<{ volumes: Volume[] | null }>(`/apps/${appID}/volumes`),
  });

  const add = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/volumes`, { name, path }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['volumes', appID] });
      setAdding(false);
      setName('');
      setPath('');
    },
  });

  const rows = volumes.data?.volumes ?? [];

  return (
    // No width of its own: the cap arrived with the scroll-into-view ref and
    // made this the one narrow section on a page of wide ones. Every section
    // here is bounded by its table's own columns, which is what keeps them
    // agreeing with each other.
    <section ref={heading}>
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Storage</h4>
        {canEdit && (
          <Button variant="secondary" onClick={() => setAdding(true)}>
            Add storage
          </Button>
        )}
      </div>

      {/* R-204, where the consequence lands rather than in documentation. */}
      <Quiet>
        Data this app keeps. Deleting the app doesn&rsquo;t delete this — Pando offers to back it up
        first and keeps it until you say otherwise.
      </Quiet>

      {volumes.isError && <Banner tone="failed">{messageOf(volumes.error)}</Banner>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          loading={volumes.isPending}
          skeletonRows={1}
          columns={[
            { key: 'id', header: 'Reference', width: 'minmax(0,28ch)', mono: true },
            { key: 'handle', header: 'Where it is', width: 'minmax(0,38ch)', mono: true, muted: true },
            { key: 'adapter_ref', header: 'Runtime', width: '18ch', mono: true, muted: true },
          ]}
          rows={rows}
          empty={<Quiet>This app keeps nothing between deploys.</Quiet>}
        />
      </div>

      {adding && canEdit && (
        <Dialog
          open
          onClose={() => setAdding(false)}
          title="Add storage"
          description="This takes effect at the next deploy."
          footer={
            <>
              <Button variant="ghost" onClick={() => setAdding(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                disabled={add.isPending || !name || !path}
                onClick={() => add.mutate()}
              >
                {add.isPending ? 'Adding' : 'Add storage'}
              </Button>
            </>
          }
        >
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
            {/* R-201: the warning is the point of the endpoint. Anything
                written outside a declared path is gone at the next deploy, and
                learning that from a lost database is the experience this
                sentence exists to prevent. */}
            <Banner tone="info">
              Anything this app writes outside a path you declare here is gone at the next deploy.
            </Banner>

            <Input
              label="Name"
              value={name}
              helper="How you will refer to it. Unique within this app."
              onChange={(e) => setName(e.target.value)}
            />
            <Input
              label="Path inside the app"
              mono
              value={path}
              helper="For example /var/lib/data. The app writes here and Pando keeps it."
              onChange={(e) => setPath(e.target.value)}
            />

            {add.isError && <Banner tone="failed">{messageOf(add.error)}</Banner>}
          </div>
        </Dialog>
      )}
    </section>
  );
}
