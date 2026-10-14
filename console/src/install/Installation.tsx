// Installation, policy and audit — the three read-mostly screens behind
// install.view, install.policy.manage and install.audit.read.
//
// Each is shown on the verb it needs, never on "is an administrator". There is
// no implication graph between verbs (R-082), so a sidebar that assumed one
// would offer a screen whose every request comes back 403.

import { createContext, useContext, useLayoutEffect, useRef, useState } from 'react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Banner,
  Button,
  CodeBlock,
  Dialog,
  EmptyState,
  Icon,
  Input,
  Radio,
  Select,
  StatusIndicator,
  Switch,
  Tag,
  Tooltip,
} from '@design';

import { api, base } from '@api/client';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { AdapterDialog } from './AdapterDialog';
import { AuthorizeDialog, covers, useSources } from './Sources';
import type { SourceConnection } from './Sources';
import { Capacity } from './Capacity';
import { useAIFunctionOn } from './AIFunctions';
import { RestartButton } from './Restart';
import { categoryLabel, categoryNote, orderCategories } from './adapters';
import type { AdapterKind } from './adapters';
import type { ConfiguredAdapter } from './AdapterDialog';
import { Quiet, Screen, messageOf } from './Accounts';
import { NoMatches, SearchField } from '../ui/SearchField';
import { matches } from '../ui/search';
import { Table } from '../ui/Table';
import { TipButton } from '../ui/TipButton';
import { BesideField } from '../ui/BesideField';
import { FieldSkeleton, LineSkeleton } from '../ui/Loading';
import { ActorField } from './ActorField';
import type { Person } from './ActorField';
import { useNamedPeople } from '../ui/people';
import { NO_FILTERS, WHEN, auditQuery, filtersFromSearch } from './audit';
import type { AuditFilters } from './audit';
import { AIButton } from '../ui/AskAI';
import { AuditAI } from './AuditAI';
import { ARCHIVES_ANCHOR, AuditSinks } from './AuditSinks';
import { PolicyAI } from './PolicyAI';
import {
  EGRESS_MODES,
  ENTRY_FORMS,
  LOOSENING,
  approvalsNeeded,
  egressPatch,
  installEgress,
  looseningRule,
} from './policyEgress';
import { ListField } from '../ui/ListField';
import { TagField } from '../ui/TagField';
import { useNarrow } from '../ui/narrow';
import { useSettled, withParams } from '../ui/paged';

/** Where the startup configuration declares an adapter, in words: a key in
 *  the config file, or a variable such as PANDO_REGISTRY_URL. */
function declaredAt(row: AdapterRow): string {
  const src = row.source as { kind?: string; name?: string; key?: string } | undefined;
  if (src?.kind === 'env' && src.name) return `the environment variable ${src.name}`;
  return src?.key ? `${src.name ?? 'the config file'}, at ${src.key}` : 'the config file';
}

interface AdapterRow extends ConfiguredAdapter {
  /** `overridden` when the config file replaces this stored adapter (R-271). */
  status?: string;
  /** Declared in the config file, so read-only here (R-271). */
  declared?: boolean;
  /** An older name for id, from before GET /adapters settled its shape. */
  ref?: string;
  /** Saved since Pando started, so not yet what runs (R-253). */
  pending_restart?: boolean;
  /** What a routing adapter has Pando run in front of itself, such as Traefik
   *  (R-174). The reason is only there for whoever may change the adapter. */
  edge?: { running: boolean; message?: string; checked_at?: string };
  [key: string]: unknown;
}

export function Installation({ query }: { query?: string } = {}) {
  const queries = useQueryClient();
  // Only on the verb (R-082): install.view shows the list, and holding it says
  // nothing about being allowed to change what is in it.
  const canManage = useInstallVerb(InstallVerb.AdaptersManage);
  const [editing, setEditing] = useState<{ existing?: AdapterRow; category?: string } | null>(null);
  // Which adapter was just saved, to name it in the restart notice. The
  // notice itself follows the server's restart_needed, so it stays until
  // Pando has restarted — across a reload, and for everyone who looks.
  const [saved, setSaved] = useState<string | null>(null);
  // A source connection (R-091) and an image registry (issue #153) are used
  // the moment they are saved, so saving one says that instead of asking for
  // a restart.
  const [notice, setNotice] = useState<string | null>(null);
  const [authorizing, setAuthorizing] = useState<SourceConnection | null>(null);
  const [disconnecting, setDisconnecting] = useState<SourceConnection | null>(null);

  // A browser authorization of a source comes back here with its outcome.
  const back = new URLSearchParams(query ?? '');
  const returnedError = back.get('error');
  const returnedOK = back.get('authorized');

  // Each source connection's state — ready, not authorized, or why it cannot
  // be used — beside its row.
  const sources = useSources();
  const connections = new Map((sources.data?.sources ?? []).map((s) => [s.id, s]));
  const disconnect = useMutation({
    mutationFn: (id: string) => api.del<void>(`/sources/${encodeURIComponent(id)}`),
    onSuccess: (_, id) => {
      setDisconnecting(null);
      setNotice(`Disconnected ${id}.`);
      void queries.invalidateQueries({ queryKey: ['adapters'] });
      void queries.invalidateQueries({ queryKey: ['sources'] });
    },
  });

  const adapters = useQuery({
    queryKey: ['adapters'],
    queryFn: () => api.get<{ adapters: AdapterRow[]; restart_needed?: boolean } | AdapterRow[]>('/adapters'),
  });
  const restartNeeded = !Array.isArray(adapters.data) && adapters.data?.restart_needed === true;

  const rows = normalize(adapters.data).map((r) => ({ ...r, id: r.id ?? r.ref ?? '' }));
  // Every kind this build can run: the adapters' proper names, and which
  // categories could be set up and are not. Shared with the dialog's query.
  const kinds = useQuery({
    queryKey: ['adapter-kinds'],
    queryFn: () => api.get<{ kinds: AdapterKind[] | null }>('/adapters/kinds'),
  });

  // In category order, then by name, each row told whether it starts its
  // category and what its kind is called — "BuildKit", not "buildkit".
  const catalog = kinds.data?.kinds ?? [];
  const order = orderCategories([...rows.map((r) => r.category), ...catalog.map((k) => k.category)]);
  const grouped = order.flatMap((category) =>
    rows
      .filter((r) => r.category === category)
      .sort((a, b) => a.id.localeCompare(b.id))
      .map((r, i) => ({
        ...r,
        first: i === 0,
        kindName: catalog.find((k) => k.category === r.category && k.kind === r.kind)?.name ?? r.kind,
        connection: r.category === 'source' ? connections.get(r.id) : undefined,
      })),
  );
  // Categories this build can run that nothing here is set up for.
  const missing = order.filter((c) => catalog.some((k) => k.category === c) && !rows.some((r) => r.category === c));

  return (
    <Screen
      heading="Adapters"
      action={
        canManage && (
          <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
            {/* Primary: adding an adapter is the one thing this screen does. */}
            <Button variant="primary" onClick={() => setEditing({})}>
              Add adapter
            </Button>
            {/* Always here, not only while a change waits on it: a restart
                is also how an adapter that failed at startup is retried.
                Last, at the edge, apart from the everyday action. */}
            <RestartButton onRestarted={() => setSaved(null)} />
          </div>
        )
      }
    >
      {adapters.isError && <Quiet>{messageOf(adapters.error)}</Quiet>}
      {returnedError && (
        <div style={{ marginBottom: 'var(--space-4)' }}>
          <Banner tone="failed">{returnedError}</Banner>
        </div>
      )}
      {(notice || (returnedOK && !returnedError)) && (
        <div style={{ marginBottom: 'var(--space-4)' }}>
          <Banner tone="info">{notice ?? `${returnedOK} is authorized and ready.`}</Banner>
        </div>
      )}

      {(restartNeeded || saved) && (
        <div style={{ marginBottom: 'var(--space-4)' }}>
          <Banner tone="info">
            {saved ? `${saved} is saved.` : 'Adapter changes are saved.'} Restart Pando to apply{' '}
            {saved ? 'it' : 'them'}.
            {canManage ? null : <> Someone who can manage adapters can restart it.</>}
          </Banner>
        </div>
      )}

      {/* One table, grouped by category: one header and one set of columns,
          so everything lines up, with a band naming each category above its
          adapters. Categories with nothing set up are one line under it. */}
      {adapters.isPending ? (
        <Table loading columns={adapterColumns(canManage, setEditing)} rows={[]} />
      ) : (
        <GroupedAdapters
          rows={grouped}
          canManage={canManage}
          onChange={(row) => setEditing({ existing: row })}
          onAuthorize={setAuthorizing}
          onDisconnect={setDisconnecting}
        />
      )}
      {!adapters.isPending && missing.length > 0 && (
        <div
          style={{
            display: 'flex',
            flexWrap: 'wrap',
            alignItems: 'center',
            gap: 'var(--space-2) var(--space-3)',
            marginTop: 'var(--space-4)',
          }}
        >
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>
            Not set up: {missing.map(categoryLabel).join(', ')}.
          </span>
          {canManage &&
            missing.map((category) => (
              <Button key={category} variant="secondary" onClick={() => setEditing({ category })}>
                {addAdapterLabel(category)}
              </Button>
            ))}
        </div>
      )}

      {editing && (
        <AdapterDialog
          existing={editing.existing}
          category={editing.category}
          adapters={rows}
          onClose={() => setEditing(null)}
          onSaved={(name, category) => {
            setEditing(null);
            if (category === 'source' || category === 'image_registry') {
              setNotice(`${name} is saved and in use. No restart is needed.`);
              if (category === 'source') void queries.invalidateQueries({ queryKey: ['sources'] });
            } else {
              setSaved(name);
            }
            void queries.invalidateQueries({ queryKey: ['adapters'] });
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
            void queries.invalidateQueries({ queryKey: ['sources'] });
          }}
        />
      )}

      {disconnecting && (
        <Dialog
          open
          title={`Disconnect ${disconnecting.name}`}
          description="Its stored credential is deleted. Apps that use it keep running. Their next deploy fails until another connection covers their repository."
          onClose={() => setDisconnecting(null)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setDisconnecting(null)}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                disabled={disconnect.isPending}
                onClick={() => disconnect.mutate(disconnecting.id)}
              >
                {disconnect.isPending ? 'Disconnecting' : 'Disconnect'}
              </Button>
            </>
          }
        >
          {disconnect.isError && <Banner tone="failed">{messageOf(disconnect.error)}</Banner>}
        </Dialog>
      )}

      {/* Each runtime by the name its kind goes by, as in the table above. */}
      <Capacity names={Object.fromEntries(grouped.map((r) => [r.id, r.kindName]))} />
    </Screen>
  );
}

/** "Add an image registry adapter", from the category's own label. */
function addAdapterLabel(category: string): string {
  const label = categoryLabel(category);
  const word = label === 'AI' ? label : label.toLowerCase();
  return `Add ${/^[aeiou]/i.test(word) ? 'an' : 'a'} ${word} adapter`;
}

interface PolicyDoc {
  source_allowlist?: string[];
  disabled_verbs?: string[];
  allow_anonymous_grants?: boolean;
  public_sharing?: 'allowed' | 'passcode_only' | 'none';
  // Egress (R-181 – R-185). egress_allowlist is the field from before issue
  // #79, still read as an allowlist when egress_mode is unset.
  egress_mode?: string;
  egress_list?: string[];
  egress_block_private?: boolean;
  egress_loosening?: string;
  egress_allowlist?: string[];

  // Deploy approval (R-154 – R-156).
  deploy_approval_required?: boolean;
  deploy_approval_apps?: string[];
  deploy_approval_count?: number;
  deploy_approval_expiry_hours?: number;

  require_backup_before_destroy?: boolean;
  max_token_lifetime_days?: number;
  min_build_isolation?: number;
  min_runtime_isolation?: number;

  agent_disabled_verbs?: string[];
  max_log_disk_bytes?: number;

  // R-242, as amended by issue #72. Disk has no counterpart.
  allow_cpu_oversubscription?: boolean;
  allow_memory_oversubscription?: boolean;
  max_concurrent_deploys?: number;

  // Apps nobody uses (R-393 – R-398) and how many a person may own (R-244),
  // issue #131. Zero is off, or unlimited.
  idle_stop_days?: number;
  idle_delete_days?: number;
  max_apps_per_user?: number;

  // Audit retention (R-347, R-348).
  audit_retention_months?: number;
  audit_archive?: string;
  audit_archive_destination?: string;

  // The security score (R-313 – R-316).
  min_security_score?: number;
  insecure_action?: string;
  insecure_grace_hours?: number;
  ignore_unfixable_findings?: boolean;

  disable_ai_screening?: boolean;
  disable_anonymous_use_audit?: boolean;
  disable_anonymous_denial_audit?: boolean;

  disable_password_sign_in?: boolean;
  disable_jit_provisioning?: boolean;

  // Event subscription webhooks (R-372).
  allow_private_webhooks?: boolean;

  // The update check (R-349, R-350).
  disable_update_check?: boolean;
  update_channel?: string;

  // The in-place upgrade (R-355, R-361).
  upgrade_in_place?: boolean;
  auto_upgrade_patches?: boolean;
  maintenance_window?: string;
}

interface Violation {
  app_id: string;
  app_name: string;
  code: string;
  message: string;
  remedy?: string;
}

export function Policy({ canEdit }: { canEdit: boolean }) {
  const queries = useQueryClient();
  const [draft, setDraft] = useState<PolicyDoc | null>(null);

  // Search over the settings themselves. Each section is shown when anything
  // in it matches — its heading, its note, a label, a description, an option —
  // read from what is on the screen rather than from a list of keywords kept
  // beside it, which would be a second description of the page that nobody
  // remembers to update when a setting is added.
  const [query, setQuery] = useState('');
  const sectionsRef = useRef<HTMLDivElement>(null);
  const [nothingMatches, setNothingMatches] = useState(false);

  const policy = useQuery({
    queryKey: ['policy'],
    queryFn: () => api.get<PolicyDoc>('/policy'),
  });

  const save = useMutation({
    mutationFn: (doc: PolicyDoc) => api.put<PolicyDoc>('/policy', doc),
    onSuccess: () => {
      setDraft(null);
      setPreview(null);
      void queries.invalidateQueries({ queryKey: ['policy'] });
    },
  });

  // What this policy would block, before it is saved (design 05 §3).
  //
  // Asked for, not automatic. Running every app's pinned spec through the
  // planner is not free, and doing it on every keystroke would make a form that
  // stutters — which teaches people to ignore the panel it is stuttering to
  // fill.
  const [preview, setPreview] = useState<Violation[] | null>(null);
  const check = useMutation({
    mutationFn: (doc: PolicyDoc) =>
      api.post<{ violations: Violation[] }>('/policy/preview', doc),
    onSuccess: (result) => setPreview(result.violations ?? []),
  });

  const current = draft ?? policy.data ?? {};

  // Drafting a change with AI (R-344), behind one button and in its own
  // dialog, which saves only when accepted.
  const draftOn = useAIFunctionOn('draft_policy');
  const [drafting, setDrafting] = useState(false);

  // Fields fixed in the startup configuration (R-271): shown, not editable,
  // and each says where it is set. GET /config is install.view, the same as
  // reading policy, so whoever sees this screen can see why.
  const startup = useQuery({ queryKey: ['config'], queryFn: () => api.get<StartupConfig>('/config') });
  const fixed = new Map((startup.data?.policy ?? []).map((f) => [f.key, f.source]));
  const locked = (field: string) => !canEdit || fixed.has(field);

  useLayoutEffect(() => {
    const sections = Array.from(sectionsRef.current?.querySelectorAll<HTMLElement>('section') ?? []);
    let shown = 0;
    for (const el of sections) {
      const match = matches(query, el.textContent ?? '');
      // Inline, because PolicySection's own inline display: flex outranks the
      // hidden attribute. Set back to flex, not cleared: React only rewrites a
      // style it sees change, so a cleared one would stay cleared.
      el.style.display = match ? 'flex' : 'none';
      if (match) shown += 1;
    }
    setNothingMatches(sections.length > 0 && shown === 0);
  });
  const edit = (patch: Partial<PolicyDoc>) => {
    // A stale answer is worse than no answer: it says "nothing breaks" about a
    // policy that is no longer the one on screen.
    setPreview(null);
    setDraft({ ...current, ...patch });
  };

  // The page's sections in outline — a heading, its note, a control, under a
  // rule — where the settings will be.
  if (policy.isPending) {
    return (
      <Screen heading="Policy">
        <div role="status" aria-label="Loading" style={{ display: 'flex', flexDirection: 'column', maxWidth: '68ch' }}>
          {[0, 1, 2].map((n) => (
            <div
              key={n}
              style={{
                display: 'flex',
                flexDirection: 'column',
                gap: 'var(--space-2)',
                padding: 'var(--space-6) 0',
                borderTop: n === 0 ? undefined : 'var(--border-width) solid var(--rule)',
              }}
            >
              <LineSkeleton width="22ch" font="var(--type-h4)" />
              <LineSkeleton width="52ch" />
              <div style={{ marginTop: 'var(--space-2)' }}>
                <FieldSkeleton />
              </div>
            </div>
          ))}
        </div>
      </Screen>
    );
  }
  if (policy.isError)
    return (
      <Screen heading="Policy">
        <Quiet>{messageOf(policy.error)}</Quiet>
      </Screen>
    );

  return (
    <Screen
      heading="Policy"
      action={
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
        {canEdit && draft && (
          <div style={{ display: 'flex', gap: 'var(--space-3)' }}>
            <Button variant="ghost" onClick={() => { setDraft(null); setPreview(null); }}>
              Discard
            </Button>
            <Button
              variant="secondary"
              // Not again while the answer on screen is for this draft: edit
              // clears it, and asking twice reads every affected app twice.
              disabled={check.isPending || preview !== null}
              onClick={() => check.mutate(current)}
            >
              {check.isPending ? 'Checking' : 'Check impact'}
            </Button>
            <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate(current)}>
              {save.isPending ? 'Saving' : 'Save policy'}
            </Button>
          </div>
        )}
          {canEdit && draftOn && !draft && <AIButton onClick={() => setDrafting(true)} />}
          <SearchField value={query} onChange={setQuery} placeholder="Search policy" />
        </div>
      }
    >
      {/* O-10, stated where the decision is made rather than in a tooltip.
          Saying it plainly is the difference between an administrator who
          knows why nothing happened and one who thinks the save failed. */}
      {drafting && <PolicyAI onClose={() => setDrafting(false)} />}

      <Banner tone="info">
        Saving policy doesn&rsquo;t affect running apps. An app that breaks a new rule is refused at
        its next deploy.
      </Banner>

      {save.isError && (
        <div style={{ marginTop: 'var(--space-4)' }}>
          <Banner tone="failed">{messageOf(save.error)}</Banner>
        </div>
      )}

      {check.isError && (
        <div style={{ marginTop: 'var(--space-4)' }}>
          <Banner tone="failed">{messageOf(check.error)}</Banner>
        </div>
      )}

      {/* O-10 says a violating app keeps running and fails its next deploy.
          This is the half of that which makes it liveable: an administrator
          tightening a rule is entitled to know it blocks four apps before they
          save, because finding out one deploy at a time is how a policy gets
          rolled back in anger. */}
      {preview !== null && (
        <div style={{ marginTop: 'var(--space-4)' }}>
          {preview.length === 0 ? (
            <Banner tone="running">
              No apps break these rules.
            </Banner>
          ) : (
            <>
              <Banner tone="building">
                {preview.length === 1
                  ? 'One app keeps running and is refused at its next deploy.'
                  : `${preview.length} apps keep running and are refused at their next deploy.`}
              </Banner>
              <div style={{ marginTop: 'var(--space-4)' }}>
                <Table
                  dense
                  columns={[
                    { key: 'app_name', header: 'App', width: 'minmax(0,24ch)' },
                    { key: 'message', header: 'Message', width: 'minmax(0,32ch)' },
                    { key: 'code', header: 'Code', width: '28ch', mono: true, muted: true },
                  ]}
                  rows={preview}
                />
              </div>
            </>
          )}
        </div>
      )}

      {nothingMatches && (
        <div style={{ marginTop: 'var(--space-6)' }}>
          <NoMatches what="settings" query={query} />
        </div>
      )}

      <FixedFields.Provider value={fixed}>
      <div
        ref={sectionsRef}
        style={{
          display: 'flex',
          flexDirection: 'column',
          marginTop: 'var(--space-6)',
          maxWidth: '68ch',
        }}
      >
        {/* Sections separated by rules, which is the system's own answer to a
            page of settings — and the reason is the page rather than the
            style: policy is a list of unrelated decisions that only grows, and
            a flat column of fourteen switches is a column nobody scans. Each
            heading says which question the controls under it answer. */}
        <PolicySection
          heading="Sources"
          note="Checked before anything is cloned."
        >
          <Fixed field="source_allowlist">
            <Input
              label="Allowed sources"
              as="textarea"
              rows={3}
              mono
              disabled={locked('source_allowlist')}
              value={(current.source_allowlist ?? []).join('\n')}
              helper="One host pattern per line, for example github.com/acme/*. Empty means anywhere."
              onChange={(e) =>
                edit({ source_allowlist: e.target.value.split('\n').map((l) => l.trim()).filter(Boolean) })
              }
            />
          </Fixed>
        </PolicySection>

        <PolicySection
          heading="Access"
          note="A minimum. App owners can only make their apps stricter."
        >
          {/* Three answers, not a switch (R-076, R-075a): anyone, anyone with the
              app's passcode, or nobody. public_sharing is the setting; the
              older allow_anonymous_grants still counts when it is unset, and
              either fixed at startup makes this read-only — whichever is,
              names where. When sharing is limited, the options people can't
              use stay visible and disabled on the Sharing tab, with a note
              saying who to ask, rather than disappearing. */}
          <Fixed field={fixed.has('public_sharing') || !fixed.has('allow_anonymous_grants') ? 'public_sharing' : 'allow_anonymous_grants'}>
            <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
                Public sharing
              </legend>
              {(
                [
                  ['allowed', 'Allowed', 'An app can be shared with anyone on the internet, with or without a passcode.'],
                  ['passcode_only', 'Only with a passcode', 'Anyone on the internet can open it only after entering its passcode.'],
                  ['none', 'Not allowed', 'Apps are shared only with people and groups who sign in.'],
                ] as const
              ).map(([value, label, description]) => (
                <Radio
                  key={value}
                  name="public_sharing"
                  value={value}
                  label={label}
                  description={description}
                  checked={publicSharing(current) === value}
                  disabled={locked('public_sharing') || locked('allow_anonymous_grants')}
                  onChange={() => edit({ public_sharing: value })}
                />
              ))}
            </fieldset>
          </Fixed>

          <Fixed field="disabled_verbs">
            <Switch
              checked={current.disabled_verbs?.includes('app.exec') ?? false}
              disabled={locked('disabled_verbs')}
              label="Disable terminal access"
              // R-085 and R-272: policy is a floor, not an override.
              description="Applies to app owners too."
              onChange={(e) => {
                const rest = (current.disabled_verbs ?? []).filter((v) => v !== 'app.exec');
                edit({ disabled_verbs: e.target.checked ? [...rest, 'app.exec'] : rest });
              }}
            />
          </Fixed>

          {/* R-227: every visit to an app is in the audit log, anonymous ones
              included by default. A busy public site may not want those. */}
          <Fixed field="disable_anonymous_use_audit">
            <Switch
              checked={current.disable_anonymous_use_audit ?? false}
              disabled={locked('disable_anonymous_use_audit')}
              label="Don't log anonymous visits"
              description="Visits by signed-in people and tokens are still logged."
              onChange={(e) => edit({ disable_anonymous_use_audit: e.target.checked })}
            />
          </Fixed>

          {/* Design 06 §6: every denial is audited, including a visitor who
              isn't signed in reaching a private app. Anyone can cause those. */}
          <Fixed field="disable_anonymous_denial_audit">
            <Switch
              checked={current.disable_anonymous_denial_audit ?? false}
              disabled={locked('disable_anonymous_denial_audit')}
              label="Don't log anonymous refusals"
              description="Refusals of signed-in people and tokens are still logged."
              onChange={(e) => edit({ disable_anonymous_denial_audit: e.target.checked })}
            />
          </Fixed>

          {/* Issue #51: how people sign in. Password sign-in off is refused
              while no identity provider is on; the Sign-in screen says so. */}
          <Fixed field="disable_password_sign_in">
            <Switch
              checked={current.disable_password_sign_in ?? false}
              disabled={locked('disable_password_sign_in')}
              label="Disable password sign-in"
              description="Needs an identity provider turned on under Sign-in. If none works, run pando admin enable-password-sign-in on the host."
              onChange={(e) => edit({ disable_password_sign_in: e.target.checked })}
            />
          </Fixed>
          <Fixed field="disable_jit_provisioning">
            <Switch
              checked={current.disable_jit_provisioning ?? false}
              disabled={locked('disable_jit_provisioning')}
              label="Disable just-in-time provisioning"
              description="Overrides each identity provider's setting. People need an account from SCIM, or linked by an administrator."
              onChange={(e) => edit({ disable_jit_provisioning: e.target.checked })}
            />
          </Fixed>

          {current.disabled_verbs && current.disabled_verbs.length > 0 && (
            <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
              {current.disabled_verbs.map((v) => (
                <Tag key={v} mono>
                  {v}
                </Tag>
              ))}
            </div>
          )}
        </PolicySection>

        <PolicySection
          heading="Isolation"
        >
          <Fixed field="min_build_isolation">
            <Select
              label="Minimum isolation for builds"
              disabled={locked('min_build_isolation')}
              value={String(current.min_build_isolation ?? '')}
              options={ISOLATION}
              helper="Adapters that can't meet this are refused at plan time."
              onChange={(e) => edit({ min_build_isolation: Number(e.target.value) || undefined })}
            />
          </Fixed>

          <Fixed field="min_runtime_isolation">
            <Select
              label="Minimum isolation for running apps"
              disabled={locked('min_runtime_isolation')}
              value={String(current.min_runtime_isolation ?? '')}
              options={ISOLATION}
              onChange={(e) => edit({ min_runtime_isolation: Number(e.target.value) || undefined })}
            />
          </Fixed>

        </PolicySection>

        <EgressPolicy current={current} edit={edit} locked={locked} fixed={fixed} />

        <DeployApprovalPolicy current={current} edit={edit} locked={locked} />


        <PolicySection
          heading="Tokens and agents"
        >
          <Fixed field="max_token_lifetime_days">
            <Input
              label="Max token lifetime (days)"
              type="number"
              disabled={locked('max_token_lifetime_days')}
              value={String(current.max_token_lifetime_days ?? 0)}
              helper="Zero means no limit."
              onChange={(e) =>
                edit({ max_token_lifetime_days: Math.max(0, Math.round(Number(e.target.value) || 0)) })
              }
            />
          </Fixed>

          <Fixed field="agent_disabled_verbs">
            <Input
              label="Disabled verbs for tokens"
              as="textarea"
              rows={3}
              mono
              disabled={locked('agent_disabled_verbs')}
              value={(current.agent_disabled_verbs ?? []).join('\n')}
              helper="One verb per line. Refused for tokens, allowed for people."
              onChange={(e) =>
                edit({
                  agent_disabled_verbs: e.target.value.split('\n').map((l) => l.trim()).filter(Boolean),
                })
              }
            />
          </Fixed>
        </PolicySection>

        <PolicySection
          heading="Security scanning"
          note="Each app is scored out of 100. Nothing is enforced until a minimum is set."
        >
          {/* The consequence is written at the point of setting it, not in a
              tooltip: this is the setting that can stop somebody's working
              service. */}
          <Fixed field="min_security_score">
            <Input
              label="Minimum security score"
              type="number"
              disabled={locked('min_security_score')}
              value={String(current.min_security_score ?? 0)}
              helper="0 to 100. Zero is off. An app below this is refused at deploy, with the findings that cost it the most."
              onChange={(e) => edit({ min_security_score: clamp(Number(e.target.value)) })}
            />
          </Fixed>

          <Fixed field="disable_ai_screening">
            <Switch
              checked={current.disable_ai_screening ?? false}
              disabled={locked('disable_ai_screening')}
              label="Disable AI screening"
              // R-336, R-337: screening sends repository contents to a provider,
              // and saying no to that should not mean deleting somebody's adapter.
              description="No deployment plan is sent to an AI provider for review, even when one is configured."
              onChange={(e) => edit({ disable_ai_screening: e.target.checked })}
            />
          </Fixed>

          <Fixed field="ignore_unfixable_findings">
            <Switch
              checked={current.ignore_unfixable_findings ?? false}
              disabled={locked('ignore_unfixable_findings')}
              label="Ignore findings with no fix available"
              // R-313a: what is counted is what is shown, both ways round.
              description="Vulnerabilities with no published fix are left out of the score and the findings list."
              onChange={(e) => edit({ ignore_unfixable_findings: e.target.checked })}
            />
          </Fixed>

          {(current.min_security_score ?? 0) > 0 && (
            <>
              <Fixed field="insecure_action">
                <Switch
                  checked={current.insecure_action === 'stop'}
                  disabled={locked('insecure_action')}
                  label="Stop running apps below the minimum"
                  // R-315: a policy change or a newly published CVE is not a
                  // reason to take a working service away without notice, so
                  // this is opt-in and grace comes with it.
                  description="The owner is told first, and the app stops when the grace period below ends."
                  onChange={(e) => edit({ insecure_action: e.target.checked ? 'stop' : 'warn' })}
                />
              </Fixed>

              {current.insecure_action === 'stop' && (
                <Fixed field="insecure_grace_hours">
                  <Input
                    label="Grace period (hours)"
                    type="number"
                    disabled={locked('insecure_grace_hours')}
                    value={String(current.insecure_grace_hours ?? 24)}
                    helper="Counted from when the app first falls below the score."
                    onChange={(e) =>
                      edit({ insecure_grace_hours: Math.max(1, Number(e.target.value) || 24) })
                    }
                  />
                </Fixed>
              )}
            </>
          )}
        </PolicySection>

        <PolicySection
          heading="Data and destruction"
        >
          <Fixed field="require_backup_before_destroy">
            <Switch
              checked={current.require_backup_before_destroy ?? false}
              disabled={locked('require_backup_before_destroy')}
              label="Require backup before delete"
              // R-284: set once, and app owners cannot override downward.
              // It also answers for Pando's own deletion of an idle app (R-398).
              description="App owners can't turn this off. Also applies to idle apps Pando deletes."
              onChange={(e) => edit({ require_backup_before_destroy: e.target.checked })}
            />
          </Fixed>

          <Fixed field="max_log_disk_bytes">
            <Input
              label="Total disk for app logs, in megabytes"
              type="number"
              disabled={locked('max_log_disk_bytes')}
              value={String(Math.round((current.max_log_disk_bytes ?? 0) / 1_000_000))}
              // R-224: checked against the sum of every app's cap at plan time,
              // not against measured usage — bounding what is committed is the
              // stronger guarantee, and the alternative acts after the disk is
              // already filling.
              helper="Across all apps. Zero means no limit."
              onChange={(e) =>
                edit({ max_log_disk_bytes: Math.max(0, Math.round(Number(e.target.value) || 0)) * 1_000_000 })
              }
            />
          </Fixed>
        </PolicySection>

        {/* Issue #131. Defaults, not floors: an app can set its own days or
            turn either off (R-397), and a person's account or group can
            carry its own limit (R-244). */}
        <PolicySection
          heading="Idle apps"
          note="Counted from an app's last use, deploy or start. The owner is told 7 days before Pando acts. Apps can set their own."
        >
          <Fixed field="idle_stop_days">
            <Input
              label="Stop idle apps after (days)"
              type="number"
              disabled={locked('idle_stop_days')}
              value={String(current.idle_stop_days ?? 0)}
              helper="Zero means never. A stopped app keeps its data."
              onChange={(e) => edit({ idle_stop_days: Math.max(0, Math.round(Number(e.target.value) || 0)) })}
            />
          </Fixed>
          <Fixed field="idle_delete_days">
            <Input
              label="Delete idle apps after (days)"
              type="number"
              disabled={locked('idle_delete_days')}
              value={String(current.idle_delete_days ?? 0)}
              helper={
                current.require_backup_before_destroy
                  ? "Zero means never. Must be longer than stopping. Pando backs up the app's storage first."
                  : "Zero means never. Must be longer than stopping. The app's storage is deleted with it unless Data and destruction requires a backup."
              }
              onChange={(e) => edit({ idle_delete_days: Math.max(0, Math.round(Number(e.target.value) || 0)) })}
            />
          </Fixed>
          <Fixed field="max_apps_per_user">
            <Input
              label="App limit"
              type="number"
              disabled={locked('max_apps_per_user')}
              value={String(current.max_apps_per_user ?? 0)}
              helper="Apps each person may own. Zero means unlimited. A limit on a person's account or group overrides this."
              onChange={(e) => edit({ max_apps_per_user: Math.max(0, Math.round(Number(e.target.value) || 0)) })}
            />
          </Fixed>
        </PolicySection>

        <PolicySection
          heading="Capacity"
          note="By default, a deploy can't reserve more CPU or memory than the runtime has. Disk is never oversubscribed."
        >
          <Fixed field="allow_cpu_oversubscription">
            <Switch
              checked={current.allow_cpu_oversubscription ?? false}
              disabled={locked('allow_cpu_oversubscription')}
              label="Allow CPU oversubscription"
              description="Busy apps share the CPU and run slower."
              onChange={(e) => edit({ allow_cpu_oversubscription: e.target.checked })}
            />
          </Fixed>
          <Fixed field="allow_memory_oversubscription">
            <Switch
              checked={current.allow_memory_oversubscription ?? false}
              disabled={locked('allow_memory_oversubscription')}
              label="Allow memory oversubscription"
              description="If the host runs out, it stops an app to free memory."
              onChange={(e) => edit({ allow_memory_oversubscription: e.target.checked })}
            />
          </Fixed>
          {/* Issue #93: read each time the deploy queue looks for work, so a
              change applies without a restart. */}
          <Fixed field="max_concurrent_deploys">
            <Input
              label="Concurrent deploys per replica"
              type="number"
              disabled={locked('max_concurrent_deploys')}
              value={String(current.max_concurrent_deploys ?? 0)}
              helper="Zero means one per CPU, at least two. Extra deploys wait in a queue."
              onChange={(e) =>
                edit({ max_concurrent_deploys: Math.max(0, Math.round(Number(e.target.value) || 0)) })
              }
            />
          </Fixed>
        </PolicySection>

        <PolicySection
          heading="Audit log"
          note="How long events stay in the live log before they're archived."
        >
          {/* R-348: three months is the floor, held by the database as well
              as here, so a shorter number is refused rather than saved. */}
          <Fixed field="audit_retention_months">
            <Input
              label="Months to keep"
              type="number"
              min={3}
              disabled={locked('audit_retention_months') || current.audit_archive === 'off'}
              value={String(current.audit_retention_months || 3)}
              helper="At least 3. Older months are archived once a day and stay downloadable from the Audit log screen."
              onChange={(e) => edit({ audit_retention_months: Math.max(3, Math.round(Number(e.target.value) || 3)) })}
            />
          </Fixed>
          <Fixed field="audit_archive">
            <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
                Archive destination
              </legend>
              {(
                [
                  ['keep', 'Kept by Pando', "Compressed and stored in Pando's own data directory."],
                  ['export', 'Exported to a backup destination', 'Compressed and written to a backup destination.'],
                  ['off', 'Not archived', 'The audit log keeps growing.'],
                ] as const
              ).map(([value, label, description]) => (
                <Radio
                  key={value}
                  name="audit_archive"
                  value={value}
                  label={label}
                  description={description}
                  checked={(current.audit_archive || 'keep') === value}
                  disabled={locked('audit_archive')}
                  onChange={() =>
                    edit(
                      value === 'export'
                        ? { audit_archive: value }
                        : { audit_archive: value, audit_archive_destination: undefined },
                    )
                  }
                />
              ))}
            </fieldset>
          </Fixed>
          {current.audit_archive === 'export' && (
            <Fixed field="audit_archive_destination">
              <Input
                label="Backup destination"
                mono
                placeholder="The default backup destination"
                disabled={locked('audit_archive_destination')}
                value={current.audit_archive_destination ?? ''}
                helper="A backup adapter's ID, from the Adapters screen. Leave empty for the default one."
                onChange={(e) => edit({ audit_archive_destination: e.target.value.trim() || undefined })}
              />
            </Fixed>
          )}
        </PolicySection>

        <PolicySection
          heading="Event webhooks"
          note="Where subscription webhooks may send."
        >
          {/* R-372: off by default. */}
          <Fixed field="allow_private_webhooks">
            <Switch
              checked={current.allow_private_webhooks ?? false}
              disabled={locked('allow_private_webhooks')}
              label="Allow private webhook addresses"
              description="The local network, this host, and link-local addresses such as a cloud metadata service. Leave this off unless your webhook receivers run inside your network."
              onChange={(e) => edit({ allow_private_webhooks: e.target.checked })}
            />
          </Fixed>
        </PolicySection>

        <PolicySection
          heading="Updates"
          note="Checking for and installing new Pando releases."
        >
          {/* R-349: on by default; off sends no request at all. */}
          <Fixed field="disable_update_check">
            <Switch
              checked={current.disable_update_check ?? false}
              disabled={locked('disable_update_check')}
              label="Disable update check"
              description="Pando checks GitHub at startup and every six hours, sending only its version."
              onChange={(e) => edit({ disable_update_check: e.target.checked })}
            />
          </Fixed>
          <Fixed field="update_channel">
            <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
                Release channel
              </legend>
              {(
                [
                  ['stable', 'Stable', 'Releases only.'],
                  ['prerelease', 'Include release candidates', 'Also offers versions such as 1.0.0-rc.1.'],
                ] as const
              ).map(([value, label, description]) => (
                <Radio
                  key={value}
                  name="update_channel"
                  value={value}
                  label={label}
                  description={description}
                  checked={(current.update_channel || 'stable') === value}
                  disabled={locked('update_channel') || (current.disable_update_check ?? false)}
                  onChange={() => edit({ update_channel: value })}
                />
              ))}
            </fieldset>
          </Fixed>

          {/* R-355: off by default, and usually decided in the deployment
              rather than here — a pinned version is put back by its next
              apply, so whoever owns that configuration turns this on. */}
          <Fixed field="upgrade_in_place">
            <Switch
              checked={current.upgrade_in_place ?? false}
              disabled={locked('upgrade_in_place')}
              label="Let Pando upgrade itself"
              description="Adds an Upgrade button to the Updates screen. Pando verifies the release, takes a backup, and rolls back if the new version doesn't start."
              onChange={(e) => edit({ upgrade_in_place: e.target.checked })}
            />
          </Fixed>
          <InPlaceSetup />

          {/* R-361: with nobody there, no passphrase, so no full backup. */}
          <Fixed field="auto_upgrade_patches">
            <Switch
              checked={current.auto_upgrade_patches ?? false}
              disabled={locked('auto_upgrade_patches') || !(current.upgrade_in_place ?? false)}
              label="Install patch releases automatically"
              description="Patch releases of the running version only (0.3.1 to 0.3.2), inside the maintenance window. Pando keeps a database copy to roll back to, but takes no full backup."
              onChange={(e) =>
                edit(
                  e.target.checked && !current.maintenance_window
                    ? { auto_upgrade_patches: true, maintenance_window: 'sun 02:00 2h' }
                    : { auto_upgrade_patches: e.target.checked },
                )
              }
            />
          </Fixed>
          {(current.auto_upgrade_patches || current.maintenance_window) && (
            <Fixed field="maintenance_window">
              <Input
                label="Maintenance window, in UTC"
                mono
                placeholder="sun 02:00 2h"
                disabled={locked('maintenance_window')}
                value={current.maintenance_window ?? ''}
                helper="Weekdays, a start time and a length in hours: sun,wed 02:00 2h, or daily 03:30 1h. Every app is unreachable for under a minute while Pando restarts."
                onChange={(e) => edit({ maintenance_window: e.target.value || undefined })}
              />
            </Fixed>
          )}
        </PolicySection>

        <StartupSettings config={startup.data} />
      </div>
      </FixedFields.Provider>
    </Screen>
  );
}

/**
 * The isolation floors, as the spec names them (design 01 §3).
 *
 * Ordered integers underneath, because policy floors are compared (R-114) — a
 * select of four options is the readable half of that, not a second model.
 */
const ISOLATION = [
  { value: '', label: 'No floor' },
  { value: '10', label: 'Container — a shared kernel' },
  { value: '20', label: 'Sandboxed — a kernel of its own, shared host' },
  { value: '30', label: 'Virtual machine' },
  { value: '40', label: 'Dedicated host' },
];

// --- the startup configuration (R-271) ---------------------------------------

interface Source {
  kind: 'env' | 'file' | 'default';
  name?: string;
  key?: string;
}

interface StartupConfig {
  file: string;
  settings: Array<{ key: string; value: unknown; source: Source; env: string }>;
  policy: Array<{ key: string; value: unknown; source: Source }>;
}

/** The policy fields fixed at startup, by name, for the controls below. */
const FixedFields = createContext<Map<string, Source>>(new Map());

/** Where a value was set, as a sentence an operator can act on. */
function setIn(src: Source): string {
  if (src.kind === 'env') {
    return `Set at startup by ${src.name}. To change it with Docker Compose, edit ${src.name} under environment: on the pando service in docker-compose.yml, then run docker compose up -d pando. A plain restart does not re-read the environment.`;
  }
  if (src.kind === 'file') {
    return `Set at startup in ${src.name}, at ${src.key}. Edit it there, then restart Pando.`;
  }
  return 'Set at startup.';
}

/**
 * A policy control whose field may be fixed at startup. When it is, the
 * control is shown as it stands — disabled, by the caller — and hovering it
 * says where it is set. Hover is taken by a wrapper, because a disabled input
 * receives no pointer events of its own.
 */
/** The rule for sharing with everyone, reading the older boolean when the new
 *  setting is unset — the same reading as the server's PublicSharingMode. */
function publicSharing(doc: PolicyDoc): 'allowed' | 'passcode_only' | 'none' {
  if (doc.public_sharing) return doc.public_sharing;
  return doc.allow_anonymous_grants === false ? 'none' : 'allowed';
}

function Fixed({ field, children }: { field: string; children: React.ReactNode }) {
  const src = useContext(FixedFields).get(field);
  const [hover, setHover] = useState(false);
  if (!src) return <>{children}</>;
  // Not the design system's Tooltip: that is a one-line label for a value,
  // and this is a sentence saying where to go — on one line it runs off the
  // page. The same surface, allowed to wrap.
  return (
    <div
      style={{ position: 'relative', cursor: 'not-allowed' }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
    >
      <div style={{ pointerEvents: 'none' }}>{children}</div>
      {hover && (
        <div
          role="tooltip"
          style={{
            position: 'absolute',
            bottom: '100%',
            left: 0,
            zIndex: 40,
            maxWidth: '56ch',
            marginBottom: 'var(--space-1)',
            padding: 'var(--space-1) var(--space-2)',
            background: 'var(--paper-raised)',
            color: 'var(--ink)',
            border: 'var(--border-width) solid var(--rule-strong)',
            borderRadius: 'var(--radius-sm)',
            boxShadow: 'var(--shadow-popover)',
            font: 'var(--type-caption)',
            pointerEvents: 'none',
          }}
        >
          {setIn(src)}
        </div>
      )}
    </div>
  );
}

/**
 * How to turn the in-place upgrade on where Pando is deployed (R-355), shown
 * beside the switch: the image has to be a moving tag, or the next apply of
 * the deployment puts the old version back, and the setting itself is best
 * made there too, where it locks this switch.
 */
function InPlaceSetup() {
  const lines = [
    'services:',
    '  pando:',
    '    image: trypando/pando:latest',
    '    environment:',
    '      PANDO_POLICY_UPGRADE_IN_PLACE: "true"',
  ];
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
        Pando's image must use a moving tag that covers the new version, such as latest, 0.3 or 1. If
        docker-compose.yml or your infrastructure code pins an exact version, the next apply puts the old version
        back and Pando won't start against the upgraded database. To set this in the deployment, which also locks
        this switch:
      </p>
      <CodeBlock title="docker-compose.yml" lines={lines} copyable dense />
    </div>
  );
}

/**
 * The startup settings by area, in the order someone setting Pando up meets
 * them. A key not listed falls into its prefix's area, or Other, so a new
 * setting is never missing from the table — only, until it is listed here,
 * from the right place in it.
 */
const SETTING_AREAS: Array<{ name: string; prefix?: string; keys: string[] }> = [
  { name: 'Server', keys: ['server.addr', 'server.work_dir', 'server.shutdown_timeout'] },
  {
    name: 'Public addresses',
    keys: ['server.external_url', 'server.base_domain', 'server.issuer', 'server.proxy_upstream'],
  },
  { name: 'Routing', keys: ['server.routing_mode', 'server.port_range_start', 'server.port_range_end'] },
  { name: 'Database', prefix: 'database.', keys: ['database.connect_timeout'] },
  { name: 'App defaults', prefix: 'apps.', keys: ['apps.cpu_millis', 'apps.memory_bytes', 'apps.disk_bytes'] },
  {
    name: 'Reconciler',
    prefix: 'reconciler.',
    keys: ['reconciler.backoff', 'reconciler.failure_threshold', 'reconciler.failure_window', 'reconciler.gc_interval'],
  },
  {
    name: 'Background work',
    prefix: 'work.',
    keys: ['work.deploys', 'work.detections', 'work.backups', 'work.auto_deploy'],
  },
  { name: 'Retention', prefix: 'retention.', keys: [] },
  { name: 'Logging', prefix: 'log.', keys: ['log.level', 'log.development'] },
];

/** Where in SETTING_AREAS a key goes: its area's index, and its place in it. */
export function settingArea(key: string): { area: string; rank: number } {
  for (const [i, a] of SETTING_AREAS.entries()) {
    const at = a.keys.indexOf(key);
    if (at >= 0) return { area: a.name, rank: i * 100 + at };
  }
  for (const [i, a] of SETTING_AREAS.entries()) {
    if (a.prefix && key.startsWith(a.prefix)) return { area: a.name, rank: i * 100 + 99 };
  }
  return { area: 'Other', rank: SETTING_AREAS.length * 100 };
}

const STARTUP_GRID = 'minmax(0,3fr) minmax(0,2fr) minmax(12ch,1fr)';

/**
 * Every other setting Pando started with — the ones that are not policy and
 * that no screen edits — grouped by area, with its value and where it came
 * from. A value something set reads in full ink with its source as a tag; a
 * default reads quietly, so the few that were set stand out. Secrets are never
 * listed (R-194): the server leaves them out.
 */
function StartupSettings({ config }: { config?: StartupConfig }) {
  // POST /restart's verb: the same restart applies a saved adapter.
  const canRestart = useInstallVerb(InstallVerb.AdaptersManage);
  // At phone width a row stacks — the setting on its own line, its value and
  // source under it — rather than scrolling sideways: the value and where it
  // came from are the point, and a sideways scroll hides both.
  const narrow = useNarrow();
  if (!config) return null;

  const rows = config.settings
    .map((s) => ({ ...s, ...settingArea(s.key) }))
    .sort((a, b) => a.rank - b.rank || a.key.localeCompare(b.key));
  const set = rows.filter((r) => r.source.kind !== 'default').length;
  const code = { font: 'var(--type-code-sm)' } as const;
  const line = {
    display: 'grid',
    gridTemplateColumns: narrow ? 'minmax(0,1fr) auto' : STARTUP_GRID,
    alignItems: 'center',
    gap: narrow ? 'var(--space-1) var(--space-3)' : 'var(--space-4)',
    padding: narrow ? 'var(--space-2) var(--space-3)' : '0 var(--space-3)',
  } as const;

  return (
    <PolicySection
      heading="Startup configuration"
      note={
        set === 0
          ? 'Read once when Pando starts. Every setting is at its default.'
          : `Read once when Pando starts. ${set} of ${rows.length} are set; the rest are defaults.`
      }
    >
      <div className={narrow ? undefined : 'pando-table'} role="table" aria-label="Startup configuration">
        <div
          role="row"
          style={{
            ...line,
            // Stacked rows say what each part is by where it sits.
            display: narrow ? 'none' : 'grid',
            minHeight: 'var(--control-console)',
            background: 'var(--paper-sunken)',
            borderTop: 'var(--border-width) solid var(--rule)',
            borderBottom: 'var(--border-width) solid var(--rule)',
          }}
        >
          {['Setting', 'Value', 'Source'].map((h) => (
            <span key={h} role="columnheader" style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>
              {h}
            </span>
          ))}
        </div>
        {rows.map((row, i) => {
          const isDefault = row.source.kind === 'default';
          const empty = row.value === '' || row.value === null || row.value === undefined;
          return (
            <div key={row.key} role="rowgroup">
              {rows[i - 1]?.area !== row.area && (
                // The same quiet band as the adapters table's categories.
                <div
                  role="row"
                  style={{
                    padding: 'var(--space-4) var(--space-3) var(--space-1)',
                    font: 'var(--type-caption)',
                    color: 'var(--ink-secondary)',
                    borderBottom: 'var(--border-width) solid var(--rule)',
                  }}
                >
                  {row.area}
                </div>
              )}
              <div
                role="row"
                style={{ ...line, minHeight: 'var(--row-height)', borderBottom: 'var(--border-width) solid var(--rule)' }}
              >
                <span
                  role="cell"
                  style={{ ...code, color: 'var(--ink)', overflowWrap: 'anywhere', gridColumn: narrow ? '1 / -1' : undefined }}
                >
                  {row.key}
                </span>
                <span
                  role="cell"
                  style={{ ...code, color: isDefault ? 'var(--ink-secondary)' : 'var(--ink)', overflowWrap: 'anywhere' }}
                >
                  {empty ? '—' : String(row.value)}
                </span>
                <span role="cell">
                  <SetBy source={row.source} env={row.env} />
                </span>
              </div>
            </div>
          );
        })}
      </div>

      {/* How to change one, after the table it is about. The Compose command
          stays in plain sight: a restart keeps the old environment, and that
          is the mistake people make. */}
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', marginTop: 'var(--space-4)' }}>
        <p style={{ margin: 0, font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', maxWidth: '72ch' }}>
          To change a setting, set it at its source and restart Pando. With Docker Compose, set{' '}
          <code style={code}>PANDO_…</code> variables under <code style={code}>environment:</code> on the{' '}
          <code style={code}>pando</code> service and run <code style={code}>docker compose up -d pando</code>.{' '}
          <code style={code}>docker compose restart</code> keeps the old environment.{' '}
          {config.file ? (
            <>
              Config file: <code style={code}>{config.file}</code>. Environment variables override it.
            </>
          ) : (
            <>
              No config file is in use. Start Pando with <code style={code}>pando serve --config &lt;path&gt;</code>{' '}
              to read one.
            </>
          )}
        </p>
        {canRestart && (
          <div>
            <RestartButton />
          </div>
        )}
      </div>
    </PolicySection>
  );
}

/** Where a setting came from, as a tag; the exact variable or file on hover
 *  or focus. A default is plain words, quieter than a tag, since it is what
 *  most rows are. */
function SetBy({ source, env }: { source: Source; env: string }) {
  if (source.kind === 'default') {
    return (
      <TipButton
        tip={`Not set. Set ${env} to change it.`}
        label={`Default: not set. Set ${env} to change it.`}
        style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}
      >
        Default
      </TipButton>
    );
  }
  const exact =
    source.kind === 'env'
      ? (source.name ?? env)
      : `${source.name ?? 'the config file'}${source.key ? `, at ${source.key}` : ''}`;
  return (
    <TipButton tip={exact} label={`${source.kind === 'env' ? 'Environment' : 'File'}: ${exact}`}>
      <Tag tone="contour">{source.kind === 'env' ? 'Environment' : 'File'}</Tag>
    </TipButton>
  );
}

/**
 * One group of policy settings.
 *
 * A heading, a line saying which question the controls answer, and a rule above
 * it — the system's "sections separated by rules" rather than a card each,
 * because these are settings on one page and not objects in a list.
 */
function PolicySection({
  heading,
  note,
  children,
}: {
  heading: string;
  note?: string;
  children: React.ReactNode;
}) {
  return (
    <section
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-4)',
        padding: 'var(--space-6) 0',
        borderTop: 'var(--border-width) solid var(--rule)',
      }}
    >
      <div>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-1)' }}>{heading}</h4>
        {note && <Quiet>{note}</Quiet>}
      </div>
      {children}
    </section>
  );
}

interface PolicyControls {
  current: PolicyDoc;
  edit: (patch: Partial<PolicyDoc>) => void;
  locked: (field: string) => boolean;
}

/**
 * Where apps may connect out to (R-181 – R-185).
 *
 * Three questions, in the order an administrator answers them: which mode and
 * which list, whether private addresses are blocked — a separate switch that
 * works with any mode, including anywhere — and what an app may do to loosen
 * the rules for itself. Build egress is a different setting (R-189) and is not
 * here.
 */
function EgressPolicy({ current, edit, locked, fixed }: PolicyControls & { fixed: Map<string, Source> }) {
  const { mode, list } = installEgress(current);
  // The field from before issue #79, fixed at startup, decides the mode and
  // the list as well: writing the new fields here would be overridden by it,
  // or override it, and neither is what the file says.
  const legacyFixed = fixed.has('egress_allowlist') && !fixed.has('egress_mode');
  const modeField = legacyFixed ? 'egress_allowlist' : 'egress_mode';
  const listField = legacyFixed ? 'egress_allowlist' : 'egress_list';
  const modeLocked = locked(modeField) || locked('egress_list');
  const listLocked = locked(listField) || locked('egress_mode');
  const loosening = looseningRule(current);

  return (
    <PolicySection
      heading="Egress"
      note="Rules for traffic leaving apps. Apps can narrow them, and loosen them only as allowed below. Changes apply at each app’s next deploy."
    >
      <Fixed field={modeField}>
        <fieldset style={FIELDSET}>
          <legend style={LEGEND}>Mode</legend>
          {EGRESS_MODES.map(([value, label, description]) => (
            <Radio
              key={value}
              name="egress_mode"
              value={value}
              label={label}
              description={description}
              checked={mode === value}
              disabled={modeLocked}
              onChange={() => edit(egressPatch(value, list))}
            />
          ))}
        </fieldset>
      </Fixed>

      {mode !== 'allow_all' && (
        <Fixed field={listField}>
          <ListField
            label={mode === 'allowlist' ? 'Allowlist' : 'Denylist'}
            disabled={listLocked}
            value={list}
            helper={ENTRY_FORMS}
            onChange={(next) => edit(egressPatch(mode, next))}
          />
        </Fixed>
      )}

      <Fixed field="egress_block_private">
        <Switch
          checked={current.egress_block_private ?? false}
          disabled={locked('egress_block_private')}
          label="Block private addresses"
          description="Apps can’t reach the local network, this host’s loopback, or a cloud metadata address. Checked against the resolved address."
          onChange={(e) => edit({ egress_block_private: e.target.checked })}
        />
      </Fixed>

      <Fixed field="egress_loosening">
        <fieldset style={FIELDSET}>
          <legend style={LEGEND}>Loosening</legend>
          <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-1)' }}>
            Adding to the allowlist, removing from the denylist, or turning off private-address blocking for
            one app. Narrowing is always allowed.
          </p>
          {LOOSENING.map(([value, label, description]) => (
            <Radio
              key={value}
              name="egress_loosening"
              value={value}
              label={label}
              description={description}
              checked={loosening === value}
              disabled={locked('egress_loosening')}
              onChange={() => edit({ egress_loosening: value })}
            />
          ))}
        </fieldset>
      </Fixed>
    </PolicySection>
  );
}

/**
 * Who has to sign off on a deploy before it runs (R-154 – R-156).
 *
 * Off unless something here, or an app's own settings, turns it on. Each
 * setting says what it does to auto-deploy, because that is the consequence
 * an administrator finds out about later otherwise (R-158).
 */
function DeployApprovalPolicy({ current, edit, locked }: PolicyControls) {
  const chosen = current.deploy_approval_apps ?? [];
  // Only the chosen apps are drawn, as tags, and others are found by typing:
  // never every app in the install, which can be twenty thousand (issue #72).
  const [search, setSearch] = useState('');
  const settled = useSettled(search.trim());
  type Named = { id: string; name: string };
  // The chosen apps' names. Asked for by ID, a hundred at a time being as many
  // as the API returns in one page; past that a tag shows the app's ID.
  const named = chosen.slice(0, 100);
  const picked = useQuery({
    queryKey: ['apps', 'approval-policy', 'picked', named],
    queryFn: async () =>
      named.length > 0
        ? ((await api.get<{ apps: Named[] | null }>(withParams('/apps', { id: named, limit: 100 }))).apps ?? [])
        : [],
    placeholderData: (previous) => previous,
    retry: false,
  });
  const found = useQuery({
    queryKey: ['apps', 'approval-policy', 'search', settled],
    queryFn: async () =>
      (await api.get<{ apps: Named[] | null }>(withParams('/apps', { q: settled, limit: 20 }))).apps ?? [],
    placeholderData: (previous) => previous,
    retry: false,
  });
  const nameOf = (id: string) => picked.data?.find((a) => a.id === id)?.name ?? id;
  // An app that was chosen and has since gone — or that this account cannot
  // see — keeps a tag with its ID, so it can still be taken off.
  const unknown = picked.isSuccess ? named.filter((id) => !picked.data.some((a) => a.id === id)) : [];
  const everyApp = current.deploy_approval_required ?? false;

  return (
    <PolicySection
      heading="Deploy approval"
      note="Deploys wait for approval before they run. Rollbacks to a version that already ran, restarts and secret rotations never wait."
    >
      <Fixed field="deploy_approval_required">
        <Switch
          checked={everyApp}
          disabled={locked('deploy_approval_required')}
          label="Require approval for every app"
          description="Every app’s deploys wait for approval, and auto-deploy stops on every app."
          onChange={(e) => edit({ deploy_approval_required: e.target.checked })}
        />
      </Fixed>

      <Fixed field="deploy_approval_apps">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
          <TagField
            label="Apps that always need approval"
            value={chosen}
            onChange={(ids) => edit({ deploy_approval_apps: ids })}
            nameOf={nameOf}
            text={search}
            onText={setSearch}
            options={(found.data ?? []).map((a) => ({ id: a.id, name: a.name }))}
            placeholder="Type an app’s name"
            disabled={locked('deploy_approval_apps') || everyApp}
            helper={
              everyApp
                ? 'Has no effect while every app requires approval.'
                : 'Owners can’t turn this off.'
            }
          />
          {found.isSuccess && settled !== '' && found.data.length === 0 && (
            <Quiet>{`No apps match “${settled}”.`}</Quiet>
          )}
          {(picked.isError || found.isError) && <Quiet>{messageOf(picked.error ?? found.error)}</Quiet>}
          {unknown.length > 0 && (
            <Quiet>
              {unknown.length === 1
                ? `Pando can’t find ${unknown[0]}. It may have been deleted. Take it off with ×.`
                : `Pando can’t find ${unknown.length} of these apps: ${unknown.join(', ')}. They may have been deleted. Take them off with ×.`}
            </Quiet>
          )}
        </div>
      </Fixed>

      <Fixed field="deploy_approval_count">
        <Input
          label="Approvals needed"
          type="number"
          min={1}
          disabled={locked('deploy_approval_count')}
          value={String(approvalsNeeded(current.deploy_approval_count))}
          helper="Number of different people. One rejection ends the request."
          onChange={(e) => edit({ deploy_approval_count: Math.max(1, Math.round(Number(e.target.value) || 1)) })}
        />
      </Fixed>

      <Fixed field="deploy_approval_expiry_hours">
        <Input
          label="Request expiry (hours)"
          type="number"
          min={0}
          disabled={locked('deploy_approval_expiry_hours')}
          value={String(current.deploy_approval_expiry_hours ?? 0)}
          helper="Zero means never. The default is 168, one week."
          onChange={(e) =>
            edit({ deploy_approval_expiry_hours: Math.max(0, Math.round(Number(e.target.value) || 0)) })
          }
        />
      </Fixed>
    </PolicySection>
  );
}

const FIELDSET: React.CSSProperties = {
  border: 0,
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: 'var(--space-3)',
};

const LEGEND: React.CSSProperties = { font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' };

/** 0 to 100, because a threshold outside it is a threshold nothing can meet. */
function clamp(n: number): number {
  if (Number.isNaN(n) || n < 0) return 0;
  if (n > 100) return 100;
  return Math.round(n);
}

export interface AuditRecord {
  id: number;
  occurred_at: string;
  principal_kind: string;
  principal_id?: string;
  on_behalf_of?: string;
  action: string;
  app_id?: string;
  target_kind?: string;
  target_id?: string;
  request_id?: string;
  // What R-379 adds. Absent on events written before it.
  /** success, denied or failed. */
  outcome?: string;
  /** The client's address, from the trusted forwarding chain. */
  source_ip?: string;
  /** The address the connection came from: the proxy in front, if any. */
  peer_ip?: string;
  user_agent?: string;
  /** The actor's name and email when the event was written, so an account
   *  deleted since still reads as who it was. */
  actor_name?: string;
  actor_email?: string;
  schema_version?: number;
}

// The kinds of thing the server records events against.
const TARGET_KINDS = [
  'app',
  'user',
  'group',
  'role',
  'grant',
  'token',
  'session',
  'secret',
  'backup',
  'deployment',
  'policy',
  'adapter',
  'volume',
  'slot',
  'spec_revision',
  'workload',
  'launcher_section',
];

/** The audit log, paged on the server's cursor, for one set of filters. */
export function useAuditLog(filters: AuditFilters) {
  // Pages on the server's cursor (design 04 §2.8): "Load older events" asks
  // for the page before the last one shown, so events arriving meanwhile never
  // shift what has already been read.
  const log = useInfiniteQuery({
    queryKey: ['audit', filters],
    initialPageParam: '',
    queryFn: ({ pageParam }) =>
      api.get<{ events: AuditRecord[] | null; next_before: string }>('/audit' + auditQuery(filters, pageParam || undefined)),
    getNextPageParam: (last) => last.next_before || undefined,
  });
  const events = log.data?.pages.flatMap((p) => p.events ?? []) ?? [];
  return { log, events };
}

/** Names for the "who" column and the actor pickers: the accounts the events
 *  on screen and the chosen filters name, asked about by ID (issue #72), not
 *  the install's whole list. install.audit.read does not imply install.view —
 *  an account can hold only the first — so when the lookup is refused, the
 *  pickers take an ID and the columns show IDs. */
export function usePeople(events: AuditRecord[], chosen: string[] = []): Person[] {
  return useNamedPeople([
    ...chosen,
    ...events.flatMap((e) => [e.principal_id, e.on_behalf_of, e.target_kind === 'user' ? e.target_id : undefined]),
  ]);
}

export function Audit({
  initial = NO_FILTERS,
  onFilters,
  onAdapters,
}: {
  /** Filters carried in from a link, such as an account's page. */
  initial?: AuditFilters;
  /** Told of every change, so the address bar can hold the filters and a
   *  reload or a copied link shows the same events. */
  onFilters?: (f: AuditFilters) => void;
  /** Opens the Adapters screen, where audit sinks are configured. */
  onAdapters?: () => void;
}) {
  const [filters, setFilters] = useState<AuditFilters>(initial);
  const change = (next: AuditFilters) => {
    setFilters(next);
    onFilters?.(next);
  };
  const set = (patch: Partial<AuditFilters>) => change({ ...filters, ...patch });
  const clear = () => change(NO_FILTERS);
  const filtered = JSON.stringify(filters) !== JSON.stringify(NO_FILTERS);

  const { log, events } = useAuditLog(filters);
  const people = usePeople(events, [filters.actor, filters.involving]);

  // Asking a question with AI (R-345), behind one button. Its answer becomes
  // the filters below, where it can be read and changed; the table is the
  // same table, so nothing shown depends on the AI's word.
  const searchOn = useAIFunctionOn('search_audit');
  const [asking, setAsking] = useState(false);

  const custom = filters.when === 'custom';
  const range = (
    <Field>
      <Select
        label="Time range"
        value={filters.when}
        options={WHEN.map((w) => ({ value: w.value, label: w.label }))}
        onChange={(e) => set({ when: e.target.value })}
      />
    </Field>
  );

  return (
    <Screen heading="Audit log" action={searchOn && <AIButton onClick={() => setAsking(true)} />}>
      {asking && <AuditAI onClose={() => setAsking(false)} onShow={(f) => change(filtersFromSearch(f))} />}

      {/* R-385: every copy of this log that leaves the installation, said
          where the log is read rather than only where it is configured. */}
      <AuditSinks onAdapters={onAdapters} />

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', marginBottom: 'var(--space-5)' }}>
        <FilterRow>
          <Field>
            <Input
              label="Action"
              mono
              value={filters.action}
              placeholder="Prefix, e.g. app."
              onChange={(e) => set({ action: e.target.value })}
            />
          </Field>

          <Field>
            <Input
              label="App ID"
              mono
              value={filters.app}
              placeholder="app_…"
              onChange={(e) => set({ app: e.target.value })}
            />
          </Field>

          <Field>
            {/* Typed, not picked from a list: an installation has too many
                accounts for a dropdown to be usable. Without the accounts list
                (no install.view) it still takes an ID. */}
            <ActorField people={people} value={filters.actor} onChange={(actor) => set({ actor })} />
          </Field>

          <Field>
            <Select
              label="Target type"
              value={filters.targetKind}
              options={[{ value: '', label: 'Any' }, ...TARGET_KINDS.map((k) => ({ value: k, label: k }))]}
              onChange={(e) => set({ targetKind: e.target.value })}
            />
          </Field>

          <Field>
            <Input
              label="Target ID"
              mono
              placeholder="app_…"
              value={filters.targetID}
              onChange={(e) => set({ targetID: e.target.value })}
            />
          </Field>

          {/* The one filter that is an OR: this account as the actor, or as
              the target. What an account's page links here with. */}
          <Field>
            <ActorField
              label="Actor or target"
              pando={false}
              people={people}
              value={filters.involving}
              onChange={(involving) => set({ involving })}
            />
          </Field>

          {!custom && range}

          {filtered && !custom && <ClearFilters onClear={clear} />}
        </FilterRow>

        {/* A custom range is three fields that belong together, so they take
            a row of their own rather than wrapping one at a time onto it. */}
        {custom && (
          <FilterRow>
            {range}
            <Field>
              <Input
                label="From"
                type="datetime-local"
                value={filters.since}
                onChange={(e) => set({ since: e.target.value })}
              />
            </Field>
            <Field>
              <Input
                label="To"
                type="datetime-local"
                value={filters.until}
                onChange={(e) => set({ until: e.target.value })}
              />
            </Field>
            <ClearFilters onClear={clear} />
          </FilterRow>
        )}
      </div>

      {log.isError && <Quiet>{messageOf(log.error)}</Quiet>}

      <AuditTable
        events={events}
        people={people}
        loading={log.isPending}
        // A filter that matches nothing and a log that holds nothing look
        // identical as an empty table, and on this screen "nothing happened"
        // and "your filter is wrong" are very different answers.
        empty={
          <EmptyState heading={filtered ? 'No matching events' : 'No events recorded'}>
            {filtered ? 'Action is a prefix match: app. matches every app event.' : null}
          </EmptyState>
        }
      />

      <LoadOlder log={log} />

      <ArchivedMonths />
    </Screen>
  );
}

/** The log's rows: time, action, actor, target. */
export function AuditTable({
  events,
  people,
  empty,
  loading = false,
}: {
  events: AuditRecord[];
  people: Person[];
  empty: React.ReactNode;
  /** The first page has not arrived. A change of filter is a new query, so
   *  this is also what shows between one filter and its results — rather than
   *  "No matching events" for a moment before there are some. */
  loading?: boolean;
}) {
  const nameOf = (id?: string) => (id ? (people.find((u) => u.id === id)?.external_id ?? id) : '');
  // The name the event recorded comes first (R-379): it is who the actor was
  // when it happened, and needs no lookup. Older events have none.
  const actorOf = (row: AuditRecord) => row.actor_name || nameOf(row.principal_id);
  return (
    <Table
      dense
      loading={loading}
      skeletonRows={6}
      empty={empty}
      columns={[
        {
          key: 'occurred_at',
          header: 'Time',
          width: '20ch',
          muted: true,
          render: (row: AuditRecord) => new Date(row.occurred_at).toLocaleString(),
        },
        {
          key: 'action',
          header: 'Action',
          width: 'minmax(0,30ch)',
          mono: true,
          // Success is most of the log and says nothing; a refusal or a
          // failure is what someone scanning it is looking for.
          render: (row: AuditRecord) => (
            <span style={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2)' }}>
              {row.action}
              <Outcome outcome={row.outcome} />
            </span>
          ),
        },
        {
          key: 'principal_id',
          header: 'Actor',
          width: 'minmax(0,20ch)',
          mono: true,
          // A delegated token records both itself and the person it acted
          // for (R-229). Showing only one of them is how "who did this"
          // stops being answerable.
          render: (row: AuditRecord) => {
            const who =
              row.on_behalf_of && row.on_behalf_of !== row.principal_id
                ? `${actorOf(row)} for ${nameOf(row.on_behalf_of)}`
                : actorOf(row) || row.principal_kind;
            const more = [row.actor_email, row.actor_name ? row.principal_id : undefined].filter(Boolean).join(' · ');
            return more ? <Tooltip content={more}>{who}</Tooltip> : who;
          },
        },
        {
          key: 'target_id',
          header: 'Target',
          width: 'minmax(0,24ch)',
          mono: true,
          muted: true,
          render: (row: AuditRecord) =>
            row.target_id
              ? `${row.target_kind ? row.target_kind + ' ' : ''}${row.target_kind === 'user' ? nameOf(row.target_id) : row.target_id}`
              : row.app_id || '—',
        },
        {
          key: 'source_ip',
          header: 'Source IP',
          width: 'minmax(0,16ch)',
          mono: true,
          muted: true,
          // The user agent and the connecting address are there on hover:
          // needed when investigating, noise when scanning.
          render: (row: AuditRecord) => {
            if (!row.source_ip) return '—';
            const more = [
              row.peer_ip && row.peer_ip !== row.source_ip ? `Through ${row.peer_ip}` : '',
              row.user_agent ?? '',
            ]
              .filter(Boolean)
              .join(' · ');
            return more ? <Tooltip content={more}>{row.source_ip}</Tooltip> : row.source_ip;
          },
        },
      ]}
      rows={events}
    />
  );
}

/** An event's outcome when it was not a success, as a symbol and a word:
 *  denied is a refusal, failed is an attempt that went wrong, and they read
 *  differently. */
function Outcome({ outcome }: { outcome?: string }) {
  if (outcome === 'denied') return <StatusIndicator status="stopped" label="Denied" />;
  if (outcome === 'failed') return <StatusIndicator status="failed" label="Failed" />;
  return null;
}

interface AuditArchive {
  id: string;
  month: string;
  adapter_ref?: string;
  row_count: number;
  size_bytes: number;
  sha256: string;
}

/**
 * Months past retention (R-347): archived, checked, and removed from the live
 * log, so the table above no longer finds them. Each downloads as gzipped JSON
 * lines from the same endpoint the CLI uses.
 */
function ArchivedMonths() {
  const archives = useQuery({
    queryKey: ['audit-archives'],
    queryFn: () => api.get<{ archives: AuditArchive[] }>('/audit/archives'),
  });
  const rows = archives.data?.archives ?? [];
  return (
    <section
      id={ARCHIVES_ANCHOR}
      style={{ marginTop: 'var(--space-6)', paddingTop: 'var(--space-6)', borderTop: 'var(--border-width) solid var(--rule)' }}
    >
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-1)' }}>Archived months</h4>
      <Quiet>Months past the retention set in Policy.</Quiet>
      {archives.isError && <Quiet>{messageOf(archives.error)}</Quiet>}
      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          dense
          loading={archives.isPending}
          skeletonRows={2}
          empty={<EmptyState heading="No months archived" />}
          columns={[
            { key: 'month', header: 'Month', width: '10ch', mono: true },
            { key: 'row_count', header: 'Events', width: '12ch', render: (row: AuditArchive) => row.row_count.toLocaleString() },
            { key: 'adapter_ref', header: 'Destination', width: 'minmax(0,20ch)', mono: true, render: (row: AuditArchive) => row.adapter_ref || 'Pando' },
            {
              key: 'sha256',
              header: 'SHA-256',
              width: 'minmax(0,1fr)',
              mono: true,
              muted: true,
              render: (row: AuditArchive) => <Tooltip content={row.sha256}>{row.sha256.slice(0, 16) + '…'}</Tooltip>,
            },
            {
              key: 'id',
              header: '',
              width: '12ch',
              render: (row: AuditArchive) => (
                <a href={`${base}/audit/archives/${encodeURIComponent(row.id)}`} download>
                  Download
                </a>
              ),
            },
          ]}
          rows={rows}
        />
      </div>
    </section>
  );
}

export function LoadOlder({ log }: { log: ReturnType<typeof useAuditLog>['log'] }) {
  if (!log.hasNextPage) return null;
  return (
    <div style={{ marginTop: 'var(--space-4)' }}>
      <Button variant="secondary" disabled={log.isFetchingNextPage} onClick={() => void log.fetchNextPage()}>
        {log.isFetchingNextPage ? 'Loading' : 'Load older events'}
      </Button>
    </div>
  );
}

function FilterRow({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-end', gap: 'var(--space-3) var(--space-4)' }}>
      {children}
    </div>
  );
}

function ClearFilters({ onClear }: { onClear: () => void }) {
  return (
    <BesideField>
      <Button variant="ghost" onClick={onClear}>
        Clear filters
      </Button>
    </BesideField>
  );
}

function Field({ children }: { children: React.ReactNode }) {
  return <div style={{ flex: '1 1 18ch', minWidth: '18ch', maxWidth: '28ch' }}>{children}</div>;
}

type GroupedRow = AdapterRow & { first?: boolean; kindName?: string; connection?: SourceConnection };

/** Reachable or not — or, saved since Pando started, not running yet. */
function AdapterStatus({ row }: { row: AdapterRow }) {
  // Replaced by one the config file declares (R-271): not running, and not
  // broken either. It applies again when the declaration is removed.
  if (row.status === 'overridden') return <StatusIndicator status="stopped" label="Replaced by config file" />;
  if (row.pending_restart) return <StatusIndicator status="info" label="Restart to apply" />;
  // Reachable is not the same as working: a Traefik that could not start
  // leaves every app behind it unreachable, and says why to those who can fix it.
  if (row.healthy !== false && row.edge && !row.edge.running) {
    return <StatusIndicator status="failed" label="Not running" title={row.edge.message} />;
  }
  return row.healthy === false ? (
    <StatusIndicator status="failed" label="Unreachable" />
  ) : (
    <StatusIndicator status="running" label="Reachable" />
  );
}

/** A source connection's state (R-091): whether it can read repositories now.
 *  Not reachability — a connection is built when it is used, and the question
 *  is whether it holds what it needs. */
function ConnectionStatus({ connection }: { connection: SourceConnection }) {
  if (connection.problem) return <StatusIndicator status="failed" label="Cannot be used" title={connection.problem} />;
  if (!connection.capabilities.authorized) return <StatusIndicator status="info" label="Not authorized" />;
  return <StatusIndicator status="running" label="Ready" title={covers(connection)} />;
}

// The adapters' columns: name, ID, status, and the actions for whoever may.
const ADAPTER_GRID = 'minmax(0,1fr) minmax(0,22ch) 16ch max-content';

/**
 * The adapters, grouped by category.
 *
 * Not the design system's Table, which has no way to mark where a group begins:
 * the same header and row styles, one grid for every row so the columns line
 * up from group to group, and a quiet line above each group naming its
 * category.
 */
function GroupedAdapters({
  rows,
  canManage,
  onChange,
  onAuthorize,
  onDisconnect,
}: {
  rows: GroupedRow[];
  canManage: boolean;
  onChange: (row: AdapterRow) => void;
  onAuthorize: (c: SourceConnection) => void;
  onDisconnect: (c: SourceConnection) => void;
}) {
  const cell = { minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;
  const line = {
    display: 'grid',
    gridTemplateColumns: ADAPTER_GRID,
    alignItems: 'center',
    gap: 'var(--space-4)',
    padding: '0 var(--space-3)',
  } as const;

  return (
    <div className="pando-table" role="table" aria-label="Adapters">
      <div
        role="row"
        style={{
          ...line,
          minHeight: 'var(--control-console)',
          background: 'var(--paper-sunken)',
          borderTop: 'var(--border-width) solid var(--rule)',
          borderBottom: 'var(--border-width) solid var(--rule)',
        }}
      >
        {['Adapter', 'ID', 'Status', ''].map((h) => (
          <span key={h || 'actions'} role="columnheader" style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>
            {h}
          </span>
        ))}
      </div>

      {rows.map((row) => (
        <div key={row.id} role="rowgroup">
          {row.first && (
            // Quiet on purpose: the name, small and secondary, with a little
            // room above it — enough to see where a group starts, and no more.
            <div
              role="row"
              style={{
                padding: 'var(--space-4) var(--space-3) var(--space-1)',
                font: 'var(--type-caption)',
                color: 'var(--ink-secondary)',
                borderBottom: 'var(--border-width) solid var(--rule)',
              }}
            >
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)' }}>
                {categoryLabel(row.category)}
                {/* What this kind of adapter is for, on hover or focus, so the
                    category names explain themselves without a paragraph each. */}
                {categoryNote(row.category) && (
                  <TipButton
                    tip={categoryNote(row.category)}
                    label={`About ${categoryLabel(row.category)} adapters: ${categoryNote(row.category)}`}
                    side="right"
                  >
                    <Icon name="info" size={14} />
                  </TipButton>
                )}
              </span>
            </div>
          )}
          <div
            role="row"
            style={{
              ...line,
              minHeight: 'var(--row-height)',
              borderBottom: 'var(--border-width) solid var(--rule)',
            }}
          >
            <span style={{ ...cell, font: 'var(--type-body-ui)' }}>{row.kindName ?? row.kind}</span>
            <span style={{ ...cell, font: 'var(--type-code-sm)', color: 'var(--ink-secondary)' }}>{row.id}</span>
            <span style={cell}>
              {/* Live, not stored: an adapter that was reachable at startup and
                  is not now is exactly what this column exists to show. */}
              {row.connection ? <ConnectionStatus connection={row.connection} /> : <AdapterStatus row={row} />}
            </span>
            <span style={{ justifySelf: 'end' }}>
              {/* Declared in the config file, so read-only here while it is
                  (R-271); the file is where it changes. */}
              {row.declared ? (
                <span title={declaredAt(row)} style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
                  {(row.source as { kind?: string } | undefined)?.kind === 'env' ? 'Set at startup' : 'Set in config file'}
                </span>
              ) : (
                canManage &&
                row.status !== 'overridden' && (
                  <span style={{ display: 'inline-flex', gap: 'var(--space-2)' }}>
                    {/* A source connection signed in with OAuth is authorized
                        here, and disconnected here (R-091). */}
                    {row.connection &&
                      (row.connection.capabilities.device_authorization ||
                        row.connection.capabilities.web_authorization) && (
                        <Button variant="secondary" onClick={() => onAuthorize(row.connection!)}>
                          {row.connection.capabilities.authorized ? 'Authorize again' : 'Authorize'}
                        </Button>
                      )}
                    <Button variant="secondary" onClick={() => onChange(row)}>
                      Edit
                    </Button>
                    {row.connection && (
                      <Button variant="ghost" onClick={() => onDisconnect(row.connection!)}>
                        Disconnect
                      </Button>
                    )}
                  </span>
                )
              )}
            </span>
          </div>
        </div>
      ))}
    </div>
  );
}

/** The adapters table's columns: the category on the first row of each group,
 *  then the adapter by name, its ID, whether it is reachable, and Edit. */
function adapterColumns(
  canManage: boolean,
  setEditing: (e: { existing?: AdapterRow; category?: string }) => void,
) {
  type Row = AdapterRow & { first?: boolean; kindName?: string };
  return [
    {
      key: 'category',
      header: 'Category',
      width: '14ch',
      render: (row: Row) =>
        row.first ? <span style={{ font: 'var(--type-label)' }}>{categoryLabel(row.category)}</span> : null,
    },
    {
      key: 'kind',
      header: 'Adapter',
      width: 'minmax(0,1fr)',
      render: (row: Row) => row.kindName ?? row.kind,
    },
    { key: 'id', header: 'ID', width: 'minmax(0,22ch)', mono: true, muted: true },
    {
      key: 'healthy',
      header: 'Status',
      width: '16ch',
      render: (row: Row) => (
        // Live, not stored: an adapter that was reachable at startup and
        // is not now is exactly what this column exists to show.
        <AdapterStatus row={row} />
      ),
    },
    ...(canManage
      ? [
          {
            key: 'actions',
            header: '',
            width: '12ch',
            align: 'right' as const,
            render: (row: Row) => (
              <Button variant="secondary" onClick={() => setEditing({ existing: row })}>
                Edit
              </Button>
            ),
          },
        ]
      : []),
  ];
}

/** GET /adapters has returned both shapes during this phase; accept either
 *  rather than break the screen on the one that turns out to be current. */
function normalize(data: { adapters: AdapterRow[] } | AdapterRow[] | undefined): AdapterRow[] {
  if (!data) return [];
  if (Array.isArray(data)) return data;
  return data.adapters ?? [];
}
