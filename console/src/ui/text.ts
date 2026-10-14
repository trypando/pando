/**
 * `s` without its trailing slashes. A loop rather than `/\/+$/`, which
 * backtracks over every run of slashes that does not reach the end.
 */
export function trimTrailingSlashes(s: string): string {
  let end = s.length;
  while (end > 0 && s[end - 1] === '/') end--;
  return s.slice(0, end);
}
