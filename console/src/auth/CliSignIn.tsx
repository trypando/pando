// Approving a CLI sign-in (R-406, design 06 §3.5).
//
// `pando login` opens this page with a PKCE challenge in its address. The
// person is signed in here however their account signs in — this is the only
// way an account from an identity provider gets the CLI a token — and either
// approves or cancels. Approving sends the browser to the CLI's loopback
// address with a one-time code, or, when the CLI is on another machine, shows
// the code to paste into it. The code is worth nothing without the verifier,
// which never left the CLI.
//
// The CLI sends the browser back here afterwards (`?signed_in=1`,
// `?canceled=1`), so what the person is told is said by Pando's own page.

import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Banner, Button, Input } from '@design';

import { api } from '@api/client';
import { Frame, withRemedy } from './Login';
import { copyText } from '../ui/clipboard';

/** What `pando login` put in the address, as POST /cli-sign-ins takes it. */
export interface CliRequest {
  code_challenge: string;
  code_challenge_method: string;
  name: string;
  /** The CLI's loopback port, or 0 when the code is pasted. */
  port: number;
  state: string;
}

interface Approved {
  code: string;
  redirect_url: string;
}

const STATE = /^[A-Za-z0-9._~-]{16,128}$/;

/** The request in `search`, or null when it is not one `pando login` makes. The
 *  server checks every field again; this only decides which page to draw. */
export function cliRequest(search: string): CliRequest | null {
  const q = new URLSearchParams(search);
  const challenge = q.get('code_challenge') ?? '';
  const name = (q.get('name') ?? '').trim();
  const rawPort = q.get('port');
  const port = rawPort === null ? 0 : Number(rawPort);
  if (challenge === '' || name === '' || !Number.isInteger(port)) return null;
  return {
    code_challenge: challenge,
    code_challenge_method: q.get('code_challenge_method') ?? '',
    name,
    port,
    state: q.get('state') ?? '',
  };
}

/** Where the CLI sent the browser back to say how it ended, if it did. */
export function outcome(search: string): 'signed_in' | 'canceled' | null {
  const q = new URLSearchParams(search);
  if (q.get('signed_in') === '1') return 'signed_in';
  if (q.get('canceled') === '1') return 'canceled';
  return null;
}

/**
 * Where Cancel tells the CLI to stop waiting: its loopback address, built
 * from a port number and a plain state and nothing else, so this page can be
 * made to send the browser nowhere but 127.0.0.1. Null for a pasted code,
 * where nothing is listening.
 */
export function cancelAddress(req: CliRequest): string | null {
  if (req.port < 1024 || req.port > 65535 || !STATE.test(req.state)) return null;
  return `http://127.0.0.1:${req.port}/callback?error=access_denied&state=${encodeURIComponent(req.state)}`;
}

const stack = { display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' } as const;

function Ended({ heading, lede }: Readonly<{ heading: string; lede: string }>) {
  return (
    <Frame heading={heading} lede={lede}>
      {null}
    </Frame>
  );
}

/** A pasted code, with a way to copy it. */
function CodeToPaste({ code }: Readonly<{ code: string }>) {
  const [copied, setCopied] = useState<'yes' | 'no' | null>(null);

  async function copy() {
    if (await copyText(code)) {
      setCopied('yes');
      window.setTimeout(() => setCopied(null), 2_000);
    } else {
      setCopied('no');
    }
  }

  return (
    <div style={stack}>
      <Input
        label="Code"
        id="cli-code"
        mono
        readOnly
        value={code}
        spellCheck={false}
        autoComplete="off"
        helper={copied === 'no' ? 'Your browser blocked copying. Select the code and copy it from here.' : undefined}
      />
      <Button variant="primary" fullWidth onClick={() => void copy()}>
        {copied === 'yes' ? 'Copied' : 'Copy code'}
      </Button>
    </div>
  );
}

export function CliSignIn({
  username,
  search = window.location.search,
  navigate = (to: string) => window.location.assign(to),
}: Readonly<{
  username?: string;
  search?: string;
  /** Leaves this page for the CLI's loopback address. */
  navigate?: (to: string) => void;
}>) {
  const [canceled, setCanceled] = useState(false);
  const req = cliRequest(search);

  const approve = useMutation({
    mutationFn: (r: CliRequest) => api.post<Approved>('/cli-sign-ins', r),
    onSuccess: (got) => {
      if (got.redirect_url) navigate(got.redirect_url);
    },
  });

  const ended = outcome(search);
  if (ended === 'signed_in') {
    return (
      <Ended heading="The CLI is signed in" lede="Go back to your terminal. You can close this tab." />
    );
  }
  if (ended === 'canceled' || canceled) {
    return (
      <Ended
        heading="Sign-in canceled"
        lede="Nothing was approved, and the CLI stored nothing. You can close this tab."
      />
    );
  }
  if (!req) {
    return (
      <Ended
        heading="Sign in the CLI"
        lede="This address is missing what pando login puts in it. Run pando login again and open the address it prints."
      />
    );
  }

  if (approve.data && !approve.data.redirect_url) {
    return (
      <Frame
        heading="Paste this code into your terminal"
        lede="It works once, within 5 minutes, and only in the pando login that printed this page’s address."
      >
        <CodeToPaste code={approve.data.code} />
      </Frame>
    );
  }

  function cancel() {
    const to = req ? cancelAddress(req) : null;
    if (to) navigate(to);
    else setCanceled(true);
  }

  const who = username ? `as ${username}` : 'as you';
  return (
    <Frame
      heading="Sign in the CLI"
      lede={`“${req.name}” is asking to sign in to Pando ${who}. It gets a token that can do everything you can, until you revoke it under API and tools.`}
    >
      <div style={stack}>
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          Approve only if you ran pando login yourself just now.
        </p>
        {approve.isError && <Banner tone="failed">{withRemedy(approve.error)}</Banner>}
        <Button
          variant="primary"
          fullWidth
          disabled={approve.isPending || approve.isSuccess}
          onClick={() => approve.mutate(req)}
        >
          {approve.isPending || approve.isSuccess ? 'Approving' : 'Approve'}
        </Button>
        <Button variant="secondary" fullWidth disabled={approve.isPending} onClick={cancel}>
          Cancel
        </Button>
      </div>
    </Frame>
  );
}
