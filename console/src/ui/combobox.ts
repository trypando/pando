// What every popover list in the console does the same way: a click outside
// closes it, and the arrow keys move through it and wrap at either end.
// ActorField, RecipientField, TagField and Menu share these so that they keep
// behaving alike.

import { useEffect } from 'react';
import type { RefObject } from 'react';

/** The part of a document listenOutside uses. */
type Listened = Pick<Document, 'addEventListener' | 'removeEventListener'>;

/**
 * Calls close on a mousedown outside root, until the returned function is
 * called. Apart from the hook so it can be tested without a DOM.
 */
export function listenOutside(doc: Listened, root: RefObject<Node | null>, close: () => void): () => void {
  const outside = (e: MouseEvent) => {
    if (!root.current?.contains(e.target as Node)) close();
  };
  doc.addEventListener('mousedown', outside);
  return () => doc.removeEventListener('mousedown', outside);
}

/** Calls close on a mousedown outside root, while open. */
export function useCloseOnOutside(root: RefObject<HTMLElement | null>, open: boolean, close: () => void) {
  // close is a fresh closure on every render; the listener only needs the one
  // from the render that opened the list.
  useEffect(() => (open ? listenOutside(document, root, close) : undefined), [open, root]);
}

/** The index ArrowDown or ArrowUp moves to from i, in a list of n, wrapping. */
export function stepIndex(key: string, i: number, n: number): number {
  return key === 'ArrowDown' ? (i + 1) % n : (i - 1 + n) % n;
}
