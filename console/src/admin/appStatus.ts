// What an app and each of its parts is doing now (R-221, R-261).
//
// "Degraded" is one word for a whole app, and an app can be a web service, a
// worker and a database it brought with it. Read from GET /apps/{id}/status,
// the same endpoint `pando app status` and pando_get_status use.

import { useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { recordIsBehind, statusInterval } from '../ui/polling';

export interface Part {
  name: string;
  primary: boolean;
  present: boolean;
  running: boolean;
  restarting: boolean;
  restart_count: number;
  healthy: boolean | null;
  exit_code?: number;
}

export interface AppStatus {
  state: string;
  desired_state: string;
  observability?: string;
  auto_deploy_paused?: boolean;
  workloads?: Part[] | null;
}

/**
 * The app's status, one query for every component that reads it — the parts
 * table, the log's part picker, the deploy settings and the app screen's own
 * watch for a deploy finishing — so they cost one request between them.
 *
 * Fast while something moves and slow once it has settled (statusInterval):
 * a restarting workload changes state faster than anything else on the
 * screen, and a stale count is what makes a crash loop look like a single
 * restart, but an app that is running as it should has nothing to report
 * every five seconds.
 */
export function useAppStatus(app: { id: string; state?: string; pinned_spec_id?: string }) {
  return useQuery({
    queryKey: ['apps', app.id, 'status'],
    queryFn: () => api.get<AppStatus>(`/apps/${app.id}/status`),
    enabled: Boolean(app.pinned_spec_id),
    refetchInterval: (query) => statusInterval(app.state, query.state.data),
    // Coming back to the tab shows the app as it is now; the record follows
    // the status (useRecordFollowsStatus). Off elsewhere (main.tsx).
    refetchOnWindowFocus: true,
  });
}

/**
 * Keeps the app's record in step with its status, for the app screen.
 *
 * The record is not polled. The status is, and when it says the app has moved
 * on — a deploy finished, somebody else started one, a part crashed — the
 * record and the deploys are read again, once. That is what used to need the
 * deploy list asked for every three seconds and the security report every two.
 */
export function useRecordFollowsStatus(app: App | undefined, recordUpdatedAt: number) {
  const queries = useQueryClient();
  const status = useAppStatus(app ?? { id: '', state: undefined, pinned_spec_id: undefined });
  const behind = recordIsBehind(
    { state: app?.state, updatedAt: recordUpdatedAt },
    { state: status.data?.state, updatedAt: status.dataUpdatedAt },
  );
  const appID = app?.id;
  useEffect(() => {
    if (!behind || !appID) return;
    void queries.invalidateQueries({ queryKey: ['apps', appID], exact: true });
    void queries.invalidateQueries({ queryKey: ['apps', appID, 'deployments'] });
    void queries.invalidateQueries({ queryKey: ['apps', appID, 'security'] });
  }, [behind, appID, status.dataUpdatedAt, queries]);
}

/**
 * Restarting first.
 *
 * A container the runtime keeps restarting is up at almost every instant
 * anybody looks at it, so "running" is the reading that makes a crash-looping
 * app look fine — which is how this one went unnoticed.
 */
export function symbolFor(part: Part): 'running' | 'failed' | 'building' | 'stopped' {
  if (part.restarting) return 'building';
  if (part.running) return 'running';
  if (part.present) return 'failed';
  return 'stopped';
}

export function labelFor(part: Part): string {
  if (part.restarting) {
    return part.restart_count > 1 ? `Restarting, ${part.restart_count} times` : 'Restarting';
  }
  if (part.running) return 'Running';
  if (part.exit_code !== undefined && part.exit_code !== null) return `Exited (${part.exit_code})`;
  if (part.present) return 'Stopped';
  return 'Not running';
}
