// A small menu behind a three-dot button.
//
// The design system has no menu component; this is one built from its rules
// for popovers: paper-raised, a 1px rule border and the one popover shadow,
// 6px corners as a bounded object. Items are plain text — a menu of three
// things does not need an icon beside each.
//
// Keyboard: the button opens it and focuses the first item, the arrow keys
// move, Escape closes it and puts focus back on the button, and so does
// choosing anything. A click outside closes it.

import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Icon, IconButton } from '@design';
import { useCloseOnOutside } from './combobox';

export function Menu({
  label,
  children,
  onOpenChange,
  align = 'end',
  view,
}: {
  /** What the button says to a screen reader, e.g. "Options for Notes". */
  label: string;
  /** Which edge of the button the menu lines up with. `end` opens leftward,
   *  for a button at the right of a tile; `start` rightward, for one beside a
   *  heading at the left of the page. */
  align?: 'start' | 'end';
  /** Changes when the menu shows a different page of items, so the first of
   *  them takes focus — otherwise the focused item vanishes with the page and
   *  a keyboard user is left nowhere. */
  view?: string;
  /** The items, given a way to close the menu. */
  children: (close: () => void) => ReactNode;
  onOpenChange?: (open: boolean) => void;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLDivElement>(null);

  const set = (next: boolean) => {
    setOpen(next);
    onOpenChange?.(next);
  };
  const close = () => {
    set(false);
    root.current?.querySelector<HTMLButtonElement>('button')?.focus();
  };

  useEffect(() => {
    if (open) list.current?.querySelector<HTMLElement>('[role="menuitem"], input')?.focus();
  }, [open, view]);

  useCloseOnOutside(root, open, () => set(false));

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      close();
      return;
    }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    e.preventDefault();
    const items = Array.from(list.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
    const at = items.indexOf(document.activeElement as HTMLElement);
    const next = e.key === 'ArrowDown' ? (at + 1) % items.length : (at - 1 + items.length) % items.length;
    items[next]?.focus();
  };

  return (
    <div ref={root} style={{ position: 'relative' }} onKeyDown={onKeyDown}>
      <IconButton label={label} aria-haspopup="menu" aria-expanded={open} onClick={() => set(!open)}>
        <Icon name="ellipsis-vertical" size={16} />
      </IconButton>
      {open && (
        <div
          ref={list}
          role="menu"
          aria-label={label}
          style={{
            position: 'absolute',
            top: 'calc(100% + var(--space-1))',
            ...(align === 'end' ? { right: 0 } : { left: 0 }),
            zIndex: 10,
            minWidth: '22ch',
            padding: 'var(--space-1)',
            background: 'var(--paper-raised)',
            border: 'var(--border-width) solid var(--rule-strong)',
            borderRadius: 'var(--radius-md)',
            boxShadow: 'var(--shadow-popover)',
            display: 'flex',
            flexDirection: 'column',
          }}
        >
          {children(close)}
        </div>
      )}
    </div>
  );
}

const itemStyle: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 'var(--space-2)',
  width: '100%',
  padding: 'var(--space-2) var(--space-3)',
  border: 'none',
  borderRadius: 'var(--radius-sm)',
  background: 'transparent',
  font: 'var(--type-body-ui)',
  color: 'var(--ink)',
  textAlign: 'left',
  textDecoration: 'none',
  cursor: 'pointer',
};

/** One choice. A link when given `href`, a button otherwise. */
export function MenuItem({
  children,
  onSelect,
  href,
  checked,
}: {
  children: ReactNode;
  onSelect?: () => void;
  href?: string;
  /** Marks the current choice in a list of options, such as a section. */
  checked?: boolean;
}) {
  const [hover, setHover] = useState(false);
  const style = {
    ...itemStyle,
    background: hover ? 'var(--paper-sunken)' : 'transparent',
  };
  const events = {
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    onFocus: () => setHover(true),
    onBlur: () => setHover(false),
  };
  const mark = checked !== undefined && (
    <span style={{ width: '1em', display: 'inline-flex' }}>{checked && <Icon name="check" size={14} />}</span>
  );

  if (href) {
    return (
      <a role="menuitem" href={href} target="_blank" rel="noopener noreferrer" style={style} onClick={onSelect} {...events}>
        {mark}
        {children}
      </a>
    );
  }
  return (
    <button role="menuitem" type="button" style={style} onClick={onSelect} {...events}>
      {mark}
      {children}
    </button>
  );
}

export function MenuDivider() {
  return (
    <div
      role="separator"
      style={{ height: 'var(--border-width)', background: 'var(--rule)', margin: 'var(--space-1) 0' }}
    />
  );
}
