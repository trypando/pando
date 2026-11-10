// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, type Mounted } from '../ui/testing';
import { CliSignIn, cancelAddress, cliRequest, outcome } from './CliSignIn';

const challenge = 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM';
const search = `?code_challenge=${challenge}&code_challenge_method=S256&name=laptop+CLI&port=49152&state=state-0123456789abcdef`;
const pasted = `?code_challenge=${challenge}&code_challenge_method=S256&name=laptop+CLI`;

let sent: { method: string; path: string; body: unknown }[];
let screen: Mounted | undefined;

function serve(answer: (path: string) => Response) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const path = String(url).replace(/^.*\/api\/v1/, '');
      sent.push({ method: init?.method ?? 'GET', path, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      return answer(path);
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

describe('reading the address', () => {
  it('reads what pando login puts in it', () => {
    expect(cliRequest(search)).toEqual({
      code_challenge: challenge,
      code_challenge_method: 'S256',
      name: 'laptop CLI',
      port: 49152,
      state: 'state-0123456789abcdef',
    });
    expect(cliRequest(pasted)?.port).toBe(0);
    expect(cliRequest(search)?.kind).toBeUndefined();
    expect(cliRequest(`${search}&kind=service`)?.kind).toBe('service');
    expect(cliRequest(`${search}&kind=admin`)?.kind).toBeUndefined();
  });

  it('is not a request without a challenge, a name or a whole port', () => {
    expect(cliRequest('')).toBeNull();
    expect(cliRequest('?name=x')).toBeNull();
    expect(cliRequest(`?code_challenge=${challenge}&name=+`)).toBeNull();
    expect(cliRequest(`?code_challenge=${challenge}&name=x&port=4.5`)).toBeNull();
  });

  it('says how it ended', () => {
    expect(outcome('?signed_in=1')).toBe('signed_in');
    expect(outcome('?created=1')).toBe('created');
    expect(outcome('?canceled=1')).toBe('canceled');
    expect(outcome(search)).toBeNull();
  });

  it('cancels only to the loopback address', () => {
    expect(cancelAddress(cliRequest(search)!)).toBe(
      'http://127.0.0.1:49152/callback?error=access_denied&state=state-0123456789abcdef',
    );
    expect(cancelAddress(cliRequest(pasted)!)).toBeNull();
    expect(cancelAddress({ ...cliRequest(search)!, port: 80 })).toBeNull();
    expect(cancelAddress({ ...cliRequest(search)!, state: 'x@evil.example/' })).toBeNull();
  });
});

// R-406: the person approves in the browser, and the code goes to the CLI.
describe('the approval page', () => {
  it('says who the CLI will act as, and sends the browser to it on Approve', async () => {
    const redirect = 'http://127.0.0.1:49152/callback?code=c&state=state-0123456789abcdef';
    serve(() => reply({ code: 'c', redirect_url: redirect }, 201));
    const navigate = vi.fn();
    screen = mount(<CliSignIn username="ada" search={search} navigate={navigate} />);

    expect(screen.text()).toContain('Sign in the CLI');
    expect(screen.text()).toContain('“laptop CLI” is asking to sign in to Pando as ada');
    expect(screen.text()).toContain('Approve only if you ran pando login yourself just now.');

    await screen.press('Approve');
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith(redirect));
    expect(sent).toEqual([{ method: 'POST', path: '/cli-sign-ins', body: cliRequest(search) }]);
  });

  it('shows the code to paste when the CLI is not listening', async () => {
    serve(() => reply({ code: 'the-one-time-code', redirect_url: '' }, 201));
    const navigate = vi.fn();
    screen = mount(<CliSignIn search={pasted} navigate={navigate} />);

    await screen.press('Approve');
    await vi.waitFor(() => expect(screen!.text()).toContain('Paste this code into your terminal'));
    expect((document.getElementById('cli-code') as HTMLInputElement).value).toBe('the-one-time-code');
    expect(navigate).not.toHaveBeenCalled();
  });

  it('copies the code, or says the browser would not', async () => {
    serve(() => reply({ code: 'the-one-time-code', redirect_url: '' }, 201));
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    screen = mount(<CliSignIn search={pasted} navigate={vi.fn()} />);
    await screen.press('Approve');
    await vi.waitFor(() => expect(screen!.text()).toContain('Copy code'));

    await screen.press('Copy code');
    await vi.waitFor(() => expect(screen!.text()).toContain('Copied'));
    expect(writeText).toHaveBeenCalledWith('the-one-time-code');

    writeText.mockRejectedValue(new Error('denied'));
    await screen.press('Copied');
    await vi.waitFor(() => expect(screen!.text()).toContain('Your browser blocked copying.'));
  });

  it('says what the server said when approving is refused', async () => {
    serve(() =>
      reply(
        {
          code: 'PERM_DENIED',
          message: 'This account must change its password before it can sign in the CLI.',
          remedy: 'Choose a new password in the console, then run pando login again.',
        },
        403,
      ),
    );
    screen = mount(<CliSignIn search={search} navigate={vi.fn()} />);
    await screen.press('Approve');
    await vi.waitFor(() => expect(screen!.text()).toContain('must change its password'));
    expect(screen.text()).toContain('then run pando login again.');
  });

  it('tells a waiting CLI it was canceled', async () => {
    const navigate = vi.fn();
    screen = mount(<CliSignIn search={search} navigate={navigate} />);
    await screen.press('Cancel');
    expect(navigate).toHaveBeenCalledWith(cancelAddress(cliRequest(search)!));
    expect(sent).toEqual([]);
  });

  it('cancels in place when nothing is waiting', async () => {
    screen = mount(<CliSignIn search={pasted} navigate={vi.fn()} />);
    await screen.press('Cancel');
    expect(screen.text()).toContain('Sign-in canceled');
  });

  it('says how it ended when the CLI sends the browser back', () => {
    screen = mount(<CliSignIn search="?signed_in=1" />);
    expect(screen.text()).toContain('The CLI is signed in');
    screen.unmount();
    screen = mount(<CliSignIn search="?canceled=1" />);
    expect(screen.text()).toContain('the CLI stored nothing');
  });

  // R-060, issue #396: pando token create --service asks for a service token.
  it('asks to approve a service token for pando token create --service', async () => {
    const redirect = 'http://127.0.0.1:49152/callback?code=c&state=state-0123456789abcdef';
    serve(() => reply({ code: 'c', redirect_url: redirect }, 201));
    const navigate = vi.fn();
    const asService = search.replace('laptop+CLI', 'nightly+agent') + '&kind=service';
    screen = mount(<CliSignIn username="ada" search={asService} navigate={navigate} />);

    expect(screen.text()).toContain('Create a service token');
    expect(screen.text()).toContain('“nightly agent” will reach only the apps shared with it');
    expect(screen.text()).toContain('Approve only if you ran pando token create yourself just now.');

    await screen.press('Approve');
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith(redirect));
    expect(sent[0]?.body).toMatchObject({ name: 'nightly agent', kind: 'service' });
  });

  it('says a token was made when pando token create sends the browser back', () => {
    screen = mount(<CliSignIn search="?created=1" />);
    expect(screen.text()).toContain('Token created');
  });

  it('explains an address pando login did not make', () => {
    screen = mount(<CliSignIn search="?name=x" />);
    expect(screen.text()).toContain('Run pando login again');
  });
});
