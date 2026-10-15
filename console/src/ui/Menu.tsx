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
//
// The list is portaled to the body and placed against the window, not inside
// the button's parent. The launcher's header hides its overflow to crop its
// contour map, and a list placed inside it was cut off at the header's edge;
// a button near the left of the admin sidebar opened its list off the screen.
// So it is kept a --space-4 gutter inside the window, its width capped to the
// window, and opens above the button when there is no room below.

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Icon, IconButton } from '@design';
import { useCloseOnOutside } from './combobox';

interface Box {
  top: number;
  bottom: number;
  left: number;
  right: number;
}

/**
 * Where the list goes: gap under the button, lined up with the edge align
 * names, moved in to stay gutter inside the window's sides, and above the
 * button when it does not fit below but does above. When it fits on neither
 * side it goes on the roomier one with maxHeight set to that room, and
 * scrolls, so no item is out of reach below the window.
 */
export function placeMenu(
  button: Box,
  list: { width: number; height: number },
  viewport: { width: number; height: number },
  align: 'start' | 'end',
  gap: number,
  gutter: number,
): { top: number; left: number; maxHeight?: number } {
  const wanted = align === 'end' ? button.right - list.width : button.left;
  const left = Math.max(gutter, Math.min(wanted, viewport.width - list.width - gutter));
  const below = button.bottom + gap;
  const roomBelow = viewport.height - gap - below;
  const roomAbove = button.top - 2 * gap;
  if (list.height <= roomBelow) return { top: below, left };
  if (list.height <= roomAbove) return { top: button.top - gap - list.height, left };
  if (roomBelow >= roomAbove) return { top: below, left, maxHeight: Math.max(roomBelow, 0) };
  return { top: gap, left, maxHeight: roomAbove };
}

/** A spacing token in pixels, for arithmetic CSS cannot do; fallback when the
 *  token cannot be read. */
function spacePx(token: string, fallback: number): number {
  return Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue(token)) || fallback;
}

export function Menu({
  label,
  children,
  onOpenChange,
  align = 'end',
  view,
  icon = 'ellipsis-vertical',
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
  /** The button's icon, for a menu that is not "more options". */
  icon?: string;
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

  // The list is not inside root once portaled, so a press on it is not outside.
  const inside = useMemo(() => [root, list], []);
  useCloseOnOutside(inside, open, () => set(false));

  // Placed before paint, again whenever its page changes size, and kept beside
  // the button while anything scrolls or the window resizes.
  useLayoutEffect(() => {
    if (!open) return undefined;
    const place = () => {
      const button = root.current?.getBoundingClientRect();
      const el = list.current;
      if (!button || !el) return;
      // Measured at its full height, not the height an earlier placing capped.
      el.style.maxHeight = '';
      const at = placeMenu(
        button,
        { width: el.offsetWidth, height: el.offsetHeight },
        {
          width: document.documentElement.clientWidth || window.innerWidth,
          height: document.documentElement.clientHeight || window.innerHeight,
        },
        align,
        spacePx('--space-1', 4),
        spacePx('--space-4', 16),
      );
      el.style.top = `${at.top}px`;
      el.style.left = `${at.left}px`;
      el.style.maxHeight = at.maxHeight === undefined ? '' : `${at.maxHeight}px`;
    };
    place();
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => {
      window.removeEventListener('scroll', place, true);
      window.removeEventListener('resize', place);
    };
  }, [open, view, align]);

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
    <div ref={root} style={{ position: 'relative' }}>
      <IconButton label={label} aria-haspopup="menu" aria-expanded={open} onClick={() => set(!open)}>
        <Icon name={icon} size={16} />
      </IconButton>
      {open &&
        createPortal(
          <div
            ref={list}
            role="menu"
            aria-label={label}
            tabIndex={-1}
            onKeyDown={onKeyDown}
            style={{
              position: 'fixed',
              top: 0,
              left: 0,
              // Above the phone-width admin menu's scrim, which is 20.
              zIndex: 30,
              minWidth: 'min(22ch, calc(100vw - 2 * var(--space-4)))',
              maxWidth: 'calc(100vw - 2 * var(--space-4))',
              // Scrolls when placing caps its height to a short window.
              overflowY: 'auto',
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
          </div>,
          document.body,
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
  // The menu is capped at the window's width, so a long section name wraps
  // inside it rather than running past the edge.
  overflowWrap: 'anywhere',
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
