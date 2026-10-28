// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import { mount, unanswered, type Mounted } from '../ui/testing';
import { Login } from './Login';
import { clearSignedOut, markSignedOut } from './sso';

const only = { password_sign_in: false, providers: [{ id: 'idp_1', name: 'Okta', kind: 'oidc' }] };

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  clearSignedOut();
  vi.unstubAllGlobals();
});

function open(search: string) {
  vi.stubGlobal('fetch', vi.fn(() => unanswered()));
  vi.stubGlobal('matchMedia', () => ({ matches: true, addEventListener() {}, removeEventListener() {} }));
  const assign = vi.fn();
  vi.stubGlobal('location', { ...window.location, search, href: `https://notes.example.com/.pando/login${search}`, assign });
  screen = mount(<Login />, [
    [['setup'], { needed: false }],
    [['sign-in-options'], only],
  ]);
  return assign;
}

// R-172: on an app's own hostname, with one provider as the only way in, the
// sign-in page goes to it without a click.
describe('the only way to sign in', () => {
  it('starts by itself', () => {
    expect(open('?next=%2Fnotes')).toHaveBeenCalledWith('/.pando/api/v1/auth/providers/idp_1/start?next=%2Fnotes');
  });

  it('waits after signing out, and a click starts it again', async () => {
    markSignedOut();
    const assign = open('');
    expect(assign).not.toHaveBeenCalled();
    await screen!.press('Sign in with Okta');
    expect(assign).toHaveBeenCalledWith('/.pando/api/v1/auth/providers/idp_1/start?next=%2F');
  });

  it('waits after a failed sign-in', () => {
    expect(open('?sso_error=expired')).not.toHaveBeenCalled();
  });
});
