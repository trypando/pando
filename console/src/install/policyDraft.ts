// Host policy as one draft across System's pages.
//
// Policy is split over seven pages, and the update settings sit on Overview,
// but it is one document saved in one PUT: a change on Access and another on
// Egress are saved together, and one save bar says how many there are. So the
// draft lives above the pages, in System, and each page edits it.

import { createContext, useContext, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from '@api/client';
import type { PolicyPage } from '../app/route';

export interface PolicyDoc {
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
  allow_failover_oversubscription?: boolean;
  allow_memory_oversubscription?: boolean;
  max_concurrent_deploys?: number;

  // Apps nobody uses (R-393 – R-398) and how many a person may own (R-244),
  // issue #131. Zero is off, or unlimited.
  idle_stop_days?: number;
  idle_delete_days?: number;
  max_apps_per_user?: number;
  // R-408: on unless set false.
  stop_orphaned_apps?: boolean;

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
  // R-405: local accounts sign in with a second factor.
  require_local_two_factor?: boolean;

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

export interface Violation {
  app_id: string;
  app_name: string;
  code: string;
  message: string;
  remedy?: string;
}

/** Where a startup setting came from (R-271). */
export interface Source {
  kind: 'env' | 'file' | 'default';
  name?: string;
  key?: string;
}

export interface StartupConfig {
  file: string;
  settings: Array<{ key: string; value: unknown; source: Source; env: string }>;
  policy: Array<{ key: string; value: unknown; source: Source }>;
}

/** Which page each field is set on. Overview holds the update settings. */
const FIELD_PAGES: Record<keyof PolicyDoc, PolicyPage | 'overview'> = {
  source_allowlist: 'sources',
  min_build_isolation: 'sources',
  min_runtime_isolation: 'sources',
  min_security_score: 'sources',
  insecure_action: 'sources',
  insecure_grace_hours: 'sources',
  ignore_unfixable_findings: 'sources',
  disable_ai_screening: 'sources',

  public_sharing: 'access',
  allow_anonymous_grants: 'access',
  disabled_verbs: 'access',
  disable_password_sign_in: 'access',
  disable_jit_provisioning: 'access',
  require_local_two_factor: 'access',
  disable_anonymous_use_audit: 'access',
  disable_anonymous_denial_audit: 'access',

  egress_mode: 'egress',
  egress_list: 'egress',
  egress_allowlist: 'egress',
  egress_block_private: 'egress',
  egress_loosening: 'egress',
  allow_private_webhooks: 'egress',

  deploy_approval_required: 'deployments',
  deploy_approval_apps: 'deployments',
  deploy_approval_count: 'deployments',
  deploy_approval_expiry_hours: 'deployments',
  max_concurrent_deploys: 'deployments',
  allow_cpu_oversubscription: 'deployments',
  allow_memory_oversubscription: 'deployments',
  allow_failover_oversubscription: 'deployments',

  max_token_lifetime_days: 'tokens',
  agent_disabled_verbs: 'tokens',

  idle_stop_days: 'apps',
  idle_delete_days: 'apps',
  max_apps_per_user: 'apps',
  stop_orphaned_apps: 'apps',
  require_backup_before_destroy: 'apps',
  max_log_disk_bytes: 'apps',

  audit_retention_months: 'audit',
  audit_archive: 'audit',
  audit_archive_destination: 'audit',

  disable_update_check: 'overview',
  update_channel: 'overview',
  upgrade_in_place: 'overview',
  auto_upgrade_patches: 'overview',
  maintenance_window: 'overview',
};

/** What an unset field means, where that is not off, zero or empty. */
const UNSET: Partial<Record<keyof PolicyDoc, unknown>> = {
  stop_orphaned_apps: true,
  update_channel: 'stable',
  audit_archive: 'keep',
};

/** A field's value as it reads: unset, off, zero and empty are one thing,
 *  except where unset means something else. */
function reading(key: keyof PolicyDoc, v: unknown): unknown {
  const empty = v === undefined || v === null || v === '' || v === false || v === 0 || (Array.isArray(v) && v.length === 0);
  if (v === undefined || v === null) return key in UNSET ? UNSET[key] : null;
  if (empty && !(key in UNSET)) return null;
  return v;
}

/** The fields the draft changes from what is saved, by what they read as —
 *  turning a switch on and off again is no change. */
export function changedFields(saved: PolicyDoc, draft: PolicyDoc): Array<keyof PolicyDoc> {
  const keys = new Set([...Object.keys(saved), ...Object.keys(draft)] as Array<keyof PolicyDoc>);
  return [...keys].filter(
    (k) => JSON.stringify(reading(k, saved[k])) !== JSON.stringify(reading(k, draft[k])),
  );
}

/** The pages holding a change, for the dot beside each in the navigation. */
export function changedPages(fields: Array<keyof PolicyDoc>): Set<string> {
  return new Set(fields.map((f) => FIELD_PAGES[f] ?? 'sources'));
}

/** "1 unsaved change", "3 unsaved changes". */
export function changesLabel(n: number): string {
  return n === 1 ? '1 unsaved change' : `${n} unsaved changes`;
}

/** The rule for sharing with everyone, reading the older boolean when the new
 *  setting is unset — the same reading as the server's PublicSharingMode. */
export function publicSharing(doc: PolicyDoc): 'allowed' | 'passcode_only' | 'none' {
  if (doc.public_sharing) return doc.public_sharing;
  return doc.allow_anonymous_grants === false ? 'none' : 'allowed';
}

/** The draft, what is saved, and what saving or checking it says. */
export function usePolicyDraft(enabled: boolean, canEdit: boolean) {
  const queries = useQueryClient();
  const [draft, setDraft] = useState<PolicyDoc | null>(null);
  // What this policy would block, before it is saved (design 05 §3). Asked
  // for, not automatic: running every app's pinned spec through the planner
  // on every keystroke would make a form that stutters.
  const [preview, setPreview] = useState<Violation[] | null>(null);

  const policy = useQuery({ queryKey: ['policy'], queryFn: () => api.get<PolicyDoc>('/policy'), enabled });
  // Fields fixed in the startup configuration (R-271): shown, not editable.
  // GET /config is install.view, the same as reading policy.
  const startup = useQuery({ queryKey: ['config'], queryFn: () => api.get<StartupConfig>('/config'), enabled });

  const save = useMutation({
    mutationFn: (doc: PolicyDoc) => api.put<PolicyDoc>('/policy', doc),
    onSuccess: () => {
      setDraft(null);
      setPreview(null);
      void queries.invalidateQueries({ queryKey: ['policy'] });
      void queries.invalidateQueries({ queryKey: ['updates'] });
    },
  });
  const check = useMutation({
    mutationFn: (doc: PolicyDoc) => api.post<{ violations: Violation[] }>('/policy/preview', doc),
    onSuccess: (result) => setPreview(result.violations ?? []),
  });

  const current = draft ?? policy.data ?? {};
  const changed = draft && policy.data ? changedFields(policy.data, draft) : [];
  const fixed = new Map((startup.data?.policy ?? []).map((f) => [f.key, f.source]));

  return {
    policy,
    startup,
    current,
    changed,
    fixed,
    canEdit,
    preview,
    save,
    check,
    locked: (field: string) => !canEdit || fixed.has(field),
    edit: (patch: Partial<PolicyDoc>) => {
      // Nothing to change until what is saved has arrived: a draft made from
      // nothing would save as a policy with every other field cleared.
      if (!policy.data) return;
      // A stale answer is worse than none: it says "nothing breaks" about a
      // policy that is no longer the one on screen.
      setPreview(null);
      setDraft({ ...current, ...patch });
    },
    discard: () => {
      setDraft(null);
      setPreview(null);
      save.reset();
      check.reset();
    },
  };
}

export type PolicyDraft = ReturnType<typeof usePolicyDraft>;

export const PolicyDraftContext = createContext<PolicyDraft | null>(null);

/** The draft System holds. Only used under System, which always provides it. */
export function usePolicy(): PolicyDraft {
  const d = useContext(PolicyDraftContext);
  if (!d) throw new Error('usePolicy is used outside System');
  return d;
}
