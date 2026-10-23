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
  | { method: 'patch'; grant: string; body: { role_id: string } }
  | { method: 'delete'; grant: string };

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

/** Who can open an app, as GET /apps reports it (discussion #262). */
const ACCESS: Record<string, { label: string; meaning: string }> = {
  private: { label: 'Private', meaning: 'Only its owner can open it.' },
  shared: { label: 'Private – shared', meaning: 'Its owner and the people it is shared with can open it.' },
  public: { label: 'Public', meaning: 'Anyone on the internet can open it, without signing in.' },
  passcode: {
    label: 'Public with passcode',
    meaning: 'Anyone on the internet who has the passcode can open it, without signing in.',
  },
};

/**
 * The apps list's Access label. One or two words, which a column needs; the
 * consequence goes with it on hover (`accessMeaning`), so "Public" never
 * stands alone (R-077, as amended for #262).
 */
export function accessLabel(access: string | undefined): string {
  return ACCESS[access ?? '']?.label ?? '';
}

/** What an Access label means, said as its consequence (R-077). */
export function accessMeaning(access: string | undefined): string {
  return ACCESS[access ?? '']?.meaning ?? '';
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

/** The least access, and the share bar's default: using the app — its tile
 *  on their launcher — and no role on it, so nothing in its settings (R-070,
 *  R-071). */
export const CAN_OPEN = 'Can open';

/** A principal with no role on the app, in the Manages it column. */
export const NO_ROLE = 'No role';

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

/** Whoever holds grants on the app: a person, a group or a token. */
export interface Holder {
  kind: string;
  id: string;
}

function theirs(existing: Grant[], who: Holder): Grant[] {
  return existing.filter((g) => g.principal_kind === who.kind && g.principal_id === who.id);
}

/**
 * The calls that let `who` open the app or stop them — the data plane alone.
 * Their role, if any, is left as it is: opening an app and managing it are
 * separate grants (R-070, R-071).
 */
export function openRequests(existing: Grant[], who: Holder, open: boolean): GrantRequest[] {
  const data = theirs(existing, who).filter((g) => g.plane === 'data');
  if (open) {
    return data.length > 0
      ? []
      : [{ method: 'post', body: { plane: 'data', principal_kind: who.kind, principal_id: who.id } }];
  }
  return data.map((g) => ({ method: 'delete', grant: g.id }));
}

/**
 * The calls that give `who` this role on the app, or none ('') — the control
 * plane alone. A role they already hold is moved rather than added beside, so
 * nobody is left with two or with nothing in between.
 */
export function roleRequests(existing: Grant[], who: Holder, role: string): GrantRequest[] {
  const [control, ...extra] = theirs(existing, who).filter((g) => g.plane === 'control');
  if (!role) return [control, ...extra].filter((g) => g !== undefined).map((g) => ({ method: 'delete', grant: g.id }));
  if (!control) {
    return [{ method: 'post', body: { plane: 'control', principal_kind: who.kind, principal_id: who.id, role_id: role } }];
  }
  return control.role_id === role ? [] : [{ method: 'patch', grant: control.id, body: { role_id: role } }];
}

/** One person, group or token in the People and groups table, with both
 *  planes side by side. */
export interface Person {
  key: string;
  kind: 'user' | 'group' | 'token';
  id: string;
  name: string;
  /** The signed-in person themselves. */
  you: boolean;
  /** The data plane: they can open the app. */
  opens: boolean;
  /** The control plane: the role they manage it with, if any. */
  role?: { id: string; name: string };
  grantIDs: string[];
}

/**
 * One row per principal, not one per grant, and nobody anonymous: the grant
 * to everyone is the Public access card's, not a person (R-075).
 */
export function people(grants: Grant[], me?: { id?: string; user_id?: string }): Person[] {
  const out = new Map<string, Person>();
  for (const g of grants) {
    if (g.principal_kind === 'anonymous') continue;
    const kind = (g.principal_kind === 'group' || g.principal_kind === 'token' ? g.principal_kind : 'user') as Person['kind'];
    const id = g.principal_id ?? '';
    const key = `${g.principal_kind}:${id}`;
    const entry = out.get(key) ?? {
      key,
      kind,
      id,
      // The name the server resolved, falling back to the identifier only when
      // there is nothing else — a principal deleted since the grant was made.
      name: g.principal_name || id,
      you: kind === 'user' && Boolean(me) && (id === me?.user_id || id === me?.id),
      opens: false,
      grantIDs: [],
    };
    entry.grantIDs.push(g.id);
    if (g.plane === 'data') entry.opens = true;
    else if (!entry.role) entry.role = { id: g.role_id ?? '', name: roleChoice({ id: g.role_id ?? '', name: g.role_name || g.role_id || '' }) };
    out.set(key, entry);
  }
  // You first; everyone else in the order the server lists them.
  return [...out.values()].sort((a, b) => Number(b.you) - Number(a.you));
}

/** The letter in a person's circle: Pando has no pictures of anybody, so the
 *  first letter of the name they go by. */
export function initialOf(name: string): string {
  const first = [...name.trim()][0];
  return first ? first.toLocaleUpperCase() : '?';
}
