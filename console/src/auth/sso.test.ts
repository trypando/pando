import { afterEach, describe, expect, it, vi } from 'vitest';

import { clearSignedOut, failedSignIn, held, markSignedOut, markStarted, providerStart, soleProvider } from './sso';

describe('providerStart', () => {
  it('carries where the person was going, under the reserved prefix', () => {
    expect(providerStart('idp_1', '?next=%2Fapps%2Fnotes', 'https://pando.example.com/login')).toBe(
      '/.pando/api/v1/auth/providers/idp_1/start?next=%2Fapps%2Fnotes',
    );
  });

  it('comes back to the page it was drawn on', () => {
    expect(providerStart('idp_1', '?port=4000&state=s', 'https://pando.example.com/admin/cli?port=4000&state=s')).toBe(
      '/.pando/api/v1/auth/providers/idp_1/start?next=%2Fadmin%2Fcli%3Fport%3D4000%26state%3Ds',
    );
    expect(providerStart('idp_1', '', 'https://pando.example.com/login')).toBe(
      '/.pando/api/v1/auth/providers/idp_1/start?next=%2F',
    );
  });

  it('refuses to carry another origin', () => {
    expect(providerStart('idp_1', '?next=https%3A%2F%2Fevil.example', 'https://pando.example.com/login')).toBe(
      '/.pando/api/v1/auth/providers/idp_1/start?next=%2F',
    );
  });
});

describe('failedSignIn', () => {
  it('reads a flow ID or a fixed key', () => {
    expect(failedSignIn('?sso_error=expired')).toBe('expired');
    expect(failedSignIn('?sso_error=Ab_9-x&next=%2F')).toBe('Ab_9-x');
  });

  it('ignores anything that is not an ID', () => {
    expect(failedSignIn('?sso_error=Call%20555-0100')).toBeNull();
    expect(failedSignIn('')).toBeNull();
  });
});

// R-172: opening an app on its own hostname, with one provider as the only way
// in, goes to the provider without a click.
describe('soleProvider', () => {
  const okta = { id: 'idp_1', name: 'Okta', kind: 'oidc' };
  const only = { password_sign_in: false, providers: [okta] };

  it('starts the only way in', () => {
    expect(soleProvider(only, '?next=%2F', false)).toBe('idp_1');
  });

  it('waits for a click when there is a choice', () => {
    expect(soleProvider({ password_sign_in: true, providers: [okta] }, '', false)).toBeNull();
    expect(soleProvider({ password_sign_in: false, providers: [okta, { ...okta, id: 'idp_2' }] }, '', false)).toBeNull();
    expect(soleProvider({ password_sign_in: false, providers: null }, '', false)).toBeNull();
    expect(soleProvider(undefined, '', false)).toBeNull();
  });

  it('does not go back to a provider that failed', () => {
    expect(soleProvider(only, '?sso_error=expired&next=%2F', false)).toBeNull();
    expect(soleProvider(only, '?sso_error=', false)).toBeNull();
  });

  it('does not undo signing out', () => {
    expect(soleProvider(only, '', true)).toBeNull();
  });
});

/** A storage area, as a browser keeps one. */
function area() {
  const kept = new Map<string, string>();
  return {
    getItem: (k: string) => kept.get(k) ?? null,
    setItem: (k: string, v: string) => void kept.set(k, v),
    removeItem: (k: string) => void kept.delete(k),
  };
}

/** A tab on one hostname: its own sessionStorage, the hostname's localStorage. */
function tab(localStorage: ReturnType<typeof area>) {
  return { localStorage, sessionStorage: area() };
}

describe('signing out', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('holds every tab on the hostname until a way back in is chosen', () => {
    const hostname = area();
    const a = tab(hostname);
    const b = tab(hostname);

    vi.stubGlobal('window', a);
    expect(held(0)).toBe(false);
    markSignedOut();

    vi.stubGlobal('window', b);
    expect(held(0)).toBe(true);
    clearSignedOut();

    vi.stubGlobal('window', a);
    expect(held(0)).toBe(false);
  });
});

describe('starting the provider by itself', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('waits for a click when it comes back within 30 seconds', () => {
    vi.stubGlobal('window', tab(area()));
    markStarted(1_000);
    expect(held(1_000 + 29_999)).toBe(true);
    expect(held(1_000 + 30_000)).toBe(false);
  });

  it('holds only the tab that started it', () => {
    const hostname = area();
    vi.stubGlobal('window', tab(hostname));
    markStarted(1_000);
    vi.stubGlobal('window', tab(hostname));
    expect(held(1_000)).toBe(false);
  });
});

describe('a browser that refuses storage', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('waits for a click, and still signs out', () => {
    const refuse = () => {
      throw new DOMException('denied', 'SecurityError');
    };
    vi.stubGlobal('window', {
      get localStorage() {
        return refuse();
      },
      get sessionStorage() {
        return refuse();
      },
    });
    expect(() => markSignedOut()).not.toThrow();
    expect(() => clearSignedOut()).not.toThrow();
    expect(() => markStarted(0)).not.toThrow();
    expect(held(0)).toBe(true);
  });
});
