// An account's sign-in identities (O-1, issue #51): the external identities
// that sign in to it, and linking one by hand.
//
// Linking adds an alias. It never merges two accounts: an account's ID is what
// apps key their data on (R-054), so the one an identity leaves is kept,
// suspended, rather than folded into this one.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, Dialog, Input, Select, Tag } from '@design';

import { api } from '@api/client';
import type { Identity, ProviderView } from '@api/types.gen';
import { Quiet, refusal } from './Accounts';
import { LineSkeleton } from '../ui/Loading';

export function Identities({ userID, manage }: { userID: string; manage: boolean }) {
  const queries = useQueryClient();
  const ids = useQuery({
    queryKey: ['users', userID, 'identities'],
    queryFn: () => api.get<{ identities: Identity[] | null }>(`/users/${userID}/identities`),
  });
  const [linking, setLinking] = useState(false);
  const unlink = useMutation({
    mutationFn: (i: Identity) =>
      api.del(`/users/${userID}/identities?adapter_id=${encodeURIComponent(i.adapter_id)}&external_id=${encodeURIComponent(i.external_id)}`),
    onSuccess: () => void queries.invalidateQueries({ queryKey: ['users', userID, 'identities'] }),
  });

  const list = ids.data?.identities ?? [];

  return (
    <section>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 'var(--space-3)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>Sign-in identities</h4>
        {manage && (
          <Button variant="ghost" onClick={() => setLinking(true)}>
            Link identity
          </Button>
        )}
      </div>
      {ids.isPending ? (
        <LineSkeleton width="30ch" />
      ) : ids.isError ? (
        <Banner tone="failed">{refusal(ids.error)}</Banner>
      ) : list.length === 0 ? (
        <Quiet>No identity provider signs in to this account.</Quiet>
      ) : (
        <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          {list.map((i) => (
            <li
              key={`${i.adapter_id}/${i.external_id}`}
              style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2) var(--space-3)', font: 'var(--type-body-ui)' }}
            >
              <span>{i.adapter_name}</span>
              <Tag mono>{i.external_id}</Tag>
              {i.origin && <Tag>Where the account came from</Tag>}
              {i.scim && <Tag>SCIM</Tag>}
              <span style={{ color: 'var(--ink-secondary)', font: 'var(--type-caption)' }}>
                {i.last_sign_in_at ? `Last signed in ${new Date(i.last_sign_in_at).toLocaleString()}` : 'Not signed in yet'}
              </span>
              {manage && (
                <Button variant="ghost" disabled={unlink.isPending} onClick={() => unlink.mutate(i)}>
                  Unlink
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      {unlink.isError && <Banner tone="failed">{refusal(unlink.error)}</Banner>}
      {linking && <LinkDialog userID={userID} onClose={() => setLinking(false)} />}
    </section>
  );
}

function LinkDialog({ userID, onClose }: { userID: string; onClose: () => void }) {
  const queries = useQueryClient();
  const providers = useQuery({
    queryKey: ['identity-providers'],
    queryFn: () => api.get<{ providers: ProviderView[] | null }>('/identity-providers'),
  });
  const external = (providers.data?.providers ?? []).filter((p) => p.kind !== 'local');
  const [adapterID, setAdapterID] = useState('');
  const [externalID, setExternalID] = useState('');
  const [replace, setReplace] = useState(false);
  const chosen = adapterID || external[0]?.id || '';

  const link = useMutation({
    mutationFn: () =>
      api.post(`/users/${userID}/identities`, { adapter_id: chosen, external_id: externalID.trim(), replace_account: replace }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['users'] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      title="Link an identity"
      description="Whoever signs in with this identity will sign in to this account. Nothing is merged."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!chosen || !externalID.trim() || link.isPending} onClick={() => link.mutate()}>
            {link.isPending ? 'Linking' : 'Link identity'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {external.length === 0 && providers.isSuccess ? (
          <Quiet>This installation has no identity provider yet. Add one on the Sign-in screen.</Quiet>
        ) : (
          <>
            <Select
              label="Identity provider"
              value={chosen}
              options={external.map((p) => ({ value: p.id, label: p.name }))}
              onChange={(e) => setAdapterID(e.target.value)}
            />
            <Input
              label="The provider's ID for this person"
              mono
              value={externalID}
              helper="A test sign-in shows it as the identity. When someone without an account is refused, the message tells them theirs."
              onChange={(e) => setExternalID(e.target.value)}
            />
            <Checkbox
              label="If it already signs in to another account, move it here"
              description="The other account is kept, suspended, as an alias of this one. It is never deleted, because apps may hold data under its ID."
              checked={replace}
              onChange={(e) => setReplace(e.target.checked)}
            />
          </>
        )}
        {link.isError && <Banner tone="failed">{refusal(link.error)}</Banner>}
      </div>
    </Dialog>
  );
}
