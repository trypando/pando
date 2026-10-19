// Source connections (R-091, issue #127): how Pando reads private repositories.
//
// A source connection is an adapter like any other, listed, added and edited on
// the Adapters screen. A connection belongs to the installation (O-3): an
// administrator connects GitHub, GitLab, Azure DevOps, Bitbucket, Gitea or any
// git host once, and every app whose repository it covers is read with it. What
// is particular to one lives here: its state from GET /sources, and signing one
// in with OAuth, or creating and installing its GitHub App, through
// POST /sources/{id}/authorize (R-261).

import { useEffect, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Banner, Button, CodeBlock, Dialog } from '@design';

import { api } from '@api/client';
import { Quiet, refusal } from './Accounts';

/** A source connection, as GET /sources returns it. */
export interface SourceConnection {
  id: string;
  name: string;
  kind: string;
  problem?: string;
  capabilities: {
    method?: string;
    host?: string;
    scope?: string;
    list_repositories?: boolean;
    device_authorization?: boolean;
    web_authorization?: boolean;
    authorized?: boolean;
  };
}

/** Usable now: built, and holding what it needs to read. */
export function ready(s: SourceConnection): boolean {
  return !s.problem && s.capabilities.authorized === true;
}

/** Built, not authorized yet, and able to be: a GitHub App not yet created or
 *  installed, or an OAuth connection with no token. */
export function needsAuthorizing(s: SourceConnection): boolean {
  const c = s.capabilities;
  return !s.problem && !c.authorized && Boolean(c.web_authorization || c.device_authorization);
}

/** What a connection covers, as a person reads it: github.com/acme. */
export function covers(s: SourceConnection): string {
  const { host = '', scope = '' } = s.capabilities;
  return scope ? `${host}/${scope}` : host;
}

export function useSources(enabled = true) {
  return useQuery({
    queryKey: ['sources'],
    queryFn: () => api.get<{ sources: SourceConnection[] | null }>('/sources'),
    enabled,
  });
}

interface Authorization {
  mode: 'device' | 'web';
  user_code?: string;
  verification_url?: string;
  authorize_url?: string;
  interval_seconds?: number;
  /** Posted to the provider instead of opening authorize_url: a GitHub App's
   *  manifest, which GitHub takes only as a form. */
  form?: { action: string; fields: Record<string, string> };
}

/** Posts a form to the provider from this page, as a browser must. */
export function postForm(form: { action: string; fields: Record<string, string> }) {
  const el = document.createElement('form');
  el.method = 'post';
  el.action = form.action;
  el.hidden = true;
  for (const [name, value] of Object.entries(form.fields)) {
    const input = document.createElement('input');
    input.type = 'hidden';
    input.name = name;
    input.value = value;
    el.appendChild(input);
  }
  document.body.appendChild(el);
  el.submit();
}

/** Signs a connection in with OAuth: a code to enter on the provider's site,
 *  which needs nothing to reach Pando, or the browser, which comes back here.
 *  A GitHub App is created and installed in the browser, and comes back too. */
export function AuthorizeDialog({
  source,
  onClose,
  onAuthorized,
}: {
  source: SourceConnection;
  onClose: () => void;
  onAuthorized: () => void;
}) {
  const path = `/sources/${encodeURIComponent(source.id)}/authorize`;
  const begin = useMutation({
    mutationFn: (mode: 'device' | 'web') => api.post<Authorization>(path, { mode }),
    onSuccess: (a) => {
      if (a.mode !== 'web') return;
      if (a.form) postForm(a.form);
      else if (a.authorize_url) window.location.assign(a.authorize_url);
    },
  });
  const device = begin.data?.mode === 'device' ? begin.data : undefined;

  // Poll at the interval the provider asked for, slower when it says so.
  const [pollError, setPollError] = useState<string | null>(null);
  useEffect(() => {
    if (!device) return undefined;
    let interval = Math.max(device.interval_seconds ?? 5, 1) * 1000;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      try {
        const st = await api.post<{ status: string; slow_down?: boolean }>(`${path}/poll`, {});
        if (stopped) return;
        if (st.status === 'authorized') {
          onAuthorized();
          return;
        }
        if (st.slow_down) interval += 5000;
        timer = setTimeout(() => void tick(), interval);
      } catch (e) {
        if (!stopped) setPollError(refusal(e));
      }
    };
    timer = setTimeout(() => void tick(), interval);
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [device, path, onAuthorized]);

  const caps = source.capabilities;
  const app = caps.method === 'app';
  return (
    <Dialog
      open
      title={`Authorize ${source.name}`}
      description={
        app
          ? 'Create the App on GitHub, then pick the repositories it reads.'
          : "Sign in to the provider and approve Pando's access to read repositories."
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {device ? 'Stop waiting' : 'Cancel'}
          </Button>
          {!device && caps.web_authorization && (
            <Button
              variant={caps.device_authorization ? 'secondary' : 'primary'}
              disabled={begin.isPending}
              onClick={() => begin.mutate('web')}
            >
              {app ? 'Continue to GitHub' : 'Authorize in the browser'}
            </Button>
          )}
          {!device && caps.device_authorization && (
            <Button variant="primary" disabled={begin.isPending} onClick={() => begin.mutate('device')}>
              Show a code
            </Button>
          )}
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {!device && caps.device_authorization && (
          <Quiet>A code works even when the provider cannot reach this installation.</Quiet>
        )}
        {device && (
          <>
            <Quiet>
              Go to{' '}
              <a href={device.verification_url} target="_blank" rel="noreferrer">
                {device.verification_url}
              </a>{' '}
              and enter this code. This closes when you have approved it.
            </Quiet>
            <CodeBlock copyable lines={device.user_code ?? ''} />
          </>
        )}
        {begin.isError && <Banner tone="failed">{refusal(begin.error)}</Banner>}
        {pollError && <Banner tone="failed">{pollError}</Banner>}
      </div>
    </Dialog>
  );
}
