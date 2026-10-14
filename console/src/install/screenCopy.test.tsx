import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import type { ReactNode } from 'react';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { Audit, Installation } from './Installation';
import { Identity } from './Identity';
import { Events } from './Events';
import { AccountPage } from './Account';

/** Renders a screen over a cache seeded with `data`, by query key. */
function render(screen: ReactNode, data: Array<[unknown[], unknown]>): string {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  for (const [key, value] of data) client.setQueryData(key, value);
  // React separates adjacent text with empty comments in server output.
  return renderToString(<QueryClientProvider client={client}>{screen}</QueryClientProvider>).replaceAll('<!-- -->', '');
}

const kind = (category: string) => ({ category, kind: 'x', name: 'X', description: '', id_prefix: '', fields: null });

describe('the screens’ copy', () => {
  it('offers to add each category nothing is set up for, by its label', () => {
    const html = render(<Installation />, [
      [['me'], { verbs: ['install.adapters.manage'] }],
      [['adapters'], { adapters: [] }],
      [['adapter-kinds'], { kinds: [kind('ai'), kind('image_registry'), kind('routing')] }],
      [['sources'], { sources: [] }],
    ]);
    expect(html).toContain('Add an AI adapter');
    expect(html).toContain('Add an image registry adapter');
    expect(html).toContain('Add a routing adapter');
  });

  it('names where an archived month went, Pando when no adapter took it', () => {
    const month = { id: 'arc_1', month: '2026-01', row_count: 12, size_bytes: 1, sha256: 'a'.repeat(64) };
    const html = render(<Audit />, [
      [['audit-archives'], { archives: [month, { ...month, id: 'arc_2', month: '2026-02', adapter_ref: 's3/archive' }] }],
    ]);
    expect(html).toContain('>Pando<');
    expect(html).toContain('s3/archive');
  });

  it('says which scope a role applies to', () => {
    const roles = [
      { id: 'role_admin', name: 'admin', scope: 'install', builtin: true, verbs: [] },
      { id: 'role_viewer', name: 'viewer', scope: 'app', builtin: true, verbs: [] },
    ];
    const html = render(<Identity canEdit={false} />, [[['roles', 'all'], { roles }]]);
    expect(html).toContain('>Installation<');
    expect(html).toContain('>App<');
  });

  it('says what a subscription is about: an app by name, or the installation', () => {
    const sub = { id: 'sub_1', owner_id: 'usr_1', events: ['app.deployed'], destination: 'webhook', url: 'https://hooks.example.com', description: '', enabled: true, consecutive_failures: 0, created_at: '2026-10-01T00:00:00Z' };
    const html = render(<Events canManageAll={false} apps={[]} />, [
      [['subscriptions', '', {}], { pages: [{ subscriptions: [sub, { ...sub, id: 'sub_2', app_id: 'app_1', app_name: 'inventory' }] }], pageParams: [''] }],
    ]);
    expect(html).toContain('>Installation<');
    expect(html).toContain('>inventory<');
  });

  it('offers to edit an account an identity provider holds', () => {
    const account = { id: 'usr_1', adapter_id: 'idp_okta', external_id: 'dana', status: 'active', must_change_password: false, install_role_id: '' };
    const html = render(<AccountPage userID="usr_1" onBack={() => {}} onAudit={() => {}} />, [
      [['me'], { verbs: ['install.users.manage'] }],
      [['users', 'usr_1'], account],
    ]);
    expect(html).toContain('>Edit<');
  });
});
