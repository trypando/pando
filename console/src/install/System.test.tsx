// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, unanswered, type Mounted, type Seed } from '../ui/testing';
import type { Route } from '../app/route';
import { System, systemPages } from './System';
import { changedFields, changedPages, changesLabel } from './policyDraft';

// System's pages beside their list, and policy as one draft across them: a
// change on any page shows one save bar with how many there are, a dot on the
// page that holds it, and Discard, Check impact and Save policy.

const seed = (policy: object = {}): Seed => [
  [['me'], { verbs: ['install.view', 'install.policy.manage', 'install.backup.manage'] }],
  [['policy'], policy],
  [['config'], { file: '', settings: [], policy: [] }],
  [['updates'], { current: '0.4.0', development: false, enabled: true, channel: 'stable', available: false, releases: [], security: false, breaking: false }],
  [['capacity'], { runtimes: [] }],
  [['adapters'], { adapters: [] }],
  [['adapter-kinds'], { kinds: [] }],
  [['sources'], { sources: [] }],
  [['upgrade-last'], { upgrade: null }],
];

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

function open(route: Partial<Route>, go = vi.fn(), policy?: object) {
  const pages = systemPages({ view: true, managePolicy: true, manageBackups: true });
  screen = mount(
    <System route={{ view: 'admin', section: 'system', ...route }} go={go} pages={pages} canManagePolicy behind={2} />,
    seed(policy),
    [],
  );
  return go;
}

describe('System’s pages', () => {
  it('lists each page a person may see, Policy’s under it', () => {
    expect(systemPages({ view: true, managePolicy: true, manageBackups: true })).toEqual(['overview', 'adapters', 'policy', 'agents', 'backups']);
    expect(systemPages({ view: false, managePolicy: true, manageBackups: false })).toEqual(['policy', 'agents']);
    expect(systemPages({ view: false, managePolicy: false, manageBackups: false })).toEqual([]);

    vi.stubGlobal('fetch', () => unanswered());
    open({ tab: 'policy', sub: 'access' });
    const links = [...document.querySelectorAll('nav[aria-label="System"] a')].map((a) => a.getAttribute('href'));
    expect(links).toEqual([
      '/admin/system/overview',
      '/admin/system/adapters',
      '/admin/system/policy',
      '/admin/system/policy/sources',
      '/admin/system/policy/access',
      '/admin/system/policy/egress',
      '/admin/system/policy/deployments',
      '/admin/system/policy/tokens',
      '/admin/system/policy/apps',
      '/admin/system/policy/audit',
      '/admin/system/agents',
      '/admin/system/backups',
    ]);
    expect(document.querySelector('a[aria-current="page"]')?.textContent).toBe('Access');
    // The releases behind are beside Overview.
    expect(document.querySelector('a[href="/admin/system/overview"]')?.textContent).toContain('2');
  });

  it('goes to a page in place', async () => {
    vi.stubGlobal('fetch', () => unanswered());
    const go = open({ tab: 'overview' });
    await screen!.click('Egress');
    expect(go).toHaveBeenCalledWith({ view: 'admin', section: 'system', tab: 'policy', sub: 'egress' }, true);
  });

  it('opens the first page when the address names none', () => {
    vi.stubGlobal('fetch', () => unanswered());
    open({});
    expect(screen!.text()).toContain('Overview');
    expect(screen!.text()).toContain('Startup settings');
  });
});

describe('at phone width', () => {
  it('picks a page from a select at the top', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: true, media: q, addEventListener: () => {}, removeEventListener: () => {} }));
    vi.stubGlobal('fetch', () => unanswered());
    const go = open({ tab: 'backups' });
    const select = document.querySelector<HTMLSelectElement>('select[aria-label="Page"]')!;
    const options = [...select.options].map((o) => o.textContent);
    expect(options[0]).toBe('Overview (2)');
    expect(options).toContain('Policy · Access');
    expect(select.value).toBe('backups');
    expect(document.querySelector('nav[aria-label="System"]')).toBeNull();
    await screen!.settle(() => {
      Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value')!.set!.call(select, 'policy/tokens');
      select.dispatchEvent(new Event('change', { bubbles: true }));
    });
    expect(go).toHaveBeenCalledWith({ view: 'admin', section: 'system', tab: 'policy', sub: 'tokens' }, true);
  });
});

describe('one draft of policy across its pages', () => {
  it('counts the changes, marks the page, and discards them', async () => {
    vi.stubGlobal('fetch', () => unanswered());
    open({ tab: 'policy', sub: 'access' });
    expect(screen!.text()).not.toContain('unsaved change');
    await screen!.toggle('Terminal access');
    expect(screen!.text()).toContain('1 unsaved change');
    expect(screen!.text()).toContain('Applies at each app’s next deployment.');
    expect(document.querySelector('a[href="/admin/system/policy/access"] [aria-label="Unsaved changes"]')).toBeTruthy();
    await screen!.toggle('Terminal access');
    expect(screen!.text()).not.toContain('unsaved change');
    await screen!.toggle('Password sign-in');
    await screen!.press('Discard');
    expect(screen!.text()).not.toContain('unsaved change');
  });

  it('checks the impact, then saves what was changed', async () => {
    const calls: Array<{ method: string; path: string; body?: string }> = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      const path = String(url);
      calls.push({ method: init?.method ?? 'GET', path, body: init?.body as string | undefined });
      if (path.includes('/policy/preview')) return reply({ violations: [{ app_id: 'app_1', app_name: 'crewmate', code: 'PLAN_EGRESS', message: 'Egress is refused.' }] });
      if (init?.method === 'PUT') return reply({});
      return unanswered();
    });
    open({ tab: 'policy', sub: 'access' }, vi.fn(), { disable_password_sign_in: false });
    await screen!.toggle('Password sign-in');
    await screen!.press('Check impact');
    expect(screen!.text()).toContain('One app keeps running and is refused at its next deployment.');
    expect(screen!.text()).toContain('crewmate');
    await screen!.press('Save policy');
    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.path).toContain('/policy');
    expect(JSON.parse(put!.body!)).toMatchObject({ disable_password_sign_in: true });
  });
});

describe('what counts as a change', () => {
  it('reads unset, off, zero and empty as one, except where unset means on', () => {
    expect(changedFields({}, { disable_ai_screening: false, max_apps_per_user: 0, source_allowlist: [] })).toEqual([]);
    expect(changedFields({}, { stop_orphaned_apps: true })).toEqual([]);
    expect(changedFields({}, { stop_orphaned_apps: false })).toEqual(['stop_orphaned_apps']);
    expect(changedFields({ egress_mode: 'allowlist' }, { egress_mode: 'denylist', update_channel: 'prerelease' }).sort()).toEqual([
      'egress_mode',
      'update_channel',
    ]);
    expect([...changedPages(['egress_mode', 'update_channel'])].sort()).toEqual(['egress', 'overview']);
    expect(changesLabel(1)).toBe('1 unsaved change');
    expect(changesLabel(3)).toBe('3 unsaved changes');
  });
});
