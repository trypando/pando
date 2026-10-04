// Sign-in: the identity providers people sign in with (issue #51; R-043,
// R-045, R-047, R-048).
//
// One card per provider, local accounts first. Each says whether it is on,
// how long a sign-in lasts and how soon removing someone takes effect — the
// revocation window stated rather than implied (design 06 §3.1) — and, for an
// external provider, exactly what to register with it. A provider is added
// off, tested with a real sign-in that reports the claims it sent and what
// Pando would do with them, and only then turned on.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Card, Checkbox, CodeBlock, Dialog, Input, Select, StatusIndicator, Tag } from '@design';

import { api, base } from '@api/client';
import type { ProviderView, TestReport } from '@api/types.gen';
import { Quiet, Screen, refusal } from './Accounts';
import { FieldInput } from './AdapterDialog';
import { isShown } from './adapters';
import { LineSkeleton, Loading } from '../ui/Loading';
import { Term } from '../ui/Term';
import {
  groupsText,
  outcomeText,
  presetValues,
  problems,
  providerRequest,
  revocationText,
  storedValues,
} from './signin';
import type { Outcome, ProviderKind, Values } from './signin';

interface Listing {
  providers: ProviderView[] | null;
  kinds: ProviderKind[] | null;
}

const KIND_LABEL: Record<string, string> = { local: 'Username and password', oidc: 'OpenID Connect', saml: 'SAML 2.0' };

export function SignIn({
  canEdit,
  canManagePolicy = false,
  query,
  onClearTest,
}: {
  canEdit: boolean;
  /** Password sign-in is host policy (disable_password_sign_in), so turning
   *  local accounts off takes install.policy.manage. */
  canManagePolicy?: boolean;
  /** provider= and test= when a test sign-in has just come back. */
  query?: string;
  onClearTest: () => void;
}) {
  const listing = useQuery({
    queryKey: ['identity-providers'],
    queryFn: () => api.get<Listing>('/identity-providers'),
  });
  const policy = useQuery({
    queryKey: ['policy'],
    queryFn: () => api.get<{ disable_password_sign_in?: boolean; disable_jit_provisioning?: boolean }>('/policy'),
  });
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<ProviderView | null>(null);

  const params = new URLSearchParams(query ?? '');
  const testProvider = params.get('provider');
  const testID = params.get('test');

  const kinds = listing.data?.kinds ?? [];
  const providers = listing.data?.providers ?? [];

  return (
    <Screen
      heading="Sign-in"
      action={
        canEdit && (
          <Button variant="primary" onClick={() => setAdding(true)}>
            Add identity provider
          </Button>
        )
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)', maxWidth: '72ch' }}>
        <Quiet>
          People sign in with a Pando username and password, or through your organization&rsquo;s identity provider.
          A provider says who someone is and which of its groups they are in; what they can do here is always
          decided in Pando.
        </Quiet>


        {listing.isPending ? (
          <Loading gap="var(--space-4)">
            <LineSkeleton width="40ch" />
            <LineSkeleton width="32ch" />
          </Loading>
        ) : listing.isError ? (
          <Banner tone="failed">{refusal(listing.error)}</Banner>
        ) : (
          providers.map((p) => (
            <ProviderCard
              key={p.id}
              provider={p}
              canEdit={canEdit}
              canManagePolicy={canManagePolicy}
              jitOff={policy.data?.disable_jit_provisioning ?? false}
              onEdit={() => setEditing(p)}
            />
          ))
        )}
      </div>

      {adding && <ProviderDialog kinds={kinds} onClose={() => setAdding(false)} />}
      {editing && (
        <ProviderDialog kinds={kinds} existing={editing} onClose={() => setEditing(null)} />
      )}
      {testProvider && testID && (
        <TestResult
          providerID={testProvider}
          testID={testID}
          name={providers.find((p) => p.id === testProvider)?.name ?? 'the provider'}
          onClose={onClearTest}
        />
      )}
    </Screen>
  );
}

function ProviderCard({
  provider: p,
  canEdit,
  canManagePolicy,
  jitOff,
  onEdit,
}: {
  provider: ProviderView;
  canEdit: boolean;
  canManagePolicy: boolean;
  jitOff: boolean;
  onEdit: () => void;
}) {
  const queries = useQueryClient();
  const refresh = () => void queries.invalidateQueries({ queryKey: ['identity-providers'] });
  const [token, setToken] = useState<{ token: string; scim_base_url: string } | null>(null);
  const [removing, setRemoving] = useState(false);
  const external = p.kind !== 'local';

  const toggle = useMutation({
    mutationFn: () => api.patch(`/identity-providers/${p.id}`, { enabled: !p.enabled }),
    onSuccess: refresh,
  });
  // Local accounts are turned off by host policy, which refuses it while no
  // identity provider is on — so this cannot empty the sign-in page.
  const passwords = useMutation({
    mutationFn: async () => {
      const doc = await api.get<Record<string, unknown>>('/policy');
      return api.put('/policy', { ...doc, disable_password_sign_in: p.enabled });
    },
    onSettled: () => {
      refresh();
      void queries.invalidateQueries({ queryKey: ['policy'] });
      void queries.invalidateQueries({ queryKey: ['sign-in-options'] });
    },
  });
  const check = useMutation({
    mutationFn: () => api.post<{ ok: boolean; message?: string; remedy?: string }>(`/identity-providers/${p.id}/check`),
  });
  const scim = useMutation({
    mutationFn: () => api.post<{ token: string; scim_base_url: string }>(`/identity-providers/${p.id}/scim-token`),
    onSuccess: (t) => {
      setToken(t);
      refresh();
    },
  });
  const scimOff = useMutation({
    mutationFn: () => api.del(`/identity-providers/${p.id}/scim-token`),
    onSuccess: refresh,
  });
  const remove = useMutation({
    mutationFn: () => api.del(`/identity-providers/${p.id}`),
    onSuccess: refresh,
  });
  const failure = toggle.error ?? passwords.error ?? scim.error ?? scimOff.error ?? remove.error;

  const status = p.problem ? (
    <StatusIndicator status="failed" label="Not usable" title={p.problem} />
  ) : p.enabled ? (
    <StatusIndicator status="running" label="On" />
  ) : (
    <StatusIndicator status="stopped" label="Off" />
  );

  return (
    <Card padding="lg">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
          <h2 style={{ font: 'var(--type-h4)', color: 'var(--ink)', margin: 0 }}>{p.name}</h2>
          <Tag>{KIND_LABEL[p.kind] ?? p.kind}</Tag>
          {status}
        </div>

        {p.problem && <Banner tone="failed">{p.problem}</Banner>}
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          {!external && !p.enabled
            ? 'Password sign-in is off: the sign-in page shows only identity providers, and a username and password are refused. If no provider works, whoever runs this installation can turn it back on with pando admin enable-password-sign-in.'
            : revocationText(p.kind, p.revocation)}
        </p>

        {external && (
          <>
            <dl style={{ display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: 'var(--space-2) var(--space-4)', margin: 0 }}>
              <Term label="New people">
                {p.jit_provisioning && !jitOff
                  ? 'Get an account at their first sign-in, with no access until it is given.'
                  : p.jit_provisioning
                    ? 'Would get an account at first sign-in, but Policy turns that off.'
                    : 'Need an account first: from SCIM, or linked by an administrator.'}
              </Term>
              <Term label="Existing accounts">
                {p.link_by_email
                  ? 'Linked at first sign-in by an email address the provider has verified.'
                  : 'Linked only by an administrator, on the account’s page.'}
              </Term>
              <Term label="SCIM">
                {p.scim_enabled
                  ? `On. The provider matches people by ${p.scim_identity_attribute}.`
                  : 'Off. Group memberships come from each sign-in.'}
              </Term>
            </dl>

            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
              <p style={{ font: 'var(--type-label)', color: 'var(--ink)', margin: 0 }}>Register with the provider</p>
              <CodeBlock
                copyable
                lines={[
                  p.kind === 'saml' ? `ACS URL: ${p.callback_url}` : `Redirect URI: ${p.callback_url}`,
                  ...(p.kind === 'saml' ? [`Entity ID and metadata: ${p.entity_id}`] : []),
                  ...(p.scim_enabled ? [`SCIM base URL: ${p.scim_base_url}`] : []),
                ]}
              />
            </div>
          </>
        )}

        {check.data && (
          <Banner tone={check.data.ok ? 'info' : 'failed'}>
            {check.data.ok ? `${p.name} answers.` : `${check.data.message ?? ''} ${check.data.remedy ?? ''}`.trim()}
          </Banner>
        )}
        {failure && <Banner tone="failed">{refusal(failure)}</Banner>}

        {canManagePolicy && !external && (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            <Button onClick={() => passwords.mutate()} disabled={passwords.isPending}>
              {p.enabled ? 'Turn off password sign-in' : 'Turn on password sign-in'}
            </Button>
          </div>
        )}

        {canEdit && external && (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            <Button onClick={() => window.location.assign(`${base}/identity-providers/${p.id}/test`)}>
              Test sign-in
            </Button>
            <Button onClick={() => toggle.mutate()} disabled={toggle.isPending}>
              {p.enabled ? 'Turn off' : 'Turn on'}
            </Button>
            <Button variant="ghost" onClick={onEdit}>
              Edit
            </Button>
            <Button variant="ghost" onClick={() => check.mutate()} disabled={check.isPending}>
              {check.isPending ? 'Checking' : 'Check connection'}
            </Button>
            <Button variant="ghost" onClick={() => scim.mutate()} disabled={scim.isPending}>
              {p.scim_enabled ? 'Replace SCIM token' : 'Turn on SCIM'}
            </Button>
            {p.scim_enabled && (
              <Button variant="ghost" onClick={() => scimOff.mutate()} disabled={scimOff.isPending}>
                Turn off SCIM
              </Button>
            )}
            <Button variant="ghost" onClick={() => setRemoving(true)}>
              Remove
            </Button>
          </div>
        )}
      </div>

      {token && (
        <Dialog
          open
          title="SCIM token"
          description="Give the provider this token and base URL. Pando shows the token once; replacing it retires this one."
          onClose={() => setToken(null)}
          footer={
            <Button variant="primary" onClick={() => setToken(null)}>
              Done
            </Button>
          }
        >
          <CodeBlock copyable lines={[`Base URL: ${token.scim_base_url}`, `Token: ${token.token}`]} />
        </Dialog>
      )}
      {removing && (
        <Dialog
          open
          title={`Remove ${p.name}`}
          description="Only a provider nobody has signed in through can be removed. One that has is turned off instead, so its identities are never reused."
          onClose={() => setRemoving(false)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setRemoving(false)}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                onClick={() => {
                  setRemoving(false);
                  remove.mutate();
                }}
              >
                Remove provider
              </Button>
            </>
          }
        />
      )}
    </Card>
  );
}

function ProviderDialog({
  kinds,
  existing,
  onClose,
}: {
  kinds: ProviderKind[];
  existing?: ProviderView;
  onClose: () => void;
}) {
  const queries = useQueryClient();
  const [kindName, setKindName] = useState(existing?.kind ?? kinds[0]?.kind ?? 'oidc');
  const kind = kinds.find((k) => k.kind === kindName);
  const [presetID, setPresetID] = useState('');
  const preset = kind?.presets?.find((p) => p.id === presetID);
  const [name, setName] = useState(existing?.name ?? '');
  const [values, setValues] = useState<Values>(existing ? storedValues(existing.config) : {});
  const [jit, setJIT] = useState(existing?.jit_provisioning ?? false);
  const [linkByEmail, setLinkByEmail] = useState(existing?.link_by_email ?? false);
  const [tried, setTried] = useState(false);

  const stored = existing?.credentials ?? [];
  const found = kind ? problems(kind, name, values, stored) : {};
  const shown = (key: string) => (tried ? found[key] : undefined);

  const save = useMutation({
    mutationFn: () => {
      const body = providerRequest(kind!, name, values, { jit, linkByEmail }, !existing);
      return existing ? api.patch(`/identity-providers/${existing.id}`, body) : api.post('/identity-providers', body);
    },
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['identity-providers'] });
      onClose();
    },
  });

  const choosePreset = (id: string) => {
    setPresetID(id);
    const p = kind?.presets?.find((x) => x.id === id);
    if (kind && p) {
      setValues(presetValues(kind, p));
      if (!name) setName(p.label);
    }
  };

  return (
    <Dialog
      open
      width={640}
      title={existing ? `Edit ${existing.name}` : 'Add identity provider'}
      description={
        existing
          ? 'Changes apply at the next sign-in. Saving replaces the settings with these.'
          : 'A new provider starts off. Test it with a real sign-in, then turn it on.'
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!kind || save.isPending}
            onClick={() => {
              setTried(true);
              if (Object.keys(found).length === 0) save.mutate();
            }}
          >
            {existing ? (save.isPending ? 'Saving' : 'Save provider') : save.isPending ? 'Adding' : 'Add provider'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {!existing && (
          <Select
            label="Protocol"
            value={kindName}
            options={kinds.map((k) => ({ value: k.kind, label: k.name }))}
            onChange={(e) => {
              setKindName(e.target.value);
              setPresetID('');
              setValues({});
            }}
          />
        )}
        {!existing && (kind?.presets?.length ?? 0) > 0 && (
          <Select
            label="Provider"
            value={presetID}
            helper="Fills in the settings a known provider needs. You can change any of them."
            options={[
              { value: '', label: 'Another provider' },
              ...(kind?.presets ?? []).map((p) => ({ value: p.id, label: p.label })),
            ]}
            onChange={(e) => choosePreset(e.target.value)}
          />
        )}
        {preset?.help && <Banner tone="info">{preset.help}</Banner>}
        {existing?.callback_url && (
          <CodeBlock
            copyable
            lines={[
              existing.kind === 'saml' ? `ACS URL: ${existing.callback_url}` : `Redirect URI: ${existing.callback_url}`,
              ...(existing.kind === 'saml' ? [`Entity ID: ${existing.entity_id}`] : []),
            ]}
          />
        )}
        <Input
          label="Name (required)"
          value={name}
          helper="What the sign-in page calls it, such as Okta."
          error={shown('name')}
          onChange={(e) => setName(e.target.value)}
        />
        {kind &&
          (kind.fields ?? [])
            .filter((f) => isShown(kind, f, values))
            .map((f) => (
              <FieldInput
                key={f.key}
                field={f}
                value={values[f.key]}
                stored={existing ? stored.includes(f.key) : undefined}
                error={shown(f.key)}
                onChange={(v) => setValues({ ...values, [f.key]: v })}
              />
            ))}
        <Checkbox
          label="Create an account at someone's first sign-in"
          description="Off by default. The account has no access until it is given some, or is in a group that has some. Policy can turn this off everywhere."
          checked={jit}
          onChange={(e) => setJIT(e.target.checked)}
        />
        <Checkbox
          label="Link to an existing account by verified email"
          description="Only when the provider vouches for the address. Microsoft Entra ID does not, so its accounts are linked by hand."
          checked={linkByEmail}
          onChange={(e) => setLinkByEmail(e.target.checked)}
        />
        {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}

/** A test sign-in's report: what the provider sent, and what a real sign-in
 *  would have done with it. */
function TestResult({
  providerID,
  testID,
  name,
  onClose,
}: {
  providerID: string;
  testID: string;
  name: string;
  onClose: () => void;
}) {
  const report = useQuery({
    queryKey: ['identity-provider-test', providerID, testID],
    queryFn: () => api.get<TestReport>(`/identity-providers/${providerID}/tests/${testID}`),
    retry: false,
  });
  const r = report.data;
  const outcome = r?.outcome as Outcome | undefined;
  const groups = r?.subject?.groups ?? [];

  return (
    <Dialog
      open
      width={720}
      title={`Test sign-in with ${name}`}
      description="Nobody was signed in. This is what the provider sent, and what a real sign-in would do."
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {report.isPending && <LineSkeleton width="40ch" />}
        {report.isError && <Banner tone="failed">{refusal(report.error)}</Banner>}
        {r?.error && (
          <Banner tone="failed">{r.error.remedy ? `${r.error.message} ${r.error.remedy}` : r.error.message}</Banner>
        )}
        {r?.subject && (
          <>
            <StatusIndicator status={r.ok ? 'running' : 'failed'} label={r.ok ? 'Signed in at the provider' : 'Refused'} />
            <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>{outcomeText(outcome)}</p>
            <dl style={{ display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: 'var(--space-2) var(--space-4)', margin: 0 }}>
              <Term label="Identity">{r.subject.external_id}</Term>
              <Term label="Username">{r.subject.username || 'Not sent'}</Term>
              <Term label="Email">
                {r.subject.email
                  ? `${r.subject.email} (${r.subject.email_verified ? 'verified' : 'not verified'})`
                  : 'Not sent'}
              </Term>
              <Term label="Name">{r.subject.display_name || 'Not sent'}</Term>
              <Term label="Groups">{groupsText(outcome, groups)}</Term>
            </dl>
            <CodeBlock
              title="Everything the provider sent"
              copyable
              lines={Object.entries(r.attributes ?? {}).map(([k, v]) => `${k}: ${(v ?? []).join(', ')}`)}
            />
          </>
        )}
      </div>
    </Dialog>
  );
}
