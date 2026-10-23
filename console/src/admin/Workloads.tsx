// Each workload of an app: how it is, and what it is using (R-221, R-245).
//
// One card per workload, from two endpoints: GET /status says whether it is
// running and how often it has restarted, and GET /usage what it is using
// beside its limits — the same endpoints `pando app status`, `pando app usage`
// and their MCP tools read. The usage readings the page takes while it is open
// are kept in this component and drawn as a line (R-245); nothing is kept once
// the page closes.

import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Button, Card, Icon, Skeleton, StatusIndicator } from '@design';

import { api } from '@api/client';
import type { App, AppSpec, BackupAttempt } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { relative } from '../ui/time';
import { USAGE_MS, useOnScreen } from '../ui/polling';
import { attemptLabel } from '../ui/backupAttempt';
import { labelFor, symbolFor, useAppStatus, type Part } from './appStatus';
import { useOpenPanel } from './AppPanel';
import { bytes } from './usage-format';
import { coreNumber, ResourcesDialog } from './ResourcesDialog';
import { Sparkline } from './Sparkline';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

interface VolumeUsage {
  id: string;
  name: string;
  path: string;
  bytes: number;
}

export interface PartUsage {
  name: string;
  primary: boolean;
  running: boolean;
  cpu_millis: number;
  cpu_limit_millis: number;
  memory_bytes: number;
  memory_limit_bytes: number;
  disk_bytes: number;
  volumes: VolumeUsage[];
}

export interface Reading {
  supported: boolean;
  reported_at?: string;
  workloads?: PartUsage[];
  host_cpu_millis?: number;
  host_memory_bytes?: number;
}

/**
 * The usage query's key. It carries the app's state and the spec it runs, so
 * a reading is taken again the moment either moves on — a deploy finished, a
 * part stopped or started — rather than at the next thirty-second tick. Every
 * usage key starts with ['apps', id, 'usage'], so invalidating that still
 * reaches it.
 */
export function usageKey(app: Pick<App, 'id' | 'state' | 'pinned_spec_id'>) {
  return ['apps', app.id, 'usage', app.state, app.pinned_spec_id ?? ''] as const;
}

/** One reading of one workload. */
export interface Sample {
  at: string;
  memory: number;
  cpu: number;
  disk: number;
}

/** How many readings a line keeps: an hour of them at the page's interval. */
const KEEP = 120;

/**
 * The readings so far, with this one added to each workload it reports on.
 * A reading already added — the same answer seen twice — changes nothing.
 */
export function addReading(
  history: Readonly<Record<string, Sample[]>>,
  reading: Reading | undefined,
  keep = KEEP,
): Record<string, Sample[]> {
  if (!reading?.supported || !reading.reported_at) return history as Record<string, Sample[]>;
  const at = reading.reported_at;
  const next: Record<string, Sample[]> = { ...history };
  let changed = false;
  for (const w of reading.workloads ?? []) {
    const past = history[w.name] ?? [];
    if (past.at(-1)?.at === at) continue;
    // A part that is not running has no reading but its disk; leaving it out
    // keeps a stop from drawing as a fall to zero.
    if (!w.running) continue;
    next[w.name] = [...past, { at, memory: w.memory_bytes, cpu: w.cpu_millis, disk: Math.max(0, w.disk_bytes) }].slice(-keep);
    changed = true;
  }
  return changed ? next : (history as Record<string, Sample[]>);
}

/** The readings this page has taken, kept for as long as it is open. */
function useReadings(reading: Reading | undefined): Record<string, Sample[]> {
  const [history, setHistory] = useState<Record<string, Sample[]>>(() => addReading({}, reading));
  useEffect(() => {
    setHistory((h) => addReading(h, reading));
  }, [reading]);
  return history;
}

/** Near enough a memory limit to be killed at it: drawn in the warning color. */
export function nearLimit(used: number, limit: number): boolean {
  return limit > 0 && used / limit >= 0.9;
}

export function Workloads({ app }: Readonly<{ app: App & { last_backup?: BackupAttempt } }>) {
  const [section, visible] = useOnScreen<HTMLElement>();
  const status = useAppStatus(app);
  const usage = useQuery({
    queryKey: usageKey(app),
    queryFn: () => api.get<Reading>(`/apps/${app.id}/usage`),
    enabled: Boolean(app.pinned_spec_id),
    // Each reading takes the runtime about a second to sample (issue #72):
    // every thirty seconds, and only while the section is on screen.
    refetchInterval: visible ? USAGE_MS : false,
    placeholderData: (previous) => previous,
  });
  const history = useReadings(usage.data);
  const newest = useNewestSpec(app.id);
  const [resizing, setResizing] = useState<string | null>(null);

  if (!app.pinned_spec_id) return null;

  const reading = usage.data;
  const parts = status.data?.workloads ?? [];
  const usages = reading?.supported ? (reading.workloads ?? []) : [];
  // The usage answer is in the spec's order with what Pando provides after;
  // the status answer fills in a workload the reading has not got to.
  const names = [...new Set([...usages.map((u) => u.name), ...parts.map((p) => p.name)])];
  const declared = new Set((newest.spec?.workloads ?? []).map((w) => w.name));

  return (
    <section ref={section} aria-labelledby="workloads" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 'var(--space-4)', flexWrap: 'wrap' }}>
        <h4 id="workloads" style={{ font: 'var(--type-h4)', margin: 0 }}>
          Workloads
        </h4>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)' }}>
          {reading?.supported && (
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
              Since you opened this page. Disk excludes storage.
            </span>
          )}
          <Button
            variant="secondary"
            disabled={usage.isFetching}
            onClick={() => {
              void usage.refetch();
              void status.refetch();
            }}
          >
            {usage.isFetching ? 'Refreshing' : 'Refresh'}
          </Button>
        </span>
      </div>

      {status.data?.observability === 'unreachable' && <Quiet>Pando couldn’t reach the runtime. State unknown.</Quiet>}
      {usage.isError && <Quiet>{messageOf(usage.error)}</Quiet>}
      {reading && !reading.supported && <Quiet>This app&rsquo;s runtime doesn&rsquo;t report usage.</Quiet>}

      {names.length === 0 && (usage.isPending || status.isPending) ? (
        <div role="status" aria-label="Loading">
          <Skeleton height="12rem" radius="md" />
        </div>
      ) : (
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 18rem), 1fr))',
            gap: 'var(--space-4)',
            alignItems: 'stretch',
          }}
        >
          {names.map((name) => (
            <WorkloadCard
              key={name}
              name={name}
              part={parts.find((p) => p.name === name)}
              use={usages.find((u) => u.name === name)}
              samples={history[name] ?? []}
              several={names.length > 1}
              provided={newest.spec !== undefined && !declared.has(name)}
              lastBackup={app.last_backup}
              onResources={newest.spec && declared.has(name) ? () => setResizing(name) : undefined}
            />
          ))}
        </div>
      )}

      {resizing && newest.spec && (
        <ResourcesDialog
          appID={app.id}
          appName={app.name}
          workload={resizing}
          spec={newest.spec as AppSpec}
          using={usingNow(usages.find((u) => u.name === resizing))}
          onClose={() => setResizing(null)}
        />
      )}
    </section>
  );
}

function usingNow(u: PartUsage | undefined) {
  return u?.running ? { memory_bytes: u.memory_bytes, cpu_millis: u.cpu_millis } : undefined;
}

function restarts(n: number): string {
  if (n === 0) return 'No restarts';
  return n === 1 ? '1 restart' : `${n} restarts`;
}

/** What a workload is, in a few words: whether it serves the address or Pando
 *  provides it, then how often it has restarted. */
export function workloadLine(part: Part | undefined, use: PartUsage | undefined, several: boolean, provided: boolean): string {
  const said: string[] = [];
  if (provided) said.push('Provided by Pando');
  else if (several && (part?.primary || use?.primary)) said.push('Serves the address');
  if (part) {
    said.push(restarts(part.restart_count));
  }
  return said.join(' · ');
}

function WorkloadCard({
  name,
  part,
  use,
  samples,
  several,
  provided,
  lastBackup,
  onResources,
}: Readonly<{
  name: string;
  part?: Part;
  use?: PartUsage;
  samples: Sample[];
  several: boolean;
  provided: boolean;
  lastBackup?: BackupAttempt;
  onResources?: () => void;
}>) {
  const openOutput = useOpenPanel('output');
  const openTerminal = useOpenPanel('terminal');
  // R-241's verb, and the spec edit its write goes through (POST /specs).
  const canOverride = useCan(AppVerb.ResourcesOverride);
  const canEdit = useCan(AppVerb.SpecEdit);
  const canResize = canOverride && canEdit;
  const running = part ? part.running && !part.restarting : Boolean(use?.running);
  const line = workloadLine(part, use, several, provided);
  const volumes = use?.volumes ?? [];

  return (
    <Card as="article" aria-label={name} padding="md" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', minWidth: 0 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 'var(--space-3)' }}>
          <h5 style={{ font: 'var(--type-code)', color: 'var(--ink)', margin: 0, overflowWrap: 'anywhere' }}>{name}</h5>
          {part ? (
            <StatusIndicator status={symbolFor(part)} label={labelFor(part)} />
          ) : (
            use && <StatusIndicator status={use.running ? 'running' : 'stopped'} label={use.running ? 'Running' : 'Not running'} />
          )}
        </div>
        {line && <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{line}</span>}

        {use?.running && (
          <>
            <Reading
              label="Memory"
              value={use.memory_limit_bytes > 0 ? `${bytes(use.memory_bytes)} of ${bytes(use.memory_limit_bytes)}` : bytes(use.memory_bytes)}
              warn={nearLimit(use.memory_bytes, use.memory_limit_bytes)}
              values={samples.map((s) => s.memory)}
              limit={use.memory_limit_bytes}
            />
            <Reading
              label="CPU"
              value={
                use.cpu_limit_millis > 0
                  ? `${coreNumber(use.cpu_millis)} of ${coreNumber(use.cpu_limit_millis)}`
                  : `${coreNumber(use.cpu_millis)} cores`
              }
              values={samples.map((s) => s.cpu)}
              limit={use.cpu_limit_millis}
            />
          </>
        )}
        {use && (
          <Reading
            label="Disk"
            value={use.disk_bytes < 0 ? '—' : bytes(use.disk_bytes)}
            values={use.running ? samples.map((s) => s.disk) : []}
          />
        )}

        {volumes.length > 0 && (
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              gap: 'var(--space-1)',
              borderTop: 'var(--border-width) solid var(--rule)',
              paddingTop: 'var(--space-3)',
            }}
          >
            {volumes.map((v) => (
              <Pair key={v.id} label="Storage">
                <code style={{ font: 'var(--type-code-sm)' }}>{v.name || v.id}</code> · {v.bytes < 0 ? 'size unknown' : bytes(v.bytes)}
              </Pair>
            ))}
            {lastBackup && (
              <Pair label="Last backup">
                {attemptLabel(lastBackup)} {relative(lastBackup.attempted_at).toLowerCase()}
              </Pair>
            )}
          </div>
        )}

        <div style={{ marginTop: 'auto', display: 'flex', flexWrap: 'wrap', gap: 'var(--space-1)', paddingTop: 'var(--space-2)' }}>
          {openOutput && (
            <Button variant="ghost" icon={<Icon name="scroll-text" />} onClick={() => openOutput({ view: 'output', workload: name })}>
              Logs
            </Button>
          )}
          {/* A shell needs something running to open in (R-084). */}
          {openTerminal && running && (
            <Button variant="ghost" icon={<Icon name="square-terminal" />} onClick={() => openTerminal({ view: 'terminal', workload: name })}>
              Terminal
            </Button>
          )}
          {canResize && onResources && (
            <Button variant="ghost" icon={<Icon name="gauge" />} title="Change resources" onClick={onResources}>
              Resources
            </Button>
          )}
        </div>
    </Card>
  );
}

function Reading({
  label,
  value,
  values,
  limit = 0,
  warn = false,
}: Readonly<{ label: string; value: string; values: number[]; limit?: number; warn?: boolean }>) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <Pair label={label}>
        <span style={{ font: 'var(--type-code-sm)', color: warn ? 'var(--contour-text)' : 'var(--ink)' }}>{value}</span>
      </Pair>
      {values.length > 0 && <Sparkline values={values} limit={limit} tone={warn ? 'warn' : 'normal'} />}
    </div>
  );
}

function Pair({ label, children }: Readonly<{ label: string; children: React.ReactNode }>) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 'var(--space-3)' }}>
      <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{label}</span>
      <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', textAlign: 'right', minWidth: 0 }}>{children}</span>
    </div>
  );
}
