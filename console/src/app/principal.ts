// Who is signed in, and whether they see the management console.
//
// R-265: "users holding any administrative verb see an Admin entry point from
// the launcher, exposing the console scoped to whatever privileges they hold."
//
// "Any administrative verb" is two things, because there are two scopes. A
// person may administer the installation — users, policy, adapters, the audit
// log — or they may administer one app they hold a control-plane grant on.
// Either is a reason to see the console, and the console it opens differs.
//
// Both answers come from the server. `GET /me` returns the install-level verbs
// the caller holds; `GET /apps` is control-plane scoped, a different list from
// `GET /me/apps` (R-070, R-071), so a non-empty result means "there is an app
// here you can administer". This file composes the two into one boolean and
// decides nothing on its own.
//
// Until install verbs existed (O-17) only the second half was implementable,
// which is why the first half is new here and the second is not.
//
// The entry is an affordance, not the enforcement: every install-level endpoint
// checks its verb itself, so a hand-typed /admin URL reaches a page whose
// requests are refused.

import { useQuery } from '@tanstack/react-query';
import { api } from '@api/client';

/** Install-scoped verbs (design 06 §5). The `app.*` verbs are per-app and are
 *  never in this list. */
export const InstallVerb = {
  View: 'install.view',
  UsersManage: 'install.users.manage',
  PolicyManage: 'install.policy.manage',
  AdaptersManage: 'install.adapters.manage',
  AuditRead: 'install.audit.read',
  BackupManage: 'install.backup.manage',
  /** app.view and app.grants.manage on every app (R-081). Each app verb has
   *  an install.apps.* counterpart; these are the two the console asks about. */
  AppsView: 'install.apps.view',
  AppsGrantsManage: 'install.apps.grants.manage',
  TokensManage: 'install.tokens.manage',
  /** Approve any deploy that needs approval, on any app (R-155). */
  DeploysApprove: 'install.deploys.approve',
  Upgrade: 'install.upgrade',
  /** Install-wide event subscriptions, and everybody's (R-368). */
  EventsManage: 'install.events.manage',
  /** Send the audit log to an audit sink, off the installation (R-385). */
  AuditExport: 'install.audit.export',
  AppCreate: 'app.create',
} as const;

export type InstallVerb = (typeof InstallVerb)[keyof typeof InstallVerb];

/**
 * How long an install-wide count — apps, accounts — is kept before a screen
 * that shows it asks again. A change the console makes asks at once.
 */
export const COUNT_STALE_MS = 5 * 60_000;

export interface Principal {
  principal_kind: string;
  id: string;
  user_id?: string;
  username?: string;
  email?: string;
  display_name?: string;
  groups?: string[] | null;
  must_change_password?: boolean;

  /** Install-level verbs the principal holds. Empty for most accounts. */
  verbs?: string[] | null;
}

export function usePrincipal() {
  return useQuery({
    queryKey: ['me'],
    queryFn: () => api.get<Principal>('/me'),
    retry: false,
    // A session that ended, or verbs that changed, while the tab was away is
    // noticed on return. One row; most queries do not do this (main.tsx).
    refetchOnWindowFocus: true,
  });
}

/**
 * The install-level verbs the signed-in principal holds.
 *
 * One query, shared with `usePrincipal` through the query key, so asking twice
 * on one screen costs one request.
 */
export function useInstallVerbs(): string[] {
  const me = usePrincipal();
  return me.data?.verbs ?? [];
}

/**
 * Whether the principal holds a specific install verb.
 *
 * For deciding what to put *inside* the console — the users screen needs
 * `install.users.manage`, the policy screen `install.policy.manage`. There is
 * no implication graph (R-082): holding one verb says nothing about another, so
 * each screen asks for the one it needs.
 */
export function useInstallVerb(verb: InstallVerb): boolean {
  return useInstallVerbs().includes(verb);
}

/**
 * Whether the principal holds a control-plane grant on at least one app.
 *
 * The server's scoping: the list contains the apps they may administer and no
 * others, which is the "scoped to whatever privileges they hold" half of R-265
 * for app administration.
 */
export function useManageableApps(): number {
  return useManageableAppsTotal().total ?? 0;
}

/**
 * The count of manageable apps as GET /apps answers it: `total`, and whether
 * that is a lower bound (O-53).
 */
export function useManageableAppsTotal(): { total?: number; total_is_lower_bound?: boolean } {
  // One row asked for and `total` read: the count, not the list, which can
  // be twenty thousand apps long (issue #72).
  const apps = useQuery({
    queryKey: ['apps', 'count'],
    queryFn: () => api.get<{ total?: number; total_is_lower_bound?: boolean }>('/apps?limit=1'),
    // A 403 means no control-plane access, which is an answer rather than a
    // failure — so it is not retried and not surfaced as an error.
    retry: false,
    // Counted to at most 10,000 (O-53), on every screen that shows the Admin
    // entry: kept for five minutes rather than ten seconds. Adding or removing
    // an app asks again at once (invalidateAppLists).
    staleTime: COUNT_STALE_MS,
  });
  return apps.data ?? {};
}

/**
 * Whether to show the Admin entry (R-265).
 *
 * Either scope. An install administrator with no app grants sees it, and so
 * does someone who owns exactly one app and administers nothing else — they
 * need the app screens, and those screens are in here.
 */
export function useAdministrative(): boolean {
  const install = useInstallVerbs().length > 0;
  const apps = useManageableApps();
  return install || apps > 0;
}
