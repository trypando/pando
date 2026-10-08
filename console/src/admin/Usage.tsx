// What each part of an app is using now: CPU, memory, disk and its volumes,
// beside the limits it runs under (R-245).
//
// A reading, refreshed while the page is open — not a history. Pando keeps no
// metrics and draws no graphs (R-016); what it can answer is "is this part
// near its limit right now", which is the question somebody looking at a slow
// or crashing app has.
//
// Read from GET /apps/{id}/usage, the same endpoint `pando app usage` and
// pando_get_usage use.

import { useLayoutEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Button, Skeleton, Tag } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { Table } from '../ui/Table';
import { bytes, cores } from './usage-format';
import { USAGE_MS, useOnScreen } from '../ui/polling';

interface VolumeUsage {
  id: string;
  name: string;
  path: string;
  bytes: number;
}

interface PartUsage {
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

interface Reading {
  supported: boolean;
  reported_at?: string;
  workloads?: PartUsage[];
  host_cpu_millis?: number;
  host_memory_bytes?: number;
}

export function Usage({ app, layout = 'table' }: { app: App; layout?: 'table' | 'stack' }) {
  const [section, visible] = useOnScreen<HTMLElement>();
  const reading = useQuery({
    queryKey: ['apps', app.id, 'usage'],
    queryFn: () => api.get<Reading>(`/apps/${app.id}/usage`),
    enabled: Boolean(app.pinned_spec_id),
    // Each reading takes the runtime about a second to sample, and every open
    // overview asks (issue #72): every thirty seconds, and only while the
    // section is on screen. Refresh is there for somebody watching a part
    // climb toward its limit.
    refetchInterval: visible ? USAGE_MS : false,
  });

  if (!app.pinned_spec_id) return null;
  const data = reading.data;
  const parts = data?.workloads ?? [];

  const asOf = data?.reported_at && (
    <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 'var(--space-2) 0 0' }}>
      As of {new Date(data.reported_at).toLocaleTimeString()}. Refreshes every thirty seconds. Disk is what a part wrote
      outside its storage.
    </p>
  );

  return (
    <section ref={section} style={{ maxWidth: MEASURE }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 'var(--space-3)', marginBottom: 'var(--space-2)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>In use</h4>
        {/* Now, rather than at the next thirty-second tick: after a deploy, or
            while watching a part climb. */}
        {data?.supported && (
          <Button variant="secondary" disabled={reading.isFetching} onClick={() => void reading.refetch()}>
            {reading.isFetching ? 'Refreshing' : 'Refresh'}
          </Button>
        )}
      </div>
      {reading.isError && <Quiet>{messageOf(reading.error)}</Quiet>}

      {/* The first reading takes the runtime about a second to sample, so the
          section holds the shape of what is coming rather than standing empty.
          How many parts is not known yet; one is the common case. */}
      {reading.isPending && layout === 'stack' && (
        <div role="status" aria-label="Loading" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <Skeleton width="12ch" />
          {['CPU', 'Memory', 'Disk'].map((label) => (
            <div key={label} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
              <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>{label}</span>
              <Skeleton width="16ch" />
              {label !== 'Disk' && <Skeleton width="24ch" height="var(--space-1)" radius="xs" />}
            </div>
          ))}
        </div>
      )}
      {reading.isPending && layout === 'table' && (
        <Table
          loading
          skeletonRows={1}
          columns={[
            { key: 'name', header: 'Part', width: 'minmax(0,18ch)' },
            { key: 'cpu', header: 'CPU', width: 'minmax(0,1fr)' },
            { key: 'memory', header: 'Memory', width: 'minmax(0,1fr)' },
            { key: 'disk', header: 'Disk', width: 'minmax(0,1fr)' },
          ]}
          rows={[]}
        />
      )}

      {data && !data.supported && (
        <Quiet>This app&rsquo;s runtime does not report what its parts are using.</Quiet>
      )}

      {data?.supported && layout === 'stack' && (
        <Capped>
          {parts.map((row) => (
            <div
              key={row.name}
              style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', paddingBottom: 'var(--space-3)', borderBottom: 'var(--border-width) solid var(--rule)' }}
            >
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)', font: 'var(--type-code)' }}>
                {row.name}
                {row.primary && parts.length > 1 && <Tag>address</Tag>}
                {/* A stopped part says so once, beside its name, rather than
                    once per reading it has none of. Disk still has an answer. */}
                {!row.running && (
                  <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>· Not running</span>
                )}
              </span>
              {row.running && (
                <>
                  <Labeled label="CPU">
                    <Meter used={row.cpu_millis} limit={row.cpu_limit_millis} host={data.host_cpu_millis} format={cores} />
                  </Labeled>
                  <Labeled label="Memory">
                    <Meter used={row.memory_bytes} limit={row.memory_limit_bytes} host={data.host_memory_bytes} format={bytes} />
                  </Labeled>
                </>
              )}
              <Labeled label="Disk">
                <Disk row={row} />
              </Labeled>
            </div>
          ))}
          {asOf}
        </Capped>
      )}

      {data?.supported && layout === 'table' && (
        <div style={{ marginTop: 'var(--space-3)' }}>
          <Table
            columns={[
              {
                key: 'name',
                header: 'Part',
                width: 'minmax(0,18ch)',
                mono: true,
                render: (row: PartUsage) => (
                  <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)' }}>
                    {row.name}
                    {row.primary && parts.length > 1 && <Tag>address</Tag>}
                  </span>
                ),
              },
              {
                key: 'cpu',
                header: 'CPU',
                width: 'minmax(0,1fr)',
                render: (row: PartUsage) =>
                  row.running ? (
                    <Meter
                      used={row.cpu_millis}
                      limit={row.cpu_limit_millis}
                      host={data.host_cpu_millis}
                      format={cores}
                    />
                  ) : (
                    <Stopped />
                  ),
              },
              {
                key: 'memory',
                header: 'Memory',
                width: 'minmax(0,1fr)',
                render: (row: PartUsage) =>
                  row.running ? (
                    <Meter
                      used={row.memory_bytes}
                      limit={row.memory_limit_bytes}
                      host={data.host_memory_bytes}
                      format={bytes}
                    />
                  ) : (
                    <Stopped />
                  ),
              },
              {
                key: 'disk',
                header: 'Disk',
                width: 'minmax(0,1fr)',
                // The part's own layer, then each volume it mounts: the second
                // is where an app's data is, and usually the number that grows.
                render: (row: PartUsage) => <Disk row={row} />,
              },
            ]}
            rows={parts.map((p) => ({ ...p, id: p.name }))}
          />
          {asOf}
        </div>
      )}
    </section>
  );
}

/**
 * A number beside its limit, with a bar. With no limit, the part may use
 * what the host has, so the host is the scale — and says so, rather than
 * drawing a bar against a limit that is not there.
 */
function Meter({
  used,
  limit,
  host,
  format,
}: {
  used: number;
  limit: number;
  host?: number;
  format: (n: number) => string;
}) {
  const scale = limit > 0 ? limit : host && host > 0 ? host : 0;
  const share = scale > 0 ? Math.min(1, used / scale) : 0;
  // Red only near a real limit: that is where a part is about to be throttled
  // or killed. Near the host's total is a different conversation.
  const near = limit > 0 && share >= 0.9;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', padding: 'var(--space-2) 0' }}>
      <span style={{ font: 'var(--type-body-ui)' }}>
        {format(used)}
        <span style={{ color: 'var(--ink-secondary)' }}>
          {limit > 0 ? ` of ${format(limit)}` : host && host > 0 ? ` of ${format(host)} on the host` : ''}
        </span>
      </span>
      {scale > 0 && (
        <div
          role="meter"
          aria-valuemin={0}
          aria-valuemax={scale}
          aria-valuenow={used}
          style={{ height: 'var(--space-1)', background: 'var(--paper-sunken)', borderRadius: 'var(--radius-xs)', overflow: 'hidden', maxWidth: '24ch' }}
        >
          <div
            style={{
              width: `${share * 100}%`,
              height: '100%',
              background: near ? 'var(--marker)' : 'var(--ink-secondary)',
              transition: 'width var(--dur-fast) var(--ease)',
            }}
          />
        </div>
      )}
    </div>
  );
}

// How tall the stacked readings may stand before "Show more": about two parts'
// worth. An app with many parts would otherwise push everything under it off
// the screen for a panel most people glance at.
const CAP = 'calc(var(--space-8) * 6)';

/** The stacked readings, cut off at CAP with a way to see the rest — offered
 *  only when there is a rest to see. */
function Capped({ children }: { children: React.ReactNode }) {
  const box = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [overflows, setOverflows] = useState(false);

  // Measured after every render: a reading can add a part, or a volume line,
  // and the answer to "is there more" changes with it.
  useLayoutEffect(() => {
    const el = box.current;
    if (el) setOverflows(el.scrollHeight > el.clientHeight + 1);
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <div
        ref={box}
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-4)',
          maxHeight: open ? undefined : CAP,
          overflow: 'hidden',
          // The last visible line fades rather than being sliced through, so
          // it reads as "there is more" rather than as a rendering fault.
          maskImage: !open && overflows ? 'linear-gradient(to bottom, black 80%, transparent)' : undefined,
          WebkitMaskImage: !open && overflows ? 'linear-gradient(to bottom, black 80%, transparent)' : undefined,
        }}
      >
        {children}
      </div>
      {(overflows || open) && (
        <Button variant="ghost" onClick={() => setOpen((v) => !v)} style={{ alignSelf: 'flex-start' }}>
          {open ? 'Show less' : 'Show more'}
        </Button>
      )}
    </div>
  );
}

/** The part's own layer, then each volume it mounts: the second is where an
 *  app's data is, and usually the number that grows. */
function Disk({ row }: { row: PartUsage }) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', padding: 'var(--space-2) 0' }}>
      <span style={{ font: 'var(--type-body-ui)' }}>{row.disk_bytes < 0 ? '—' : bytes(row.disk_bytes)}</span>
      {row.volumes.map((v) => (
        <span key={v.id} style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          {v.name || v.id} at {v.path}: {v.bytes < 0 ? 'size unknown' : bytes(v.bytes)}
        </span>
      ))}
    </div>
  );
}

/** One reading in the stacked layout: its name above it, as the table's
 *  header would be. Stacked explicitly, as the loading skeleton is, so the
 *  layout does not depend on whether the value happens to be a block. */
function Labeled({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>{label}</span>
      {children}
    </div>
  );
}

/** A table cell for a reading a stopped part has none of. Same type and
 *  padding as a Meter's number and the Disk column, so the row lines up. */
function Stopped() {
  return (
    <div style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', padding: 'var(--space-2) 0' }}>
      Not running
    </div>
  );
}
