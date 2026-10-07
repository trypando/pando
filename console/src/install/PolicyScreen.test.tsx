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
  // The approval section asks for the chosen apps by ID and a search's worth
  // of others (issue #72); both answers, as the server would give them.
  const apps = [{ id: 'app_01', name: 'crewmate' }];
  const chosen = (policy as { deploy_approval_apps?: string[] }).deploy_approval_apps ?? [];
  queries.setQueryData(['apps', 'approval-policy', chosen, ''], {
    picked: apps.filter((a) => chosen.includes(a.id)),
    found: apps,
    more: false,
  });
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
    expect(html).toContain('Approvals needed');
    expect(html).toContain('value="168"');
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
