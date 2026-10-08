// Lists that grow with the organization — accounts, groups, apps, waiting
// approvals — come a page at a time (design 04 §1, issue #72): `limit` and
// `cursor` ask, `next_cursor` continues, and `total` counts every match. One
// hook and one button for all of them, so every such list pages alike.
//
// Search is the server's (`q`), not a filter over the rows already loaded: at
// 100,000 accounts the one being looked for is rarely on the first page.

import { useEffect, useState } from 'react';
import { useInfiniteQuery } from '@tanstack/react-query';
import type { QueryKey } from '@tanstack/react-query';
import { Button } from '@design';

import { api } from '@api/client';

/** What every paged list answers with, beside its rows. */
export interface PageOf {
  next_cursor?: string;
  total?: number;
  /** The list holds more than `total`, which is the count's cap (O-53). */
  total_is_lower_bound?: boolean;
}

/**
 * A list's total as a person reads it: "10,000+" when the server stopped
 * counting at its cap (O-53), the exact number otherwise.
 */
export function totalLabel(page?: PageOf): string {
  const n = (page?.total ?? 0).toLocaleString('en-US');
  return page?.total_is_lower_bound ? `${n}+` : n;
}

/** A path with query parameters added, skipping empty ones. */
export function withParams(path: string, params: Record<string, string | string[] | number | undefined>): string {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '') continue;
    for (const v of Array.isArray(value) ? value : [value]) q.append(key, String(v));
  }
  const s = q.toString();
  if (!s) return path;
  return path + (path.includes('?') ? '&' : '?') + s;
}

/**
 * Every page of a list read so far, flattened, with the total and a way to
 * read the next. `search` is sent as `q`.
 */
export function usePaged<P extends PageOf, R>({
  key,
  path,
  rows,
  search = '',
  params = {},
  enabled = true,
  retry,
}: {
  key: QueryKey;
  path: string;
  /** The rows of one page, e.g. `(p) => p.users`. */
  rows: (page: P) => R[] | null | undefined;
  search?: string;
  params?: Record<string, string | string[] | number | undefined>;
  enabled?: boolean;
  retry?: boolean;
}) {
  const query = useInfiniteQuery({
    queryKey: [...key, search, params],
    initialPageParam: '',
    queryFn: ({ pageParam }) => api.get<P>(withParams(path, { ...params, q: search, cursor: pageParam })),
    getNextPageParam: (last) => last.next_cursor || undefined,
    enabled,
    retry,
  });
  const pages = query.data?.pages ?? [];
  return {
    query,
    rows: pages.flatMap((p) => rows(p) ?? []),
    total: pages[0]?.total,
    totalLabel: totalLabel(pages[0]),
  };
}

/** "Show more" under a paged list, while there is more. */
export function ShowMore({
  query,
  label = 'Show more',
}: {
  query: { hasNextPage: boolean; isFetchingNextPage: boolean; fetchNextPage: () => unknown };
  label?: string;
}) {
  if (!query.hasNextPage) return null;
  return (
    <div>
      <Button variant="secondary" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
        {query.isFetchingNextPage ? 'Loading' : label}
      </Button>
    </div>
  );
}

/** The value, once it has stopped changing for `ms`: what a search box sends. */
export function useSettled<T>(value: T, ms = 300): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return settled;
}
