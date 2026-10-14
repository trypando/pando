// A terminal inside a running workload (R-084, R-086).
//
// The copy on this screen is doing real work. R-086 says the documentation must
// state plainly that exec is the highest-privilege action in the system and that
// the verb list is **not** a security boundary against someone holding it — a
// holder can read the database directly and read injected environment including
// secrets. So the screen says that, before opening anything, rather than
// implying a terminal is just another tab.
//
// It also says what is recorded: the command, not the session (O-7). Someone
// about to type a password into a shell is entitled to know which of those two
// ends up in the audit log.

import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal as Xterm } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';

import { Button, Card, Select } from '@design';

import { api, base } from '@api/client';
import type { AppSpec } from '@api/types.gen';
import { MEASURE } from '../ui/layout';
import { FieldSkeleton, LineSkeleton, Loading } from '../ui/Loading';

export function Terminal({ appID }: { appID: string }) {
  const [open, setOpen] = useState(false);
  const [workload, setWorkload] = useState<string>('');

  // Which parts this app has. A compose file describes several services, and
  // the exec endpoint has always taken a workload — the console just never
  // asked, so there was no way to reach anything but the one the app's URL
  // points at.
  const names = useWorkloads(appID);

  // Default to the primary, which is the one somebody means by "this app"
  // (R-026), and only once the list has arrived.
  const chosen = workload || names.primary;

  if (!open) {
    return (
      <Warning
        names={names.all}
        loading={names.loading}
        chosen={chosen}
        onChoose={setWorkload}
        onOpen={() => setOpen(true)}
      />
    );
  }
  return (
    <Session
      // Keyed on the workload: choosing another part must tear the old session
      // down, not leave its socket open behind a new one.
      key={chosen}
      appID={appID} workload={chosen} names={names.all} onChoose={(w) => {
      // Changing the target closes this session and opens one in the other
      // part. Session keys on the workload, so React rebuilds it.
      setWorkload(w);
    }} onClose={() => setOpen(false)} />
  );
}

/** The app's workload names, and which one is primary. */
function useWorkloads(appID: string): { all: string[]; primary: string; loading: boolean } {
  const specs = useQuery({
    queryKey: ['apps', appID, 'specs'],
    queryFn: () => api.get<{ revisions: Array<{ id: string; revision: number }> | null; pinned_spec_id: string }>(
      `/apps/${appID}/specs`,
    ),
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

/**
 * Which part of the app the terminal is in.
 *
 * Shown even when there is only one, because it answers a question as well as
 * offering a choice: a terminal that drops you straight into a shell does not
 * say *where*. On a single-service app the answer is obvious only to somebody
 * who already knows the app has one service.
 */
function WorkloadPicker({
  names,
  chosen,
  onChoose,
  loading = false,
}: {
  names: string[];
  chosen: string;
  onChoose: (w: string) => void;
  loading?: boolean;
}) {
  // The field's shape while the spec loads, so Open terminal does not move
  // down under the pointer when the list arrives.
  if (loading) {
    return (
      <Loading gap="var(--space-2)">
        <LineSkeleton width="18ch" font="var(--type-label)" />
        <FieldSkeleton width="32ch" />
      </Loading>
    );
  }
  // Nothing for an app with no workloads at all — which is an app that has
  // never been through review.
  if (names.length === 0) return null;
  return (
    <Select
      label="Part"
      value={chosen}
      options={names.map((n) => ({ value: n, label: n }))}
      onChange={(e) => onChoose(e.target.value)}
      style={{ maxWidth: '32ch' }}
    />
  );
}

/**
 * What exec actually grants, said before it is granted.
 *
 * Not a tooltip and not a dialog to dismiss: R-086 asks for this to be stated
 * plainly, and a dialog is a thing people click past.
 */
function Warning({
  names,
  loading,
  chosen,
  onChoose,
  onOpen,
}: {
  names: string[];
  loading: boolean;
  chosen: string;
  onChoose: (w: string) => void;
  onOpen: () => void;
}) {
  return (
    <Card padding="md" style={{ maxWidth: MEASURE }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Terminal</h4>

        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>
          A terminal has everything the app has, including its database and its secrets. Changes made
          here don&rsquo;t appear in its configuration.
        </p>

        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          Pando records the session and the command it starts. Input and output aren&rsquo;t recorded.
        </p>

        <WorkloadPicker names={names} chosen={chosen} onChoose={onChoose} loading={loading} />

        <Button variant="primary" onClick={onOpen} style={{ alignSelf: 'flex-start' }}>
          Open terminal
        </Button>
      </div>
    </Card>
  );
}

function Session({
  appID,
  workload,
  names,
  onChoose,
  onClose,
}: {
  appID: string;
  workload?: string;
  names: string[];
  onChoose: (w: string) => void;
  onClose: () => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<string>('Connecting.');

  useEffect(() => {
    const element = host.current;
    if (!element) return;

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

    const query = workload ? `?workload=${encodeURIComponent(workload)}` : '';
    const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(
      `${scheme}//${window.location.host}${base}/apps/${appID}/exec${query}`,
    );
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
      setStatus('');
      sendSize();
    };

    socket.onmessage = (event) => {
      if (event.data instanceof ArrayBuffer) {
        terminal.write(new Uint8Array(event.data));
        return;
      }
      terminal.write(String(event.data));
    };

    socket.onclose = (event) => {
      // The server closes with a reason — "exited with status 1" — so the
      // terminal says how the shell ended rather than going blank.
      setStatus(event.reason ? `Session ended: ${event.reason}` : 'Session ended.');
    };

    socket.onerror = () => setStatus('Pando couldn’t open a terminal in this app.');

    const input = terminal.onData((data) => {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(new TextEncoder().encode(data));
      }
    });

    const observer = new ResizeObserver(sendSize);
    observer.observe(element);

    return () => {
      observer.disconnect();
      input.dispose();
      socket.close();
      terminal.dispose();
    };
  }, [appID, workload]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 'var(--space-3)',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'flex-end', gap: 'var(--space-4)' }}>
          <WorkloadPicker names={names} chosen={workload ?? ''} onChoose={onChoose} />
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{status}</span>
        </div>
        <Button variant="secondary" onClick={onClose}>
          Close terminal
        </Button>
      </div>

      <div
        ref={host}
        style={{
          background: 'var(--terminal)',
          borderRadius: 'var(--radius-md)',
          padding: 'var(--space-3)',

          // xterm builds its own element inside this one, with its own
          // background and square corners. Without clipping, it overhangs the
          // rounded box by however much the row height fails to divide the
          // available space — a dark strip with sharp corners under a rounded
          // terminal.
          overflow: 'hidden',

          // Sized in lines, because that is what a terminal is measured in.
          // This was `var(--space-10)` — a spacing token used as a height, and
          // 128px is about eight lines, which is too few to read a stack trace
          // in.
          height: '26em',
        }}
      />
    </div>
  );
}
