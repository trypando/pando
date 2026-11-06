// An app's webhook routes (R-425): the paths a verified delivery reaches
// without a Pando credential. A route is in the spec, so a change waits for the
// next deploy; its secret is not, so replacing or removing one is immediate.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Checkbox, Dialog, Input, Select, Tag } from '@design';

import { api } from '@api/client';
import type { App, WebhookRoute } from '@api/types.gen';
import { Quiet as Failure, messageOf } from '../install/Accounts';
import { CardHead, Quiet } from './settingsParts';
import { AppVerb, useCan } from './verbs';

interface RouteView extends WebhookRoute {
  secret_set: boolean;
  deployed: boolean;
}

interface Draft {
  name: string;
  method: string;
  path: string;
  kind: string;
  header: string;
  prefix: string;
  secret: string;
  allow_repeats: boolean;
}

const emptyDraft: Draft = {
  name: '',
  method: 'POST',
  path: '',
  kind: 'hmac_sha256',
  header: '',
  prefix: '',
  secret: '',
  allow_repeats: false,
};

/** The draft as the API takes it, with only what its kind of verification uses. */
export function routeBody(d: Draft) {
  const hmac = d.kind === 'hmac_sha256';
  return {
    name: d.name.trim(),
    method: d.method,
    path: d.path.trim(),
    verification: { kind: d.kind, header: d.header.trim(), prefix: hmac ? d.prefix.trim() : '' },
    allow_repeats: hmac && d.allow_repeats,
    secret: d.secret,
  };
}

const routesKey = (appID: string) => ['apps', appID, 'webhook-routes'];

export function WebhookRoutesCard({ app }: Readonly<{ app: App }>) {
  const canEdit = useCan(AppVerb.SpecEdit);
  const canWriteSecrets = useCan(AppVerb.SecretsWrite);
  const [adding, setAdding] = useState(false);
  const [replacing, setReplacing] = useState<string | null>(null);
  const [shown, setShown] = useState<{ name: string; secret: string } | null>(null);

  const routes = useQuery({
    queryKey: routesKey(app.id),
    queryFn: () => api.get<{ routes: RouteView[] }>(`/apps/${app.id}/webhook-routes`),
  });
  const list = routes.data?.routes ?? [];

  return (
    <Card tone="paper" padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <CardHead
          title="Webhook routes"
          action={
            canEdit &&
            canWriteSecrets && (
              <Button variant="secondary" onClick={() => setAdding(true)}>
                Add route
              </Button>
            )
          }
        />
        {routes.isError && <Failure>{messageOf(routes.error)}</Failure>}
        {routes.isSuccess && list.length === 0 && <Quiet>No webhook routes</Quiet>}
        {list.map((route) => (
          <RouteRow
            key={route.name}
            appID={app.id}
            route={route}
            canEdit={canEdit}
            canWriteSecrets={canWriteSecrets}
            onReplace={() => setReplacing(route.name)}
          />
        ))}
        {shown && (
          <Input label={`Secret for ${shown.name}`} mono readOnly value={shown.secret} helper="Shown once." />
        )}
      </div>
      {adding && (
        <AddRoute
          appID={app.id}
          onClose={() => setAdding(false)}
          onSecret={(name, secret) => setShown({ name, secret })}
        />
      )}
      {replacing && (
        <ReplaceSecret
          appID={app.id}
          name={replacing}
          onClose={() => setReplacing(null)}
          onSecret={(name, secret) => setShown({ name, secret })}
        />
      )}
    </Card>
  );
}

function RouteRow({
  appID,
  route,
  canEdit,
  canWriteSecrets,
  onReplace,
}: Readonly<{
  appID: string;
  route: RouteView;
  canEdit: boolean;
  canWriteSecrets: boolean;
  onReplace: () => void;
}>) {
  const queries = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.del(`/apps/${appID}/webhook-routes/${route.name}`),
    onSuccess: () => void queries.invalidateQueries({ queryKey: routesKey(appID) }),
  });
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2)' }}>
        <span style={{ font: 'var(--type-label)' }}>{route.name}</span>
        <Tag mono>
          {route.method} {route.path}
        </Tag>
        {!route.deployed && <Tag>Next deploy</Tag>}
        {!route.secret_set && <Tag>No secret</Tag>}
        <span style={{ flex: 1 }} />
        {canWriteSecrets && (
          <Button variant="ghost" onClick={onReplace}>
            Replace secret
          </Button>
        )}
        {canEdit && (
          <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate()}>
            Remove
          </Button>
        )}
      </div>
      {remove.isError && <Failure>{messageOf(remove.error)}</Failure>}
    </div>
  );
}

function AddRoute({
  appID,
  onClose,
  onSecret,
}: Readonly<{ appID: string; onClose: () => void; onSecret: (name: string, secret: string) => void }>) {
  const queries = useQueryClient();
  const [draft, setDraft] = useState(emptyDraft);
  const set = (patch: Partial<Draft>) => setDraft({ ...draft, ...patch });
  const hmac = draft.kind === 'hmac_sha256';

  const save = useMutation({
    mutationFn: () => api.post<{ secret?: string }>(`/apps/${appID}/webhook-routes`, routeBody(draft)),
    onSuccess: (out) => {
      void queries.invalidateQueries({ queryKey: routesKey(appID) });
      if (out.secret) onSecret(draft.name.trim(), out.secret);
      onClose();
    },
  });
  const complete = draft.name.trim() !== '' && draft.path.trim() !== '' && draft.header.trim() !== '';

  return (
    <Dialog
      open
      onClose={onClose}
      title="Add webhook route"
      description="Takes effect at the next deploy."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={save.isPending || !complete} onClick={() => save.mutate()}>
            {save.isPending ? 'Adding' : 'Add route'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <Input label="Name" mono value={draft.name} onChange={(e) => set({ name: e.target.value })} />
        <Select
          label="Method"
          value={draft.method}
          options={['POST', 'PUT', 'PATCH'].map((m) => ({ value: m, label: m }))}
          onChange={(e) => set({ method: e.target.value })}
        />
        <Input
          label="Path"
          mono
          placeholder="/api/v1/webhook/noodle"
          value={draft.path}
          onChange={(e) => set({ path: e.target.value })}
        />
        <Select
          label="Verification"
          value={draft.kind}
          options={[
            { value: 'hmac_sha256', label: 'HMAC-SHA256 signature' },
            { value: 'header', label: 'Secret in a header' },
          ]}
          onChange={(e) => set({ kind: e.target.value })}
        />
        <Input
          label="Header"
          mono
          placeholder={hmac ? 'X-Hub-Signature-256' : 'X-Webhook-Token'}
          value={draft.header}
          onChange={(e) => set({ header: e.target.value })}
        />
        {hmac && (
          <Input
            label="Signature prefix"
            mono
            placeholder="sha256="
            value={draft.prefix}
            onChange={(e) => set({ prefix: e.target.value })}
          />
        )}
        <Input
          label="Secret"
          type="password"
          autoComplete="new-password"
          helper="Empty generates one."
          value={draft.secret}
          onChange={(e) => set({ secret: e.target.value })}
        />
        {hmac && (
          <Checkbox
            label="Allow repeats"
            description="Forwards a signature already accepted."
            checked={draft.allow_repeats}
            onChange={(e) => set({ allow_repeats: e.target.checked })}
          />
        )}
      </div>
      {save.isError && <Failure>{messageOf(save.error)}</Failure>}
    </Dialog>
  );
}

function ReplaceSecret({
  appID,
  name,
  onClose,
  onSecret,
}: Readonly<{
  appID: string;
  name: string;
  onClose: () => void;
  onSecret: (name: string, secret: string) => void;
}>) {
  const queries = useQueryClient();
  const [secret, setSecret] = useState('');
  const save = useMutation({
    mutationFn: () =>
      api.post<{ secret?: string } | undefined>(
        `/apps/${appID}/webhook-routes/${name}/secret`,
        secret === '' ? undefined : { secret },
      ),
    onSuccess: (out) => {
      void queries.invalidateQueries({ queryKey: routesKey(appID) });
      if (out?.secret) onSecret(name, out.secret);
      onClose();
    },
  });
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Replace secret for ${name}`}
      description="The old secret stops working at once."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? 'Replacing' : 'Replace secret'}
          </Button>
        </>
      }
    >
      <Input
        label="Secret"
        type="password"
        autoComplete="new-password"
        helper="Empty generates one."
        value={secret}
        onChange={(e) => setSecret(e.target.value)}
      />
      {save.isError && <Failure>{messageOf(save.error)}</Failure>}
    </Dialog>
  );
}
