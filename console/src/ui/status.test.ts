import { describe, expect, it } from 'vitest';

import { statusLabel, statusSymbol } from './status';

describe('statusLabel', () => {
  // R-396: an app Pando stopped for being idle says so, so it is not taken for
  // one somebody stopped, or for a failed one (R-151).
  it('says an app was stopped for inactivity', () => {
    expect(statusLabel('stopped', true)).toBe('Stopped for inactivity');
    expect(statusSymbol('stopped')).toBe('stopped');
  });

  // R-403: and one stopped for staying over its disk limit says that.
  it('says an app was stopped over its disk limit', () => {
    expect(statusLabel('stopped', false, true)).toBe('Stopped over disk limit');
    expect(statusLabel('running', false, true)).toBe('Running');
  });

  it('says only stopped when somebody stopped it', () => {
    expect(statusLabel('stopped')).toBe('Stopped');
    expect(statusLabel('stopped', false)).toBe('Stopped');
  });

  it('ignores the flag on an app that is not stopped', () => {
    expect(statusLabel('running', true)).toBe('Running');
    expect(statusLabel('failed', true)).toBe('Failed');
  });
});
