// An app's deployments, in the side panel (R-170): the history on the left, the
// chosen one's build log on the right with Copy and Roll back to this.
//
// The list starts with "Not deployed yet" when a saved revision is newer than
// the one running — editing writes a revision and leaves the pinned one alone
// (R-152), so this is where somebody finds out a change is waiting, deploys
// it, or compares it with what runs.

import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, StatusSymbol } from '@design';

import { api, base, RequestFailed } from '@api/client';
import type { App, Deployment } from '@api/types.gen';
import { messageOf } from '../install/Accounts';
import { deployLabelFor, deployStatus } from '../ui/deploys';
import { deploymentsInterval } from '../ui/polling';
import { relative } from '../ui/time';
import { WithCode } from '../ui/ErrorCode';
import { LineSkeleton, Loading } from '../ui/Loading';
import { ApprovalRequest } from './ApprovalRequest';
import { isAwaiting, type ApprovalDeployment } from './approval';
import { APP_LIST_KEY } from './appList';
import { DeployButton } from './DeployButton';
import { Body, CopyButton, LogBox, Note, Toolbar } from './panelParts';
import { AppVerb, useCan } from './verbs';

/** The address's name for the "Not deployed yet" entry. */
export const NOT_DEPLOYED = 'unshipped';

interface Revision {
  id: string;
  revision: number;
}

interface Comparison {
  changes?: Array<{ path: string; summary: string }> | null;
}

/** What a deploy that never started says in place of its log. */
const NEVER_RAN: Record<string, string> = {
  rejected: 'Rejected. Who rejected it, and why, is in the audit log.',
  expired: 'Expired without enough approvals. Deploy again to request approval.',
  // Not superseded: a deploy replaced part-way through a build has a log
  // worth reading.
};

/** What started a deploy, in a few words. */
export function startedBy(trigger: string): string {
  switch (trigger) {
    case 'manual':
      return 'by hand';
    case 'rollback':
      return 'rollback';
    case 'branch_updated':
      return 'new commit';
    case 'release_tagged':
      return 'new release';
    default:
      return trigger.replaceAll('_', ' ');
  }
}

/** How long a deploy took, as "1m 42s"; nothing for one still running. */
export function took(started: string, finished?: string): string {
  if (!finished) return '';
  const seconds = Math.max(0, Math.round((Date.parse(finished) - Date.parse(started)) / 1000));
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, '0')}s`;
}

/** The caption under a deploy: which revision, when, what started it, how long it took. */
export function caption(d: Deployment): string {
  const revision = d.spec_revision === undefined ? '' : `Revision ${d.spec_revision}`;
  return [revision, relative(d.started_at), startedBy(d.trigger), took(d.started_at, d.finished_at)]
    .filter(Boolean)
    .join(' · ');
}

export function PanelDeployments({
  app,
  deployment,
  onDeployment,
}: Readonly<{
  app: App;
  /** The chosen deployment's id, or NOT_DEPLOYED. The newest otherwise. */
  deployment?: string;
  onDeployment: (id: string) => void;
}>) {
  const deployments = useQuery({
    queryKey: ['apps', app.id, 'deployments'],
    queryFn: () => api.get<{ deployments: ApprovalDeployment[] | null }>(`/apps/${app.id}/deployments`),
    // Not polled while a deploy runs: its log streams, and the app screen asks
    // for the list again when the status says the deploy has finished.
    refetchInterval: (query) => deploymentsInterval(query.state.data?.deployments),
  });
  const specs = useQuery({
    queryKey: ['apps', app.id, 'specs'],
    queryFn: () => api.get<{ revisions: Revision[] | null; pinned_spec_id: string }>(`/apps/${app.id}/specs`),
    enabled: Boolean(app.pinned_spec_id),
  });

  const revisions = (specs.data?.revisions ?? []).slice().sort((a, b) => b.revision - a.revision);
  const newest = revisions[0];
  const pinned = revisions.find((r) => r.id === app.pinned_spec_id);
  const unshipped = newest && pinned && newest.revision > pinned.revision ? { newest, pinned } : undefined;

  const rows = deployments.data?.deployments ?? [];
  const shown = deployment === NOT_DEPLOYED && unshipped ? undefined : (rows.find((d) => d.id === deployment) ?? rows[0]);

  // Said in the panel, where the button was pressed: the app screen's own
  // banner is under the panel's scrim.
  const [refusal, setRefusal] = useState<{ message: string; remedy?: string; code?: string } | null>(null);
  const [waiting, setWaiting] = useState(false);
  const onRefused = (message: string, remedy?: string, code?: string) => {
    setRefusal(message ? { message, remedy, code } : null);
    if (message) setWaiting(false);
  };

  return (
    <div style={{ flex: '1 1 auto', minHeight: 0, display: 'flex' }}>
      <ol
        aria-label="Deployments"
        style={{
          listStyle: 'none',
          margin: 0,
          padding: 'var(--space-3) 0',
          width: '20rem',
          flex: '0 0 auto',
          overflowY: 'auto',
          borderRight: 'var(--border-width) solid var(--rule)',
        }}
      >
        {unshipped && (
          <Unshipped
            app={app}
            revision={unshipped.newest.revision}
            chosen={!shown}
            onCompare={() => onDeployment(NOT_DEPLOYED)}
            onRefused={onRefused}
            onWaiting={() => setWaiting(true)}
          />
        )}
        {deployments.isPending && (
          <li style={{ padding: 'var(--space-3) var(--space-5)' }}>
            <Loading gap="var(--space-2)">
              <LineSkeleton width="16ch" />
              <LineSkeleton width="24ch" />
            </Loading>
          </li>
        )}
        {deployments.isError && (
          <li style={{ padding: 'var(--space-3) var(--space-5)' }}>
            <Note>{messageOf(deployments.error)}</Note>
          </li>
        )}
        {deployments.isSuccess && rows.length === 0 && (
          <li style={{ padding: 'var(--space-3) var(--space-5)' }}>
            <Note>No deployments yet.</Note>
          </li>
        )}
        {rows.map((d) => (
          <li key={d.id}>
            <button
              type="button"
              aria-current={d.id === shown?.id ? 'true' : undefined}
              onClick={() => onDeployment(d.id)}
              style={{
                display: 'flex',
                flexDirection: 'column',
                gap: 'var(--space-1)',
                width: '100%',
                padding: 'var(--space-2) var(--space-5)',
                border: 'none',
                borderTop: 'var(--border-width) solid var(--rule)',
                background: d.id === shown?.id ? 'var(--paper-sunken)' : 'transparent',
                color: 'var(--ink)',
                textAlign: 'left',
                cursor: 'pointer',
              }}
            >
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)', font: 'var(--type-label)' }}>
                <StatusSymbol status={deployStatus(d.status, d.result_state)} />
                {deployLabelFor(d)}
              </span>
              <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{caption(d)}</span>
            </button>
          </li>
        ))}
      </ol>

      <div style={{ flex: '1 1 auto', minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        {(refusal || waiting) && (
          <div style={{ padding: 'var(--space-3) var(--space-5) 0' }}>
            {refusal ? (
              <Banner
                tone="failed"
                action={
                  <Button variant="ghost" onClick={() => setRefusal(null)}>
                    Dismiss
                  </Button>
                }
              >
                <WithCode code={refusal.code}>
                  {refusal.message}
                  {refusal.remedy ? ` ${refusal.remedy}` : ''}
                </WithCode>
              </Banner>
            ) : (
              <Banner
                tone="info"
                action={
                  <Button variant="ghost" onClick={() => setWaiting(false)}>
                    Dismiss
                  </Button>
                }
              >
                This deploy is waiting for approval.
              </Banner>
            )}
          </div>
        )}
        {shown ? (
          <DeploymentPane
            key={shown.id}
            app={app}
            deployment={shown}
            running={shown.spec_id === app.pinned_spec_id}
            onRefused={onRefused}
            onWaiting={() => setWaiting(true)}
          />
        ) : (
          unshipped && (
            <ComparePane appID={app.id} from={unshipped.pinned.revision} to={unshipped.newest.revision} />
          )
        )}
      </div>
    </div>
  );
}

/** "Not deployed yet": a saved revision newer than the one running. */
function Unshipped({
  app,
  revision,
  chosen,
  onCompare,
  onRefused,
  onWaiting,
}: Readonly<{
  app: App;
  revision: number;
  chosen: boolean;
  onCompare: () => void;
  onRefused: (message: string, remedy?: string, code?: string) => void;
  onWaiting: () => void;
}>) {
  const canDeploy = useCan(AppVerb.Deploy);
  return (
    <li
      aria-current={chosen ? 'true' : undefined}
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-1)',
        padding: 'var(--space-2) var(--space-5) var(--space-3)',
        background: chosen ? 'var(--paper-sunken)' : 'transparent',
      }}
    >
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)', font: 'var(--type-label)' }}>
        <StatusSymbol status="building" />
        Not deployed yet
      </span>
      <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>Revision {revision}</span>
      <span style={{ display: 'flex', gap: 'var(--space-2)', marginTop: 'var(--space-1)' }}>
        {canDeploy && (
          <DeployButton app={app} variant="secondary" label="Deploy" onRefused={onRefused} onWaiting={onWaiting} />
        )}
        <Button variant="ghost" onClick={onCompare}>
          Compare
        </Button>
      </span>
    </li>
  );
}

/** What the saved revision changes from the one running. */
function ComparePane({ appID, from, to }: Readonly<{ appID: string; from: number; to: number }>) {
  const diff = useQuery({
    queryKey: ['apps', appID, 'spec-diff', from, to],
    queryFn: () => api.get<Comparison>(`/apps/${appID}/specs/${from}/diff/${to}`),
  });
  const changes = diff.data?.changes ?? [];
  return (
    <>
      <Toolbar
        start={
          <span style={{ display: 'flex', flexDirection: 'column' }}>
            <span style={{ font: 'var(--type-label)' }}>Revision {to}, not deployed yet</span>
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>Compared with revision {from}, which is running</span>
          </span>
        }
      />
      <Body>
        {diff.isPending && (
          <Loading gap="var(--space-2)">
            <LineSkeleton width="40ch" />
            <LineSkeleton width="32ch" />
          </Loading>
        )}
        {diff.isError && <Note>{messageOf(diff.error)}</Note>}
        {diff.isSuccess && changes.length === 0 && <Note>No changes.</Note>}
        {changes.length > 0 && (
          <ul style={{ margin: 0, paddingLeft: 'var(--space-5)', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
            {changes.map((c) => (
              <li key={c.path + c.summary} style={{ padding: 'var(--space-1) 0' }}>
                {c.summary}
              </li>
            ))}
          </ul>
        )}
      </Body>
    </>
  );
}

/** One deployment: what it was, Copy and Roll back to this, and its log. */
function DeploymentPane({
  app,
  deployment,
  running,
  onRefused,
  onWaiting,
}: Readonly<{
  app: App;
  deployment: ApprovalDeployment;
  /** Whether this deployment's revision is the one the app runs. */
  running: boolean;
  onRefused: (message: string, remedy?: string, code?: string) => void;
  onWaiting: () => void;
}>) {
  const queries = useQueryClient();
  const canDeploy = useCan(AppVerb.Deploy);
  const [lines, setLines] = useState<string[]>([]);
  const [ended, setEnded] = useState(false);
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLDivElement>(null);
  const silent = isAwaiting(deployment) || NEVER_RAN[deployment.status] !== undefined;

  // Rolling back is deploying a revision that ran before (R-152), which never
  // waits for approval when it ran successfully (R-157).
  const rollback = useMutation({
    mutationFn: () =>
      api.post<{ status?: string } | undefined>(`/apps/${app.id}/deployments/rollback`, { to: deployment.spec_revision }),
    onSuccess: (started) => {
      onRefused('');
      if (started?.status === 'awaiting_approval') onWaiting();
      void queries.invalidateQueries({ queryKey: ['apps', app.id], exact: true });
      for (const part of ['deployments', 'status', 'security']) {
        void queries.invalidateQueries({ queryKey: ['apps', app.id, part] });
      }
      void queries.invalidateQueries({ queryKey: APP_LIST_KEY });
    },
    onError: (error) => {
      const failed = error instanceof RequestFailed ? error : null;
      onRefused(failed?.message ?? 'Pando couldn’t start the rollback. Try again.', failed?.remedy, failed?.code);
    },
  });

  // Streamed rather than polled: this is where somebody watches a deploy
  // happen, and latency is the experience.
  useEffect(() => {
    if (silent) return undefined;
    const stream = new EventSource(`${base}/apps/${app.id}/deployments/${deployment.id}/logs`);
    let quiet: number | undefined;
    const done = () => {
      setEnded(true);
      stream.close();
      window.clearTimeout(quiet);
    };
    stream.onmessage = (event) => {
      window.clearTimeout(quiet);
      setLines((previous) => [...previous, event.data as string]);
    };
    stream.addEventListener('end', done);
    stream.onerror = done;
    // A deploy whose output Pando no longer holds never ends: the log store
    // opens an empty stream for it and waits. A finished deploy that has
    // said nothing for four seconds has nothing to say.
    if (deployment.finished_at) quiet = window.setTimeout(done, 4_000);
    return () => {
      window.clearTimeout(quiet);
      stream.close();
    };
  }, [app.id, deployment.id, deployment.finished_at, silent]);

  const offerRollback =
    canDeploy && !running && deployment.status === 'succeeded' && deployment.spec_revision !== undefined;
  const when = new Date(deployment.started_at).toLocaleString();
  const length = took(deployment.started_at, deployment.finished_at);

  return (
    <>
      <Toolbar
        start={
          <span style={{ display: 'flex', flexDirection: 'column' }}>
            <span style={{ font: 'var(--type-label)' }}>
              {deployment.spec_revision === undefined ? 'Deploy' : `Deploy of revision ${deployment.spec_revision}`}
            </span>
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
              Started {when}
              {length ? ` · finished in ${length}` : ''}
            </span>
          </span>
        }
        end={
          <>
            {!silent && <CopyButton text={lines.join('\n')} target={box} />}
            {offerRollback && (
              <Button variant="secondary" disabled={rollback.isPending} onClick={() => rollback.mutate()}>
                Roll back to this
              </Button>
            )}
          </>
        }
      />
      <Body>
        <PaneBody deployment={deployment} lines={lines} ended={ended} box={box} follow={follow} onFollow={setFollow} />
      </Body>
    </>
  );
}

function PaneBody({
  deployment,
  lines,
  ended,
  box,
  follow,
  onFollow,
}: Readonly<{
  deployment: ApprovalDeployment;
  lines: string[];
  ended: boolean;
  box: React.RefObject<HTMLDivElement | null>;
  follow: boolean;
  onFollow: (follow: boolean) => void;
}>) {
  // A deploy that never started has no log (R-154): the request itself while
  // it waits, and a sentence after.
  if (isAwaiting(deployment)) return <ApprovalRequest deployment={deployment} />;
  const never = NEVER_RAN[deployment.status];
  if (never) return <Note>{never}</Note>;
  // Deploy output lives in memory and is written nowhere, so a deploy from
  // before the last restart has none. An empty black box would read as a
  // deploy that printed nothing.
  if (ended && lines.length === 0) {
    return <Note>No output for this deploy. Deploy logs are cleared when Pando restarts.</Note>;
  }
  return <LogBox lines={lines} box={box} follow={follow} onFollow={onFollow} label="Deploy log" />;
}
