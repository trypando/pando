// The install's image registry (issue #72): where builds go when a runtime
// pulls rather than imports. GET /image-registry with install.view, PUT and
// DELETE with install.adapters.manage — the same calls the CLI and the MCP
// tools make (R-261).
//
// A setting made in the startup configuration (PANDO_REGISTRY_*) wins over
// what is saved here and is shown, not editable, with where it is set (R-271).
// The password is write-only: the server says whether one is set and never
// sends it (R-194).

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Input, Radio, Switch } from '@design';

import { api } from '@api/client';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { Quiet, refusal } from './Accounts';

interface Source {
  kind: 'env' | 'file' | 'default';
  name?: string;
  key?: string;
}

export interface RegistryView {
  configured: boolean;
  url: string;
  username: string;
  kind: 'basic' | 'ecr';
  layout: 'per_app' | 'single';
  insecure: boolean;
  always: boolean;
  password_set: boolean;
  fixed: Array<{ key: string; value?: unknown; source: Source }>;
  updated_by?: string;
  updated_at?: string;
}

type Draft = Partial<Pick<RegistryView, 'url' | 'username' | 'kind' | 'layout' | 'insecure' | 'always'>> & {
  password?: string;
};

/** Where a startup value is set, as a sentence an operator can act on. */
export function setAtStartup(src: Source): string {
  if (src.kind === 'env') {
    return `Set at startup by ${src.name}. Change it there and restart Pando, or remove it there to manage it here.`;
  }
  if (src.kind === 'file') {
    return `Set at startup in ${src.name}, at ${src.key}. Change it there and restart Pando, or remove it there to manage it here.`;
  }
  return 'Set at startup.';
}

export function ImageRegistry() {
  const queries = useQueryClient();
  const canManage = useInstallVerb(InstallVerb.AdaptersManage);
  const [draft, setDraft] = useState<Draft>({});
  const [removePassword, setRemovePassword] = useState(false);

  const registry = useQuery({
    queryKey: ['image-registry'],
    queryFn: () => api.get<RegistryView>('/image-registry'),
  });

  const done = () => {
    setDraft({});
    setRemovePassword(false);
    void queries.invalidateQueries({ queryKey: ['image-registry'] });
  };
  const save = useMutation({
    mutationFn: (body: Draft) => api.put<RegistryView>('/image-registry', body),
    onSuccess: done,
  });
  const clear = useMutation({
    mutationFn: () => api.del<void>('/image-registry'),
    onSuccess: done,
  });

  const view = registry.data;
  const fixed = new Map((view?.fixed ?? []).map((f) => [f.key, f.source]));
  const locked = (field: string) => !canManage || fixed.has(field);
  const helper = (field: string, otherwise?: string) => {
    const src = fixed.get(field);
    return src ? setAtStartup(src) : otherwise;
  };
  const current = { ...view, ...draft } as RegistryView;
  const changed = Object.keys(draft).length > 0 || removePassword;

  const body = (): Draft => {
    const out: Draft = { ...draft };
    if (removePassword) out.password = '';
    return out;
  };

  return (
    <section
      aria-labelledby="image-registry-heading"
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-4)',
        marginTop: 'var(--space-8)',
        paddingTop: 'var(--space-6)',
        borderTop: 'var(--border-width) solid var(--rule)',
        maxWidth: '64ch',
      }}
    >
      <div>
        <h2 id="image-registry-heading" style={{ font: 'var(--type-h3)', margin: '0 0 var(--space-1)' }}>
          Image registry
        </h2>
        <Quiet>
          Where Pando pushes built images when a runtime pulls them rather than taking them directly. A single
          Docker host needs none. Every Pando replica uses a change at its next push or pull.
        </Quiet>
      </div>

      {registry.isError && <Quiet>{refusal(registry.error)}</Quiet>}
      {(save.isError || clear.isError) && <Banner tone="failed">{refusal(save.error ?? clear.error)}</Banner>}

      {view && (
        <>
          <Input
            label="Registry address"
            mono
            value={current.url ?? ''}
            disabled={locked('url')}
            placeholder="https://registry.internal:5000"
            helper={helper('url', 'The registry, and a path to push under if you want one. Empty means none.')}
            onChange={(e) => setDraft({ ...draft, url: e.target.value })}
          />

          <fieldset style={{ border: 0, padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
              Credential
            </legend>
            {(
              [
                ['basic', 'Username and password', 'What the registry signs Pando in with.'],
                ['ecr', 'AWS access key', 'For Amazon ECR. Pando gets a registry password from the key before each push and pull.'],
              ] as const
            ).map(([value, label, description]) => (
              <Radio
                key={value}
                name="registry_kind"
                value={value}
                label={label}
                description={description}
                checked={(current.kind || 'basic') === value}
                disabled={locked('kind')}
                onChange={() => setDraft({ ...draft, kind: value })}
              />
            ))}
            {fixed.has('kind') && <Quiet>{helper('kind')}</Quiet>}
          </fieldset>

          <Input
            label={current.kind === 'ecr' ? 'Access key ID' : 'Username'}
            mono
            autoComplete="off"
            value={current.username ?? ''}
            disabled={locked('username')}
            helper={helper('username')}
            onChange={(e) => setDraft({ ...draft, username: e.target.value })}
          />

          <Input
            label={current.kind === 'ecr' ? 'Secret access key' : 'Password'}
            type="password"
            autoComplete="new-password"
            value={draft.password ?? ''}
            disabled={locked('password') || removePassword}
            placeholder={view.password_set ? 'Set. Type a new one to replace it.' : ''}
            helper={helper(
              'password',
              view.password_set
                ? 'Stored encrypted. Pando never shows it again.'
                : 'Stored encrypted, and never shown again.',
            )}
            onChange={(e) => setDraft({ ...draft, password: e.target.value })}
          />
          {view.password_set && !fixed.has('password') && canManage && (
            <Switch
              checked={removePassword}
              label="Remove the stored password"
              description="Pando then reaches the registry without signing in."
              onChange={(e) => setRemovePassword(e.target.checked)}
            />
          )}

          <fieldset style={{ border: 0, padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
              Where images go
            </legend>
            {(
              [
                ['per_app', 'A repository per app', 'Each app has its own, so a deleted app’s images are found and removed together.'],
                ['single', 'One repository for everything', 'For a registry where a repository has to exist before anything is pushed to it, such as ECR.'],
              ] as const
            ).map(([value, label, description]) => (
              <Radio
                key={value}
                name="registry_layout"
                value={value}
                label={label}
                description={description}
                checked={(current.layout || 'per_app') === value}
                disabled={locked('layout')}
                onChange={() => setDraft({ ...draft, layout: value })}
              />
            ))}
            {fixed.has('layout') && <Quiet>{helper('layout')}</Quiet>}
          </fieldset>

          <Switch
            checked={current.insecure ?? false}
            disabled={locked('insecure')}
            label="Allow plain HTTP"
            description={helper(
              'insecure',
              'Only for a registry on a private network. Every host that pulls must also list it as an insecure registry.',
            )}
            onChange={(e) => setDraft({ ...draft, insecure: e.target.checked })}
          />
          <Switch
            checked={current.always ?? false}
            disabled={locked('always')}
            label="Send every build through the registry"
            description={helper('always', 'Even on a runtime that can take a built image directly, such as Docker on one host.')}
            onChange={(e) => setDraft({ ...draft, always: e.target.checked })}
          />

          {canManage && (
            <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
              <Button variant="secondary" disabled={!changed || save.isPending} onClick={() => save.mutate(body())}>
                Save registry
              </Button>
              {changed && (
                <Button variant="ghost" onClick={() => { setDraft({}); setRemovePassword(false); }}>
                  Discard changes
                </Button>
              )}
              {!changed && (view.configured || view.password_set) && (
                <Button variant="ghost" disabled={clear.isPending} onClick={() => clear.mutate()}>
                  Remove saved registry
                </Button>
              )}
            </div>
          )}
          {save.isSuccess && !changed && <Quiet>Saved.</Quiet>}
        </>
      )}
    </section>
  );
}
