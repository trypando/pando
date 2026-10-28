// Signing in through an identity provider, from the sign-in page (issue #51).
//
// Pure functions of the address bar, so they can be tested without rendering
// the page. The redirects themselves are the server's: this module only
// builds the link a button follows.

import { returnTo } from './return-to';

/** How people can sign in here, as GET /auth/options answers. */
export interface SignInOptions {
  password_sign_in: boolean;
  providers: { id: string; name: string; kind: string }[] | null;
}

/**
 * Where a provider's button goes: its start address, under the reserved
 * prefix so it works on an app's own hostname too (R-172), carrying where the
 * person was going. `next` is checked the same way the password form checks
 * it, and the server checks it again.
 *
 * Without a `next`, where they were going is the page they are on: the form
 * is drawn in place of a page that needs a session — the CLI's approval page
 * (R-406) is one — and a password sign-in stays on it, so a provider's should
 * come back to it.
 */
export function providerStart(providerID: string, search: string, here: string): string {
  const next = returnTo(search, here) ?? stayHere(here);
  return `/.pando/api/v1/auth/providers/${encodeURIComponent(providerID)}/start?next=${encodeURIComponent(next)}`;
}

/** The page at `here` as a path, unless it is the sign-in page itself. */
function stayHere(here: string): string {
  const back = returnTo(`?next=${encodeURIComponent(here)}`, here);
  return back === null || /^(\/\.pando)?\/login([?#]|$)/.test(back) ? '/' : back;
}

/**
 * The failed sign-in to explain, from `?sso_error=`. The server looks it up
 * and answers with its own sentence; nothing in the address bar is shown as
 * text, so a link cannot put words on this page.
 */
export function failedSignIn(search: string): string | null {
  const id = new URLSearchParams(search).get('sso_error');
  return id && /^[A-Za-z0-9_-]{1,64}$/.test(id) ? id : null;
}

/**
 * Set once someone signs out here, until they sign in with a button. In
 * localStorage, so every tab on this hostname sees it: signing out ends the
 * hostname's session, and a second tab's sign-in page would otherwise start the
 * provider and undo it. Per origin, like the session: another hostname's
 * session is its own, and has nothing to undo. It outlasts the browser, so the
 * next visit after signing out waits for a click too.
 */
const signedOutKey = 'pando_signed_out';

/**
 * When this tab last started the provider without a click. A session cookie
 * the browser never sends back returns it here with no error to show, and the
 * provider would sign it in again, round after round.
 */
const startedKey = 'pando_sso_started';

/** How long after starting the provider by itself the page waits for a click. */
const startedHold = 30_000;

/** Remembers that someone signed out here, so the sign-in page waits for a click. */
export function markSignedOut(): void {
  try {
    window.localStorage.setItem(signedOutKey, '1');
  } catch {
    // A browser that refuses storage cannot remember it, and held then waits
    // for a click anyway; sign-out must still finish.
  }
}

/** Forgets it: someone chose a way to sign in. */
export function clearSignedOut(): void {
  try {
    window.localStorage.removeItem(signedOutKey);
  } catch {
    // Nothing was kept to forget.
  }
}

/** Remembers that this tab is starting the provider without a click. */
export function markStarted(now: number): void {
  try {
    window.sessionStorage.setItem(startedKey, String(now));
  } catch {
    // Unreachable in practice: held refuses to start when storage is refused.
  }
}

/**
 * The provider to start without a click: the only way to sign in here (R-172).
 * Opening an app on its own hostname then costs a redirect, not a click, when
 * the provider already has its visitor signed in. Not after a failed sign-in,
 * which would send the browser straight back to what failed, and not while the
 * page is held for a click.
 */
export function soleProvider(options: SignInOptions | undefined, search: string, waiting: boolean): string | null {
  const [only, ...others] = options?.providers ?? [];
  if (options?.password_sign_in !== false || !only || others.length > 0) return null;
  if (waiting || new URLSearchParams(search).has('sso_error')) return null;
  return only.id;
}

/**
 * Whether the page waits for a click: someone signed out here and has not
 * chosen a way back in, or this tab started the provider by itself moments
 * ago and came back without a session. A browser that refuses storage cannot
 * say either, so it waits: a click, never a sign-in the person may have just
 * undone.
 */
export function held(now: number): boolean {
  try {
    if (window.localStorage.getItem(signedOutKey) === '1') return true;
    const started = Number(window.sessionStorage.getItem(startedKey));
    return started > 0 && now - started < startedHold;
  } catch {
    return true;
  }
}
