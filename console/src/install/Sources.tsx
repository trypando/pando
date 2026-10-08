// Source connections (R-091, issue #127): how Pando reads private repositories.
//
// A connection belongs to the installation (O-3). An administrator connects
// GitHub, GitLab, Azure DevOps, Bitbucket, Gitea or any git host once, and every
// app whose repository it covers is read with it — nobody adding an app is asked
// for a token. The calls are the API's: GET /sources, POST /adapters to connect
// one, POST /sources/{id}/authorize to sign one in, DELETE to disconnect (R-261).
//
// A connection is used the moment it is saved. Unlike the other adapters there
// is no restart, so this screen never says to restart.

import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, CodeBlock, Dialog, EmptyState, StatusIndicator } from '@design';

import { api } from '@api/client';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { AdapterDialog } from './AdapterDialog';
import type { ConfiguredAdapter } from './AdapterDialog';
import { Quiet, Screen, messageOf, refusal } from './Accounts';
import { Table } from '../ui/Table';

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

/** What a connection covers, as a person reads it: github.com/acme. */
export function covers(s: SourceConnection): string {
  const { host = '', scope = '' } = s.capabilities;
  return scope ? `${host}/${scope}` : host;
}

const METHODS: Record<string, string> = {
  token: 'Token',
  ssh: 'SSH key',
  app: 'GitHub App',
  oauth: 'OAuth',
  service_principal: 'Service principal',
};

export function useSources(enabled = true) {
  return useQuery({
    queryKey: ['sources'],
    queryFn: () => api.get<{ sources: SourceConnection[] | null }>('/sources'),
    enabled,
  });
}

export function Sources({ query }: { query?: string }) {
  const queries = useQueryClient();
  const canManage = useInstallVerb(InstallVerb.AdaptersManage);
  const sources = useSources();
  const rows = sources.data?.sources ?? [];

  // Stored settings, to edit a connection from what is there.
  const adapters = useQuery({
    queryKey: ['adapters'],
    queryFn: () => api.get<{ adapters: ConfiguredAdapter[] }>('/adapters'),
    enabled: canManage,
  });
  const configured = (adapters.data?.adapters ?? []).filter((a) => a.category === 'source');

  const [editing, setEditing] = useState<{ existing?: ConfiguredAdapter } | null>(null);
  const [authorizing, setAuthorizing] = useState<SourceConnection | null>(null);
  const [removing, setRemoving] = useState<SourceConnection | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // A browser authorization comes back here with its outcome in the address.
  const back = new URLSearchParams(query ?? '');
  const returnedError = back.get('error');
  const returnedOK = back.get('authorized');

  const refresh = () => {
    void queries.invalidateQueries({ queryKey: ['sources'] });
    void queries.invalidateQueries({ queryKey: ['adapters'] });
  };

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/sources/${encodeURIComponent(id)}`),
    onSuccess: (_, id) => {
      setRemoving(null);
      setNotice(`Disconnected ${id}.`);
      refresh();
    },
  });

  const columns = [
    { key: 'name', header: 'Connection', width: 'minmax(0,1fr)' },
    {
      key: 'covers',
      header: 'Covers',
      width: 'minmax(0,24ch)',
      mono: true,
      render: (row: SourceConnection) => covers(row) || '—',
    },
    {
      key: 'method',
      header: 'Signs in with',
      width: '18ch',
      render: (row: SourceConnection) => METHODS[row.capabilities.method ?? ''] ?? row.capabilities.method ?? '—',
    },
    {
      key: 'status',
      header: 'Status',
      width: '18ch',
      render: (row: SourceConnection) =>
        row.problem ? (
          <StatusIndicator status="failed" label="Cannot be used" title={row.problem} />
        ) : row.capabilities.authorized ? (
          <StatusIndicator status="running" label="Ready" />
        ) : (
          <StatusIndicator status="info" label="Not authorized" />
        ),
    },
    ...(canManage
      ? [
          {
            key: 'actions',
            header: '',
            width: '28ch',
            align: 'right' as const,
            render: (row: SourceConnection) => {
              const existing = configured.find((a) => a.id === row.id);
              const canAuthorize = row.capabilities.device_authorization || row.capabilities.web_authorization;
              return (
                <div style={{ display: 'flex', gap: 'var(--space-2)', justifyContent: 'flex-end' }}>
                  {canAuthorize && (
                    <Button variant="secondary" onClick={() => setAuthorizing(row)}>
                      {row.capabilities.authorized ? 'Authorize again' : 'Authorize'}
                    </Button>
                  )}
                  {existing && (
                    <Button variant="secondary" onClick={() => setEditing({ existing })}>
                      Edit
                    </Button>
                  )}
                  <Button variant="ghost" onClick={() => setRemoving(row)}>
                    Disconnect
                  </Button>
                </div>
              );
            },
          },
        ]
      : []),
  ];

  return (
    <Screen
      heading="Sources"
      action={
        canManage && (
          <Button variant="primary" onClick={() => setEditing({})}>
            Connect a source
          </Button>
        )
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <Quiet>
          Pando reads private repositories with the connection that covers them. Add an app from any repository a
          connection covers and it is read with that connection; nobody adding an app is asked for a token.
        </Quiet>
        {returnedError && <Banner tone="failed">{returnedError}</Banner>}
        {returnedOK && !returnedError && <Banner tone="info">{returnedOK} is authorized and ready.</Banner>}
        {notice && <Banner tone="info">{notice}</Banner>}
        {sources.isError && <Quiet>{messageOf(sources.error)}</Quiet>}

        <Table
          loading={sources.isPending}
          columns={columns}
          rows={rows}
          empty={
            <EmptyState heading="No sources connected">
              Public repositories work without one. Connect GitHub, GitLab, Azure DevOps, Bitbucket, Gitea or any
              git host to add apps from private repositories.
            </EmptyState>
          }
        />
      </div>

      {editing && (
        <AdapterDialog
          existing={editing.existing}
          category="source"
          adapters={configured}
          onClose={() => setEditing(null)}
          onSaved={(name) => {
            setEditing(null);
            setNotice(`${name} is saved and in use.`);
            refresh();
          }}
        />
      )}

      {authorizing && (
        <AuthorizeDialog
          source={authorizing}
          onClose={() => setAuthorizing(null)}
          onAuthorized={() => {
            setNotice(`${authorizing.name} is authorized and ready.`);
            setAuthorizing(null);
            refresh();
          }}
        />
      )}

      {removing && (
        <Dialog
          open
          title={`Disconnect ${removing.name}`}
          description="Its stored credential is deleted. Apps read with it keep running; their next deploy fails, saying the connection is gone, until another connection covers their repository."
          onClose={() => setRemoving(null)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setRemoving(null)}>
                Cancel
              </Button>
              <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate(removing.id)}>
                {remove.isPending ? 'Disconnecting' : 'Disconnect'}
              </Button>
            </>
          }
        >
          {remove.isError && <Banner tone="failed">{refusal(remove.error)}</Banner>}
        </Dialog>
      )}
    </Screen>
  );
}

interface Authorization {
  mode: 'device' | 'web';
  user_code?: string;
  verification_url?: string;
  authorize_url?: string;
  interval_seconds?: number;
}

/** Signs a connection in with OAuth: a code to enter on the provider's site,
 *  which needs nothing to reach Pando, or the browser, which comes back here. */
function AuthorizeDialog({
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
      if (a.mode === 'web' && a.authorize_url) window.location.assign(a.authorize_url);
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
  return (
    <Dialog
      open
      title={`Authorize ${source.name}`}
      description="Sign in to the provider and approve Pando's access to read repositories."
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
              Authorize in the browser
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
