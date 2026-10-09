// The `next` parameter exists because of R-172: the sign-in page is reachable
// on an app's own hostname, so it has to be able to send the person back to the
// app they were opening. These assert it can only ever send them somewhere on
// Pando's own origin.

import { describe, expect, it } from 'vitest';

import { returnTo } from './return-to';

const here = 'https://pando.example.com/.pando/login';

describe('R-172 returnTo', () => {
  it('returns a same-origin path', () => {
    expect(returnTo('?next=%2Fapps%2Fbilling', here)).toBe('/apps/billing');
  });

  it('keeps the query and the fragment', () => {
    expect(returnTo('?next=%2Fapps%3Ftab%3Dlogs%23tail', here)).toBe('/apps?tab=logs#tail');
  });

  it('is null when there is no next', () => {
    expect(returnTo('', here)).toBeNull();
    expect(returnTo('?other=1', here)).toBeNull();
    expect(returnTo('?next=', here)).toBeNull();
  });

  // Each of these is an open redirect if it gets through: the person signs in
  // on Pando's real domain, having typed a real password, and lands somewhere
  // else. The backslash cases are the ones a prefix check misses — "/\evil.com"
  // has one leading slash, no second slash and no scheme, and the URL parser
  // still reads it as "//evil.com".
  it.each([
    'https://evil.example/',
    '//evil.example/',
    '/\\evil.example/',
    '/\\/evil.example/',
    '\\\\evil.example/',
    'https:/\\evil.example/',
    'javascript:alert(1)',
    'data:text/html,<script>alert(1)</script>',
    'http://pando.example.com/apps', // same host, wrong scheme
    'https://pando.example.com.evil.example/',
    'https://evil.example\\@pando.example.com/',
  ])('refuses %s', (next) => {
    expect(returnTo('?next=' + encodeURIComponent(next), here)).toBeNull();
  });

  // Each of these passes the origin check and resolves to a path beginning
  // "//", which on its own is a protocol-relative address to evil.example.
  it.each([
    'https://pando.example.com//evil.example/',
    '/.//evil.example/',
    'https://pando.example.com/\\evil.example/',
    '/./\\evil.example/',
  ])('keeps %s on this origin', (next) => {
    const out = returnTo('?next=' + encodeURIComponent(next), here);
    expect(out).toBe('/evil.example/');
    expect(new URL(out!, here).origin).toBe(new URL(here).origin);
  });

  // A path is resolved against the page doing the redirect, so a relative one
  // cannot climb out of the origin however many segments it walks up.
  it('resolves a relative path inside the origin', () => {
    expect(returnTo('?next=' + encodeURIComponent('../../../../apps'), here)).toBe('/apps');
  });
});
