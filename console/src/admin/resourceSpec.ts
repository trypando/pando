// The spec a change to one workload's resources writes (R-241, R-241a).
//
// A workload is held within the app's limit (R-240), and one with no limit of
// its own runs at the app's. So giving one workload more than the app has
// raises the app's limit to match — and every other workload that had no limit
// of its own is written the one it ran under, so that one change never quietly
// lets the rest grow. Apart from the dialog so it can be tested on its own.

import type { AppSpec, ResourceLimits } from '@api/types.gen';

/** What somebody asked for one workload: undefined is no limit of its own. */
export interface Wanted {
  memory_bytes?: number;
  cpu_millis?: number;
}

type Dimension = keyof Wanted;
const DIMENSIONS: Dimension[] = ['memory_bytes', 'cpu_millis'];

/** The app's limits after the change, for the line that says so before saving. */
export function raisedAppLimits(spec: AppSpec, wanted: Wanted): Partial<Record<Dimension, { from: number; to: number }>> {
  const raised: Partial<Record<Dimension, { from: number; to: number }>> = {};
  for (const d of DIMENSIONS) {
    const app = spec.resources?.[d] ?? 0;
    const want = wanted[d] ?? 0;
    // An app with no limit has nothing to raise.
    if (app > 0 && want > app) raised[d] = { from: app, to: want };
  }
  return raised;
}

/** The spec with `workload`'s own limits set to `wanted`, R-241a applied. */
export function withWorkloadResources(spec: AppSpec, workload: string, wanted: Wanted): AppSpec {
  const raised = raisedAppLimits(spec, wanted);
  const resources = { ...spec.resources };
  const workloads = (spec.workloads ?? []).map((w) => {
    if (w.name === workload) {
      const own: ResourceLimits = {};
      for (const d of DIMENSIONS) {
        const want = wanted[d];
        if (want !== undefined && want > 0) own[d] = want;
      }
      const next = { ...w };
      if (Object.keys(own).length > 0) next.resources = own;
      else delete next.resources;
      return next;
    }
    // Every other workload keeps the limit it ran under: its own, or the app's
    // as it was before this change.
    let own: ResourceLimits | undefined = w.resources;
    for (const d of DIMENSIONS) {
      const r = raised[d];
      if (r && !((w.resources?.[d] ?? 0) > 0)) own = { ...own, [d]: r.from };
    }
    return own === w.resources ? w : { ...w, resources: own };
  });
  for (const d of DIMENSIONS) {
    const r = raised[d];
    if (r) {
      resources[d] = r.to;
      resources.overridden = true;
    }
  }
  return { ...spec, resources, workloads };
}
