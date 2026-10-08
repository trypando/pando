import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// React Query runs no interval where there is no window, which is what a test
// in Node looks like to it; and the API client reads the page's address.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { InfiniteQueryObserver, QueryClient, QueryObserver } from '@tanstack/react-query';
import type { InfiniteData } from '@tanstack/react-query';

import { mergeHead, polledHeadOptions, type HeadShape } from './headPoll';

interface Page {
  rows: string[];
  next?: string;
}

const shape: HeadShape<Page, string> = {
  rowsOf: (p) => p.rows,
  withRows: (p, rows) => ({ ...p, rows }),
  idOf: (r) => r,
  hasMore: (p) => Boolean(p.next),
};

describe('mergeHead', () => {
  it('replaces a lone first page with the fresh one, removals and cursor included', () => {
    const merged = mergeHead([{ rows: ['c', 'b', 'a'] }], { rows: ['d', 'c', 'a'], next: 'a' }, shape);
    expect(merged).toEqual([{ rows: ['d', 'c', 'a'], next: 'a' }]);
  });

  it('keeps rows a full fresh page pushed down, and shows none twice', () => {
    const pages = [
      { rows: ['e', 'd', 'c'], next: 'c' },
      { rows: ['b', 'a'] },
    ];
    // Two new rows: d and c no longer fit on a page of three, and the second
    // page, read after c, does not have them.
    const merged = mergeHead(pages, { rows: ['g', 'f', 'e'], next: 'e' }, shape);
    expect(merged.flatMap((p) => p.rows)).toEqual(['g', 'f', 'e', 'd', 'c', 'b', 'a']);
    expect(merged).toHaveLength(2);
    expect(merged[0]?.next).toBe('c');
  });

  it('drops what a short fresh page no longer has: the list ends inside it', () => {
    const pages = [
      { rows: ['c', 'b'], next: 'b' },
      { rows: ['a'] },
    ];
    const merged = mergeHead(pages, { rows: ['c', 'a'] }, shape);
    expect(merged.flatMap((p) => p.rows)).toEqual(['c', 'a']);
  });

  it('takes a row off a later page once the fresh page holds it', () => {
    const pages = [
      { rows: ['c', 'b'], next: 'b' },
      { rows: ['a'] },
    ];
    const merged = mergeHead(pages, { rows: ['a', 'c'], next: 'c' }, shape);
    expect(merged.flatMap((p) => p.rows)).toEqual(['a', 'c', 'b']);
  });
});

describe('polledHeadOptions', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('asks for the first page alone on each tick, however many pages are loaded', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const asked: string[] = [];
    let newest = 5;
    // Newest first, two to a page, numbered rows; `cursor` is the last row seen.
    const fetchPage = (cursor: string) => {
      asked.push(cursor);
      const all = Array.from({ length: newest }, (_, i) => String(newest - i));
      const from = cursor === '' ? 0 : all.indexOf(cursor) + 1;
      const rows = all.slice(from, from + 2);
      const next = from + 2 < all.length ? rows[rows.length - 1] : undefined;
      return Promise.resolve<Page>({ rows, next });
    };
    const options = {
      key: ['feed'],
      headKey: ['feed-head'],
      fetchPage,
      nextParam: (p: Page) => p.next,
      shape,
      interval: 15_000,
    };

    const { list, head } = polledHeadOptions(client, options, true);
    const listObserver = new InfiniteQueryObserver(client, list);
    const stopList = listObserver.subscribe(() => {});
    await vi.advanceTimersByTimeAsync(0);
    await listObserver.fetchNextPage();
    await listObserver.fetchNextPage();
    expect(asked).toEqual(['', '4', '2']);

    const headObserver = new QueryObserver(client, head);
    const stopHead = headObserver.subscribe(() => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(asked, 'the head was seeded by the first page and does not ask at once').toHaveLength(3);

    newest = 7;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(asked).toEqual(['', '4', '2', '']);
    const rows = client.getQueryData<InfiniteData<Page, string>>(['feed'])?.pages.flatMap((p) => p.rows);
    expect(rows).toEqual(['7', '6', '5', '4', '3', '2', '1']);

    await vi.advanceTimersByTimeAsync(15_000);
    expect(asked).toEqual(['', '4', '2', '', '']);

    stopHead();
    stopList();
    client.clear();
  });
});
