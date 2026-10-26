// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, unanswered, type Mounted } from '../ui/testing';
import { Login, digitsOnly } from './Login';
import { TwoFactorSetup, groupKey, recoveryCodesText, withPassword } from './TwoFactorSetup';

const enrollment = {
  secret: 'JBSWY3DPEHPK3PXPJBSWY3DP',
  uri: 'otpauth://totp/Pando:ada?secret=JBSWY3DPEHPK3PXPJBSWY3DP&issuer=Pando',
  qr_svg: '<svg xmlns="http://www.w3.org/2000/svg"></svg>',
};
const codes = Array.from({ length: 10 }, (_, i) => `abcd-efgh-ijkl-mn${String(i).padStart(2, '0')}`);

/** Each request as it was sent: method, path after /api/v1, and body. */
let sent: { method: string; path: string; body: unknown }[];
let screen: Mounted | undefined;

function serve(answer: (method: string, path: string, body: unknown) => Response | Promise<Response>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const path = String(url).replace(/^.*\/api\/v1/, '');
      const method = init?.method ?? 'GET';
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      sent.push({ method, path, body });
      return answer(method, path, body);
    }),
  );
}

beforeEach(() => {
  sent = [];
  window.matchMedia = (() => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} })) as never;
});

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('the helpers', () => {
  it('keeps only digits of a typed or pasted code', () => {
    expect(digitsOnly('123 456')).toBe('123456');
    expect(digitsOnly('12a-3')).toBe('123');
  });

  it('groups a key in fours', () => {
    expect(groupKey('JBSWY3DPEHPK3PXP')).toBe('JBSW Y3DP EHPK 3PXP');
    expect(groupKey('ABCDEF')).toBe('ABCD EF');
    expect(groupKey('')).toBe('');
  });

  it('sends the password only when one was typed', () => {
    expect(withPassword('')).toEqual({});
    expect(withPassword('hunter22')).toEqual({ current_password: 'hunter22' });
  });

  it('writes the codes to a file that says what they are for', () => {
    const text = recoveryCodesText(['a', 'b'], 'ada', 'pando.example.com');
    expect(text).toContain('Pando recovery codes for ada at pando.example.com');
    expect(text).toContain('Each code signs you in once');
    expect(text.split('\n')).toEqual(expect.arrayContaining(['a', 'b']));
    expect(recoveryCodesText(['a'], undefined, 'h')).toContain('Pando recovery codes at h');
  });
});

// R-405: a code after the right password, for an account that has it.
describe('signing in with a second factor', () => {
  function signIn() {
    screen = mount(<Login />, [
      [['setup'], { needed: false }],
      [['sign-in-options'], { password_sign_in: true, providers: [] }],
    ]);
    return screen;
  }

  it('asks for a code after the password, and sends both', async () => {
    serve((_, path, body) => {
      if (path !== '/sessions') return unanswered();
      if (!(body as { code?: string }).code) {
        return reply({ code: 'AUTH_TWO_FACTOR_REQUIRED', message: 'Enter the code from your authenticator app.' }, 401);
      }
      return unanswered();
    });
    const s = signIn();
    await s.fill('Username', 'ada');
    await s.fill('Password', 'correct horse');
    await s.submit('Password');

    expect(s.text()).toContain('Enter your code');
    expect(s.text()).toContain('Enter the 6-digit code from your authenticator app.');
    // Not shown as a failure: it is the next step.
    expect(s.text()).not.toContain('Enter the code from your authenticator app.');

    await s.fill('Code', '123 456');
    await s.submit('Code');
    expect(sent.at(-1)).toEqual({
      method: 'POST',
      path: '/sessions',
      body: { username: 'ada', password: 'correct horse', code: '123456' },
    });
  });

  it('takes a recovery code instead, and can start over', async () => {
    serve((_, path, body) => {
      if (path !== '/sessions') return unanswered();
      if (!(body as { code?: string }).code) return reply({ code: 'AUTH_TWO_FACTOR_REQUIRED', message: 'x' }, 401);
      return reply({ code: 'AUTH_INVALID', message: 'That code is not right.' }, 401);
    });
    const s = signIn();
    await s.fill('Username', 'ada');
    await s.fill('Password', 'correct horse');
    await s.submit('Password');

    await s.press('Use a recovery code');
    expect(s.text()).toContain('Each one works once.');
    await s.fill('Recovery code', ' abcd-efgh-ijkl-mnop ');
    await s.submit('Recovery code');
    expect((sent.at(-1)!.body as { code: string }).code).toBe('abcd-efgh-ijkl-mnop');
    expect(s.text()).toContain('That code is not right.');

    await s.press('Use your authenticator app');
    expect(s.text()).not.toContain('That code is not right.');

    await s.press('Start over');
    expect(s.text()).toContain('Sign in to Pando');
    expect((document.getElementById('f-password') as HTMLInputElement).value).toBe('');
  });
});

describe('the setup page', () => {
  it('offers Not now when it is not required, and says so to the server', async () => {
    serve((method, path) => (method === 'POST' && path === '/me/two-factor/skip' ? new Response(null, { status: 204 }) : reply({})));
    const onDone = vi.fn();
    screen = mount(<TwoFactorSetup username="ada" onDone={onDone} />);
    expect(screen.text()).toContain('Add two-factor authentication');
    expect(screen.text()).not.toContain('Sign out');

    await screen.press('Not now');
    expect(sent.some((r) => r.method === 'POST' && r.path === '/me/two-factor/skip')).toBe(true);
    expect(onDone).toHaveBeenCalled();
  });

  it('has no Not now when the installation requires it', () => {
    serve(() => unanswered());
    screen = mount(<TwoFactorSetup required />);
    expect(screen.text()).toContain('This installation requires two-factor authentication');
    expect(screen.text()).not.toContain('Not now');
    expect(screen.text()).toContain('Sign out');
  });

  it('goes from the QR code to the recovery codes, and waits for them to be saved', async () => {
    serve((_, path) => {
      if (path === '/me/two-factor/totp') return reply(enrollment);
      if (path === '/me/two-factor/totp/confirm') return reply({ recovery_codes: codes });
      return reply({});
    });
    const onStarted = vi.fn();
    const onDone = vi.fn();
    screen = mount(<TwoFactorSetup required username="ada" onStarted={onStarted} onDone={onDone} />);

    await screen.press('Set up');
    expect(onStarted).toHaveBeenCalled();
    expect(screen.text()).toContain('Scan this code');
    expect(screen.text()).toContain('JBSW Y3DP EHPK 3PXP JBSW Y3DP');
    const img = document.querySelector('img')!;
    expect(img.getAttribute('src')).toMatch(/^data:image\/svg\+xml/);

    await screen.fill('Code from the app', '12345');
    expect([...document.querySelectorAll('button')].find((b) => b.textContent === 'Turn on')!.disabled).toBe(true);
    await screen.fill('Code from the app', '123456');
    await screen.submit('Code from the app');
    expect(sent.find((r) => r.path === '/me/two-factor/totp/confirm')!.body).toEqual({ code: '123456' });

    expect(screen.text()).toContain('Save your recovery codes');
    for (const c of codes) expect(screen.text()).toContain(c);
    const proceed = () => [...document.querySelectorAll('button')].find((b) => b.textContent === 'Continue')!;
    expect(proceed().disabled).toBe(true);

    await screen.click('I saved these recovery codes');
    expect(proceed().disabled).toBe(false);
    await screen.press('Continue');
    expect(onDone).toHaveBeenCalled();
  });

  it('asks for the password again on an old session, and sends it', async () => {
    serve((_, path) =>
      path === '/me/two-factor/totp'
        ? reply({ code: 'AUTH_INVALID', message: 'That current password is not right.' }, 401)
        : unanswered(),
    );
    screen = mount(<TwoFactorSetup passwordRequired />);
    const button = () => [...document.querySelectorAll('button')].find((b) => b.textContent === 'Set up')!;
    expect(button().disabled).toBe(true);
    await screen.fill('Current password', 'hunter22');
    await screen.press('Set up');
    expect(sent.at(-1)!.body).toEqual({ current_password: 'hunter22' });
    expect(screen.text()).toContain('That current password is not right.');
  });

  it('copies and downloads the codes', async () => {
    serve((_, path) => {
      if (path === '/me/two-factor/totp') return reply(enrollment);
      if (path === '/me/two-factor/totp/confirm') return reply({ recovery_codes: codes });
      return unanswered();
    });
    const writeText = vi.fn(async () => {});
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    URL.createObjectURL = vi.fn(() => 'blob:codes');
    URL.revokeObjectURL = vi.fn();
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});

    screen = mount(<TwoFactorSetup />);
    await screen.press('Set up');
    await screen.fill('Code from the app', '123456');
    await screen.submit('Code from the app');

    await screen.press('Copy');
    expect(writeText).toHaveBeenCalledWith(codes.join('\n'));
    expect(screen.text()).toContain('Copied');

    await screen.press('Download');
    expect(clicked).toHaveBeenCalled();
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:codes');
    clicked.mockRestore();
  });

  // R-405: the holder is told to save the codes, and over plain HTTP, as the
  // shipped compose file serves Pando, there is no clipboard API (#322).
  it('R-405 copies the recovery codes over plain HTTP', async () => {
    serve((_, path) => {
      if (path === '/me/two-factor/totp') return reply(enrollment);
      if (path === '/me/two-factor/totp/confirm') return reply({ recovery_codes: codes });
      return unanswered();
    });
    vi.stubGlobal('navigator', {});
    let copied = '';
    const execCommand = vi.fn(() => {
      const field = document.activeElement as HTMLTextAreaElement;
      copied = field.value.slice(field.selectionStart, field.selectionEnd);
      return true;
    });
    Object.defineProperty(document, 'execCommand', { value: execCommand, configurable: true });

    screen = mount(<TwoFactorSetup />);
    await screen.press('Set up');
    await screen.fill('Code from the app', '123456');
    await screen.submit('Code from the app');
    await screen.press('Copy');

    expect(execCommand).toHaveBeenCalledWith('copy');
    expect(copied).toBe(codes.join('\n'));
    expect(screen.text()).toContain('Copied');
    Reflect.deleteProperty(document, 'execCommand');
  });

  it('says how to copy when the browser will not', async () => {
    serve((_, path) => {
      if (path === '/me/two-factor/totp') return reply(enrollment);
      if (path === '/me/two-factor/totp/confirm') return reply({ recovery_codes: codes });
      return unanswered();
    });
    vi.stubGlobal('navigator', {});
    screen = mount(<TwoFactorSetup />);
    await screen.press('Set up');
    await screen.fill('Code from the app', '123456');
    await screen.submit('Code from the app');
    await screen.press('Copy');
    expect(screen.text()).toContain('Your browser blocked copying.');
  });
});
