// Policy, one page at a time: each a raised card of short setting rows — a
// label, at most one line under it, and the control on the right.
//
// Switches are phrased as what is on: "Terminal access", not "Disable
// terminal access". The API keeps its fields (disabled_verbs,
// disable_password_sign_in, …), so the value is turned over here, at the
// boundary, and nowhere else.
//
// A field fixed in the startup configuration (R-271) is shown as it stands,
// disabled, with a lock and where it is set.

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Banner, Button, CodeBlock, Icon, Input, Select, Switch, Tag } from '@design';

import { api } from '@api/client';
import type { PolicyPage } from '../app/route';
import { ListField } from '../ui/ListField';
import { TagField } from '../ui/TagField';
import { TipButton } from '../ui/TipButton';
import { Table } from '../ui/Table';
import { useSettled, withParams } from '../ui/paged';
import { Quiet, messageOf } from './Accounts';
import { EGRESS_MODES, LOOSENING, approvalsNeeded, egressPatch, installEgress, looseningRule } from './policyEgress';
import { changesLabel, publicSharing, usePolicy } from './policyDraft';
import type { Source } from './policyDraft';

/** Each page's name, in the order the navigation lists them. */
export const POLICY_LABELS: Record<PolicyPage, string> = {
  sources: 'Sources and builds',
  access: 'Access',
  egress: 'Egress',
  deployments: 'Deployments',
  tokens: 'Tokens',
  apps: 'Apps',
  audit: 'Audit log',
};

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

/** A card of setting rows. */
function SettingsCard({ children }: Readonly<{ children: React.ReactNode }>) {
  return <section className="pando-setcard">{children}</section>;
}

/**
 * One setting: its label and at most one line under it, and its control on
 * the right — or under it, for one too wide to sit beside it. `field` is the
 * policy field it sets, for the lock when the startup configuration fixes it.
 */
function SettingRow({
  label,
  caption,
  field,
  wide = false,
  children,
}: Readonly<{
  label: string;
  caption?: React.ReactNode;
  field?: string;
  wide?: boolean;
  children: React.ReactNode;
}>) {
  const src = useFixed(field);
  return (
    <div className={wide ? 'pando-setrow pando-setrow--wide' : 'pando-setrow'}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
        <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{label}</span>
        {caption && <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{caption}</span>}
      </div>
      <div className="pando-setrow-control">
        {src && <Locked source={src} />}
        {children}
      </div>
    </div>
  );
}

function useFixed(field?: string): Source | undefined {
  const { fixed } = usePolicy();
  return field ? fixed.get(field) : undefined;
}

/** "Set in config file", with the lock, and the whole of where on hover. */
function Locked({ source }: Readonly<{ source: Source }>) {
  const where = source.kind === 'env' ? 'Set in environment' : 'Set in config file';
  return (
    <TipButton tip={setIn(source)} label={`${where}. ${setIn(source)}`}>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)', font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
        <Icon name="lock" size={14} />
        {where}
      </span>
    </TipButton>
  );
}

/** A switch for one row: the row's label is its name. */
function Toggle({
  label,
  on,
  field,
  onChange,
  disabled = false,
}: Readonly<{ label: string; on: boolean; field: string; onChange: (on: boolean) => void; disabled?: boolean }>) {
  const { locked } = usePolicy();
  return (
    <Switch aria-label={label} checked={on} disabled={locked(field) || disabled} onChange={(e) => onChange(e.target.checked)} />
  );
}

/** A switch row: a label, its line, and the switch. */
function SwitchRow({
  label,
  caption,
  field,
  on,
  onChange,
  disabled,
}: Readonly<{ label: string; caption?: string; field: string; on: boolean; onChange: (on: boolean) => void; disabled?: boolean }>) {
  return (
    <SettingRow label={label} caption={caption} field={field}>
      <Toggle label={label} on={on} field={field} onChange={onChange} disabled={disabled} />
    </SettingRow>
  );
}

/** A few short choices as one segmented control, radios underneath. */
function Segmented<T extends string>({
  name,
  label,
  value,
  options,
  onChange,
  disabled = false,
}: Readonly<{
  name: string;
  label: string;
  value: T;
  options: ReadonlyArray<readonly [T, string]>;
  onChange: (value: T) => void;
  disabled?: boolean;
}>) {
  return (
    <div className="pando-seg" role="radiogroup" aria-label={label} aria-disabled={disabled || undefined}>
      {options.map(([v, text]) => (
        <label key={v} data-on={v === value}>
          <input type="radio" name={name} value={v} checked={v === value} disabled={disabled} onChange={() => onChange(v)} />
          {text}
        </label>
      ))}
    </div>
  );
}

/** A whole number from a field, at least min. */
function whole(value: string, min = 0, fallback = min): number {
  return Math.max(min, Math.round(Number(value) || fallback));
}

/** A number row. */
function NumberRow({
  label,
  caption,
  field,
  value,
  min = 0,
  onChange,
  disabled,
}: Readonly<{
  label: string;
  caption?: string;
  field: string;
  value: number;
  min?: number;
  onChange: (n: number) => void;
  disabled?: boolean;
}>) {
  const { locked } = usePolicy();
  return (
    <SettingRow label={label} caption={caption} field={field}>
      <Input
        aria-label={label}
        type="number"
        min={min}
        disabled={locked(field) || disabled}
        value={String(value)}
        onChange={(e) => onChange(whole(e.target.value, min))}
      />
    </SettingRow>
  );
}

/** One verb or pattern per line, as a list. */
function lines(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean);
}

/**
 * The isolation floors, as the spec names them (design 01 §3). Ordered
 * integers underneath, because policy floors are compared (R-114).
 */
const ISOLATION = [
  { value: '', label: 'No floor' },
  { value: '10', label: 'Container' },
  { value: '20', label: 'Sandboxed' },
  { value: '30', label: 'Virtual machine' },
  { value: '40', label: 'Dedicated host' },
];

/** The page asked for. */
export function PolicyPageView({ page }: Readonly<{ page: PolicyPage }>) {
  const { policy } = usePolicy();
  if (policy.isError) return <Quiet>{messageOf(policy.error)}</Quiet>;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      {page === 'sources' && <SourcesPage />}
      {page === 'access' && <AccessPage />}
      {page === 'egress' && <EgressPage />}
      {page === 'deployments' && <DeploymentsPage />}
      {page === 'tokens' && <TokensPage />}
      {page === 'apps' && <AppsPage />}
      {page === 'audit' && <AuditPage />}
    </div>
  );
}

function Lede({ children }: Readonly<{ children: React.ReactNode }>) {
  return <p style={{ margin: 0, font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{children}</p>;
}

function SourcesPage() {
  const { current, edit, locked } = usePolicy();
  const score = current.min_security_score ?? 0;
  const stops = current.insecure_action === 'stop';
  return (
    <SettingsCard>
      <SettingRow label="Allowed sources" caption="One host pattern per line, such as github.com/acme/*. Empty is anywhere." field="source_allowlist" wide>
        <Input
          aria-label="Allowed sources"
          as="textarea"
          rows={3}
          mono
          disabled={locked('source_allowlist')}
          value={(current.source_allowlist ?? []).join('\n')}
          onChange={(e) => edit({ source_allowlist: lines(e.target.value) })}
        />
      </SettingRow>
      <SettingRow label="Build isolation" caption="Builders that can't meet it are refused." field="min_build_isolation">
        <Select
          aria-label="Build isolation"
          disabled={locked('min_build_isolation')}
          value={String(current.min_build_isolation ?? '')}
          options={ISOLATION}
          onChange={(e) => edit({ min_build_isolation: Number(e.target.value) || undefined })}
        />
      </SettingRow>
      <SettingRow label="Runtime isolation" field="min_runtime_isolation">
        <Select
          aria-label="Runtime isolation"
          disabled={locked('min_runtime_isolation')}
          value={String(current.min_runtime_isolation ?? '')}
          options={ISOLATION}
          onChange={(e) => edit({ min_runtime_isolation: Number(e.target.value) || undefined })}
        />
      </SettingRow>
      {/* R-313: the consequence is said where it is set. */}
      <NumberRow
        label="Minimum security score"
        caption="0 to 100. Zero is off. An app below it is refused at deployment."
        field="min_security_score"
        value={score}
        onChange={(n) => edit({ min_security_score: Math.min(100, n) })}
      />
      {score > 0 && (
        // R-315: opt-in, with a grace period.
        <SwitchRow
          label="Stop apps below the minimum"
          caption="The owner is told first."
          field="insecure_action"
          on={stops}
          onChange={(on) => edit({ insecure_action: on ? 'stop' : 'warn' })}
        />
      )}
      {score > 0 && stops && (
        <NumberRow
          label="Grace period, in hours"
          field="insecure_grace_hours"
          value={current.insecure_grace_hours ?? 24}
          min={1}
          onChange={(n) => edit({ insecure_grace_hours: n })}
        />
      )}
      {/* R-313a: what is counted is what is shown, both ways round. */}
      <SwitchRow
        label="Count findings with no fix"
        field="ignore_unfixable_findings"
        on={!current.ignore_unfixable_findings}
        onChange={(on) => edit({ ignore_unfixable_findings: !on })}
      />
      {/* R-336, R-337: saying no to sending plans to a provider should not
          mean deleting somebody's adapter. */}
      <SwitchRow
        label="AI screening"
        caption="Deployment plans are sent to the AI adapter for review."
        field="disable_ai_screening"
        on={!current.disable_ai_screening}
        onChange={(on) => edit({ disable_ai_screening: !on })}
      />
    </SettingsCard>
  );
}

const SHARING = [
  ['allowed', 'Allowed'],
  ['passcode_only', 'Passcode only'],
  ['none', 'Not allowed'],
] as const;

function AccessPage() {
  const { current, edit, locked, fixed } = usePolicy();
  const verbs = current.disabled_verbs ?? [];
  const others = verbs.filter((v) => v !== 'app.exec');
  // public_sharing is the setting; the older allow_anonymous_grants still
  // counts when it is unset, and either fixed at startup locks this.
  const sharingField = fixed.has('public_sharing') || !fixed.has('allow_anonymous_grants') ? 'public_sharing' : 'allow_anonymous_grants';
  return (
    <>
      {/* R-085, R-272: policy is a floor, not an override. */}
      <Lede>A minimum for every app. Owners can only make their apps stricter.</Lede>
      <SettingsCard>
        <SettingRow label="Public sharing" caption="Who apps can be opened by without signing in." field={sharingField}>
          <Segmented
            name="public_sharing"
            label="Public sharing"
            value={publicSharing(current)}
            options={SHARING}
            disabled={locked('public_sharing') || locked('allow_anonymous_grants')}
            onChange={(v) => edit({ public_sharing: v })}
          />
        </SettingRow>
        <SwitchRow
          label="Terminal access"
          caption="Open a terminal inside a running app."
          field="disabled_verbs"
          on={!verbs.includes('app.exec')}
          onChange={(on) => edit({ disabled_verbs: on ? others : [...others, 'app.exec'] })}
        />
        {/* Issue #51. Off is refused while no identity provider is on. */}
        <SwitchRow
          label="Password sign-in"
          caption="Local accounts sign in with a password."
          field="disable_password_sign_in"
          on={!current.disable_password_sign_in}
          onChange={(on) => edit({ disable_password_sign_in: !on })}
        />
        <SwitchRow
          label="Create accounts at first sign-in"
          caption="From the identity provider."
          field="disable_jit_provisioning"
          on={!current.disable_jit_provisioning}
          onChange={(on) => edit({ disable_jit_provisioning: !on })}
        />
        {/* R-405. Provider accounts are the provider's. */}
        <SwitchRow
          label="Two-factor for local accounts"
          caption="Required at every sign-in."
          field="require_local_two_factor"
          on={current.require_local_two_factor ?? false}
          onChange={(on) => edit({ require_local_two_factor: on })}
        />
        {/* R-227 and design 06 §6: anonymous visits and refusals are logged
            unless policy says otherwise. */}
        <SwitchRow
          label="Log anonymous visits"
          field="disable_anonymous_use_audit"
          on={!current.disable_anonymous_use_audit}
          onChange={(on) => edit({ disable_anonymous_use_audit: !on })}
        />
        <SwitchRow
          label="Log anonymous refusals"
          field="disable_anonymous_denial_audit"
          on={!current.disable_anonymous_denial_audit}
          onChange={(on) => edit({ disable_anonymous_denial_audit: !on })}
        />
        {others.length > 0 && (
          <SettingRow label="Turned off verbs" field="disabled_verbs">
            <span style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)', justifyContent: 'flex-end' }}>
              {others.map((v) => (
                <Tag key={v} mono>
                  {v}
                </Tag>
              ))}
            </span>
          </SettingRow>
        )}
      </SettingsCard>
    </>
  );
}

/** Where apps may connect out to (R-181 – R-185). Build egress is a
 *  different setting (R-189) and is not here. */
function EgressPage() {
  const { current, edit, locked, fixed } = usePolicy();
  const { mode, list } = installEgress(current);
  // The field from before issue #79, fixed at startup, decides the mode and
  // the list as well.
  const legacyFixed = fixed.has('egress_allowlist') && !fixed.has('egress_mode');
  const modeField = legacyFixed ? 'egress_allowlist' : 'egress_mode';
  const listField = legacyFixed ? 'egress_allowlist' : 'egress_list';
  const loosening = looseningRule(current);
  return (
    <SettingsCard>
      <SettingRow label="Destinations" caption={EGRESS_MODES.find(([v]) => v === mode)?.[2]} field={modeField}>
        <Select
          aria-label="Destinations"
          name="egress_mode"
          disabled={locked(modeField) || locked('egress_list')}
          value={mode}
          options={EGRESS_MODES.map(([value, label]) => ({ value, label }))}
          onChange={(e) => edit(egressPatch(e.target.value as typeof mode, list))}
        />
      </SettingRow>
      {mode !== 'allow_all' && (
        <SettingRow label={mode === 'allowlist' ? 'Allowlist' : 'Denylist'} caption="One per line: a hostname, *.example.com, an IP address or a CIDR range, each with an optional port." field={listField} wide>
          <ListField
            label={mode === 'allowlist' ? 'Allowlist' : 'Denylist'}
            disabled={locked(listField) || locked('egress_mode')}
            value={list}
            onChange={(next) => edit(egressPatch(mode, next))}
          />
        </SettingRow>
      )}
      <SwitchRow
        label="Block private addresses"
        caption="The local network, this host and cloud metadata addresses."
        field="egress_block_private"
        on={current.egress_block_private ?? false}
        onChange={(on) => edit({ egress_block_private: on })}
      />
      <SettingRow label="Loosening by an app" caption={LOOSENING.find(([v]) => v === loosening)?.[2]} field="egress_loosening">
        <Select
          aria-label="Loosening by an app"
          name="egress_loosening"
          disabled={locked('egress_loosening')}
          value={loosening}
          options={LOOSENING.map(([value, label]) => ({ value, label }))}
          onChange={(e) => edit({ egress_loosening: e.target.value })}
        />
      </SettingRow>
      {/* R-372: off by default. */}
      <SwitchRow
        label="Private webhook addresses"
        caption="Event webhooks may send to the local network and this host."
        field="allow_private_webhooks"
        on={current.allow_private_webhooks ?? false}
        onChange={(on) => edit({ allow_private_webhooks: on })}
      />
    </SettingsCard>
  );
}

/** Who signs off on a deployment (R-154 – R-156), and how many run at once. */
function DeploymentsPage() {
  const { current, edit, locked } = usePolicy();
  const everyApp = current.deploy_approval_required ?? false;
  return (
    <SettingsCard>
      <SwitchRow
        label="Approval for every app"
        caption="Auto-deploy stops. Rollbacks, restarts and secret rotations never wait."
        field="deploy_approval_required"
        on={everyApp}
        onChange={(on) => edit({ deploy_approval_required: on })}
      />
      <SettingRow
        label="Apps that always need approval"
        caption={everyApp ? 'Every app needs approval.' : 'Owners can’t turn this off.'}
        field="deploy_approval_apps"
        wide
      >
        <ApprovalApps chosen={current.deploy_approval_apps ?? []} disabled={locked('deploy_approval_apps') || everyApp} onChange={(ids) => edit({ deploy_approval_apps: ids })} />
      </SettingRow>
      <NumberRow
        label="Approvals needed"
        caption="Different people. One rejection ends the request."
        field="deploy_approval_count"
        value={approvalsNeeded(current.deploy_approval_count)}
        min={1}
        onChange={(n) => edit({ deploy_approval_count: n })}
      />
      <NumberRow
        label="Request expiry, in hours"
        caption="Zero is never."
        field="deploy_approval_expiry_hours"
        value={current.deploy_approval_expiry_hours ?? 0}
        onChange={(n) => edit({ deploy_approval_expiry_hours: n })}
      />
      {/* Issue #93: read each time the queue looks for work. */}
      <NumberRow
        label="Concurrent deployments per replica"
        caption="Zero is one per CPU, at least two."
        field="max_concurrent_deploys"
        value={current.max_concurrent_deploys ?? 0}
        onChange={(n) => edit({ max_concurrent_deploys: n })}
      />
      {/* R-242: by default a deployment can't reserve more than the runtime
          has. Disk is never oversubscribed. */}
      <SwitchRow
        label="CPU oversubscription"
        caption="Busy apps share the CPU and run slower."
        field="allow_cpu_oversubscription"
        on={current.allow_cpu_oversubscription ?? false}
        onChange={(on) => edit({ allow_cpu_oversubscription: on })}
      />
      <SwitchRow
        label="Memory oversubscription"
        caption="The host may stop an app to free memory."
        field="allow_memory_oversubscription"
        on={current.allow_memory_oversubscription ?? false}
        onChange={(on) => edit({ allow_memory_oversubscription: on })}
      />
      {/* R-419: on unless set false; an app's spec may say otherwise. */}
      <SwitchRow
        label="Warm standby"
        caption="A stopped copy of each app on another host."
        field="standby"
        on={current.standby ?? true}
        onChange={(on) => edit({ standby: on })}
      />
      {/* R-422: standbys keep room for losing any one host unless this is
          on. Disk is never oversubscribed. */}
      <SwitchRow
        label="Failover oversubscription"
        caption="A host taking over starts what fits, highest priority first."
        field="allow_failover_oversubscription"
        on={current.allow_failover_oversubscription ?? false}
        onChange={(on) => edit({ allow_failover_oversubscription: on })}
      />
    </SettingsCard>
  );
}

/**
 * The apps that always need approval. Only the chosen apps are drawn, and
 * others are found by typing — never every app in the install (issue #72).
 */
function ApprovalApps({ chosen, disabled, onChange }: Readonly<{ chosen: string[]; disabled: boolean; onChange: (ids: string[]) => void }>) {
  const [search, setSearch] = useState('');
  const settled = useSettled(search.trim());
  type Named = { id: string; name: string };
  // Asked for by ID, a hundred at a time being a page; past that a tag shows
  // the app's ID.
  const named = chosen.slice(0, 100);
  const picked = useQuery({
    queryKey: ['apps', 'approval-policy', 'picked', named],
    queryFn: async () =>
      named.length > 0 ? ((await api.get<{ apps: Named[] | null }>(withParams('/apps', { id: named, limit: 100 }))).apps ?? []) : [],
    placeholderData: (previous) => previous,
    retry: false,
  });
  const found = useQuery({
    queryKey: ['apps', 'approval-policy', 'search', settled],
    queryFn: async () => (await api.get<{ apps: Named[] | null }>(withParams('/apps', { q: settled, limit: 20 }))).apps ?? [],
    placeholderData: (previous) => previous,
    retry: false,
  });
  const nameOf = (id: string) => picked.data?.find((a) => a.id === id)?.name ?? id;
  // An app chosen and since gone keeps a tag with its ID, so it can be taken off.
  const unknown = picked.isSuccess ? named.filter((id) => !picked.data.some((a) => a.id === id)) : [];
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <TagField
        label="Apps that always need approval"
        value={chosen}
        onChange={onChange}
        nameOf={nameOf}
        text={search}
        onText={setSearch}
        options={(found.data ?? []).map((a) => ({ id: a.id, name: a.name }))}
        placeholder="Type an app’s name"
        disabled={disabled}
      />
      {found.isSuccess && settled !== '' && found.data.length === 0 && <Quiet>{`No apps match “${settled}”.`}</Quiet>}
      {(picked.isError || found.isError) && <Quiet>{messageOf(picked.error ?? found.error)}</Quiet>}
      {unknown.length > 0 && (
        <Quiet>
          {unknown.length === 1
            ? `Pando can’t find ${unknown[0]}. It may have been deleted.`
            : `Pando can’t find ${unknown.length} of these apps: ${unknown.join(', ')}. They may have been deleted.`}
        </Quiet>
      )}
    </div>
  );
}

function TokensPage() {
  const { current, edit, locked } = usePolicy();
  return (
    <SettingsCard>
      <NumberRow
        label="Longest token lifetime, in days"
        caption="Zero is no limit."
        field="max_token_lifetime_days"
        value={current.max_token_lifetime_days ?? 0}
        onChange={(n) => edit({ max_token_lifetime_days: n })}
      />
      <SettingRow label="Verbs tokens can't use" caption="One verb per line. People can still use them." field="agent_disabled_verbs" wide>
        <Input
          aria-label="Verbs tokens can't use"
          as="textarea"
          rows={3}
          mono
          disabled={locked('agent_disabled_verbs')}
          value={(current.agent_disabled_verbs ?? []).join('\n')}
          onChange={(e) => edit({ agent_disabled_verbs: lines(e.target.value) })}
        />
      </SettingRow>
    </SettingsCard>
  );
}

/** Idle apps (issue #131), apps without an owner (R-408), and what deleting
 *  one takes with it. Defaults, not floors: an app can set its own days. */
function AppsPage() {
  const { current, edit } = usePolicy();
  return (
    <SettingsCard>
      <NumberRow
        label="Stop idle apps after, in days"
        caption="Zero is never. A stopped app keeps its data."
        field="idle_stop_days"
        value={current.idle_stop_days ?? 0}
        onChange={(n) => edit({ idle_stop_days: n })}
      />
      <NumberRow
        label="Delete idle apps after, in days"
        caption={
          current.require_backup_before_destroy
            ? 'Zero is never. Its storage is backed up first.'
            : 'Zero is never. Its storage is deleted with it.'
        }
        field="idle_delete_days"
        value={current.idle_delete_days ?? 0}
        onChange={(n) => edit({ idle_delete_days: n })}
      />
      <NumberRow
        label="Apps per person"
        caption="Zero is unlimited. A person's or group's own limit comes first."
        field="max_apps_per_user"
        value={current.max_apps_per_user ?? 0}
        onChange={(n) => edit({ max_apps_per_user: n })}
      />
      <SwitchRow
        label="Stop apps with no active owner"
        caption="Administrators are told either way."
        field="stop_orphaned_apps"
        on={current.stop_orphaned_apps ?? true}
        onChange={(on) => edit({ stop_orphaned_apps: on })}
      />
      {/* R-284: owners cannot turn it off; R-398: it covers Pando's own
          deletion of an idle app. */}
      <SwitchRow
        label="Backup before delete"
        caption="Owners can't turn this off."
        field="require_backup_before_destroy"
        on={current.require_backup_before_destroy ?? false}
        onChange={(on) => edit({ require_backup_before_destroy: on })}
      />
      {/* R-224: checked against the sum of every app's cap at plan time. */}
      <NumberRow
        label="Disk for app logs, in megabytes"
        caption="Across all apps. Zero is no limit."
        field="max_log_disk_bytes"
        value={Math.round((current.max_log_disk_bytes ?? 0) / 1_000_000)}
        onChange={(n) => edit({ max_log_disk_bytes: n * 1_000_000 })}
      />
    </SettingsCard>
  );
}

const ARCHIVE = [
  { value: 'keep', label: 'Kept by Pando' },
  { value: 'export', label: 'A backup destination' },
  { value: 'off', label: 'Not archived' },
];

/** How long events stay in the live log (R-347, R-348). */
function AuditPage() {
  const { current, edit, locked } = usePolicy();
  const archive = current.audit_archive || 'keep';
  return (
    <SettingsCard>
      {/* R-348: three months is the floor, held by the database too. */}
      <NumberRow
        label="Months to keep"
        caption="At least 3. Older months are archived."
        field="audit_retention_months"
        value={current.audit_retention_months || 3}
        min={3}
        disabled={archive === 'off'}
        onChange={(n) => edit({ audit_retention_months: n })}
      />
      <SettingRow label="Archive" caption={archive === 'off' ? 'The log keeps growing.' : undefined} field="audit_archive">
        <Select
          aria-label="Archive"
          name="audit_archive"
          disabled={locked('audit_archive')}
          value={archive}
          options={ARCHIVE}
          onChange={(e) =>
            edit(
              e.target.value === 'export'
                ? { audit_archive: 'export' }
                : { audit_archive: e.target.value, audit_archive_destination: undefined },
            )
          }
        />
      </SettingRow>
      {archive === 'export' && (
        <SettingRow label="Backup destination" caption="A backup adapter's ID. Empty is the default one." field="audit_archive_destination">
          <Input
            aria-label="Backup destination"
            mono
            disabled={locked('audit_archive_destination')}
            value={current.audit_archive_destination ?? ''}
            onChange={(e) => edit({ audit_archive_destination: e.target.value.trim() || undefined })}
          />
        </SettingRow>
      )}
    </SettingsCard>
  );
}

const CHANNELS = [
  ['stable', 'Stable'],
  ['prerelease', 'With release candidates'],
] as const;

/** The update settings, on Overview's Pando card (R-349, R-350, R-355,
 *  R-361). Host policy, saved with the rest of it. */
export function UpdateSettings() {
  const { current, edit, locked } = usePolicy();
  const checks = !current.disable_update_check;
  const inPlace = current.upgrade_in_place ?? false;
  return (
    <SettingsCard>
      {/* R-349: on by default; off sends no request at all. */}
      <SwitchRow
        label="Check for updates"
        caption="At startup and every six hours. Only the version is sent."
        field="disable_update_check"
        on={checks}
        onChange={(on) => edit({ disable_update_check: !on })}
      />
      <SettingRow label="Release channel" field="update_channel">
        <Segmented
          name="update_channel"
          label="Release channel"
          value={(current.update_channel || 'stable') as 'stable' | 'prerelease'}
          options={CHANNELS}
          disabled={locked('update_channel') || !checks}
          onChange={(v) => edit({ update_channel: v })}
        />
      </SettingRow>
      {/* R-355: usually set in the deployment, where it locks this. */}
      <SwitchRow
        label="Upgrade from here"
        caption="Pando checks the release, takes a backup and rolls back if it doesn't start."
        field="upgrade_in_place"
        on={inPlace}
        onChange={(on) => edit({ upgrade_in_place: on })}
      />
      {!inPlace && <InPlaceSetup />}
      {/* R-361: with nobody there, no passphrase, so no full backup. */}
      <SwitchRow
        label="Install patch releases"
        caption="In the maintenance window. No full backup is taken."
        field="auto_upgrade_patches"
        on={current.auto_upgrade_patches ?? false}
        disabled={!inPlace}
        onChange={(on) =>
          edit(on && !current.maintenance_window ? { auto_upgrade_patches: true, maintenance_window: 'sun 02:00 2h' } : { auto_upgrade_patches: on })
        }
      />
      {(current.auto_upgrade_patches || current.maintenance_window) && (
        <SettingRow label="Maintenance window, in UTC" caption="Such as sun,wed 02:00 2h or daily 03:30 1h." field="maintenance_window">
          <Input
            aria-label="Maintenance window, in UTC"
            mono
            placeholder="sun 02:00 2h"
            disabled={locked('maintenance_window')}
            value={current.maintenance_window ?? ''}
            onChange={(e) => edit({ maintenance_window: e.target.value || undefined })}
          />
        </SettingRow>
      )}
    </SettingsCard>
  );
}

/**
 * How to turn the in-place upgrade on where Pando is deployed (R-355): the
 * image has to be a moving tag, or the next apply of the deployment puts the
 * old version back.
 */
function InPlaceSetup() {
  return (
    <div className="pando-setrow pando-setrow--wide">
      <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
        Pando's image needs a moving tag, such as latest. A pinned version is put back by the next apply.
      </span>
      <CodeBlock
        title="docker-compose.yml"
        lines={['services:', '  pando:', '    image: trypando/pando:latest', '    environment:', '      PANDO_POLICY_UPGRADE_IN_PLACE: "true"']}
        copyable
        dense
      />
    </div>
  );
}

/**
 * The one save bar for every unsaved change in policy, wherever it was made
 * (O-10): a saved policy applies at each app's next deployment, and Check
 * impact says which apps that refuses before it is saved.
 */
/** What saving the draft does, or what checking its impact found. */
function previewLine(preview: readonly unknown[] | null): string {
  if (preview === null) return 'Applies at each app’s next deployment.';
  if (preview.length === 0) return 'No apps break these rules.';
  if (preview.length === 1) return 'One app keeps running and is refused at its next deployment.';
  return `${preview.length} apps keep running and are refused at their next deployment.`;
}

export function SaveBar() {
  const { changed, current, discard, save, check, preview, canEdit } = usePolicy();
  if (!canEdit || changed.length === 0) return null;
  const line = previewLine(preview);
  return (
    <section className="pando-savebar" aria-label="Unsaved policy">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
        <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>{changesLabel(changed.length)}</span>
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{line}</span>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
        <Button variant="ghost" onClick={discard}>
          Discard
        </Button>
        <Button
          variant="secondary"
          // Not again while the answer on screen is for this draft.
          disabled={check.isPending || preview !== null}
          onClick={() => check.mutate(current)}
        >
          {check.isPending ? 'Checking' : 'Check impact'}
        </Button>
        <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate(current)}>
          {save.isPending ? 'Saving' : 'Save policy'}
        </Button>
      </div>
      {(save.isError || check.isError) && (
        <div style={{ flexBasis: '100%' }}>
          <Banner tone="failed">{messageOf(save.error ?? check.error)}</Banner>
        </div>
      )}
      {preview !== null && preview.length > 0 && (
        <div style={{ flexBasis: '100%' }}>
          <Table
            dense
            columns={[
              { key: 'app_name', header: 'App', width: 'minmax(0,24ch)' },
              { key: 'message', header: 'Message', width: 'minmax(0,32ch)' },
              { key: 'code', header: 'Code', width: '28ch', mono: true, muted: true },
            ]}
            rows={preview.map((v) => ({ ...v, id: `${v.app_id}/${v.code}` }))}
          />
        </div>
      )}
    </section>
  );
}

