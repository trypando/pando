// Where to go after signing in, from the `next` the proxy put in the query.
//
// Its own module because it is a security check rather than a piece of the
// sign-in screen, and because a pure function of two strings can be tested
// without rendering anything.

/**
 * The same-origin path in `search`'s `next` parameter, or null.
 *
 * A value taken from the address bar and handed to location.assign is an open
 * redirect, which is how a phishing link borrows a real domain. So `next` is
 * accepted only when it lands back on `here`'s own origin.
 *
 * The check is a parse and an origin comparison rather than a look at the
 * first few characters, because the string this function is given and the
 * string location.assign acts on are read by the same parser, and only that
 * parser decides what counts as a host. `/\evil.com` passes every prefix test
 * — one leading slash, no second slash, no scheme — and the URL parser still
 * reads it as `//evil.com`, because at the point where an authority may begin
 * a backslash and a slash mean the same thing. Resolving against the current
 * document and comparing origins agrees with the browser by construction, and
 * it turns away `javascript:` and `data:` in the same breath: neither has an
 * origin that can equal this one.
 *
 * What comes back is rebuilt from the parsed URL, so the string the caller
 * navigates to is the string that was checked. A same-origin URL can still
 * have a path that starts with two slashes — "https://pando.example.com//evil.example/",
 * or "/.//evil.example/" once the dot segment is removed — and that path on its
 * own is a protocol-relative address to evil.example. So the leading slashes
 * are collapsed to one: the path stays on this origin whatever it was.
 *
 * @param search the query string, including the leading "?" — window.location.search
 * @param here   the address of the page doing the redirect — window.location.href
 */
export function returnTo(search: string, here: string): string | null {
  const next = new URLSearchParams(search).get('next');
  if (!next) return null;

  let base: URL;
  let resolved: URL;
  try {
    base = new URL(here);
    resolved = new URL(next, base);
  } catch {
    return null;
  }

  if (resolved.origin !== base.origin) return null;

  const path = '/' + resolved.pathname.replace(/^\/+/, '');
  return path + resolved.search + resolved.hash;
}
