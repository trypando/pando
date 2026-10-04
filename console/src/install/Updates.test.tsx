import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { Attempt, Plan, Status } from '@api/types.gen';
import { Policy } from './Installation';
import { Updates } from './Updates';

const status: Status = {
  current: '0.3.1',
  development: false,
  enabled: true,
  channel: 'stable',
  checked_at: '2026-10-04T06:00:00Z',
  latest: '0.4.0',
  available: true,
  releases: [{ version: '0.4.0', prerelease: false, published_at: '2026-10-03T00:00:00Z', url: '', notes: '### Security\n\nNo new advisories.\n', security: false, breaking: true }],
  security: false,
  breaking: true,
  upgrade: { version: '0.4.0', image: 'trypando/pando:0.4.0', command: 'curl … && docker compose up -d', instructions: 'Download it.' },
};

function render(plan: Plan, last: Attempt | null = null, verbs: string[] = ['install.view', 'install.upgrade']) {
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queries.setQueryData(['me'], { verbs });
  queries.setQueryData(['updates'], status);
  queries.setQueryData(['upgrade-plan', '0.4.0'], plan);
  queries.setQueryData(['upgrade-last'], { upgrade: last });
  // React separates adjacent text with empty comments in server output.
  return renderToString(
    <QueryClientProvider client={queries}>
      <Updates onPolicy={() => {}} />
    </QueryClientProvider>,
  ).replaceAll('<!-- -->', '');
}

const possible: Plan = {
  current: '0.3.1',
  target: '0.4.0',
  possible: true,
  reasons: [],
  tag: 'trypando/pando:latest',
  breaking: [],
  note: 'Every app is unreachable while Pando restarts.',
};

describe('upgrading in place from the Updates screen (R-355 – R-359)', () => {
  it('offers the upgrade when Pando can replace itself, beside the command', () => {
    const html = render(possible);
    expect(html).toContain('Upgrade to 0.4.0');
    expect(html).toContain('Or upgrade it yourself');
    expect(html).toContain('docker compose up -d');
  });

  it('says it needs install.upgrade to someone without it', () => {
    const html = render(possible, null, ['install.view']);
    expect(html).toContain('needs install.upgrade');
  });

  it('lists every reason it cannot, each with what to change', () => {
    const html = render({
      ...possible,
      possible: false,
      reasons: [
        'In-place upgrades are off. Turn on "Let Pando upgrade itself" on the Policy screen.',
        "Pando's image is pinned to trypando/pando:0.3.1, an exact version.",
      ],
    });
    expect(html).toContain('Pando can&#x27;t upgrade itself from here');
    expect(html).toContain('pinned to trypando/pando:0.3.1');
    expect(html).toContain('Open policy');
  });

  it('shows a rollback with why, and the new version’s last lines', () => {
    const html = render(possible, {
      id: 'upg_1',
      from: '0.3.1',
      to: '0.4.0',
      image: '',
      started_at: '2026-10-04T02:00:00Z',
      started_by: 'usr_admin',
      automatic: false,
      skip_backup: false,
      state: 'rolled_back',
      reason: '0.4.0 did not start, so Pando put 0.3.1 back.',
      logs: 'panic: migration 43',
      snapshot_gone: true,
      recorded: true,
    });
    expect(html).toContain('Rolled back: 0.4.0 did not start');
    expect(html).toContain('panic: migration 43');
  });
});

describe('the Policy screen’s in-place settings (R-355, R-361)', () => {
  function renderPolicy(policy: object, fixed: object[] = []) {
    const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queries.setQueryData(['policy'], policy);
    queries.setQueryData(['config'], { file: '', settings: [], policy: fixed });
    queries.setQueryData(['apps'], { apps: [] });
    return renderToString(
      <QueryClientProvider client={queries}>
        <Policy canEdit />
      </QueryClientProvider>,
    );
  }

  it('says how to turn it on in the deployment, and why the tag matters', () => {
    const html = renderPolicy({});
    expect(html).toContain('Let Pando upgrade itself');
    expect(html).toContain('PANDO_POLICY_UPGRADE_IN_PLACE');
    expect(html).toContain('trypando/pando:latest');
    expect(html).toContain('puts the old version back');
  });

  it('says automatic upgrades take no full backup, and shows the window', () => {
    const html = renderPolicy({ upgrade_in_place: true, auto_upgrade_patches: true, maintenance_window: 'sun 02:00 2h' });
    expect(html).toContain('takes no full backup');
    expect(html).toContain('Maintenance window, in UTC');
    expect(html).toContain('sun 02:00 2h');
  });
});
