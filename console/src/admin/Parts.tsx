// What each part of an app is doing (R-221, R-261).
//
// "Degraded" is one word for a whole app, and an app can be a web service, a
// worker and a database it brought with it. crewmate reported degraded while
// its application container restarted every two seconds on a missing variable,
// and the only place that was visible was `docker ps` on the host — which is
// exactly the thing Pando exists so nobody has to do.
//
// Read from GET /apps/{id}/status, the same endpoint `pando app status` and
// pando_get_status use.

import { useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, StatusIndicator, Tag } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { Table } from '../ui/Table';
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

export function Parts({ app, onLogs }: { app: App; onLogs?: (workload: string) => void }) {
  const status = useAppStatus(app);
  const parts = status.data?.workloads ?? [];

  // One part is the app, and the app's own status line already says how it is.
  // Nothing while the status loads either, not a skeleton: most apps are one
  // part, and a placeholder for a section that then does not appear moves
  // everything under it twice.
  if (!app.pinned_spec_id || parts.length < 2) return null;

  return (
    <section style={{ maxWidth: MEASURE }}>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Parts</h4>
      <Quiet>
        This app runs as more than one thing. Each has its own state and its own log.
      </Quiet>

      {status.data?.observability === 'unreachable' && (
        <Quiet>
          Pando couldn’t reach the runtime, so this is unknown rather than false.
        </Quiet>
      )}
      {status.isError && <Quiet>{messageOf(status.error)}</Quiet>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          columns={[
            {
              key: 'name',
              header: 'Part',
              width: 'minmax(0,24ch)',
              mono: true,
              render: (row: Part) => (
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)' }}>
                  {row.name}
                  {row.primary && <Tag>address</Tag>}
                </span>
              ),
            },
            {
              key: 'state',
              header: 'State',
              width: '22ch',
              render: (row: Part) => <StatusIndicator status={symbolFor(row)} label={labelFor(row)} />,
            },
            {
              key: 'healthy',
              header: 'Health',
              width: '16ch',
              muted: true,
              // R-221: no health check is not a failing one.
              render: (row: Part) =>
                row.healthy === null ? 'No check' : row.healthy ? 'Healthy' : 'Failing',
            },
            {
              key: 'restart_count',
              header: 'Restarts',
              width: '12ch',
              align: 'right',
              muted: true,
            },
            {
              key: 'logs',
              header: '',
              width: '12ch',
              align: 'right',
              render: (row: Part) =>
                onLogs && (
                  <Button variant="secondary" onClick={() => onLogs(row.name)}>
                    Logs
                  </Button>
                ),
            },
          ]}
          rows={parts}
        />
      </div>
    </section>
  );
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
