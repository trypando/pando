// The credential a private image is pulled with (issue #41).
//
// It belongs to the app (O-3) and is never given to it: Pando hands it to the
// runtime for the pull and nowhere else, and nothing reads the secret half
// back. So the settings section says which kind is set and offers to replace
// or remove it, and never shows a password field filled in.
//
// Two kinds, because two kinds cover the registries people use: a username
// and a token (Docker Hub, GitHub, GitLab, Quay, Harbor), and AWS access keys
// for ECR, whose own passwords expire after twelve hours — so Pando keeps the
// key and mints one for every pull.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Dialog, Input, Select } from '@design';

import { api } from '@api/client';
import { Quiet, messageOf } from '../install/Accounts';
import { AppVerb, useCan } from './verbs';

export interface CredentialDraft {
  kind: 'basic' | 'ecr';
  username: string;
  password: string;
  access_key_id: string;
  secret_access_key: string;
  region: string;
}

export const emptyCredential: CredentialDraft = {
  kind: 'basic',
  username: '',
  password: '',
  access_key_id: '',
  secret_access_key: '',
  region: '',
};

/** Whether the draft has everything its kind needs. */
export function credentialComplete(c: CredentialDraft): boolean {
  return c.kind === 'basic'
    ? c.username.trim() !== '' && c.password !== ''
    : c.access_key_id.trim() !== '' && c.secret_access_key !== '';
}

/** The draft as the API takes it, with only its own kind's fields. */
export function credentialBody(c: CredentialDraft): Record<string, string> {
  if (c.kind === 'basic') return { kind: 'basic', username: c.username.trim(), password: c.password };
  const body: Record<string, string> = {
    kind: 'ecr',
    access_key_id: c.access_key_id.trim(),
    secret_access_key: c.secret_access_key,
  };
  if (c.region.trim() !== '') body.region = c.region.trim();
  return body;
}

/** The fields for one credential. */
export function CredentialFields({
  value,
  onChange,
}: {
  value: CredentialDraft;
  onChange: (next: CredentialDraft) => void;
}) {
  const set = (patch: Partial<CredentialDraft>) => onChange({ ...value, ...patch });
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <Select
        label="How Pando signs in to the registry"
        value={value.kind}
        options={[
          { value: 'basic', label: 'A username and access token' },
          { value: 'ecr', label: 'AWS access keys, for Amazon ECR' },
        ]}
        onChange={(e) => set({ kind: e.target.value as CredentialDraft['kind'] })}
      />
      {value.kind === 'basic' ? (
        <>
          <Input
            label="Username"
            autoComplete="off"
            value={value.username}
            onChange={(e) => set({ username: e.target.value })}
          />
          <Input
            label="Access token or password"
            type="password"
            autoComplete="new-password"
            value={value.password}
            helper="A token that can read the image. On GitHub, a personal access token with the read:packages scope."
            onChange={(e) => set({ password: e.target.value })}
          />
        </>
      ) : (
        <>
          <Input
            label="Access key ID"
            mono
            autoComplete="off"
            value={value.access_key_id}
            onChange={(e) => set({ access_key_id: e.target.value })}
          />
          <Input
            label="Secret access key"
            type="password"
            autoComplete="new-password"
            value={value.secret_access_key}
            helper="Its policy needs ecr:GetAuthorizationToken, and ecr:BatchGetImage and ecr:GetDownloadUrlForLayer on the repository."
            onChange={(e) => set({ secret_access_key: e.target.value })}
          />
          <Input
            label="Region"
            mono
            value={value.region}
            placeholder="us-east-1"
            helper="Leave empty to read it from the image's address."
            onChange={(e) => set({ region: e.target.value })}
          />
        </>
      )}
    </div>
  );
}

interface Summary {
  set: boolean;
  credential?: { kind: 'basic' | 'ecr'; username?: string; access_key_id?: string; region?: string };
}

/** The registry credential section of an image app's settings. */
export function RegistryCredential({ appID }: { appID: string }) {
  const queries = useQueryClient();
  const canWrite = useCan(AppVerb.SecretsWrite);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<CredentialDraft>(emptyCredential);

  const key = ['apps', appID, 'registry-credential'];
  const summary = useQuery({
    queryKey: key,
    queryFn: () => api.get<Summary>(`/apps/${appID}/registry-credential`),
  });

  const save = useMutation({
    mutationFn: () => api.put(`/apps/${appID}/registry-credential`, credentialBody(draft)),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: key });
      setEditing(false);
      setDraft(emptyCredential);
    },
  });
  const remove = useMutation({
    mutationFn: () => api.del(`/apps/${appID}/registry-credential`),
    onSuccess: () => void queries.invalidateQueries({ queryKey: key }),
  });

  const c = summary.data?.credential;
  const described = !summary.data?.set
    ? 'None. Pando pulls the image without signing in.'
    : c?.kind === 'ecr'
      ? `AWS access key ${c.access_key_id ?? ''}${c.region ? ` in ${c.region}` : ''}. Pando asks ECR for a fresh password before every pull.`
      : `Signed in as ${c?.username ?? ''}.`;

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Registry credential</h4>
      <Quiet>
        How Pando signs in to pull this app&rsquo;s image when it is private. It belongs to the app, and
        the app is never given it.
      </Quiet>
      {summary.isError ? (
        <Quiet>{messageOf(summary.error)}</Quiet>
      ) : (
        <p style={{ font: 'var(--type-body-ui)', margin: 'var(--space-3) 0 0' }}>{described}</p>
      )}
      {remove.isError && <Quiet>{messageOf(remove.error)}</Quiet>}

      {canWrite && (
        <div style={{ display: 'flex', gap: 'var(--space-3)', marginTop: 'var(--space-4)' }}>
          <Button variant="secondary" onClick={() => setEditing(true)}>
            {summary.data?.set ? 'Replace' : 'Add a credential'}
          </Button>
          {summary.data?.set && (
            <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate()}>
              Remove
            </Button>
          )}
        </div>
      )}

      {editing && (
        <Dialog
          open
          onClose={() => setEditing(false)}
          title="Registry credential"
          description="Replaces any credential the app had. The next deploy pulls the image with it."
          footer={
            <>
              <Button variant="ghost" onClick={() => setEditing(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                disabled={save.isPending || !credentialComplete(draft)}
                onClick={() => save.mutate()}
              >
                {save.isPending ? 'Saving' : 'Save'}
              </Button>
            </>
          }
        >
          <CredentialFields value={draft} onChange={setDraft} />
          {save.isError && <Quiet>{messageOf(save.error)}</Quiet>}
        </Dialog>
      )}
    </section>
  );
}
