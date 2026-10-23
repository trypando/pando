// What a workload is printing (R-222), in the side panel.
//
// Followed live over server-sent events (O-51), not polled: every viewer of a
// workload on the Pando server shares one stream from the runtime, so a hundred
// people watching one app is one log read, not a hundred every five seconds.

import { useEffect, useRef, useState } from 'react';
import { Button, Skeleton, StatusIndicator, Switch } from '@design';

import { api, base } from '@api/client';
import type { App } from '@api/types.gen';
import { messageOf } from '../install/Accounts';
import { labelFor, symbolFor, useAppStatus } from './Parts';
import { appendCapped, followLog } from './logStream';
import { Body, CopyButton, LogBox, Note, Toolbar, WorkloadPicker } from './panelParts';

/** Lines asked for when the live stream cannot be opened and the plain read is the fallback. */
const TAIL = 500;

/** Where the live log stands, for what the box shows around it. */
type Output =
  | { kind: 'connecting' }
  | { kind: 'live' }
  | { kind: 'revoked'; message: string }
  | { kind: 'error'; message: string };

export function PanelOutput({
  app,
  workload,
  onWorkload,
}: Readonly<{
  app: App;
  /** The workload in the address, if any: the app's primary otherwise. */
  workload?: string;
  onWorkload: (name: string) => void;
}>) {
  // Only an app that has been deployed has a runtime to ask. The endpoint
  // refuses one with no pinned spec, and asking anyway would put an error on
  // the screen where the answer is "not yet".
  const enabled = Boolean(app.pinned_spec_id);

  const parts = useAppStatus(app).data?.workloads ?? [];
  const primary = parts.find((p) => p.primary)?.name ?? parts[0]?.name ?? '';
  const showing = workload ?? '';
  const part = parts.find((p) => p.name === (showing || primary));

  const [lines, setLines] = useState<string[]>([]);
  const [output, setOutput] = useState<Output>({ kind: 'connecting' });
  const [generation, setGeneration] = useState(0);
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!enabled) return;
    setLines([]);
    setOutput({ kind: 'connecting' });
    const query = showing ? `workload=${encodeURIComponent(showing)}` : '';
    let current = true;

    const stop = followLog(`${base}/apps/${app.id}/logs/stream${query ? `?${query}` : ''}`, {
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
          .text(`/apps/${app.id}/logs?tail=${TAIL}${query ? `&${query}` : ''}`)
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
    <>
      <Toolbar
        start={
          <>
            <WorkloadPicker names={parts.map((p) => p.name)} chosen={showing || primary} onChoose={onWorkload} />
            {part && <StatusIndicator status={symbolFor(part)} label={labelFor(part)} />}
          </>
        }
        end={
          <>
            <Switch label="Follow new lines" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            <CopyButton text={lines.join('\n')} target={box} />
          </>
        }
      />
      <Body>
        <OutputBody
          enabled={enabled}
          output={output}
          lines={lines}
          box={box}
          follow={follow}
          onFollow={setFollow}
          label={`Output of ${showing || primary || app.name}`}
          onRetry={() => setGeneration((n) => n + 1)}
        />
      </Body>
    </>
  );
}

function OutputBody({
  enabled,
  output,
  lines,
  box,
  follow,
  onFollow,
  label,
  onRetry,
}: Readonly<{
  enabled: boolean;
  output: Output;
  lines: string[];
  box: React.RefObject<HTMLDivElement | null>;
  follow: boolean;
  onFollow: (follow: boolean) => void;
  label: string;
  onRetry: () => void;
}>) {
  if (!enabled) return <Note>No output yet.</Note>;
  if (output.kind === 'error') {
    return (
      <>
        <Note>{output.message}</Note>
        <Button variant="secondary" onClick={onRetry} style={{ alignSelf: 'flex-start' }}>
          Try again
        </Button>
      </>
    );
  }
  if (output.kind === 'connecting') {
    // The box's shape, not an empty box: the runtime has not answered yet,
    // which is not the same as having printed nothing.
    return (
      <div role="status" aria-label="Loading" style={{ flex: '1 1 auto', display: 'flex' }}>
        <Skeleton height="100%" radius="md" style={{ flex: 1 }} />
      </div>
    );
  }
  return (
    <>
      <LogBox lines={lines} box={box} follow={follow} onFollow={onFollow} label={label} />
      {lines.length === 0 && <Note>No output yet.</Note>}
      {output.kind === 'revoked' && <Note>{output.message}</Note>}
    </>
  );
}
