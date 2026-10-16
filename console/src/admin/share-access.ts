// What the Sharing tab sends, kept apart from the screen so it can be tested
// without rendering one.

import type { GrantRow } from '@api/types.gen';

/** A grant row as GET /apps/{id}/grants returns it. `passcode` is set on the
 *  anonymous grant when it asks for one; the generated type predates it. */
export type Grant = GrantRow & { passcode?: boolean };

/** Somebody an app can be shared with. */
export interface Recipient {
  kind: 'user' | 'group';
  id: string;
}

/** One call against /apps/{id}/grants. */
export type GrantRequest =
  | { method: 'post'; body: { plane: 'control' | 'data'; principal_kind: string; principal_id: string; role_id?: string } }
  | { method: 'patch'; grant: string; body: { role_id: string } };

/**
 * The calls that give `who` what the form asked for, given the grants already
 * on the app.
 *
 * Opening the app is the data plane; a role is the control plane (R-070,
 * R-071). Every choice on the form includes opening the app — each role's
 * description starts "They can open the app" — so a role is always
 * accompanied by a data grant. What is
 * already there is not asked for again: a second data grant would be refused,
 * and a person who already manages the app with one role is moved to the new
 * one rather than given two.
 *
 * @param role a role ID, or null for "open the app" alone
 */
export function shareRequests(existing: Grant[], who: Recipient, role: string | null): GrantRequest[] {
  const theirs = existing.filter((g) => g.principal_kind === who.kind && g.principal_id === who.id);
  const out: GrantRequest[] = [];

  if (role) {
    const control = theirs.find((g) => g.plane === 'control');
    if (!control) {
      out.push({
        method: 'post',
        body: { plane: 'control', principal_kind: who.kind, principal_id: who.id, role_id: role },
      });
    } else if (control.role_id !== role) {
      out.push({ method: 'patch', grant: control.id, body: { role_id: role } });
    }
  }

  if (!theirs.some((g) => g.plane === 'data')) {
    out.push({ method: 'post', body: { plane: 'data', principal_kind: who.kind, principal_id: who.id } });
  }

  return out;
}

/** Whether, and how, an app is open to everyone. */
export type Everyone = 'private' | 'public' | 'passcode';

export function everyone(grants: Grant[]): Everyone {
  const anonymous = grants.find((g) => g.principal_kind === 'anonymous');
  if (!anonymous) return 'private';
  return anonymous.passcode ? 'passcode' : 'public';
}

/** The server's floor for a passcode, checked here only to keep the button
 *  disabled until it could succeed. The server enforces it. */
export const PASSCODE_MIN = 6;

/** The app roles to offer when GET /roles is refused — it is an install-level
 *  read some people who manage an app's sharing do not have. The built-ins
 *  always exist (R-081). */
export const BUILT_IN_APP_ROLES = [
  { id: 'role_viewer', name: 'viewer' },
  { id: 'role_operator', name: 'operator' },
  { id: 'role_owner', name: 'owner' },
];

/** The least access: using the app — its tile on their launcher — and no role
 *  on it, so nothing in its settings (R-070, R-071). */
export const USE_ONLY = 'Use only (no role)';

type RoleLike = { id: string; name: string; builtin?: boolean; verbs?: string[] };

// The built-ins in order of what they allow, so the list reads as a ladder.
const LADDER = ['role_viewer', 'role_operator', 'role_owner'];

/** The dropdown's label for a role: its name, as the Groups and roles screen
 *  shows it. The same name for a built-in as for one somebody made, so a custom
 *  role reads as a role rather than as a sentence about one. */
export function roleChoice(role: RoleLike): string {
  return role.name.charAt(0).toUpperCase() + role.name.slice(1);
}

/** The roles in the order the dropdown shows them: the built-ins by what they
 *  allow, then custom roles by name. */
export function orderRoles<T extends RoleLike>(roles: T[]): T[] {
  const rank = (r: T) => {
    const i = LADDER.indexOf(r.id);
    return i >= 0 ? i : LADDER.length;
  };
  return [...roles].sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name));
}

/** What the chosen access means, under the dropdown. For a custom role Pando
 *  has no sentence, so it says what the role holds. */
export function describeChoice(role: RoleLike | undefined): string {
  if (!role) return 'Open the app. No access to its settings.';
  switch (role.id) {
    case 'role_viewer':
      return 'Open the app and view its settings and logs.';
    case 'role_operator':
      return 'Open, deploy and restart the app, and change its settings.';
    case 'role_owner':
      return 'Everything, including sharing and deleting the app.';
  }
  const verbs = (role.verbs ?? []).map((v) => v.replace(/^app\./, ''));
  return verbs.length > 0
    ? `Open the app, plus: ${verbs.join(', ')}.`
    : `Open the app, with the ${roleChoice(role)} role.`;
}
