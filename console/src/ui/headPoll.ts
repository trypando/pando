// A paged list that keeps its first page current, newest first (issue #72).
//
// An infinite query polled with `refetchInterval` asks for every page it has
// loaded, one request after another, on every tick: somebody who pressed
// "Show more" five times makes six requests a minute for as long as the tab is
// open. Here the list itself is never polled. A second query asks for the
// first page alone, and what it finds is merged into the pages already read:
// what is new goes in at the top, and nothing is asked for twice.

import { infiniteQueryOptions, queryOptions, useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import type { InfiniteData, QueryClient, QueryKey } from '@tanstack/react-query';

export interface HeadShape<P, R> {
  /** The rows of one page. */
  rowsOf: (page: P) => R[];
  /** The same page, with these rows instead. */
  withRows: (page: P, rows: R[]) => P;
  /** A row's identity. */
  idOf: (row: R) => string;
  /** Whether a page says there is more after it. */
  hasMore: (page: P) => boolean;
}

/**
 * The pages read so far, with a fresh first page merged in.
 *
 * With one page loaded, the fresh one replaces it: it is the whole truth about
 * the top of the list, rows that went away included, and its cursor continues
 * from where it ends.
 *
 * With more loaded, the fresh rows lead, and the old first page's rows that
 * are not among them follow when the fresh page is full — they were pushed
 * down past its end by newer rows, and the second page, read from the old
 * cursor, does not have them. When the fresh page is short, the list ends
 * within it, so a row it lacks has gone. Every later page loses the rows the
 * fresh page now holds, so none is shown twice.
 */
export function mergeHead<P, R>(pages: P[], head: P, shape: HeadShape<P, R>): P[] {
  const [top, ...rest] = pages;
  if (top === undefined || rest.length === 0) return [head];
  const fresh = shape.rowsOf(head);
  const ids = new Set(fresh.map(shape.idOf));
  const pushedDown = shape.hasMore(head) ? shape.rowsOf(top).filter((r) => !ids.has(shape.idOf(r))) : [];
  for (const r of pushedDown) ids.add(shape.idOf(r));
  const first = shape.withRows(top, [...fresh, ...pushedDown]);
  return [first, ...rest.map((p) => shape.withRows(p, shape.rowsOf(p).filter((r) => !ids.has(shape.idOf(r)))))];
}

/**
 * A paged, newest-first list whose first page is asked for again every
 * `interval` while it is on screen and merged into what is loaded.
 *
 * `headKey` must not sit under `key`: invalidating the list reads it again
 * from the top, which seeds the head with that first page, and a head under
 * the same prefix would be read a second time beside it.
 */
export interface PolledHead<P, R> {
  key: QueryKey;
  headKey: QueryKey;
  /** One page; the first is asked for with an empty cursor. */
  fetchPage: (cursor: string) => Promise<P>;
  nextParam: (page: P) => string | undefined;
  shape: HeadShape<P, R>;
  interval: number;
  enabled?: boolean;
  retry?: boolean;
}

/** The two queries' options: the list, never polled, and its polled head. */
export function polledHeadOptions<P, R>(
  queries: QueryClient,
  { key, headKey, fetchPage, nextParam, shape, interval, enabled = true, retry }: PolledHead<P, R>,
  listLoaded: boolean,
) {
  const list = infiniteQueryOptions({
    queryKey: key,
    initialPageParam: '',
    queryFn: async ({ pageParam }: { pageParam: string }) => {
      const page = await fetchPage(pageParam);
      // A first page read here is as fresh as the head would be, so the head
      // waits its interval from now rather than asking for it again at once.
      if (pageParam === '') queries.setQueryData(headKey, page);
      return page;
    },
    getNextPageParam: nextParam,
    enabled,
    retry,
  });
  const head = queryOptions({
    queryKey: headKey,
    queryFn: async () => {
      const page = await fetchPage('');
      queries.setQueryData<InfiniteData<P, string>>(key, (old) =>
        old ? { ...old, pages: mergeHead(old.pages, page, shape) } : old,
      );
      return page;
    },
    enabled: enabled && listLoaded,
    staleTime: interval,
    refetchInterval: interval,
    // Cheap — one page — and what somebody coming back to the tab looks for.
    refetchOnWindowFocus: true,
    retry,
  });
  return { list, head };
}

export function usePolledHead<P, R>(options: PolledHead<P, R>) {
  const queries = useQueryClient();
  const { list, head } = polledHeadOptions(queries, options, true);
  const result = useInfiniteQuery(list);
  // The head waits for the list: its first page is what seeds it.
  useQuery({ ...head, enabled: head.enabled && result.isSuccess });
  return result;
}
