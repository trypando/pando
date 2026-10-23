// Changing one workload's memory and CPU (R-241, R-241a, R-242).
//
// Saving writes a spec revision through POST /specs, as every other edit on
// the app does; the attention list then offers to deploy it. The spec it
// writes is resourceSpec.ts's: raising a workload past the app's limit raises
// the app's, and every other workload keeps the limit it ran under. The dialog
// says what the app's limit becomes before anything is saved.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Icon, Input } from '@design';

import { api } from '@api/client';
import type { AppSpec } from '@api/types.gen';
import { InstallVerb, useInstallVerb } from '../app/principal';
import type { CapacityView } from '../install/Capacity';
import { messageOf } from '../install/Accounts';
import { raisedAppLimits, withWorkloadResources, type Wanted } from './resourceSpec';

const MIB = 1 << 20;

/** Thousandths of a core as a plain number of cores: 500 is 0.5. */
export function coreNumber(millis: number): string {
  return String(Number((millis / 1000).toFixed(3)));
}

function mib(bytes: number): string {
  return `${Math.round(bytes / MIB)} MiB`;
}

function gib(bytes: number): string {
  return bytes >= 1 << 30 ? `${(bytes / (1 << 30)).toFixed(1)} GiB` : mib(bytes);
}

/** A field's text as a limit; empty is no limit of the workload's own. */
function parse(text: string, unit: number): number | undefined | null {
  const t = text.trim();
  if (t === '') return undefined;
  const n = Number(t);
  if (!Number.isFinite(n) || n <= 0) return null;
  return Math.round(n * unit);
}

export function ResourcesDialog({
  appID,
  appName,
  workload,
  spec,
  using,
  onClose,
}: Readonly<{
  appID: string;
  appName: string;
  workload: string;
  /** The newest revision, which the change is made on top of. */
  spec: AppSpec;
  /** What the workload uses now, where the page has a reading. */
  using?: { memory_bytes: number; cpu_millis: number };
  onClose: () => void;
}>) {
  const queries = useQueryClient();
  const own = (spec.workloads ?? []).find((w) => w.name === workload)?.resources;
  const [memory, setMemory] = useState(own?.memory_bytes ? String(Math.round(own.memory_bytes / MIB)) : '');
  const [cpu, setCpu] = useState(own?.cpu_millis ? coreNumber(own.cpu_millis) : '');

  const memoryBytes = parse(memory, MIB);
  const cpuMillis = parse(cpu, 1000);
  const invalid = memoryBytes === null || cpuMillis === null;
  const wanted: Wanted = { memory_bytes: memoryBytes ?? undefined, cpu_millis: cpuMillis ?? undefined };
  const raised = invalid ? {} : raisedAppLimits(spec, wanted);

  const save = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/specs`, withWorkloadResources(spec, workload, wanted)),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'specs'] });
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'notices'] });
      onClose();
    },
  });

  const appMemory = spec.resources?.memory_bytes ?? 0;
  const appCpu = spec.resources?.cpu_millis ?? 0;

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Resources for ${workload}`}
      description="Saving makes a new configuration revision. Deploy to apply it."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={invalid || save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving' : 'Save'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
          <Input
            label="Memory (MiB)"
            mono
            inputMode="numeric"
            value={memory}
            placeholder={appMemory > 0 ? String(Math.round(appMemory / MIB)) : undefined}
            helper={describe(appMemory > 0 ? mib(appMemory) : '', using ? mib(using.memory_bytes) : '')}
            error={memoryBytes === null ? 'Enter a number of MiB, such as 512.' : undefined}
            onChange={(e) => setMemory(e.target.value)}
          />
          {raised.memory_bytes && (
            <Raises>
              This raises {appName}&rsquo;s memory limit from {mib(raised.memory_bytes.from)} to {mib(raised.memory_bytes.to)}.
              Other workloads keep the limit they have.
            </Raises>
          )}
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
          <Input
            label="CPU (cores)"
            mono
            inputMode="decimal"
            value={cpu}
            placeholder={appCpu > 0 ? coreNumber(appCpu) : undefined}
            helper={describe(appCpu > 0 ? coreNumber(appCpu) : '', using ? coreNumber(using.cpu_millis) : '')}
            error={cpuMillis === null ? 'Enter a number of cores, such as 0.5.' : undefined}
            onChange={(e) => setCpu(e.target.value)}
          />
          {raised.cpu_millis && (
            <Raises>
              This raises {appName}&rsquo;s CPU limit from {coreNumber(raised.cpu_millis.from)} to{' '}
              {coreNumber(raised.cpu_millis.to)} cores. Other workloads keep the limit they have.
            </Raises>
          )}
        </div>

        <Unallocated runtime={spec.runtime?.adapter_ref} />

        {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}

function describe(fallback: string, now: string): string {
  const parts = [fallback ? `Default ${fallback}.` : 'No limit by default.'];
  if (now) parts.push(`Using ${now} now.`);
  return parts.join(' ');
}

function Raises({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <span style={{ display: 'flex', gap: 'var(--space-2)', alignItems: 'flex-start', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
      <Icon name="info" color="var(--water)" style={{ flex: '0 0 auto', marginTop: 'var(--space-1)' }} />
      <span>{children}</span>
    </span>
  );
}

/**
 * How much of the host is not yet given to apps (R-242). GET /capacity is
 * install.view, so only somebody holding it is told; anyone else sees nothing
 * here rather than a request that is refused.
 */
function Unallocated({ runtime }: Readonly<{ runtime?: string }>) {
  const allowed = useInstallVerb(InstallVerb.View);
  const capacity = useQuery({
    queryKey: ['capacity'],
    queryFn: () => api.get<CapacityView>('/capacity'),
    enabled: allowed,
  });
  if (!allowed) return null;
  const rt = capacity.data?.runtimes.find((r) => r.adapter_ref === runtime);
  if (!rt?.total_memory_bytes || !rt.total_cpu_millis) return null;
  const memory = Math.max(0, rt.total_memory_bytes - (rt.allocated_memory_bytes ?? 0));
  const cpu = Math.max(0, rt.total_cpu_millis - (rt.allocated_cpu_millis ?? 0));
  return (
    <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
      This host has {gib(memory)} of memory and {coreNumber(cpu)} CPU not yet allocated to apps.
    </p>
  );
}
