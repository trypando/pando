// What the Storage page of an app's Settings lists, worked out from the spec,
// the runtime's reading and the volumes Pando has recorded. Plain functions, so
// which volume may be removed and how disk use reads are tested.

import type { AppSpec } from '@api/types.gen';
import type { Reading } from './Workloads';

/** One volume as the page lists it. */
export interface VolumeLine {
  id: string;
  name: string;
  /** Each workload that mounts it, and where. */
  mounts: Array<{ workload: string; path: string }>;
  /** Bytes it holds, when the runtime said. */
  bytes?: number;
  /** The service Pando runs that keeps it, for one the spec does not declare. */
  service?: string;
  /** Added by a person, and so theirs to remove. Detection's, an image's and a service's are not. */
  removable: boolean;
}

/** A volume Pando has recorded: GET /apps/{id}/volumes. */
export interface RecordedVolume {
  id: string;
  adapter_ref: string;
  handle: string;
}

/**
 * The page's volumes: the spec's, then any Pando recorded that the spec does
 * not declare — a database Pando runs keeps its data in one of those.
 */
export function volumeLines(
  spec: AppSpec | undefined,
  reading: Reading | undefined,
  recorded: RecordedVolume[],
  serviceName: string,
): VolumeLine[] {
  const sizes = new Map<string, number>();
  for (const w of reading?.supported ? (reading.workloads ?? []) : []) {
    for (const v of w.volumes ?? []) if (v.bytes >= 0) sizes.set(v.id, v.bytes);
  }
  const declared = (spec?.volumes ?? []).map((v) => {
    const line: VolumeLine = {
      id: v.id,
      name: v.name,
      mounts: (spec?.workloads ?? []).flatMap((w) =>
        (w.mounts ?? []).filter((m) => m.volume_id === v.id).map((m) => ({ workload: w.name, path: m.path })),
      ),
      removable: v.declared === 'user',
    };
    const bytes = sizes.get(v.id);
    if (bytes !== undefined) line.bytes = bytes;
    return line;
  });
  const known = new Set(declared.map((v) => v.id));
  const services = recorded
    .filter((r) => !known.has(r.id))
    .map((r) => ({ id: r.id, name: r.handle || r.id, mounts: [], service: serviceName, removable: false }));
  return [...declared, ...services];
}

/** The spec without one volume, and without anything mounting it. */
export function withoutVolume(spec: AppSpec, id: string): AppSpec {
  return {
    ...spec,
    volumes: (spec.volumes ?? []).filter((v) => v.id !== id),
    workloads: (spec.workloads ?? []).map((w) => ({ ...w, mounts: (w.mounts ?? []).filter((m) => m.volume_id !== id) })),
  };
}

/**
 * What the app holds on disk, by the runtime's reading: what each workload
 * wrote outside its volumes, and every volume once. Undefined when the runtime
 * can't report it, so the page says nothing rather than zero.
 */
export function diskUsed(reading: Reading | undefined): number | undefined {
  if (!reading?.supported) return undefined;
  let total = 0;
  const seen = new Set<string>();
  for (const w of reading.workloads ?? []) {
    if (w.disk_bytes > 0) total += w.disk_bytes;
    for (const v of w.volumes ?? []) {
      if (seen.has(v.id) || v.bytes < 0) continue;
      seen.add(v.id);
      total += v.bytes;
    }
  }
  return total;
}

const GIB = 1 << 30;
const MIB = 1 << 20;

/** A number of bytes in binary units: 340 MiB, 1.2 GiB. */
export function binary(n: number): string {
  if (n >= GIB) return `${trim(n / GIB)} GiB`;
  if (n >= MIB) return `${Math.round(n / MIB)} MiB`;
  if (n >= 1024) return `${Math.round(n / 1024)} KiB`;
  return `${n} B`;
}

function trim(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}

/** Disk use against the app's limit, as "1.6 of 10 GiB"; just the use without one. */
export function diskOf(used: number, limit: number): string {
  if (limit <= 0) return binary(used);
  const unit = limit >= GIB ? GIB : MIB;
  const name = unit === GIB ? 'GiB' : 'MiB';
  return `${trim(Math.round((used / unit) * 10) / 10)} of ${trim(limit / unit)} ${name}`;
}
