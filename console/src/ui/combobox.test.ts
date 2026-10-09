import { describe, expect, it, vi } from 'vitest';

import { listenOutside, stepIndex } from './combobox';

// A document that keeps the one listener it is given, and a node that
// contains only itself.
function fakeDocument() {
  let listener: ((e: MouseEvent) => void) | null = null;
  return {
    addEventListener: vi.fn((_: string, l: (e: MouseEvent) => void) => {
      listener = l;
    }),
    removeEventListener: vi.fn(() => {
      listener = null;
    }),
    press: (target: unknown) => listener?.({ target } as MouseEvent),
    listening: () => listener !== null,
  };
}

const inside = { contains: (n: unknown) => n === inside } as unknown as Node;

describe('listenOutside', () => {
  it('closes on a press outside, not inside, and stops when undone', () => {
    const doc = fakeDocument();
    const close = vi.fn();
    const stop = listenOutside(doc as unknown as Document, { current: inside }, close);

    doc.press(inside);
    expect(close).not.toHaveBeenCalled();
    doc.press({});
    expect(close).toHaveBeenCalledTimes(1);

    stop();
    expect(doc.listening()).toBe(false);
    expect(doc.removeEventListener).toHaveBeenCalledWith('mousedown', expect.any(Function));
  });

  it('closes on any press while the root is not mounted', () => {
    const doc = fakeDocument();
    const close = vi.fn();
    listenOutside(doc as unknown as Document, { current: null }, close);
    doc.press({});
    expect(close).toHaveBeenCalledTimes(1);
  });
});

describe('stepIndex', () => {
  it('moves down and up, wrapping at either end', () => {
    expect(stepIndex('ArrowDown', 0, 3)).toBe(1);
    expect(stepIndex('ArrowDown', 2, 3)).toBe(0);
    expect(stepIndex('ArrowUp', 1, 3)).toBe(0);
    expect(stepIndex('ArrowUp', 0, 3)).toBe(2);
  });
});
