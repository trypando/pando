// Accounts, looked up by what is on the screen rather than read whole
// (issue #72).
//
// The audit log's names and the people pickers used to read the newest 500
// accounts and look everything up in that: at a hundred thousand accounts,
// most names were IDs and most people could not be picked. Now the IDs a
// screen shows are asked about by ID (`GET /users?id=…`), and a picker asks
// the server for what was typed (`GET /users?q=…`).
//
// Both are refused without install.view; install.audit.read does not imply it.
// The answer is then no names, and the audit screen shows IDs, as before.

import { useQuery } from '@tanstack/react-query';

import { api } from '@api/client';
import { useSettled, withParams } from './paged';

export interface Person {
  id: string;
  external_id?: string;
  display_name?: string;
  email?: string;
}

/** How many IDs one request asks about, which keeps its address short. */
const NAMED_PER_REQUEST = 100;
/** How many matches a picker offers. */
const FOUND = 8;

/**
 * The account IDs worth asking about, each once and in a stable order so the
 * same screen is the same query. Tokens, groups and Pando's own actors are not
 * accounts and are left out.
 */
export function accountIDs(ids: Array<string | undefined | null>): string[] {
  return [...new Set(ids.filter((id): id is string => typeof id === 'string' && id.startsWith('usr_')))].sort();
}

/** `ids` in requests of at most `size`. */
export function chunks<T>(ids: T[], size: number): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < ids.length; i += size) out.push(ids.slice(i, i + size));
  return out;
}

/** The accounts with these IDs, for naming them on screen. */
export function useNamedPeople(ids: Array<string | undefined | null>): Person[] {
  const wanted = accountIDs(ids);
  const named = useQuery({
    queryKey: ['users', 'named', wanted],
    queryFn: async () => {
      const pages = await Promise.all(
        chunks(wanted, NAMED_PER_REQUEST).map((part) =>
          api.get<{ users: Person[] | null }>(withParams('/users', { id: part, limit: part.length })),
        ),
      );
      return pages.flatMap((p) => p.users ?? []);
    },
    enabled: wanted.length > 0,
    retry: false,
    // The names already shown stay while the next screenful's are asked for.
    placeholderData: (previous) => previous,
    // An account's username rarely changes; a change made here invalidates
    // ['users'].
    staleTime: 5 * 60_000,
  });
  return named.data ?? [];
}

/** The accounts matching what was typed into a picker, once typing settles. */
export function usePeopleSearch(text: string): Person[] {
  const q = useSettled(text.trim());
  const found = useQuery({
    queryKey: ['users', 'search', q],
    queryFn: () => api.get<{ users: Person[] | null }>(withParams('/users', { q, limit: FOUND })),
    enabled: q !== '',
    retry: false,
    placeholderData: (previous) => previous,
  });
  return q === '' ? [] : (found.data?.users ?? []);
}

/** Everyone in either list, once, the first list's entry winning. */
export function unionPeople<P extends { id: string }>(a: P[], b: P[]): P[] {
  const seen = new Set(a.map((p) => p.id));
  return [...a, ...b.filter((p) => !seen.has(p.id))];
}
