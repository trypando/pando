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

/** Set while this tab has signed out, and until it signs in with a button. */
const signedOutKey = 'pando_signed_out';

/** Remembers that this tab signed out, so the sign-in page waits for a click. */
export function markSignedOut(): void {
  window.sessionStorage?.setItem(signedOutKey, '1');
}

/** Forgets it: someone chose a way to sign in. */
export function clearSignedOut(): void {
  window.sessionStorage?.removeItem(signedOutKey);
}

/**
 * The provider to start without a click: the only way to sign in here (R-172).
 * Opening an app on its own hostname then costs a redirect, not a click, when
 * the provider already has its visitor signed in. Not after a failed sign-in,
 * which would send the browser straight back to what failed, and not after
 * signing out in this tab, which the provider would undo at once.
 */
export function soleProvider(options: SignInOptions | undefined, search: string, signedOut: boolean): string | null {
  const [only, ...others] = options?.providers ?? [];
  if (options?.password_sign_in !== false || !only || others.length > 0) return null;
  if (signedOut || new URLSearchParams(search).has('sso_error')) return null;
  return only.id;
}

/** Whether this tab signed out and has not chosen a way back in yet. */
export function hasSignedOut(): boolean {
  return window.sessionStorage?.getItem(signedOutKey) === '1';
}
