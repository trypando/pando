// What every popover list in the console does the same way: a click outside
// closes it, and the arrow keys move through it and wrap at either end.
// ActorField, RecipientField, TagField and Menu share these so that they keep
// behaving alike.

import { useEffect } from 'react';
import type { RefObject } from 'react';

/** The part of a document listenOutside uses. */
type Listened = Pick<Document, 'addEventListener' | 'removeEventListener'>;

/** What counts as inside: one element, or several, such as a button and the
 *  list it opens when the list is portaled elsewhere in the document. */
type Roots = RefObject<Node | null> | readonly RefObject<Node | null>[];

/**
 * Calls close on a mousedown outside root, until the returned function is
 * called. Apart from the hook so it can be tested without a DOM.
 */
export function listenOutside(doc: Listened, root: Roots, close: () => void): () => void {
  const roots = Array.isArray(root) ? root : [root as RefObject<Node | null>];
  const outside = (e: MouseEvent) => {
    if (!roots.some((r) => r.current?.contains(e.target as Node))) close();
  };
  doc.addEventListener('mousedown', outside);
  return () => doc.removeEventListener('mousedown', outside);
}

/** Calls close on a mousedown outside root, while open. */
export function useCloseOnOutside(root: Roots, open: boolean, close: () => void) {
  // close is a fresh closure on every render; the listener only needs the one
  // from the render that opened the list.
  useEffect(() => (open ? listenOutside(document, root, close) : undefined), [open, root]);
}

/** The index ArrowDown or ArrowUp moves to from i, in a list of n, wrapping. */
export function stepIndex(key: string, i: number, n: number): number {
  return key === 'ArrowDown' ? (i + 1) % n : (i - 1 + n) % n;
}

/**
 * A combobox input's keydown for a list chosen from only: the arrow keys move
 * through n options, Enter chooses the highlighted one, Escape closes.
 */
export function pickKeys({
  open,
  at,
  n,
  setOpen,
  setAt,
  choose,
}: {
  open: boolean;
  at: number;
  n: number;
  setOpen: (open: boolean) => void;
  setAt: (step: (i: number) => number) => void;
  choose: (i: number) => void;
}) {
  return (e: { key: string; preventDefault: () => void }) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (n === 0) return;
      setOpen(true);
      setAt((i) => stepIndex(e.key, i, n));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (open && at < n) choose(at);
    } else if (e.key === 'Escape') {
      setOpen(false);
    }
  };
}
