import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { serverRender } from '../ui/testing';
import type { PolicyPage } from '../app/route';
import { PolicyPageView } from './PolicyPages';
import { PolicyDraftContext, changedFields, usePolicyDraft } from './policyDraft';

function Harness({ page }: Readonly<{ page: PolicyPage }>) {
  const draft = usePolicyDraft(true, true);
  return (
    <PolicyDraftContext.Provider value={draft}>
      <PolicyPageView page={page} />
    </PolicyDraftContext.Provider>
  );
}

function render(page: PolicyPage, policy: object, config?: object) {
  // The approval setting asks for the chosen apps by ID and searches for
  // others as the administrator types (issue #72); both answers, as the
  // server would give them.
  const apps = [
    { id: 'app_01', name: 'crewmate' },
    { id: 'app_02', name: 'nginx' },
  ];
  const chosen = (policy as { deploy_approval_apps?: string[] }).deploy_approval_apps ?? [];
  return serverRender(<Harness page={page} />, [
    [['policy'], policy],
    [['config'], config ?? { file: '', settings: [], policy: [] }],
    [['apps', 'approval-policy', 'picked', chosen.slice(0, 100)], apps.filter((a) => chosen.includes(a.id))],
    [['apps', 'approval-policy', 'search', ''], apps],
  ]);
}

/** Whether the option with this value is the one chosen in the select named `name`. */
function selected(html: string, name: string, value: string): boolean {
  const select = html.match(new RegExp(`<select[^>]*name="${name}"[^>]*>(.*?)</select>`, 's'))?.[1] ?? '';
  return new RegExp(`<option value="${value}" selected=""`).test(select);
}

/** The switch named `label`'s input. */
function switchOf(html: string, label: string): string {
  return html.match(new RegExp(`<input[^>]*aria-label="${label}"[^>]*>`))?.[0] ?? '';
}

describe('Egress (R-181, R-183)', () => {
  it('shows a policy from before issue #79 as an allowlist, with its entries', () => {
    const html = render('egress', { egress_allowlist: ['api.example.com'] });
    expect(selected(html, 'egress_mode', 'allowlist')).toBe(true);
    expect(html).toContain('api.example.com');
    expect(html).toContain('*.example.com');
  });

  it('offers the three modes, the private-address switch and the loosening rule', () => {
    const html = render('egress', { egress_mode: 'denylist', egress_list: ['evil.example'], egress_loosening: 'forbidden' });
    expect(selected(html, 'egress_mode', 'denylist')).toBe(true);
    expect(selected(html, 'egress_loosening', 'forbidden')).toBe(true);
    expect(html).toContain('Block private addresses');
    expect(html).toContain('Allowed for people with permission');
    expect(html).toContain('Need a deploy approval');
    // The chosen rule's meaning is the row's one line.
    expect(html).toContain('A deploy that would loosen these rules is refused.');
  });

  it('locks a field fixed at startup, and says where it is set', () => {
    const html = render(
      'egress',
      { egress_mode: 'allowlist', egress_list: [] },
      { file: '', settings: [], policy: [{ key: 'egress_mode', value: 'allowlist', source: { kind: 'env', name: 'PANDO_POLICY_EGRESS_MODE' } }] },
    );
    const select = html.match(/<select[^>]*name="egress_mode"[^>]*>/)?.[0] ?? '';
    expect(select).toContain('disabled');
    expect(html).toContain('Set in environment');
    expect(html).toContain('PANDO_POLICY_EGRESS_MODE');
  });
});

describe('Deployments (R-154)', () => {
  it('offers deploy approval with apps to pick, a count and an expiry', () => {
    const html = render('deployments', { deploy_approval_apps: ['app_01', 'app_gone'], deploy_approval_expiry_hours: 168 });
    expect(html).toContain('Approval for every app');
    expect(html).toContain('crewmate');
    // An app that has gone is still listed, so it can be taken off.
    expect(html).toContain('app_gone');
    expect(html).toContain('Pando can’t find app_gone');
    expect(html).toContain('aria-label="Remove crewmate"');
    expect(html).toContain('Approvals needed');
    expect(html).toContain('value="168"');
  });

  it('draws only the chosen apps, not every app in the install (issue #72)', () => {
    const html = render('deployments', { deploy_approval_apps: ['app_01'] });
    expect(html).toContain('crewmate');
    expect(html).not.toContain('nginx');
    expect(html).toContain('Apps that always need approval');
  });
});

describe('Apps (R-393, R-398, R-244, R-408)', () => {
  it('shows the days and the limit, off and unlimited by default', () => {
    const html = render('apps', {});
    expect(html).toContain('Stop idle apps after, in days');
    expect(html).toContain('Delete idle apps after, in days');
    expect(html).toContain('Apps per person');
    expect(html).toContain('Zero is unlimited');
  });

  it('says what a deletion does with storage, as the backup rule has it', () => {
    expect(render('apps', { idle_delete_days: 90, require_backup_before_destroy: true })).toContain('Its storage is backed up first.');
    expect(render('apps', { idle_delete_days: 90 })).toContain('Its storage is deleted with it.');
    expect(render('apps', { idle_stop_days: 30, max_apps_per_user: 5 })).toContain('value="30"');
  });

  it('stops apps with no owner when policy does not say, and not when it says false', () => {
    const label = 'Stop apps with no active owner';
    expect(switchOf(render('apps', {}), label)).toContain('checked=""');
    expect(switchOf(render('apps', { stop_orphaned_apps: true }), label)).toContain('checked=""');
    expect(switchOf(render('apps', { stop_orphaned_apps: false }), label)).not.toContain('checked=""');
    expect(render('apps', {})).toContain('Administrators are told either way.');
  });
});

describe('Deployments, warm standby', () => {
  it('keeps a standby when policy does not say, and not when it says false (R-419)', () => {
    const label = 'Warm standby';
    expect(switchOf(render('deployments', {}), label)).toContain('checked=""');
    expect(switchOf(render('deployments', { standby: false }), label)).not.toContain('checked=""');
    expect(changedFields({}, { standby: true })).toEqual([]);
    expect(changedFields({}, { standby: false })).toEqual(['standby']);
  });
});

describe('Access, its switches phrased as what is on', () => {
  it('reads each disable field turned over', () => {
    const on = render('access', {});
    for (const label of ['Terminal access', 'Password sign-in', 'Create accounts at first sign-in', 'Log anonymous visits', 'Log anonymous refusals']) {
      expect(switchOf(on, label), label).toContain('checked=""');
    }
    expect(switchOf(on, 'Two-factor for local accounts')).not.toContain('checked=""');

    const off = render('access', {
      disabled_verbs: ['app.exec', 'app.logs.read'],
      disable_password_sign_in: true,
      disable_jit_provisioning: true,
      disable_anonymous_use_audit: true,
      disable_anonymous_denial_audit: true,
      require_local_two_factor: true,
    });
    for (const label of ['Terminal access', 'Password sign-in', 'Create accounts at first sign-in', 'Log anonymous visits', 'Log anonymous refusals']) {
      expect(switchOf(off, label), label).not.toContain('checked=""');
    }
    expect(switchOf(off, 'Two-factor for local accounts')).toContain('checked=""');
    // A verb turned off by other means is listed, so it is not hidden.
    expect(off).toContain('app.logs.read');
  });

  it('chooses public sharing, reading the older field when the new one is unset', () => {
    const chosen = (html: string) => html.match(/<label data-on="true">.*?value="([a-z_]+)"/)?.[1];
    expect(chosen(render('access', {}))).toBe('allowed');
    expect(chosen(render('access', { allow_anonymous_grants: false }))).toBe('none');
    expect(chosen(render('access', { public_sharing: 'passcode_only' }))).toBe('passcode_only');
  });
});

describe('the other pages', () => {
  it('turn their fields over too, and show the security score’s follow-ups only when it is set', () => {
    const sources = render('sources', { min_security_score: 70, insecure_action: 'stop', ignore_unfixable_findings: true, disable_ai_screening: true });
    expect(sources).toContain('Grace period, in hours');
    expect(switchOf(sources, 'Count findings with no fix')).not.toContain('checked=""');
    expect(switchOf(sources, 'AI screening')).not.toContain('checked=""');
    expect(render('sources', {})).not.toContain('Stop apps below the minimum');

    expect(render('tokens', { agent_disabled_verbs: ['app.exec'] })).toContain('app.exec');
    const audit = render('audit', { audit_archive: 'export', audit_archive_destination: 'bak_s3' });
    expect(selected(audit, 'audit_archive', 'export')).toBe(true);
    expect(audit).toContain('bak_s3');
    expect(render('audit', { audit_archive: 'off' })).toContain('The log keeps growing.');
  });
});
