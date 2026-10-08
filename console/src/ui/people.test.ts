import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { accountIDs, chunks, unionPeople } from './people';

describe('accountIDs', () => {
  it('asks about each account once, in a stable order, and nothing that is not an account', () => {
    expect(accountIDs(['usr_b', 'tok_1', undefined, 'usr_a', 'reconciler', null, 'usr_b', ''])).toEqual([
      'usr_a',
      'usr_b',
    ]);
  });
});

describe('chunks', () => {
  it('splits into requests of at most the size', () => {
    expect(chunks([1, 2, 3, 4, 5], 2)).toEqual([[1, 2], [3, 4], [5]]);
    expect(chunks([], 2)).toEqual([]);
  });
});

describe('unionPeople', () => {
  it('keeps the first list’s entry and adds the rest once', () => {
    const a = [{ id: 'usr_1', display_name: 'Ada' }];
    const b = [
      { id: 'usr_1', display_name: 'stale' },
      { id: 'usr_2', display_name: 'Grace' },
    ];
    expect(unionPeople(a, b)).toEqual([
      { id: 'usr_1', display_name: 'Ada' },
      { id: 'usr_2', display_name: 'Grace' },
    ]);
  });
});
