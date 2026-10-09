// Logs (R-170, R-222).
//
// "Logs" means two different things to the person looking for them, and the
// console used to show neither properly: what the app has printed while
// running, and what a deploy printed while it was building. The first had no
// screen at all — `GET /apps/{id}/logs` existed and nothing called it — and the
// second was one unbounded box on Overview showing the newest deploy and no
// way back to the one before it.
//
// Both are bounded and scroll. A deploy log runs to thousands of lines, and a
// page that grows to that length pushes everything else on the screen out of
// reach and leaves no way back up but the scrollbar.

import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Button, CodeBlock, Select, Skeleton, StatusIndicator } from '@design';
import { BesideField } from '../ui/BesideField';

import { api, base } from '@api/client';
import type { App, Deployment } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { Parts, useAppStatus, labelFor } from './Parts';
import { appendCapped, followLog } from './logStream';
import { deployLabelFor, deployStatus } from '../ui/deploys';
import { Table } from '../ui/Table';
import { deploymentsInterval } from '../ui/polling';
import { ApprovalRequest } from './ApprovalRequest';
import { isAwaiting } from './approval';
import type { ApprovalDeployment } from './approval';

/**
 * A log, at a height that leaves the rest of the page reachable.
 *
 * It follows the tail while new lines arrive, and stops following the moment
 * the reader scrolls up — a log that yanks itself back to the bottom while
 * somebody is reading the failure three screens above is unreadable.
 */
function LogBox({
  title,
  lines,
  empty,
}: {
  title: string;
  lines: string[];
  empty?: React.ReactNode;
}) {
  const box = useRef<HTMLDivElement>(null);
  const [follow, setFollow] = useState(true);

  useEffect(() => {
    const el = box.current;
    if (!el || !follow) return;
    el.scrollTop = el.scrollHeight;
  }, [lines, follow]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <div
        ref={box}
        // Focusable, so the keyboard can scroll it: a log is read, and Page
        // Down is how a long one is read.
        tabIndex={0}
        onScroll={() => {
          const el = box.current;
          if (!el) return;
          // A few pixels of slack: a smooth scroll lands a fraction short of
          // the bottom, and an exact comparison would read that as "the reader
          // scrolled up" on every new line.
          setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
        }}
        style={{
          maxHeight: '60vh',
          overflowY: 'auto',
          // The wheel stops here rather than carrying on down the page once
          // the log reaches its end.
          overscrollBehavior: 'contain',
          borderRadius: 'var(--radius-md)',
          border: 'var(--border-width) solid var(--rule-strong)',
          background: 'var(--terminal)',
        }}
      >
        <CodeBlock
          title={title}
          lines={lines}
          style={{ border: 'none', borderRadius: 0, overflow: 'visible' }}
        />
      </div>
      {lines.length === 0 && empty}
      {!follow && (
        <Button variant="ghost" onClick={() => setFollow(true)} style={{ alignSelf: 'flex-start' }}>
          Follow the end
        </Button>
      )}
    </div>
  );
}

/**
 * One deployment's build output.
 *
 * Streamed over EventSource rather than polled, because this is the one place
 * where somebody is watching a thing happen and latency is the experience.
 */
function DeploymentLog({
  appID,
  deployment,
  fallback,
}: {
  appID: string;
  deployment: Deployment;
  /** Shown instead of the box when the stream ends with nothing in it. */
  fallback?: React.ReactNode;
}) {
  const [lines, setLines] = useState<string[]>([]);
  const [ended, setEnded] = useState(false);

  useEffect(() => {
    setLines([]);
    setEnded(false);

    const stream = new EventSource(`${base}/apps/${appID}/deployments/${deployment.id}/logs`);

    let silent: number | undefined;
    const done = () => {
      setEnded(true);
      stream.close();
      window.clearTimeout(silent);
    };

    stream.onmessage = (event) => {
      window.clearTimeout(silent);
      setLines((previous) => [...previous, event.data as string]);
    };
    stream.addEventListener('end', done);
    stream.onerror = done;

    // A deploy whose output Pando no longer holds never ends: the server's log
    // store has no stream for that ID, opens an empty one, and holds the
    // connection waiting for lines that will never come. Without this the box
    // stays black and empty forever — which is the bug this whole fallback
    // exists for, and the version that only listened for `end` did not catch
    // it. A finished deploy that has said nothing for four seconds has nothing
    // to say. Only a finished one: this used to compare the status with
    // 'running', which a deploy never has, so a queued or building deploy
    // that was quiet for four seconds was shown as having no output.
    if (deployment.finished_at) {
      silent = window.setTimeout(done, 4_000);
    }

    return () => {
      window.clearTimeout(silent);
      stream.close();
    };
  }, [appID, deployment.id, deployment.finished_at]);

  // Deploy output lives in memory (`deploy.LogStore`) and is written nowhere,
  // so a deploy from before the last restart has none. An empty black box is
  // the worst way to say that — it reads as a deploy that printed nothing —
  // so the box is not drawn at all.
  if (ended && lines.length === 0) {
    return (
      fallback ?? (
        <Quiet>
          Pando has no output for this deploy. Deploy logs are held in memory and go when Pando
          restarts.
        </Quiet>
      )
    );
  }

  return <LogBox title={`Deploy log · ${deployLabelFor(deployment)}`} lines={lines} />;
}

/** The app's own output, from the runtime, and the deploys before this one. */
export function Logs({ app, workload }: { app: App; workload?: string }) {
  const deployments = useQuery({
    queryKey: ['apps', app.id, 'deployments'],
    queryFn: () => api.get<{ deployments: ApprovalDeployment[] | null }>(`/apps/${app.id}/deployments`),
    // Not polled while a deploy runs: its log streams, and the app screen asks
    // for this list again when the status says the deploy has finished.
    refetchInterval: (query) => deploymentsInterval(query.state.data?.deployments),
  });

  const rows = deployments.data?.deployments ?? [];
  const [selected, setSelected] = useState<string | null>(null);
  const shown = rows.find((d) => d.id === selected) ?? rows[0];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)' }}>
      <Parts app={app} />

      <AppOutput app={app} workload={workload} />

      <section style={{ maxWidth: MEASURE }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Deploys</h4>

        <div style={{ marginTop: 'var(--space-4)' }}>
          <Table
            loading={deployments.isPending}
            skeletonRows={3}
            onRowClick={(row: ApprovalDeployment) => setSelected(row.id)}
            columns={[
              {
                key: 'started_at',
                header: 'Started',
                width: 'minmax(0,26ch)',
                render: (row: Deployment) => new Date(row.started_at).toLocaleString(),
              },
              {
                key: 'status',
                header: 'Result',
                width: '18ch',
                render: (row: Deployment) => (
                  <StatusIndicator
                    status={deployStatus(row.status, row.result_state)}
                    label={deployLabelFor(row)}
                  />
                ),
              },
              { key: 'trigger', header: 'Started by', width: '18ch', muted: true },
              {
                key: 'id',
                header: 'Reference',
                width: 'minmax(0,22ch)',
                mono: true,
                muted: true,
              },
            ]}
            rows={rows}
            empty={<Quiet>This app hasn’t been deployed yet.</Quiet>}
          />
        </div>

        {shown && (
          <div style={{ marginTop: 'var(--space-5)' }}>
            {/* A deploy that never started has no log to show (R-154): the
                request itself, while it waits, and a sentence after. */}
            {isAwaiting(shown) ? (
              <ApprovalRequest deployment={shown} />
            ) : NEVER_RAN[shown.status] ? (
              <Quiet>{NEVER_RAN[shown.status]}</Quiet>
            ) : (
              <DeploymentLog appID={app.id} deployment={shown} />
            )}
          </div>
        )}
      </section>
    </div>
  );
}

/** What a deploy that never started says in place of its log. */
const NEVER_RAN: Record<string, string> = {
  rejected: 'This deploy was rejected, so it never ran. The reasons and who rejected it are in the audit log.',
  expired: 'Nobody approved this deploy in time, so it never ran. Deploy again to ask again.',
  // Not superseded: that status is older than approval, and a deploy replaced
  // part-way through a build has a log worth reading.
};

/** Lines asked for when the live stream cannot be opened and the plain read is the fallback. */
const TAIL = 500;

/** The part the app's address resolves to, which is the log shown by default. */
function primaryName(parts: { name: string; primary: boolean }[]): string {
  return parts.find((part) => part.primary)?.name ?? parts[0]?.name ?? '';
}

/** Where the live log stands, for what the box shows around it. */
type Output =
  | { kind: 'connecting' }
  | { kind: 'live' }
  | { kind: 'revoked'; message: string }
  | { kind: 'error'; message: string };

function AppOutput({ app, workload }: { app: App; workload?: string }) {
  // Only an app that has been deployed has a runtime to ask. The endpoint says
  // so itself — it refuses an app with no pinned spec — and asking anyway would
  // put an error on the screen where the answer is "not yet".
  const enabled = Boolean(app.pinned_spec_id);

  // Which part's log this is. The endpoint has always taken `workload` and
  // nothing in the console passed one, so an app made of three containers
  // showed one log — the primary's — and the container that was actually
  // crash-looping had no screen at all.
  const parts = useAppStatus(app).data?.workloads ?? [];
  const [chosen, setChosen] = useState<string | null>(null);
  const showing = chosen ?? workload ?? '';

  // Followed live over server-sent events (O-51), not polled: every viewer of
  // this part on the Pando server shares one stream from the runtime, so a
  // hundred people watching one app is one log read, not a hundred every five
  // seconds. Refresh opens the stream again.
  const [lines, setLines] = useState<string[]>([]);
  const [output, setOutput] = useState<Output>({ kind: 'connecting' });
  const [generation, setGeneration] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    setLines([]);
    setOutput({ kind: 'connecting' });
    const part = showing ? `workload=${encodeURIComponent(showing)}` : '';
    let current = true;

    const stop = followLog(`${base}/apps/${app.id}/logs/stream${part ? `?${part}` : ''}`, {
      reset: () => {
        setLines([]);
        setOutput({ kind: 'live' });
      },
      lines: (batch) => setLines((previous) => appendCapped(previous, batch)),
      revoked: (message) => setOutput({ kind: 'revoked', message }),
      failed: () => {
        // EventSource does not expose the response that refused it. The same
        // question asked once, as plain text, gets the error's own words — or,
        // if it now answers, the recent lines.
        api
          .text(`/apps/${app.id}/logs?tail=${TAIL}${part ? `&${part}` : ''}`)
          .then((text) => {
            if (!current) return;
            const body = text.replace(/\n$/, '');
            setLines(body ? body.split('\n') : []);
            setOutput({ kind: 'live' });
          })
          .catch((error: unknown) => {
            if (current) setOutput({ kind: 'error', message: messageOf(error) });
          });
      },
    });
    return () => {
      current = false;
      stop();
    };
  }, [app.id, enabled, showing, generation]);

  return (
    <section style={{ maxWidth: MEASURE }}>
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: 'var(--space-4)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Output</h4>

        <span style={{ display: 'inline-flex', alignItems: 'flex-end', gap: 'var(--space-3)' }}>
          {parts.length > 1 && (
            <Select
              label="Part"
              value={showing || primaryName(parts)}
              options={parts.map((part) => ({
                value: part.name,
                label: `${part.name} — ${labelFor(part).toLowerCase()}`,
              }))}
              onChange={(e) => setChosen(e.target.value)}
            />
          )}
          <BesideField>
            <Button variant="secondary" disabled={!enabled} onClick={() => setGeneration((n) => n + 1)}>
              Refresh
            </Button>
          </BesideField>
        </span>
      </div>

      <div style={{ marginTop: 'var(--space-4)' }}>
        {!enabled ? (
          <Quiet>This app hasn’t been deployed yet, so it hasn’t printed anything.</Quiet>
        ) : output.kind === 'error' ? (
          <Quiet>{output.message}</Quiet>
        ) : output.kind === 'connecting' ? (
          // The box's shape, not an empty box with "hasn't printed anything"
          // under it: the runtime has not answered yet, which is not the same.
          <div role="status" aria-label="Loading">
            <Skeleton height="16rem" radius="md" />
          </div>
        ) : (
          <>
            <LogBox
              title={showing || app.name}
              lines={lines}
              empty={<Quiet>This part of the app hasn’t printed anything.</Quiet>}
            />
            {output.kind === 'revoked' && (
              <div style={{ marginTop: 'var(--space-3)' }}>
                <Quiet>{output.message}</Quiet>
              </div>
            )}
          </>
        )}
      </div>
    </section>
  );
}

/** Sentence case, as everything in the console is. */

