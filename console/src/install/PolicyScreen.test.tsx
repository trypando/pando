import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { Policy } from './Installation';

function render(policy: object, config?: object) {
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queries.setQueryData(['policy'], policy);
  queries.setQueryData(['config'], config ?? { file: '', settings: [], policy: [] });
  // The approval section asks for the chosen apps by ID and searches for
  // others as the administrator types (issue #72); both answers, as the
  // server would give them.
  const apps = [
    { id: 'app_01', name: 'crewmate' },
    { id: 'app_02', name: 'nginx' },
  ];
  const chosen = (policy as { deploy_approval_apps?: string[] }).deploy_approval_apps ?? [];
  queries.setQueryData(
    ['apps', 'approval-policy', 'picked', chosen.slice(0, 100)],
    apps.filter((a) => chosen.includes(a.id)),
  );
  queries.setQueryData(['apps', 'approval-policy', 'search', ''], apps);
  return renderToString(
    <QueryClientProvider client={queries}>
      <Policy canEdit />
    </QueryClientProvider>,
  );
}

/** Whether the radio with this value is checked, in rendered HTML. */
function checked(html: string, name: string, value: string): boolean {
  const tag = html.match(new RegExp(`<input[^>]*name="${name}"[^>]*value="${value}"[^>]*>`))?.[0] ?? '';
  return /checked=""/.test(tag);
}

describe('the Policy screen’s egress and approval settings (R-181, R-183, R-154)', () => {
  it('shows a policy from before issue #79 as an allowlist, with its entries', () => {
    const html = render({ egress_allowlist: ['api.example.com'] });
    expect(checked(html, 'egress_mode', 'allowlist')).toBe(true);
    expect(html).toContain('api.example.com');
    expect(html).toContain('*.example.com');
  });

  it('offers the three modes, the private-address switch and the loosening rule', () => {
    const html = render({ egress_mode: 'denylist', egress_list: ['evil.example'], egress_loosening: 'forbidden' });
    expect(checked(html, 'egress_mode', 'denylist')).toBe(true);
    expect(checked(html, 'egress_loosening', 'forbidden')).toBe(true);
    expect(html).toContain('Block private addresses');
    expect(html).toContain('Allowed for people with permission');
    expect(html).toContain('Need a deploy approval');
  });

  it('offers deploy approval with apps to pick, a count and an expiry', () => {
    const html = render({ deploy_approval_apps: ['app_01', 'app_gone'], deploy_approval_expiry_hours: 168 });
    expect(html).toContain('Deploy approval');
    expect(html).toContain('crewmate');
    // An app that has gone is still listed, so it can be taken off.
    expect(html).toContain('app_gone');
    expect(html).toContain('Pando can’t find app_gone');
    expect(html).toContain('aria-label="Remove crewmate"');
    expect(html).toContain('Approvals needed');
    expect(html).toContain('value="168"');
  });

  it('draws only the chosen apps, not every app in the install (issue #72)', () => {
    const html = render({ deploy_approval_apps: ['app_01'] });
    expect(html).toContain('crewmate');
    expect(html).not.toContain('nginx');
    expect(html).toContain('Apps that always need approval');
  });

  it('locks a field fixed at startup', () => {
    const html = render(
      { egress_mode: 'allowlist', egress_list: [] },
      { file: '', settings: [], policy: [{ key: 'egress_mode', value: 'allowlist', source: { kind: 'env', name: 'PANDO_POLICY_EGRESS_MODE' } }] },
    );
    const tag = html.match(/<input[^>]*name="egress_mode"[^>]*value="allowlist"[^>]*>/)?.[0] ?? '';
    expect(tag).toContain('disabled');
  });
});

describe('the Policy screen’s idle apps and app limit (R-393, R-398, R-244)', () => {
  it('shows the days and the limit, off and unlimited by default', () => {
    const html = render({});
    expect(html).toContain('Apps nobody uses');
    expect(html).toContain('Stop an app nobody has used for this many days');
    expect(html).toContain('Delete an app nobody has used for this many days');
    expect(html).toContain('Apps each person may own');
    expect(html).toContain('Zero means unlimited');
  });

  it('says what a deletion does with storage, as the backup rule has it', () => {
    expect(render({ idle_delete_days: 90, require_backup_before_destroy: true })).toContain(
      'Pando backs up the app&#x27;s storage first',
    );
    expect(render({ idle_delete_days: 90 })).toContain('storage is deleted with it unless');
    expect(render({ idle_stop_days: 30, max_apps_per_user: 5 })).toContain('value="30"');
  });
});
