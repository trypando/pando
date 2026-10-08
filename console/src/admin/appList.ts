// The admin console's app list, and the rows of it that are changing.
//
// `GET /apps` is paged (issue #72), so its cache entries sit under
// `['apps', <word>, …]` — beside each app's own `['apps', <app id>, …]`,
// which an app ID's `app_` prefix keeps apart from the words used here.

import { useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { withParams } from '../ui/paged';

/** The paged list's key; search and parameters are added after it. */
export const APP_LIST_KEY = ['apps', 'list'];

/** How often a row that is deploying or being scanned is asked about. */
const WATCH_MS = 5_000;
const WATCH_MAX = 100;

/**
 * Asks the server again about every list of apps — the paged list, the
 * count, the watched rows — and about no app's own record. For a change that
 * adds or removes an app, where refetching the removed app's record would put
 * a 404 on the screen on the way out of it.
 */
export function invalidateAppLists(queries: QueryClient) {
  return queries.invalidateQueries({
    predicate: (q) =>
      q.queryKey[0] === 'apps' && typeof q.queryKey[1] === 'string' && !q.queryKey[1].startsWith('app_'),
  });
}

/**
 * Asks the server again about one app — its record and everything read under
 * it — and about the lists of apps, where its row is. For a change to that app
 * that the list shows: its state, its name, its image. Not `['apps']`, which
 * is every other app's queries too (issue #72).
 */
export function invalidateApp(queries: QueryClient, appID: string) {
  return Promise.all([queries.invalidateQueries({ queryKey: ['apps', appID] }), invalidateAppLists(queries)]);
}

function changing(a: App): boolean {
  return Boolean(a.security_scanning) || a.state === 'deploying';
}

/**
 * The rows, with the ones that are changing kept current.
 *
 * Only those rows are asked for again, by ID, while they change — a deploy, or
 * a scan, which a deploy of a new commit starts — so the Status and Security
 * columns move without a reload. The whole list used to be asked for every
 * five seconds while any row was changing, which at twenty thousand apps is
 * always. When a watched row stops changing, the list is asked for once more.
 */
export function useWatchedRows(rows: App[], enabled: boolean): App[] {
  const queries = useQueryClient();
  // At most a hundred, which keeps the request line short; past that, the
  // rest move when the list is next read.
  const ids = rows.filter(changing).map((a) => a.id).slice(0, WATCH_MAX);
  const watched = useQuery({
    queryKey: ['apps', 'watch', ids],
    queryFn: () => api.get<{ apps: App[] | null }>(withParams('/apps', { id: ids, limit: ids.length })),
    enabled: enabled && ids.length > 0,
    refetchInterval: WATCH_MS,
  });
  const fresh = new Map((watched.data?.apps ?? []).map((a) => [a.id, a]));
  const settled = ids.some((id) => {
    const now = fresh.get(id);
    return now !== undefined && !changing(now);
  });
  useEffect(() => {
    if (settled) void queries.invalidateQueries({ queryKey: APP_LIST_KEY });
  }, [settled, queries]);
  return rows.map((a) => fresh.get(a.id) ?? a);
}
