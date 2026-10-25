// Approving an MCP client's sign-in (R-413, design 06 §3.6).
//
// OAuth's authorization endpoint. An MCP client the person is adding Pando to
// opens this page with its client ID, its redirect address and a PKCE
// challenge. The server says which client it is and where the browser goes
// back to; the person approves or cancels, and either way the server answers
// with the client's own address to go to — this page never builds one, so it
// can be made to send the browser nowhere a client did not register.

import { useMutation, useQuery } from '@tanstack/react-query';
import { Banner, Button } from '@design';

import { api } from '@api/client';
import { Frame, withRemedy } from './Login';

/** What GET /oauth/authorize answers. */
interface Consent {
  client_name: string;
  /** The host, or native app scheme, the browser goes back to. */
  returns_to: string;
}

const FIELDS = [
  'client_id',
  'redirect_uri',
  'response_type',
  'code_challenge',
  'code_challenge_method',
  'state',
  'resource',
  'scope',
] as const;

export type AuthorizeRequest = Record<(typeof FIELDS)[number], string>;

/** The request in `search`, or null without a client. The server checks every
 *  field; this only decides which page to draw. */
export function authorizeRequest(search: string): AuthorizeRequest | null {
  const q = new URLSearchParams(search);
  if (!q.get('client_id')) return null;
  return Object.fromEntries(FIELDS.map((f) => [f, q.get(f) ?? ''])) as AuthorizeRequest;
}

const stack = { display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' } as const;
const note = { font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 } as const;

export function OAuthAuthorize({
  username,
  search = window.location.search,
  navigate = (to: string) => window.location.assign(to),
}: Readonly<{
  username?: string;
  search?: string;
  /** Leaves this page for the client's address. */
  navigate?: (to: string) => void;
}>) {
  const req = authorizeRequest(search);
  const query = new URLSearchParams(req ?? {}).toString();

  const consent = useQuery({
    queryKey: ['oauth-authorize', query],
    queryFn: () => api.get<Consent>(`/oauth/authorize?${query}`),
    enabled: req !== null,
    retry: false,
  });

  const decide = useMutation({
    mutationFn: (approve: boolean) =>
      api.post<{ redirect_url: string }>('/oauth/authorize', { ...req, approve }),
    onSuccess: (got) => navigate(got.redirect_url),
  });

  if (!req) {
    return (
      <Frame
        heading="Connect an MCP client"
        lede="This address is missing what an MCP client puts in it. Start connecting again from the client."
      >
        {null}
      </Frame>
    );
  }
  if (consent.isError) {
    return (
      <Frame heading="Connect an MCP client">
        <Banner tone="failed">{withRemedy(consent.error)}</Banner>
      </Frame>
    );
  }
  if (!consent.data) {
    return <Frame heading="Connect an MCP client">{null}</Frame>;
  }

  const who = username ? `as ${username}` : 'as you';
  const busy = decide.isPending || decide.isSuccess;
  return (
    <Frame
      heading={`Connect ${consent.data.client_name}`}
      lede={`“${consent.data.client_name}” is asking to sign in to Pando ${who}. It gets a token that can do everything you can, until you revoke it under API and tools.`}
    >
      <div style={stack}>
        <p style={note}>Returns to {consent.data.returns_to}.</p>
        <p style={note}>Approve only if you are connecting it yourself just now.</p>
        {decide.isError && <Banner tone="failed">{withRemedy(decide.error)}</Banner>}
        <Button variant="primary" fullWidth disabled={busy} onClick={() => decide.mutate(true)}>
          {busy && decide.variables ? 'Approving' : 'Approve'}
        </Button>
        <Button variant="secondary" fullWidth disabled={busy} onClick={() => decide.mutate(false)}>
          Cancel
        </Button>
      </div>
    </Frame>
  );
}
