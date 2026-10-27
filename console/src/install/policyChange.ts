// A proposed policy change as the Policy screen would put it (R-344).

const show = (v: unknown) => (v === null || v === undefined ? 'unset' : JSON.stringify(v));

// Each setting by the name the Policy screen gives it, so a proposal reads as
// the switch it would flip rather than as a field name.
const LABELS: Record<string, string> = {
  source_allowlist: 'Allowed sources',
  disabled_verbs: 'Disabled verbs',
  agent_disabled_verbs: 'Disabled verbs for tokens',
  allow_anonymous_grants: 'Public sharing',
  public_sharing: 'Public sharing',
  min_build_isolation: 'Minimum isolation for builds',
  min_runtime_isolation: 'Minimum isolation for running apps',
  egress_mode: 'Egress',
  egress_list: 'Egress list',
  egress_allowlist: 'Egress list',
  egress_block_private: 'Block private addresses',
  egress_loosening: 'Egress loosening',
  deploy_approval_required: 'Require approval for every app',
  deploy_approval_apps: 'Apps that always need approval',
  deploy_approval_count: 'Approvals needed',
  deploy_approval_expiry_hours: 'Request expiry (hours)',
  require_backup_before_destroy: 'Require backup before delete',
  max_token_lifetime_days: 'Max token lifetime (days)',
  max_log_disk_bytes: 'Total disk for app logs',
  max_concurrent_deploys: 'Concurrent deploys per replica',
  idle_stop_days: 'Stop idle apps after (days)',
  idle_delete_days: 'Delete idle apps after (days)',
  max_apps_per_user: 'App limit',
  stop_orphaned_apps: 'Stop apps with no active owner',
  allow_cpu_oversubscription: 'Allow CPU oversubscription',
  allow_memory_oversubscription: 'Allow memory oversubscription',
  allow_failover_oversubscription: 'Allow failover oversubscription',
  standby: 'Warm standby',
  disable_ai_screening: 'Disable AI screening',
  disable_anonymous_use_audit: "Don't log anonymous visits",
  disable_anonymous_denial_audit: "Don't log anonymous refusals",
  min_security_score: 'Minimum security score',
  insecure_action: 'Stop running apps below the minimum',
  insecure_grace_hours: 'Grace period (hours)',
  ignore_unfixable_findings: 'Ignore findings with no fix available',
  disable_password_sign_in: 'Disable password sign-in',
  disable_jit_provisioning: 'Disable just-in-time provisioning',
  require_local_two_factor: 'Require two-factor authentication for local accounts',
};

const SHARING: Record<string, string> = { allowed: 'Allowed', passcode_only: 'Only with a passcode', none: 'Not allowed' };

const asList = (v: unknown): string[] => (Array.isArray(v) ? (v as string[]) : []);

const EGRESS_MODE: Record<string, string> = {
  allow_all: 'Anywhere',
  denylist: 'Anywhere except the list',
  allowlist: 'Only the list',
};

const LOOSENING: Record<string, string> = {
  verb: 'Allowed for people with permission',
  approval: 'Need a deploy approval',
  forbidden: 'Not allowed',
};

// Lists whose entries are named one at a time, as the permissions are, rather
// than as two JSON arrays side by side.
const LISTS = new Set(['egress_list', 'egress_allowlist', 'deploy_approval_apps', 'source_allowlist']);

/**
 * One change as the Policy screen would put it: a title, and what it does.
 * Terminal access is its own switch on that screen, though it is stored as
 * app.exec in the permissions nobody may use, so it reads as that switch.
 */
export function describeChange(c: { key: string; from: unknown; to: unknown }): { title: string; detail: string }[] {
  if (c.key === 'disabled_verbs' || c.key === 'agent_disabled_verbs') {
    const before = asList(c.from);
    const after = asList(c.to);
    const out: { title: string; detail: string }[] = [];
    const agent = c.key === 'agent_disabled_verbs';
    for (const v of after.filter((x) => !before.includes(x))) {
      out.push(
        !agent && v === 'app.exec'
          ? { title: 'Disable terminal access', detail: 'Off to on.' }
          : { title: LABELS[c.key]!, detail: `Adds ${v}.` },
      );
    }
    for (const v of before.filter((x) => !after.includes(x))) {
      out.push(
        !agent && v === 'app.exec'
          ? { title: 'Disable terminal access', detail: 'On to off.' }
          : { title: LABELS[c.key]!, detail: `Removes ${v}.` },
      );
    }
    return out;
  }
  if (LISTS.has(c.key)) {
    const before = asList(c.from);
    const after = asList(c.to);
    const title = LABELS[c.key]!;
    return [
      ...after.filter((x) => !before.includes(x)).map((v) => ({ title, detail: `Adds ${v}.` })),
      ...before.filter((x) => !after.includes(x)).map((v) => ({ title, detail: `Removes ${v}.` })),
    ];
  }
  if (c.key === 'egress_mode') {
    const word = (v: unknown) => EGRESS_MODE[String(v ?? '')] ?? (v ? String(v) : 'Anywhere');
    return [{ title: LABELS.egress_mode!, detail: `${word(c.from)} to ${word(c.to)}.` }];
  }
  if (c.key === 'egress_loosening') {
    const word = (v: unknown) => LOOSENING[String(v ?? '')] ?? (v ? String(v) : LOOSENING.verb!);
    return [{ title: LABELS.egress_loosening!, detail: `${word(c.from)} to ${word(c.to)}.` }];
  }
  if (c.key === 'deploy_approval_count') {
    // Zero is one, as the server reads it.
    const n = (v: unknown) => (typeof v === 'number' && v > 0 ? v : 1);
    return [{ title: LABELS.deploy_approval_count!, detail: `${n(c.from)} to ${n(c.to)}.` }];
  }
  if (c.key === 'deploy_approval_expiry_hours') {
    const h = (v: unknown) => (typeof v === 'number' && v > 0 ? `${v} hours` : 'never');
    const detail = `${h(c.from)} to ${h(c.to)}.`;
    return [{ title: LABELS.deploy_approval_expiry_hours!, detail: detail.charAt(0).toUpperCase() + detail.slice(1) }];
  }
  if (c.key === 'public_sharing') {
    return [{ title: LABELS.public_sharing!, detail: `${SHARING[String(c.from)] ?? 'Allowed'} to ${SHARING[String(c.to)] ?? show(c.to)}.` }];
  }
  if (typeof c.to === 'boolean' || typeof c.from === 'boolean') {
    return [{ title: LABELS[c.key] ?? c.key, detail: c.to ? 'Off to on.' : 'On to off.' }];
  }
  return [{ title: LABELS[c.key] ?? c.key, detail: `${show(c.from)} to ${show(c.to)}.` }];
}
