// The Storage page of an app's Settings: how much of its disk limit the app
// uses (R-403), and each volume it keeps.
//
// Reading is app.view. Adding a volume and removing one a person added are
// spec revisions, app.spec.edit, taking effect at the next deployment. A
// volume detection found, an image declared or a service Pando runs keeps is
// not removed from here; volumes survive deleting the app anyway (R-204).

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Card, Dialog, Input, Select, Tag } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { messageOf } from '../install/Accounts';
import { LineSkeleton, Loading } from '../ui/Loading';
import { slotLabel } from './discovery';
import { useNewestSpec } from './newestSpec';
import { PageHead, Quiet } from './settingsParts';
import { binary, diskOf, diskUsed, volumeLines, withoutVolume, type RecordedVolume, type VolumeLine } from './storage';
import { usageKey, type Reading } from './Workloads';
import { AppVerb, useCan } from './verbs';

export function SettingsStorage({ app }: Readonly<{ app: App }>) {
  const appID = app.id;
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const newest = useNewestSpec(appID);
  const spec = newest.spec;
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<VolumeLine | null>(null);

  const usage = useQuery({
    queryKey: usageKey(app),
    queryFn: () => api.get<Reading>(`/apps/${appID}/usage`),
    enabled: Boolean(app.pinned_spec_id),
  });
  const recorded = useQuery({
    queryKey: ['volumes', appID],
    queryFn: () => api.get<{ volumes: RecordedVolume[] | null }>(`/apps/${appID}/volumes`),
  });

  const provisioned = (spec?.slots ?? []).filter((s) => s.resolution?.mode === 'provisioned');
  // Which service keeps a volume the spec does not declare is not recorded
  // beside it; with one service Pando runs, it is that one.
  const service = provisioned.length === 1 ? slotLabel(provisioned[0]!.type) : 'Pando-run service';
  const lines = volumeLines(spec, usage.data, recorded.data?.volumes ?? [], service);
  const used = diskUsed(usage.data);
  const limit = spec?.resources?.disk_bytes ?? 0;

  const remove = useMutation({
    mutationFn: (id: string) => {
      if (!spec) throw new Error('no spec');
      return api.post(`/apps/${appID}/specs`, withoutVolume(spec, id));
    },
    onSuccess: () => {
      setRemoving(null);
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'specs'] });
      void queries.invalidateQueries({ queryKey: ['volumes', appID] });
    },
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <PageHead
        title="Storage"
        line="Kept across deployments and backed up daily"
        action={
          canEdit &&
          spec && (
            <Button variant="secondary" onClick={() => setAdding(true)}>
              Add storage
            </Button>
          )
        }
      />

      {used !== undefined && (
        <Card tone="paper" padding="md">
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', gap: 'var(--space-3)' }}>
              <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>Disk use</span>
              <span style={{ font: 'var(--type-code)', color: 'var(--ink)' }}>{diskOf(used, limit)}</span>
            </div>
            {limit > 0 && <Meter used={used} limit={limit} />}
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
              Includes storage and what each workload writes outside it.
            </span>
          </div>
        </Card>
      )}

      {(recorded.isError || newest.error) && <Banner tone="failed">{messageOf(recorded.error ?? newest.error)}</Banner>}

      <Card tone="paper" padding="none">
        {newest.pending ? (
          <div style={{ padding: 'var(--space-4)' }}>
            <Loading>
              <LineSkeleton width="30ch" />
            </Loading>
          </div>
        ) : lines.length === 0 ? (
          <div style={{ padding: 'var(--space-4)' }}>
            <Quiet>No storage. Anything this app writes is gone at the next deployment.</Quiet>
          </div>
        ) : (
          lines.map((v) => (
            <div key={v.id} className="pando-line">
              <span style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
                <span style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
                  <span style={{ font: 'var(--type-code)', color: 'var(--ink)', overflowWrap: 'anywhere' }}>{v.name}</span>
                  {v.service && <Tag>{v.service}</Tag>}
                </span>
                {v.mounts.map((m) => (
                  <span key={m.workload + m.path} style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
                    <span style={{ font: 'var(--type-code-sm)' }}>{m.workload}</span> at{' '}
                    <span style={{ font: 'var(--type-code-sm)' }}>{m.path}</span>
                  </span>
                ))}
              </span>
              <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink-secondary)' }}>
                {v.bytes === undefined ? '' : binary(v.bytes)}
              </span>
              <span>
                {v.removable && canEdit && (
                  <Button variant="ghost" onClick={() => setRemoving(v)}>
                    Remove
                  </Button>
                )}
              </span>
            </div>
          ))
        )}
      </Card>

      {adding && spec && (
        <AddStorage
          appID={appID}
          workloads={(spec.workloads ?? []).map((w) => w.name)}
          onClose={() => setAdding(false)}
        />
      )}

      {removing && (
        <Dialog
          open
          onClose={() => setRemoving(null)}
          title={`Remove ${removing.name}?`}
          description="Takes effect at the next deployment."
          footer={
            <>
              <Button variant="ghost" onClick={() => setRemoving(null)}>
                Cancel
              </Button>
              <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate(removing.id)}>
                {remove.isPending ? 'Removing' : 'Remove'}
              </Button>
            </>
          }
        >
          {remove.isError && <Banner tone="failed">{messageOf(remove.error)}</Banner>}
        </Dialog>
      )}
    </div>
  );
}

/** Disk use against the limit, in the warning color from 90% (R-403). */
function Meter({ used, limit }: Readonly<{ used: number; limit: number }>) {
  const share = Math.min(1, used / limit);
  return (
    <div
      role="meter"
      aria-label="Disk use"
      aria-valuemin={0}
      aria-valuemax={limit}
      aria-valuenow={used}
      style={{ height: 'var(--space-1)', borderRadius: 'var(--radius-pill)', background: 'var(--paper-sunken)', overflow: 'hidden' }}
    >
      <div
        style={{
          width: `${(share * 100).toFixed(1)}%`,
          height: '100%',
          background: share >= 0.9 ? 'var(--contour-text)' : 'var(--ink-secondary)',
        }}
      />
    </div>
  );
}

function AddStorage({ appID, workloads, onClose }: Readonly<{ appID: string; workloads: string[]; onClose: () => void }>) {
  const queries = useQueryClient();
  const [name, setName] = useState('');
  const [path, setPath] = useState('');
  const [workload, setWorkload] = useState(workloads[0] ?? '');

  const add = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/volumes`, { name, path, workload }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'specs'] });
      void queries.invalidateQueries({ queryKey: ['volumes', appID] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      onClose={onClose}
      title="Add storage"
      description="Takes effect at the next deployment."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={add.isPending || !name || !path} onClick={() => add.mutate()}>
            {add.isPending ? 'Adding' : 'Add storage'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        {/* R-201: the warning is the point of the endpoint. */}
        <Banner tone="info">Anything this app writes outside a path declared here is gone at the next deployment.</Banner>
        <Input label="Name" value={name} helper="Unique within this app." onChange={(e) => setName(e.target.value)} />
        <Input label="Path" mono value={path} helper="For example /var/lib/data." onChange={(e) => setPath(e.target.value)} />
        {workloads.length > 1 && (
          <Select
            label="Workload"
            value={workload}
            options={workloads.map((w) => ({ value: w, label: w }))}
            onChange={(e) => setWorkload(e.target.value)}
          />
        )}
        {add.isError && <Banner tone="failed">{messageOf(add.error)}</Banner>}
      </div>
    </Dialog>
  );
}
