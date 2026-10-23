// A terminal inside a running workload (R-084, R-086), in the side panel.
//
// R-086 says exec is the highest-privilege action in the system and that the
// verb list is not a security boundary against somebody holding it: a holder
// can read the database directly and read injected environment, secrets
// included. So the view says that, before anything is opened, rather than
// implying a terminal is just another tab.

import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal as Xterm } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';

import { Button, Icon, StatusIndicator } from '@design';

import { api, base } from '@api/client';
import type { AppSpec } from '@api/types.gen';
import { FieldSkeleton, Loading } from '../ui/Loading';
import { Body, Toolbar, WorkloadPicker } from './panelParts';

/** R-086's warning, said before a terminal is opened. */
export const EXEC_WARNING =
  'A terminal has everything the app has, including its database and its secrets. Changes made here don’t appear in its configuration.';

/** Where the session stands, for the toolbar. */
type Connection =
  | { kind: 'idle' }
  | { kind: 'connecting' }
  | { kind: 'connected' }
  | { kind: 'ended'; reason: string };

function connectionStatus(c: Connection): { status: 'running' | 'building' | 'stopped' | 'failed'; label: string } {
  switch (c.kind) {
    case 'connecting':
      return { status: 'building', label: 'Connecting' };
    case 'connected':
      return { status: 'running', label: 'Connected' };
    case 'ended':
      return { status: 'stopped', label: c.reason };
    default:
      return { status: 'stopped', label: 'Not connected' };
  }
}

export function Terminal({
  appID,
  workload,
  onWorkload,
}: Readonly<{
  appID: string;
  /** The workload in the address, if any: the app's primary otherwise. */
  workload?: string;
  onWorkload: (name: string) => void;
}>) {
  const [open, setOpen] = useState(false);
  const [connection, setConnection] = useState<Connection>({ kind: 'idle' });

  // The exec endpoint takes a workload, and a compose file describes several:
  // without asking, the only reachable one is the one the app's URL points at.
  const names = useWorkloads(appID);
  // The primary by default, which is the one somebody means by "this app" (R-026).
  const chosen = workload || names.primary;
  const shown = connectionStatus(connection);

  return (
    <>
      <Toolbar
        start={
          names.loading ? (
            <Loading gap="var(--space-2)">
              <FieldSkeleton width="24ch" />
            </Loading>
          ) : (
            <>
              <WorkloadPicker names={names.all} chosen={chosen} onChoose={onWorkload} />
              <StatusIndicator status={shown.status} label={shown.label} />
            </>
          )
        }
        end={
          open ? (
            <Button
              variant="secondary"
              onClick={() => {
                setOpen(false);
                setConnection({ kind: 'idle' });
              }}
            >
              Disconnect
            </Button>
          ) : (
            <Button variant="primary" disabled={!chosen} onClick={() => setOpen(true)}>
              Open terminal
            </Button>
          )
        }
      />
      <Body>
        {!open && <Warning />}
        {open ? (
          <Session
            // Keyed on the workload: choosing another one must tear the old
            // session down, not leave its socket open behind a new one.
            key={chosen}
            appID={appID}
            workload={chosen}
            onConnection={setConnection}
          />
        ) : (
          <Surface>
            <span style={{ font: 'var(--type-body-ui)', color: 'var(--terminal-text)', opacity: 0.7 }}>
              Open a terminal to start a shell in{' '}
              <code style={{ font: 'var(--type-code)', opacity: 1 }}>{chosen || 'this app'}</code>.
            </span>
          </Surface>
        )}
      </Body>
    </>
  );
}

/**
 * What exec grants, said before it is granted. Not a tooltip and not a dialog
 * to dismiss: R-086 asks for it plainly, and a dialog is a thing people click
 * past.
 */
function Warning() {
  return (
    <div
      role="note"
      style={{
        display: 'flex',
        alignItems: 'flex-start',
        gap: 'var(--space-3)',
        padding: 'var(--space-2) var(--space-3)',
        background: 'var(--paper-sunken)',
        borderRadius: 'var(--radius-sm)',
        font: 'var(--type-body-ui)',
        color: 'var(--ink)',
      }}
    >
      <Icon name="triangle-alert" color="var(--contour-text)" style={{ flex: '0 0 auto', marginTop: 'var(--space-1)' }} />
      <span>{EXEC_WARNING}</span>
    </div>
  );
}

/** The terminal surface, before and during a session. */
function Surface({ children }: Readonly<{ children?: React.ReactNode }>) {
  return (
    <div
      style={{
        flex: '1 1 auto',
        minHeight: 0,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: 'var(--terminal)',
        borderRadius: 'var(--radius-md)',
        padding: 'var(--space-3)',
      }}
    >
      {children}
    </div>
  );
}

/** The app's workload names, and which one is primary. */
function useWorkloads(appID: string): { all: string[]; primary: string; loading: boolean } {
  const specs = useQuery({
    queryKey: ['apps', appID, 'specs'],
    queryFn: () =>
      api.get<{ revisions: Array<{ id: string; revision: number }> | null; pinned_spec_id: string }>(`/apps/${appID}/specs`),
  });

  const pinned = (specs.data?.revisions ?? []).find((r) => r.id === specs.data?.pinned_spec_id);

  const full = useQuery({
    queryKey: ['apps', appID, 'spec', pinned?.revision],
    queryFn: () => api.get<{ body: AppSpec }>(`/apps/${appID}/specs/${pinned?.revision}`),
    enabled: Boolean(pinned),
  });

  const workloads = full.data?.body?.workloads ?? [];
  return {
    all: workloads.map((w) => w.name),
    primary: (workloads.find((w) => w.primary) ?? workloads[0])?.name ?? '',
    // `full` waits on a pinned revision and is pending for good without one.
    loading: specs.isPending || (Boolean(pinned) && full.isPending),
  };
}

function Session({
  appID,
  workload,
  onConnection,
}: Readonly<{
  appID: string;
  workload: string;
  onConnection: (c: Connection) => void;
}>) {
  const host = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const element = host.current;
    if (!element) return;
    onConnection({ kind: 'connecting' });

    // The terminal surface is identical in both themes (brand spec), so it
    // reads its colors from the terminal tokens rather than the page's.
    const styles = getComputedStyle(document.documentElement);
    const terminal = new Xterm({
      fontFamily: styles.getPropertyValue('--font-mono').trim() || 'monospace',
      fontSize: 13,
      cursorBlink: true,
      theme: {
        background: styles.getPropertyValue('--terminal').trim(),
        foreground: styles.getPropertyValue('--terminal-text').trim(),
        cursor: styles.getPropertyValue('--terminal-text').trim(),
      },
    });

    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(element);
    fit.fit();
    terminal.focus();

    const query = workload ? `?workload=${encodeURIComponent(workload)}` : '';
    const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(`${scheme}//${window.location.host}${base}/apps/${appID}/exec${query}`);
    socket.binaryType = 'arraybuffer';

    // Terminal bytes are binary frames; a resize is a text frame. Keeping them
    // apart is what stops a resize being typed into the user's shell.
    const sendSize = () => {
      fit.fit();
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ rows: terminal.rows, cols: terminal.cols }));
      }
    };

    socket.onopen = () => {
      onConnection({ kind: 'connected' });
      sendSize();
    };
    socket.onmessage = (event) => {
      if (event.data instanceof ArrayBuffer) {
        terminal.write(new Uint8Array(event.data));
        return;
      }
      terminal.write(String(event.data));
    };
    // The server closes with a reason — "exited with status 1" — so the
    // toolbar says how the shell ended rather than the terminal going quiet.
    socket.onclose = (event) =>
      onConnection({ kind: 'ended', reason: event.reason ? `Session ended: ${event.reason}` : 'Session ended' });
    socket.onerror = () => onConnection({ kind: 'ended', reason: 'Pando couldn’t open a terminal in this app.' });

    const input = terminal.onData((data) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(data));
    });

    const observer = new ResizeObserver(sendSize);
    observer.observe(element);

    return () => {
      observer.disconnect();
      input.dispose();
      socket.onclose = null;
      socket.close();
      terminal.dispose();
    };
  }, [appID, workload]);

  return (
    <div
      ref={host}
      style={{
        flex: '1 1 auto',
        minHeight: 0,
        background: 'var(--terminal)',
        borderRadius: 'var(--radius-md)',
        padding: 'var(--space-3)',
        // xterm builds its own element inside this one, with square corners:
        // without clipping it overhangs the rounded box.
        overflow: 'hidden',
      }}
    />
  );
}
