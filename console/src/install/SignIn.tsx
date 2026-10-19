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
import { AdapterGuide } from './AdapterGuide';
import { isShown, kindKey } from './adapters';
import { LineSkeleton, Loading } from '../ui/Loading';
import { Term } from '../ui/Term';
import {
  groupsText,
  newProviderID,
  outcomeText,
  presetValues,
  problems,
  providerRequest,
  revocationText,
  storedValues,
} from './signInForm';
import type { Outcome, ProviderKind, Values } from './signInForm';

interface Listing {
  providers: ProviderView[] | null;
  kinds: ProviderKind[] | null;
  /** A new provider's addresses, with {id} for the ID it is created with. */
  endpoints?: Endpoints;
}

/** A new provider's callback URL and entity ID, with {id} for its ID. */
interface Endpoints {
  callback_url: string;
  entity_id: string;
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
          People sign in with a Pando password or through an identity provider. Permissions are always set in Pando.
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

      {adding && <ProviderDialog kinds={kinds} endpoints={listing.data?.endpoints} onClose={() => setAdding(false)} />}
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
            ? 'Password sign-in is off. If no identity provider works, turn it back on with pando admin enable-password-sign-in on the host.'
            : revocationText(p.kind, p.revocation)}
        </p>

        {external && (
          <>
            <dl style={{ display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: 'var(--space-2) var(--space-4)', margin: 0 }}>
              <Term label="Provisioning">
                {p.jit_provisioning && !jitOff
                  ? 'Just-in-time. New accounts start with no access.'
                  : p.jit_provisioning
                    ? 'Just-in-time, turned off in Policy.'
                    : 'SCIM or an administrator only.'}
              </Term>
              <Term label="Account linking">
                {p.link_by_email
                  ? 'By verified email, at first sign-in.'
                  : 'By an administrator, on the account’s page.'}
              </Term>
              <Term label="SCIM">
                {p.scim_enabled
                  ? `On. Matches people by ${p.scim_identity_attribute}.`
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
          description="Give the provider this token and base URL. Pando shows the token once."
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
          description="Only a provider nobody has signed in through can be removed. Otherwise it is turned off."
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

/** The full form's button, as it says what it is doing. */
function saveLabel(existing: boolean, pending: boolean): string {
  if (existing) return pending ? 'Saving' : 'Save provider';
  return pending ? 'Adding' : 'Add provider';
}

const NAME_NEEDED = 'A provider needs a name. People see it on the sign-in page.';

/**
 * Adding a sign-in provider one step at a time, on the same walk-through as
 * an adapter: protocol, provider, its steps, then its name and the account
 * options. Its addresses are filled in from the ID it will be created with.
 */
function ProviderGuide({
  kinds,
  kind,
  onKind,
  providerID,
  endpoints,
  name,
  onName,
  values,
  onValues,
  options,
  rows,
  saving,
  saveError,
  onSave,
  onShowAll,
  onClose,
}: Readonly<{
  kinds: ProviderKind[];
  kind?: ProviderKind;
  onKind: (kind: string) => void;
  providerID: string;
  endpoints?: Endpoints;
  name: string;
  onName: (name: string) => void;
  values: Values;
  onValues: (values: Values) => void;
  /** The account options, as the full form shows them too. */
  options: React.ReactNode;
  rows: { label: string; value: string }[];
  saving: boolean;
  saveError: unknown;
  onSave: () => void;
  onShowAll: () => void;
  onClose: () => void;
}>) {
  // Said under the name once Continue has found it empty.
  const [nameError, setNameError] = useState<string | undefined>();
  const supplied: Record<string, string> = endpoints
    ? {
        callback_url: endpoints.callback_url.replace('{id}', providerID),
        entity_id: endpoints.entity_id.replace('{id}', providerID),
      }
    : {};
  return (
    <AdapterGuide
      categories={[]}
      category="identity"
      askCategory={false}
      onCategory={() => undefined}
      inCategory={kinds}
      chosen={kind ? kindKey(kind) : ''}
      onPick={(key) => onKind(key.slice(key.indexOf('/') + 1))}
      skipChoose={false}
      kind={kind}
      form={{ id: providerID, name, isDefault: false, values }}
      edit={(patch) => {
        if (patch.values) onValues(patch.values as Values);
        if (patch.name !== undefined) onName(patch.name);
      }}
      stored={[]}
      renderField={(f, error, instructed) => (
        <FieldInput
          guided
          instructed={instructed}
          field={f}
          value={values[f.key]}
          error={error}
          onChange={(v) => onValues({ ...values, [f.key]: v })}
        />
      )}
      showDefault={false}
      isAI={false}
      saving={saving}
      saveError={saveError}
      onSave={onSave}
      onShowAll={onShowAll}
      onClose={onClose}
      pickLabel="Protocol"
      pickTitle="Add identity provider"
      noun="provider"
      addLabel="Add provider"
      supplied={supplied}
      checkFields={(v) => {
        if (!kind) return {};
        const { name: _name, ...rest } = problems(kind, 'named', v);
        return rest;
      }}
      onProvider={(p) => {
        if (p && !name) onName(p.label);
      }}
      reviewNote="It starts off. Test it with a real sign-in, then turn it on."
      nameStep={{
        content: (
          <>
            <Input
              label="Name (required)"
              value={name}
              helper="Shown on the sign-in page."
              error={nameError}
              onChange={(e) => onName(e.target.value)}
            />
            {options}
          </>
        ),
        problems: (): Record<string, string> => {
          const problem = name.trim() ? undefined : NAME_NEEDED;
          setNameError(problem);
          return problem ? { name: problem } : {};
        },
        rows,
      }}
    />
  );
}

function ProviderDialog({
  kinds,
  existing,
  endpoints,
  onClose,
}: {
  kinds: ProviderKind[];
  existing?: ProviderView;
  endpoints?: Endpoints;
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

  // A new provider is added one step at a time, with every setting at once a
  // click away. Its ID is chosen now, so the addresses the provider is told
  // about while it is set up are the ones it is created with.
  const [guided, setGuided] = useState(!existing);
  const [providerID] = useState(() => newProviderID());

  const save = useMutation({
    mutationFn: () => {
      const body = providerRequest(kind!, name, values, { jit, linkByEmail }, !existing);
      if (existing) return api.patch(`/identity-providers/${existing.id}`, body);
      // With the addresses known, it is created with the ID they were built from.
      return api.post('/identity-providers', endpoints ? { ...body, id: providerID } : body);
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

  const options = (
    <>
      <Checkbox
        label="Just-in-time provisioning"
        description="New accounts start with no access. Policy can turn this off everywhere."
        checked={jit}
        onChange={(e) => setJIT(e.target.checked)}
      />
      <Checkbox
        label="Link accounts by verified email"
        description="Only when the provider verifies the address. Microsoft Entra ID doesn’t."
        checked={linkByEmail}
        onChange={(e) => setLinkByEmail(e.target.checked)}
      />
    </>
  );

  if (guided && !existing) {
    return (
      <ProviderGuide
        kinds={kinds}
        kind={kind}
        onKind={(k) => {
          setKindName(k);
          setValues({});
        }}
        providerID={providerID}
        endpoints={endpoints}
        name={name}
        onName={setName}
        values={values}
        onValues={setValues}
        options={options}
        rows={[
          { label: 'Name', value: name.trim() },
          { label: 'Just-in-time provisioning', value: jit ? 'On' : 'Off' },
          { label: 'Link by verified email', value: linkByEmail ? 'On' : 'Off' },
        ]}
        saving={save.isPending}
        saveError={save.isError ? save.error : undefined}
        onSave={() => save.mutate()}
        onShowAll={() => setGuided(false)}
        onClose={onClose}
      />
    );
  }

  return (
    <Dialog
      open
      width={640}
      title={existing ? `Edit ${existing.name}` : 'Add identity provider'}
      description={
        existing
          ? 'Changes apply at the next sign-in.'
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
            {saveLabel(Boolean(existing), save.isPending)}
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
            helper="Fills in its known settings."
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
          helper="Shown on the sign-in page, such as Okta."
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
        {options}
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
      description="Nobody was signed in. This is what the provider sent and what a real sign-in would do."
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
              title="Claims"
              copyable
              lines={Object.entries(r.attributes ?? {}).map(([k, v]) => `${k}: ${(v ?? []).join(', ')}`)}
            />
          </>
        )}
      </div>
    </Dialog>
  );
}
