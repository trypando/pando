// What every popover list in the console does the same way: a click outside
// closes it, and the arrow keys move through it and wrap at either end.
// ActorField, RecipientField, TagField and Menu share these so that they keep
// behaving alike.

import { useEffect } from 'react';
import type { RefObject } from 'react';

/** Calls close on a mousedown outside root, while open. */
export function useCloseOnOutside(root: RefObject<HTMLElement | null>, open: boolean, close: () => void) {
  useEffect(() => {
    if (!open) return undefined;
    const outside = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) close();
    };
    document.addEventListener('mousedown', outside);
    return () => document.removeEventListener('mousedown', outside);
    // close is a fresh closure on every render; the listener only needs the
    // one from the render that opened the list.
  }, [open, root]);
}

/** The index ArrowDown or ArrowUp moves to from i, in a list of n, wrapping. */
export function stepIndex(key: string, i: number, n: number): number {
  return key === 'ArrowDown' ? (i + 1) % n : (i - 1 + n) % n;
}
