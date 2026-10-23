// The pieces the side panel's three views share: the bar of controls across
// the top of each, the workload picker, Copy, and the log on the terminal
// surface.

import { useEffect, useState, type ReactNode, type RefObject } from 'react';
import { Button, CodeBlock, Icon, Select } from '@design';

import { copyOrSelect } from '../ui/CopyField';

/** A view's controls: what it is looking at on the left, actions on the right. */
export function Toolbar({ start, end }: Readonly<{ start?: ReactNode; end?: ReactNode }>) {
  return (
    <div
      style={{
        display: 'flex',
        flexWrap: 'wrap',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: 'var(--space-4)',
        padding: 'var(--space-3) var(--space-5)',
      }}
    >
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)', minWidth: 0 }}>{start}</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>{end}</div>
    </div>
  );
}

/**
 * Which workload a view is about.
 *
 * Shown even when the app has one, because it answers a question as well as
 * offering a choice: output or a shell that does not say *where* leaves the
 * reader to know the app has one part.
 */
export function WorkloadPicker({
  names,
  chosen,
  onChoose,
}: Readonly<{ names: string[]; chosen: string; onChoose: (name: string) => void }>) {
  if (names.length === 0) return null;
  return (
    <>
      <label htmlFor="s-workload" style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
        Workload
      </label>
      <Select
        id="s-workload"
        mono
        value={chosen}
        options={names.map((n) => ({ value: n, label: n }))}
        onChange={(e) => onChoose(e.target.value)}
      />
    </>
  );
}

/** Copies `text`, or selects `target` where the browser keeps the clipboard
 *  from the page, and says which it did. */
export function CopyButton({ text, target }: Readonly<{ text: string; target: RefObject<HTMLElement | null> }>) {
  const [done, setDone] = useState<'copied' | 'selected' | null>(null);
  useEffect(() => {
    if (!done) return undefined;
    const t = window.setTimeout(() => setDone(null), 2_000);
    return () => window.clearTimeout(t);
  }, [done]);
  return (
    <Button
      variant="ghost"
      icon={<Icon name={done === 'copied' ? 'check' : 'copy'} />}
      disabled={text === ''}
      onClick={() => void copyOrSelect(text, target.current).then(setDone)}
    >
      {done === 'copied' ? 'Copied' : done === 'selected' ? 'Selected' : 'Copy'}
    </Button>
  );
}

/**
 * A log on the terminal surface, filling what is left of the panel.
 *
 * It follows the tail while `follow` is on, and turns it off the moment the
 * reader scrolls up — a log that yanks itself back to the bottom while
 * somebody is reading the failure three screens above is unreadable. Scrolling
 * back to the bottom turns it on again.
 */
export function LogBox({
  lines,
  box,
  follow,
  onFollow,
  label,
}: Readonly<{
  lines: string[];
  box: RefObject<HTMLDivElement | null>;
  follow: boolean;
  onFollow: (follow: boolean) => void;
  /** What the log is, for a screen reader. */
  label: string;
}>) {
  useEffect(() => {
    const el = box.current;
    if (!el || !follow) return;
    el.scrollTop = el.scrollHeight;
  }, [lines, follow, box]);

  return (
    <div
      ref={box}
      // Focusable, so the keyboard can scroll it: a log is read, and Page
      // Down is how a long one is read.
      tabIndex={0}
      role="log"
      aria-label={label}
      onScroll={() => {
        const el = box.current;
        if (!el) return;
        // Slack: a smooth scroll lands a fraction short of the bottom, and an
        // exact comparison would read that as scrolling up on every line.
        const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
        if (atEnd !== follow) onFollow(atEnd);
      }}
      style={{
        flex: '1 1 auto',
        minHeight: 0,
        overflowY: 'auto',
        overscrollBehavior: 'contain',
        borderRadius: 'var(--radius-md)',
        background: 'var(--terminal)',
      }}
    >
      <CodeBlock lines={lines} copyable={false} style={{ border: 'none', borderRadius: 0, overflow: 'visible' }} />
    </div>
  );
}

/** The space under a view's toolbar, down to the panel's foot. */
export function Body({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <div
      style={{
        flex: '1 1 auto',
        minHeight: 0,
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-3)',
        padding: '0 var(--space-5) var(--space-5)',
      }}
    >
      {children}
    </div>
  );
}

/** A sentence where the log would be: nothing printed yet, or why not. */
export function Note({ children }: Readonly<{ children: ReactNode }>) {
  return <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>{children}</p>;
}
