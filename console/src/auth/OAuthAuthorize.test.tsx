// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, type Mounted } from '../ui/testing';
import { OAuthAuthorize, authorizeRequest } from './OAuthAuthorize';

const challenge = 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM';
const search =
  `?client_id=oac_01&redirect_uri=${encodeURIComponent('https://claude.ai/api/mcp/auth_callback')}` +
  `&response_type=code&code_challenge=${challenge}&code_challenge_method=S256&state=st-1`;

let sent: { method: string; path: string; body: unknown }[];
let screen: Mounted | undefined;

function serve(answer: (method: string, path: string) => Response) {
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

const consent = { client_id: 'oac_01', client_name: 'Claude', returns_to: 'claude.ai' };

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
  it('reads what an MCP client puts in it', () => {
    expect(authorizeRequest(search)).toEqual({
      client_id: 'oac_01',
      redirect_uri: 'https://claude.ai/api/mcp/auth_callback',
      response_type: 'code',
      code_challenge: challenge,
      code_challenge_method: 'S256',
      state: 'st-1',
      resource: '',
      scope: '',
    });
  });

  it('is not a request without a client', () => {
    expect(authorizeRequest('?state=x')).toBeNull();
  });
});

// R-413: the person sees which client is asking and where it returns to, and
// the browser goes only where the server says.
describe('the approval page', () => {
  it('says which client is asking, and goes where the server says on Approve', async () => {
    const redirect = 'https://claude.ai/api/mcp/auth_callback?code=c&state=st-1';
    serve((method) => (method === 'GET' ? reply(consent) : reply({ redirect_url: redirect }, 201)));
    const navigate = vi.fn();
    screen = mount(<OAuthAuthorize username="ada" search={search} navigate={navigate} />);

    await vi.waitFor(() => expect(screen!.text()).toContain('Connect Claude'));
    expect(screen.text()).toContain('“Claude” is asking to sign in to Pando as ada');
    expect(screen.text()).toContain('Returns to claude.ai.');

    await screen.press('Approve');
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith(redirect));
    expect(sent[1]).toEqual({
      method: 'POST',
      path: '/oauth/authorize',
      body: { ...authorizeRequest(search), approve: true },
    });
  });

  it('cancels to where the server says', async () => {
    const back = 'https://claude.ai/api/mcp/auth_callback?error=access_denied&state=st-1';
    serve((method) => (method === 'GET' ? reply(consent) : reply({ redirect_url: back })));
    const navigate = vi.fn();
    screen = mount(<OAuthAuthorize search={search} navigate={navigate} />);
    await vi.waitFor(() => expect(screen!.text()).toContain('as you'));

    await screen.press('Cancel');
    await vi.waitFor(() => expect(navigate).toHaveBeenCalledWith(back));
    expect(sent[1]).toMatchObject({ method: 'POST', body: { approve: false } });
  });

  it('says what the server said about a request it refuses', async () => {
    serve(() =>
      reply(
        {
          code: 'VALID_INVALID',
          message: 'This sign-in names an MCP client Pando does not know.',
          remedy: 'Remove Pando from the MCP client and add it again.',
        },
        400,
      ),
    );
    screen = mount(<OAuthAuthorize search={search} navigate={vi.fn()} />);
    await vi.waitFor(() => expect(screen!.text()).toContain('Pando does not know.'));
    expect(screen.text()).toContain('add it again.');
  });

  it('says what the server said when approving is refused', async () => {
    serve((method) =>
      method === 'GET'
        ? reply(consent)
        : reply({ code: 'PERM_DENIED', message: 'This account must change its password first.' }, 403),
    );
    const navigate = vi.fn();
    screen = mount(<OAuthAuthorize search={search} navigate={navigate} />);
    await vi.waitFor(() => expect(screen!.text()).toContain('Connect Claude'));
    await screen.press('Approve');
    await vi.waitFor(() => expect(screen!.text()).toContain('must change its password first.'));
    expect(navigate).not.toHaveBeenCalled();
  });

  it('explains an address no client made', () => {
    screen = mount(<OAuthAuthorize search="?state=x" />);
    expect(screen.text()).toContain('Start connecting again from the client.');
    expect(sent).toEqual([]);
  });
});
