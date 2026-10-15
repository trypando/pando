import { describe, expect, it } from 'vitest';

import { placeMenu } from './Menu';

const viewport = { width: 1200, height: 800 };
const list = { width: 200, height: 90 };
const gap = 4;
const gutter = 16;

// A button 32 wide and tall, its left edge at x.
const button = (x: number, y = 20) => ({ left: x, right: x + 32, top: y, bottom: y + 32 });

describe('placeMenu', () => {
  it('opens under the button, lined up with the edge it is asked to', () => {
    expect(placeMenu(button(900), list, viewport, 'end', gap, gutter)).toEqual({ top: 56, left: 732 });
    expect(placeMenu(button(900), list, viewport, 'start', gap, gutter)).toEqual({ top: 56, left: 900 });
  });

  // The admin sidebar's button sits near the window's left edge, and a list
  // lined up with its right edge ran off the screen.
  it('stays inside the window on the left', () => {
    expect(placeMenu(button(120), list, viewport, 'end', gap, gutter).left).toBe(gutter);
  });

  it('stays inside the window on the right', () => {
    expect(placeMenu(button(1150), list, viewport, 'start', gap, gutter).left).toBe(1200 - 200 - gutter);
  });

  it('opens above the button when there is no room below', () => {
    expect(placeMenu(button(500, 740), list, viewport, 'end', gap, gutter).top).toBe(740 - gap - 90);
  });

  it('stays below when there is no room above either', () => {
    const short = { width: 1200, height: 120 };
    expect(placeMenu(button(500, 40), list, short, 'end', gap, gutter).top).toBe(76);
  });
});
