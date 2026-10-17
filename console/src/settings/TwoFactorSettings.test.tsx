// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, unanswered, type Mounted, type Seed } from '../ui/testing';
import type { TwoFactorStatus } from '../app/principal';
import { ResetTwoFactor } from '../install/Account';
import type { Account } from '../install/Accounts';
import { App } from '../app/App';
import { Settings } from './Settings';
import { TwoFactorSettings, remaining } from './TwoFactorSettings';

const enrollment = { secret: 'JBSWY3DPEHPK3PXP', uri: 'otpauth://totp/x', qr_svg: '<svg/>' };
const codes = Array.from({ length: 10 }, (_, i) => `wxyz-wxyz-wxyz-wx${String(i).padStart(2, '0')}`);

const off: TwoFactorStatus = {
  enabled: false,
  required: false,
  setup_required: false,
  offer: false,
  password_required: false,
  recovery_codes_remaining: 0,
};
const on: TwoFactorStatus = { ...off, enabled: true, recovery_codes_remaining: 10 };

let sent: { method: string; path: string; body: unknown }[];
let screen: Mounted | undefined;

function serve(answer: (method: string, path: string) => Response | Promise<Response>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const path = String(url).replace(/^.*\/api\/v1/, '');
      const method = init?.method ?? 'GET';
      sent.push({ method, path, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      return answer(method, path);
    }),
  );
}

const button = (label: string) => [...document.querySelectorAll('button')].filter((b) => b.textContent === label).at(-1);

beforeEach(() => {
  sent = [];
  window.matchMedia = (() => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} })) as never;
});

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('remaining', () => {
  it('says to make more when few are left', () => {
    expect(remaining(10)).toBe('10 recovery codes left.');
    expect(remaining(3)).toContain('Make new ones');
    expect(remaining(1)).toBe('1 recovery code left. Make new ones in case you lose your phone.');
    expect(remaining(0)).toContain('No recovery codes left.');
  });
});

// R-405, in your settings.
describe('two-factor authentication in settings', () => {
  it('offers to set it up when it is off, straight to the code on a recent session', async () => {
    serve((_, path) => {
      if (path === '/me/two-factor/totp') return reply(enrollment);
      if (path === '/me/two-factor/totp/confirm') return reply({ recovery_codes: codes });
      return unanswered();
    });
    screen = mount(<TwoFactorSettings status={off} username="ada" />);
    expect(screen.text()).toContain('Off. Signing in asks only for your password.');

    await screen.press('Set up');
    expect(screen.text()).toContain('Scan this code');
    await screen.fill('Code from the app', '654321');
    await screen.press('Turn on');
    expect(screen.text()).toContain('Save your recovery codes');
    expect(button('Done')!.disabled).toBe(true);
    await screen.click('I saved these recovery codes');
    await screen.press('Done');
    expect(screen.text()).not.toContain('Save your recovery codes');
  });

  it('asks for the password first on an old session', async () => {
    serve((_, path) => (path === '/me/two-factor/totp' ? reply(enrollment) : unanswered()));
    screen = mount(<TwoFactorSettings status={{ ...on, password_required: true }} />);
    await screen.press('Replace authenticator');
    expect(screen.text()).toContain('Replace your authenticator');
    expect(sent).toHaveLength(0);
    await screen.fill('Current password', 'hunter22');
    await screen.press('Continue');
    expect(sent.at(-1)!.body).toEqual({ current_password: 'hunter22' });
    expect(screen.text()).toContain('The current one keeps working');
    await screen.press('Cancel');
    expect(screen.text()).not.toContain('Scan this code');
  });

  it('makes new recovery codes', async () => {
    serve((_, path) => (path === '/me/two-factor/recovery-codes' ? reply({ recovery_codes: codes }) : unanswered()));
    screen = mount(<TwoFactorSettings status={on} />);
    expect(screen.text()).toContain('On. Signing in asks for a code');
    await screen.press('New recovery codes');
    expect(screen.text()).toContain('Your current recovery codes stop working.');
    await screen.press('Make new codes');
    expect(sent.at(-1)).toMatchObject({ method: 'POST', path: '/me/two-factor/recovery-codes', body: {} });
    for (const c of codes) expect(screen.text()).toContain(c);
  });

  it('turns it off, sending the password when asked', async () => {
    serve((method, path) => (method === 'DELETE' && path === '/me/two-factor' ? new Response(null, { status: 204 }) : unanswered()));
    screen = mount(<TwoFactorSettings status={{ ...on, password_required: true }} />);
    await screen.press('Turn off');
    expect(button('Turn off')!.disabled).toBe(true);
    await screen.fill('Current password', 'hunter22');
    await screen.press('Turn off');
    expect(sent.at(-1)).toEqual({ method: 'DELETE', path: '/me/two-factor', body: { current_password: 'hunter22' } });
    expect(screen.text()).not.toContain('Your authenticator app’s codes');
  });

  it('shows a refusal to turn off', async () => {
    serve(() => reply({ code: 'POLICY_TWO_FACTOR_REQUIRED', message: 'This installation requires it.' }, 403));
    screen = mount(<TwoFactorSettings status={on} />);
    await screen.press('Turn off');
    await screen.press('Turn off');
    expect(screen.text()).toContain('This installation requires it.');
  });

  it('cannot be turned off where the installation requires it', () => {
    serve(() => unanswered());
    screen = mount(<TwoFactorSettings status={{ ...on, required: true }} />);
    expect(button('Turn off')).toBeUndefined();
    expect(screen.text()).toContain('so it can’t be turned off');
  });
});

describe('the settings page', () => {
  const me = (two_factor?: TwoFactorStatus) => [[['me'], { principal_kind: 'user', id: 'usr_1', two_factor }]] as Seed;

  it('has the section for a local account only', () => {
    serve(() => unanswered());
    screen = mount(<Settings onBack={() => {}} />, me(on));
    expect(screen.text()).toContain('Two-factor authentication');
    screen.unmount();
    screen = mount(<Settings onBack={() => {}} />, me(undefined));
    expect(screen.text()).not.toContain('Two-factor authentication');
  });
});

describe('resetting somebody else’s two-factor', () => {
  const account = { id: 'usr_1', adapter_id: 'idp_local', external_id: 'grace', two_factor_enabled: true } as Account;

  it('says what happens, then removes it', async () => {
    serve((method, path) =>
      method === 'DELETE' && path === '/users/usr_1/two-factor' ? new Response(null, { status: 204 }) : unanswered(),
    );
    screen = mount(<ResetTwoFactor account={account} />);
    await screen.press('Reset two-factor');
    expect(screen.text()).toContain('signs the account out everywhere');
    expect(screen.text()).toContain('they set it up again at their next sign-in');
    await screen.press('Reset two-factor');
    expect(sent.at(-1)).toMatchObject({ method: 'DELETE', path: '/users/usr_1/two-factor' });
    expect(screen.text()).not.toContain('signs the account out everywhere');
  });

  it('shows a refusal', async () => {
    serve(() => reply({ code: 'PERM_DENIED', message: 'You can’t change this account.' }, 403));
    screen = mount(<ResetTwoFactor account={account} />);
    await screen.press('Reset two-factor');
    await screen.press('Reset two-factor');
    expect(screen.text()).toContain('You can’t change this account.');
    await screen.press('Cancel');
    expect(screen.text()).not.toContain('You can’t change this account.');
  });
});

// The gate (R-405): required, nothing else is drawn; offered, it is asked once.
describe('the console after signing in', () => {
  const me = (two_factor?: TwoFactorStatus) => ({
    principal_kind: 'user',
    id: 'usr_1',
    user_id: 'usr_1',
    username: 'ada',
    verbs: [],
    two_factor,
  });

  it('shows only the setup page when the installation requires it', () => {
    serve(() => unanswered());
    screen = mount(<App />, [[['me'], me({ ...off, required: true, setup_required: true })]]);
    expect(screen.text()).toContain('Set up two-factor authentication');
    expect(screen.text()).not.toContain('Not now');
  });

  it('offers it when it has not been offered', () => {
    serve(() => unanswered());
    screen = mount(<App />, [[['me'], me({ ...off, offer: true })]]);
    expect(screen.text()).toContain('Add two-factor authentication');
    expect(screen.text()).toContain('Not now');
  });

  it('asks nothing of an account that has it or was asked', () => {
    serve(() => unanswered());
    screen = mount(<App />, [[['me'], me(off)]]);
    expect(screen.text()).not.toContain('two-factor authentication');
  });
});
