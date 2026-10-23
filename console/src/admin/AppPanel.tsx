// The side panel over an app's screen: Output, Deployments and Terminal.
//
// Each was a tab. They are not things somebody reads the app's screen for —
// they are what somebody opens from it, about one workload or one deploy, and
// closes again — so they slide over the screen at three quarters of the window
// and keep it in view beside them. The panel has its own address
// (`?panel=output&workload=worker`), so a reload or a link lands on it.
//
// Anything on the app's screen opens it through `useOpenPanel`.

import { createContext, useContext, useEffect, useId, useRef, useState } from 'react';
import { Icon, IconButton, Tabs } from '@design';
import type { TabItem } from '@design';

import type { App } from '@api/types.gen';
import type { PanelView } from '../app/route';
import { useNarrow } from '../ui/narrow';
import { PanelDeployments } from './PanelDeployments';
import { PanelOutput } from './PanelOutput';
import { Terminal } from './Terminal';
import { AppVerb, AppVerbs, can } from './verbs';
import './panel.css';

export type { PanelView };

/** What the panel shows: a view, and the workload or deployment in it. */
export interface PanelTarget {
  view: PanelView;
  /** Output and Terminal: which workload. The app's primary when absent. */
  workload?: string;
  /** Deployments: which one, or NOT_DEPLOYED. The newest when absent. */
  deployment?: string;
}

const LABELS: Record<PanelView, string> = {
  output: 'Output',
  deployments: 'Deployments',
  terminal: 'Terminal',
};

/**
 * The views this person may open, by the verbs their endpoints check: output
 * and deploy logs are `app.logs.read`, a terminal is `app.exec` (R-084).
 */
export function panelViews(verbs: readonly string[] | null | undefined): PanelView[] {
  const views: PanelView[] = [];
  if (can(verbs, AppVerb.LogsRead)) views.push('output', 'deployments');
  if (can(verbs, AppVerb.Exec)) views.push('terminal');
  return views;
}

/** Opens the panel, from anywhere on an app's screen. Absent outside one. */
const OpenPanel = createContext<((target: PanelTarget) => void) | undefined>(undefined);

export const PanelOpener = OpenPanel.Provider;

/**
 * How a control on the app's screen opens the panel. Undefined where there is
 * no panel to open, and for a view this person may not use — so a control
 * that would open it is left out rather than offered and refused.
 */
export function useOpenPanel(view?: PanelView): ((target: PanelTarget) => void) | undefined {
  const open = useContext(OpenPanel);
  const views = panelViews(useContext(AppVerbs));
  if (!open) return undefined;
  if (view && !views.includes(view)) return undefined;
  return open;
}

export function AppPanel({
  app,
  verbs,
  target,
  onChange,
  onClose,
  hrefOf,
}: Readonly<{
  app: App;
  verbs: readonly string[] | null | undefined;
  target: PanelTarget;
  /** The panel shows something else: written to the address. */
  onChange: (target: PanelTarget) => void;
  onClose: () => void;
  /** The address of a view, so each tab is a link. */
  hrefOf?: (target: PanelTarget) => string;
}>) {
  const narrow = useNarrow();
  const [full, setFull] = useState(false);
  const titleID = useId();
  const sheet = useRef<HTMLElement>(null);

  // Escape closes it — except inside the terminal, where Escape is a key the
  // shell is waiting for (vi, a pager, a prompt's history search).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      if (e.target instanceof Element && e.target.closest('.xterm')) return;
      onClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  // Focus moves into the panel, so the keyboard is where the eye is.
  useEffect(() => {
    sheet.current?.focus();
  }, []);

  const views = panelViews(verbs);
  // A link to a view this person cannot use lands on the first they can.
  const view = views.includes(target.view) ? target.view : views[0];
  if (!view) return null;

  const items: TabItem[] = views.map((v) => ({
    value: v,
    label: LABELS[v],
    href: hrefOf?.({ view: v, workload: target.workload }),
  }));

  return (
    <div style={{ position: 'fixed', inset: 0, zIndex: 50 }}>
      {/* The scrim: a click closes the panel, as Escape does. */}
      <div aria-hidden="true" onClick={onClose} style={{ position: 'absolute', inset: 0, background: 'var(--scrim)' }} />
      <aside
        ref={sheet}
        className="pando-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        tabIndex={-1}
        style={{
          position: 'absolute',
          top: 0,
          right: 0,
          bottom: 0,
          width: full || narrow ? '100%' : '75%',
          display: 'flex',
          flexDirection: 'column',
          background: 'var(--paper-raised)',
          borderLeft: 'var(--border-width) solid var(--rule)',
          boxShadow: 'var(--shadow-popover)',
          outline: 'none',
        }}
      >
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 'var(--space-4)',
            padding: 'var(--space-4) var(--space-5) 0',
          }}
        >
          <h2 id={titleID} style={{ font: 'var(--type-h3)', color: 'var(--ink)', margin: 0 }}>
            {app.name}
          </h2>
          <span style={{ display: 'inline-flex', gap: 'var(--space-1)' }}>
            {!narrow && (
              <IconButton label={full ? 'Exit full screen' : 'Full screen'} onClick={() => setFull(!full)}>
                <Icon name={full ? 'minimize-2' : 'maximize-2'} />
              </IconButton>
            )}
            <IconButton label="Close" onClick={onClose}>
              <Icon name="x" />
            </IconButton>
          </span>
        </div>

        <Tabs
          value={view}
          items={items}
          // Output and Terminal are about the same workload; a deployment
          // belongs to Deployments alone.
          onChange={(v) => onChange({ view: v as PanelView, workload: target.workload })}
          style={{ padding: '0 var(--space-5)' }}
        />

        <div style={{ flex: '1 1 auto', minHeight: 0, display: 'flex', flexDirection: 'column' }}>
          {view === 'output' && (
            <PanelOutput app={app} workload={target.workload} onWorkload={(workload) => onChange({ view, workload })} />
          )}
          {view === 'deployments' && (
            <PanelDeployments app={app} deployment={target.deployment} onDeployment={(deployment) => onChange({ view, deployment })} />
          )}
          {view === 'terminal' && (
            <Terminal appID={app.id} workload={target.workload} onWorkload={(workload) => onChange({ view, workload })} />
          )}
        </div>
      </aside>
    </div>
  );
}
