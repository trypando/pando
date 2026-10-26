// Hosts — the cluster's hosts and the tokens they join with, behind
// install.cluster.manage (R-414, R-415, docs/design/notes-cluster.md).
//
// A join token is shown once, in the dialog that created it, which stays open
// until closed: closing it takes the only copy with it, as a service token's
// does (R-063). Revoking a token is one press, as a service token's is; removing
// a host asks first, since the host is cut off at once.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Dialog, Input, Select } from '@design';

import { api } from '@api/client';
import type { Host, JoinToken } from '@api/types.gen';
import { Table } from '../ui/Table';
import { CopyField } from '../ui/CopyField';
import { FailedBanner } from '../ui/ErrorCode';
import { Quiet } from '../admin/settingsParts';
import { SystemCard } from './Updates';
import { SystemPage } from './SystemPage';
import { stamp } from './stamp';

const HOSTS = ['cluster-hosts'];
const TOKENS = ['cluster-join-tokens'];

/** "Control and app", "App". */
export function rolesLabel(roles: string[] | null | undefined): string {
  if (!roles?.length) return '—';
  const text = roles.join(' and ');
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/** "0 of 1". */
export function usesLabel(t: Pick<JoinToken, 'uses' | 'max_uses'>): string {
  return `${t.uses} of ${t.max_uses}`;
}

export function Hosts() {
  return (
    <SystemPage title="Hosts">
      <HostList />
      <TokenList />
    </SystemPage>
  );
}

function HostList() {
  const queries = useQueryClient();
  const [removing, setRemoving] = useState<Host | null>(null);
  const hosts = useQuery({ queryKey: HOSTS, queryFn: () => api.get<{ hosts: Host[] | null }>('/cluster/hosts') });
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/cluster/hosts/${encodeURIComponent(id)}`),
    onSuccess: () => queries.invalidateQueries({ queryKey: HOSTS }),
  });

  return (
    <>
      {hosts.isError && <FailedBanner error={hosts.error} remedy />}
      <Table
        loading={hosts.isPending}
        skeletonRows={2}
        empty={<Quiet>None.</Quiet>}
        columns={[
          { key: 'name', header: 'Name', width: 'minmax(0,1fr)' },
          { key: 'roles', header: 'Roles', width: '16ch', render: (row: Host) => rolesLabel(row.roles) },
          { key: 'address', header: 'Address', width: 'minmax(0,22ch)', mono: true },
          { key: 'version', header: 'Version', width: '10ch', muted: true, render: (row: Host) => row.version || '—' },
          {
            key: 'last_seen_at',
            header: 'Last seen',
            width: '14ch',
            muted: true,
            render: (row: Host) => (row.last_seen_at ? stamp(row.last_seen_at) : '—'),
          },
          {
            key: 'actions',
            header: '',
            width: '12ch',
            align: 'right',
            render: (row: Host) => (
              <Button variant="destructive" disabled={remove.isPending} onClick={() => setRemoving(row)}>
                Remove
              </Button>
            ),
          },
        ]}
        rows={hosts.data?.hosts ?? []}
      />
      {remove.isError && <FailedBanner error={remove.error} remedy />}
      {removing && (
        <Dialog
          open
          title={`Remove ${removing.name}`}
          description="It can no longer reach the installation."
          onClose={() => setRemoving(null)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setRemoving(null)}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                onClick={() => {
                  setRemoving(null);
                  remove.mutate(removing.id);
                }}
              >
                Remove host
              </Button>
            </>
          }
        />
      )}
    </>
  );
}

function TokenList() {
  const queries = useQueryClient();
  const [creating, setCreating] = useState(false);
  const tokens = useQuery({
    queryKey: TOKENS,
    queryFn: () => api.get<{ tokens: JoinToken[] | null }>('/cluster/join-tokens'),
  });
  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/cluster/join-tokens/${encodeURIComponent(id)}`),
    onSuccess: () => queries.invalidateQueries({ queryKey: TOKENS }),
  });

  return (
    <SystemCard
      heading="Join tokens"
      action={
        <Button variant="primary" onClick={() => setCreating(true)}>
          Create join token
        </Button>
      }
    >
      {tokens.isError && <FailedBanner error={tokens.error} remedy />}
      <Table
        loading={tokens.isPending}
        skeletonRows={2}
        empty={<Quiet>None.</Quiet>}
        columns={[
          { key: 'roles', header: 'Roles', width: 'minmax(0,1fr)', render: (row: JoinToken) => rolesLabel(row.roles) },
          { key: 'uses', header: 'Uses', width: '10ch', muted: true, render: usesLabel },
          { key: 'expires_at', header: 'Expires', width: '14ch', muted: true, render: (row: JoinToken) => stamp(row.expires_at) },
          {
            key: 'revoked_at',
            header: 'Revoked',
            width: '14ch',
            muted: true,
            render: (row: JoinToken) => (row.revoked_at ? stamp(row.revoked_at) : '—'),
          },
          {
            key: 'actions',
            header: '',
            width: '12ch',
            align: 'right',
            render: (row: JoinToken) =>
              !row.revoked_at && (
                <Button variant="destructive" disabled={revoke.isPending} onClick={() => revoke.mutate(row.id)}>
                  Revoke
                </Button>
              ),
          },
        ]}
        rows={tokens.data?.tokens ?? []}
      />
      {revoke.isError && <FailedBanner error={revoke.error} remedy />}
      {creating && <CreateToken onClose={() => setCreating(false)} />}
    </SystemCard>
  );
}

/** The roles a token may be for. Default leaves the choice to the API: both
 *  while the cluster is small, then app (R-414). */
const ROLE_OPTIONS = [
  { value: '', label: 'Default' },
  { value: 'control,app', label: 'Control and app' },
  { value: 'control', label: 'Control' },
  { value: 'app', label: 'App' },
];

interface Minted {
  token: JoinToken;
  join: string;
}

function CreateToken({ onClose }: Readonly<{ onClose: () => void }>) {
  const queries = useQueryClient();
  const [roles, setRoles] = useState('');
  const [uses, setUses] = useState('1');
  const [hours, setHours] = useState('24');
  const [join, setJoin] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post<Minted>('/cluster/join-tokens', {
        roles: roles ? roles.split(',') : [],
        max_uses: Number(uses),
        expires_hours: Number(hours),
      }),
    onSuccess: (minted) => {
      setJoin(minted.join);
      void queries.invalidateQueries({ queryKey: TOKENS });
    },
  });

  // Shown once (R-415). The dialog does not close on success.
  if (join) {
    return (
      <Dialog
        open
        onClose={onClose}
        title="Join token"
        description="Shown once."
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <CopyField label="Token" value={join} />
      </Dialog>
    );
  }

  const whole = (v: string) => /^\d+$/.test(v) && Number(v) > 0;

  return (
    <Dialog
      open
      onClose={onClose}
      title="Create join token"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={create.isPending || !whole(uses) || !whole(hours)}
            onClick={() => create.mutate()}
          >
            {create.isPending ? 'Creating' : 'Create'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <Select label="Roles" value={roles} options={ROLE_OPTIONS} onChange={(e) => setRoles(e.target.value)} />
        <Input label="Uses" type="number" min={1} value={uses} onChange={(e) => setUses(e.target.value)} />
        <Input
          label="Expires in hours"
          type="number"
          min={1}
          value={hours}
          onChange={(e) => setHours(e.target.value)}
        />
        {create.isError && <FailedBanner error={create.error} remedy />}
      </div>
    </Dialog>
  );
}
