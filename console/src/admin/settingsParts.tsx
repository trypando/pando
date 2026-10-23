// Pieces every page of an app's Settings is built from: the page's title with
// its one line, a card's title, and a setting's row. Short titles, at most one
// short line under each.

import { Switch } from '@design';

import './settings.css';

/** A page's title, its one line, and the action beside them. */
export function PageHead({
  title,
  line,
  action,
}: Readonly<{ title: string; line?: React.ReactNode; action?: React.ReactNode }>) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', flexWrap: 'wrap', gap: 'var(--space-4)' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        <h3 style={{ font: 'var(--type-h4)', margin: 0 }}>{title}</h3>
        {line && <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{line}</span>}
      </div>
      {action}
    </div>
  );
}

/** A card's title, its one line, and what sits beside them. */
export function CardHead({
  title,
  line,
  action,
}: Readonly<{ title: React.ReactNode; line?: React.ReactNode; action?: React.ReactNode }>) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', flexWrap: 'wrap', gap: 'var(--space-3)' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
        <h4 style={{ font: 'var(--type-label)', margin: 0, color: 'var(--ink)' }}>{title}</h4>
        {line && <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{line}</span>}
      </div>
      {action && <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>{action}</div>}
    </div>
  );
}

/** A setting that takes effect as it flips: its name and line, the switch on the right. */
export function SwitchRow({
  label,
  line,
  checked,
  disabled,
  onChange,
  children,
}: Readonly<{
  label: string;
  line: React.ReactNode;
  checked: boolean;
  disabled?: boolean;
  onChange: (on: boolean) => void;
  /** Beside the line: a number to change, say. */
  children?: React.ReactNode;
}>) {
  return (
    <div className="pando-setting-row">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
        <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{label}</span>
        <span style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2)', font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          {line}
          {children}
        </span>
      </div>
      <Switch aria-label={label} checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
    </div>
  );
}

/** The quiet line a page or card says when there is nothing in it. */
export function Quiet({ children }: Readonly<{ children: React.ReactNode }>) {
  return <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>{children}</p>;
}
