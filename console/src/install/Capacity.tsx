// How much room is left on each runtime (R-242, R-243).
//
// The readings are the ones every runtime reports in the same shape — totals,
// how many workloads are running, and what Pando's apps use now — beside what
// Pando has committed to apps on it. Committed against total is the planner's
// own arithmetic, so what this shows and what a refused deploy says agree.
// Anything else a runtime says about itself is its own shape, so it is shown
// as it came, behind Details, and never interpreted here.
//
// Read from GET /capacity. The runtimes' readings are taken by the server in
// the background while somebody is looking (issue #72), so the screen says
// when they were taken; committed is as of the request.

import { useQuery } from '@tanstack/react-query';
import { Button, Skeleton } from '@design';

import { api } from '@api/client';
import { Quiet, messageOf } from './Accounts';
import { Disclosure } from '../ui/Disclosure';
import { bytes, cores } from '../admin/usage-format';

export interface RuntimeCapacity {
  adapter_ref: string;
  status: 'ok' | 'unreachable';
  total_cpu_millis?: number;
  total_memory_bytes?: number;
  total_disk_bytes?: number;
  allocated_cpu_millis?: number;
  allocated_memory_bytes?: number;
  allocated_disk_bytes?: number;
  /** Present only from a runtime that reports usage (R-245). */
  in_use_cpu_millis?: number;
  in_use_memory_bytes?: number;
  /** -1 when the runtime cannot say. */
  running_workloads?: number;
  details?: Record<string, unknown> | null;
  reported?: string;
}

export interface CapacityView {
  runtimes: RuntimeCapacity[];
  /** When the server last read the runtimes. */
  as_of?: string;
  /** How often it reads them again while somebody is looking. */
  refresh_seconds?: number;
}

export function Capacity({ names }: { names: Record<string, string> }) {
  const capacity = useQuery({
    queryKey: ['capacity'],
    queryFn: () => api.get<CapacityView>('/capacity'),
    // Asked again as often as the server reads the runtimes: the answer comes
    // from its reading, so asking costs a sum, not a sample.
    refetchInterval: (q) => (q.state.data?.refresh_seconds ?? 0) * 1000 || false,
  });
  const runtimes = capacity.data?.runtimes ?? [];
  const asOf = capacity.data?.as_of;
  const every = capacity.data?.refresh_seconds;

  return (
    <section>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 'var(--space-3)',
          margin: 'var(--space-6) 0 var(--space-3)',
        }}
      >
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Capacity</h4>
        {runtimes.length > 0 && (
          <Button variant="secondary" disabled={capacity.isFetching} onClick={() => void capacity.refetch()}>
            {capacity.isFetching ? 'Refreshing' : 'Refresh'}
          </Button>
        )}
      </div>

      {capacity.isPending ? (
        <div role="status" aria-label="Loading" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <Skeleton width="16ch" />
          {['CPU', 'Memory', 'Disk'].map((label) => (
            <div key={label} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
              <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>{label}</span>
              <Skeleton width="28ch" />
              <Skeleton width="32ch" height="var(--space-2)" radius="xs" />
            </div>
          ))}
        </div>
      ) : capacity.isError ? (
        <Quiet>{messageOf(capacity.error)}</Quiet>
      ) : runtimes.length === 0 ? (
        <Quiet>No runtime is set up, so there is no room to report. Add a runtime adapter above.</Quiet>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
          {asOf && (
            <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
              Runtimes read at {new Date(asOf).toLocaleTimeString()}
              {every ? `, and again every ${every} seconds while this screen is open` : ''}. Committed is as of now.
            </p>
          )}
          {runtimes.map((rt) => (
            <Runtime key={rt.adapter_ref} rt={rt} name={names[rt.adapter_ref]} />
          ))}
        </div>
      )}
    </section>
  );
}

/** One runtime's readings: a block of its own, named, since there can be
 *  several on one installation. */
function Runtime({ rt, name }: { rt: RuntimeCapacity; name?: string }) {
  const reportsUse = rt.in_use_cpu_millis !== undefined;
  const reportsNothing =
    rt.status === 'ok' && !rt.total_cpu_millis && !rt.total_memory_bytes && !rt.total_disk_bytes && !reportsUse;

  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-4)',
        paddingBottom: 'var(--space-5)',
        borderBottom: 'var(--border-width) solid var(--rule)',
      }}
    >
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'baseline', gap: 'var(--space-1) var(--space-3)' }}>
        {name && <span style={{ font: 'var(--type-body-ui)', fontWeight: 600 }}>{name}</span>}
        <span style={{ font: 'var(--type-code-sm)', color: name ? 'var(--ink-secondary)' : 'var(--ink)' }}>
          {rt.adapter_ref}
        </span>
        {rt.status === 'ok' && rt.running_workloads !== undefined && rt.running_workloads >= 0 && (
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>
            · {rt.running_workloads} {rt.running_workloads === 1 ? 'workload' : 'workloads'} running
          </span>
        )}
      </div>

      {rt.status === 'unreachable' ? (
        <Quiet>
          Pando could not reach this runtime to ask how much room it has. Its status is in the adapters table above.
        </Quiet>
      ) : reportsNothing ? (
        <Quiet>This runtime does not report its capacity, so Pando does not check room before a deploy to it.</Quiet>
      ) : (
        <>
          <Reading
            label="CPU"
            total={rt.total_cpu_millis ?? 0}
            committed={rt.allocated_cpu_millis ?? 0}
            inUse={rt.in_use_cpu_millis}
            format={cores}
          />
          <Reading
            label="Memory"
            total={rt.total_memory_bytes ?? 0}
            committed={rt.allocated_memory_bytes ?? 0}
            inUse={rt.in_use_memory_bytes}
            format={bytes}
          />
          <Reading
            label="Disk"
            total={rt.total_disk_bytes ?? 0}
            committed={rt.allocated_disk_bytes ?? 0}
            format={bytes}
            unreported="Not reported, so Pando does not check disk before a deploy. Set total_disk_bytes on this adapter to have it checked."
          />
          {rt.reported && (
            <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
              As of {new Date(rt.reported).toLocaleTimeString()}. Committed is what apps on this runtime reserve;
              a deploy that would commit more than the total is refused.
            </p>
          )}
        </>
      )}

      {rt.details && Object.keys(rt.details).length > 0 && (
        <Disclosure show="Show details" hide="Hide details">
          {/* The runtime's own shape, shown as it came (R-243). */}
          <pre
            style={{
              font: 'var(--type-code-sm)',
              background: 'var(--paper-sunken)',
              border: 'var(--border-width) solid var(--rule)',
              borderRadius: 'var(--radius-sm)',
              padding: 'var(--space-4)',
              overflowX: 'auto',
              margin: 0,
            }}
          >
            {JSON.stringify(rt.details, null, 2)}
          </pre>
        </Disclosure>
      )}
    </div>
  );
}

/**
 * One resource: committed against total, with what is left, and what is in
 * use now where the runtime says. The bar is committed, with in use drawn over
 * it; the words under it carry every number, so the bar is never the only way
 * to read them.
 */
function Reading({
  label,
  total,
  committed,
  inUse,
  format,
  unreported = 'Not reported, so Pando does not check it before a deploy.',
}: {
  label: string;
  total: number;
  committed: number;
  inUse?: number;
  format: (n: number) => string;
  unreported?: string;
}) {
  const known = total > 0;
  const share = (n: number) => (known ? Math.min(1, Math.max(0, n / total)) : 0);
  // Red near the total, as on the app's In use: that is where the next deploy
  // is refused.
  const near = known && committed / total >= 0.9;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>{label}</span>
      {known ? (
        <>
          <span style={{ font: 'var(--type-body-ui)' }}>
            {format(Math.max(0, total - committed))} left
            <span style={{ color: 'var(--ink-secondary)' }}> of {format(total)}</span>
          </span>
          <div
            role="meter"
            aria-label={`${label} committed`}
            aria-valuemin={0}
            aria-valuemax={total}
            aria-valuenow={committed}
            style={{
              position: 'relative',
              height: 'var(--space-2)',
              maxWidth: '40ch',
              background: 'var(--paper-sunken)',
              borderRadius: 'var(--radius-xs)',
              overflow: 'hidden',
            }}
          >
            <Fill share={share(committed)} color={near ? 'var(--marker)' : 'var(--ink-muted)'} />
            {inUse !== undefined && <Fill share={share(inUse)} color="var(--ink)" inset />}
          </div>
          <span style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-1) var(--space-4)' }}>
            <Key color={near ? 'var(--marker)' : 'var(--ink-muted)'}>{format(committed)} committed</Key>
            {inUse !== undefined && <Key color="var(--ink)">{format(inUse)} in use now</Key>}
          </span>
        </>
      ) : (
        <>
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{unreported}</span>
          {committed > 0 && (
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
              {format(committed)} committed
            </span>
          )}
        </>
      )}
    </div>
  );
}

/** A share of the meter. In use is drawn inset, thinner, over committed, so
 *  both read where they overlap. */
function Fill({ share, color, inset = false }: { share: number; color: string; inset?: boolean }) {
  return (
    <div
      style={{
        position: 'absolute',
        left: 0,
        top: inset ? '25%' : 0,
        bottom: inset ? '25%' : 0,
        width: `${share * 100}%`,
        background: color,
        transition: 'width var(--dur-fast) var(--ease)',
      }}
    />
  );
}

/** A legend entry: a swatch matching the meter, then the number in words. */
function Key({ color, children }: { color: string; children: React.ReactNode }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)', font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
      <span aria-hidden style={{ width: 'var(--space-2)', height: 'var(--space-2)', borderRadius: 'var(--radius-xs)', background: color }} />
      {children}
    </span>
  );
}
