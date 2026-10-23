// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';

import type { App } from '@api/types.gen';
import { AppEvents } from './AppEvents';
import { PanelOpener } from './AppPanel';
import type { FeedItem } from './eventFeed';
import { AppVerb } from './verbs';
import { mount, reply, unanswered, type Seed } from '../ui/testing';

const app = { id: 'app_1', name: 'crewmate' } as App;

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

const tick = () => act(async () => new Promise((r) => setTimeout(r, 30)));

function at(daysAgo: number, hour: number): string {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate() - daysAgo, hour, 0, daysAgo === 0 ? 1 : 0).toISOString();
}

const events: FeedItem[] = [
  {
    id: 'evt_4',
    type: 'deploy.succeeded',
    occurred_at: at(0, 0),
    subject: 'Deployed revision 7',
    body: 'from a push to main',
    data: { deployment_id: 'dep_9' },
    actor: { kind: 'user', id: 'usr_me' },
  },
  {
    id: 'evt_3',
    type: 'app.restarted',
    occurred_at: at(1, 12),
    subject: 'worker restarted',
    body: 'exited with code 137',
    fields: [{ label: 'Workload', value: 'worker' }],
    actor: { kind: 'system' },
  },
  {
    id: 'evt_2',
    type: 'backup.failed',
    occurred_at: at(1, 3),
    subject: 'Backup failed',
    actor: { kind: 'system' },
  },
  {
    id: 'evt_1',
    type: 'grant.created',
    occurred_at: at(5, 9),
    subject: 'Shared with the Design group',
    actor: { kind: 'user', id: 'usr_other' },
  },
];

function seed({ next = '', verbs = [] as string[], subscriptions = [] as unknown[] } = {}): Seed {
  return [
    [['app-events', 'app_1'], { pages: [{ events, next_before: next }], pageParams: [''] }],
    [['app-events-head', 'app_1'], { events, next_before: next }],
    [['me'], { principal_kind: 'user', id: 'usr_me', user_id: 'usr_me', verbs }],
    [['events'], { events: [], destinations: [{ id: 'slack', kind: 'slack', audience: 'channel', channels: true }] }],
    [['subscriptions', '', { app_id: 'app_1' }], { pages: [{ subscriptions }], pageParams: [''] }],
  ];
}

const rowOf = (type: string) => document.querySelector<HTMLElement>(`tr[data-event="${type}"]`)!;

// R-378: the app's events of the last 30 days, a line each.
describe('the events table', () => {
  it('groups events under Today, Yesterday and the date', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed());
    const days = [...document.querySelectorAll('th[scope="colgroup"]')].map((th) => th.textContent);
    expect(days[0]).toBe('Today');
    expect(days[1]).toBe('Yesterday');
    expect(days).toHaveLength(3);
    expect(screen.text()).toContain('Deployed revision 7 — from a push to main');
    screen.unmount();
  });

  it('puts a symbol only on a success or a failure', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed());
    expect(rowOf('deploy.succeeded').querySelector('svg')).not.toBeNull();
    expect(rowOf('backup.failed').querySelector('svg')).not.toBeNull();
    expect(rowOf('app.restarted').querySelector('svg')).toBeNull();
    expect(rowOf('grant.created').querySelector('svg')).toBeNull();
    screen.unmount();
  });

  it('names the actor: You, Pando, or who it was', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed());
    expect(rowOf('deploy.succeeded').lastElementChild?.textContent).toBe('You');
    expect(rowOf('app.restarted').lastElementChild?.textContent).toBe('Pando');
    expect(rowOf('grant.created').lastElementChild?.textContent).toBe('usr_other');
    screen.unmount();
  });

  it('links the audit log for somebody who may read it', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed({ verbs: ['install.audit.read'] }));
    expect(screen.text()).toContain('Anything older than 30 days is in the audit log.');
    const link = [...document.querySelectorAll('a')].find((a) => a.textContent === 'audit log');
    expect(link?.getAttribute('href')).toBe('/admin/audit?app=app_1');

    // A plain click goes there in place, by way of the console's router.
    const heard = vi.fn();
    window.addEventListener('popstate', heard);
    act(() => link!.click());
    window.removeEventListener('popstate', heard);
    expect(window.location.pathname + window.location.search).toBe('/admin/audit?app=app_1');
    expect(heard).toHaveBeenCalled();
    window.history.replaceState({}, '', '/');
    screen.unmount();
  });

  it('says where older events are, without a link, to anybody else', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed());
    expect(screen.text()).toContain('Anything older than 30 days is in the audit log.');
    expect([...document.querySelectorAll('a')].some((a) => a.textContent === 'audit log')).toBe(false);
    screen.unmount();
  });

  it('reads the next page while the box has room, with no button to press', async () => {
    const asked: string[] = [];
    vi.stubGlobal('fetch', async (url: string) => {
      asked.push(String(url));
      return String(url).includes('before=') ? reply({ events: [], next_before: '' }) : unanswered();
    });
    const screen = mount(<AppEvents app={app} />, seed({ next: 'evt_1' }));
    await tick();
    expect(asked.some((u) => u.includes('/apps/app_1/events?before=evt_1'))).toBe(true);
    expect(screen.text()).not.toContain('Show older');
    screen.unmount();
  });

  it('reads nothing more when the last page says it is the last', async () => {
    const asked: string[] = [];
    vi.stubGlobal('fetch', async (url: string) => {
      asked.push(String(url));
      return unanswered();
    });
    const screen = mount(<AppEvents app={app} />, seed());
    await tick();
    expect(asked.some((u) => u.includes('before='))).toBe(false);
    screen.unmount();
  });

  it('opens a deploy in the side panel, at that deployment', async () => {
    vi.stubGlobal('fetch', unanswered);
    const open = vi.fn();
    const screen = mount(
      <PanelOpener value={open}>
        <AppEvents app={app} />
      </PanelOpener>,
      seed(),
      [AppVerb.LogsRead],
    );
    await act(async () => rowOf('deploy.succeeded').click());
    expect(open).toHaveBeenCalledWith({ view: 'deployments', deployment: 'dep_9' });
    screen.unmount();
  });

  it('opens any other event in a dialog with its fields', async () => {
    vi.stubGlobal('fetch', unanswered);
    const open = vi.fn();
    const screen = mount(
      <PanelOpener value={open}>
        <AppEvents app={app} />
      </PanelOpener>,
      seed(),
      [AppVerb.LogsRead],
    );
    await act(async () => rowOf('app.restarted').click());
    expect(open).not.toHaveBeenCalled();
    expect(screen.text()).toContain('Workload');
    expect(screen.text()).toContain('app.restarted');
    await screen.press('Close');
    expect(screen.text()).not.toContain('Workload');
    screen.unmount();
  });

  it('opens a deploy in a dialog for somebody who cannot read its logs', async () => {
    vi.stubGlobal('fetch', unanswered);
    const open = vi.fn();
    const screen = mount(
      <PanelOpener value={open}>
        <AppEvents app={app} />
      </PanelOpener>,
      seed(),
    );
    await act(async () => {
      rowOf('deploy.succeeded').dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(open).not.toHaveBeenCalled();
    expect(screen.text()).toContain('deploy.succeeded');
    screen.unmount();
  });
});

// R-367, R-370: where the app's events go, as a column of cards.
describe('subscription cards', () => {
  const on = {
    id: 'sub_1',
    owner_id: 'usr_me',
    app_id: 'app_1',
    events: ['deploy.failed', 'backup.failed'],
    destination: 'notify',
    adapter_id: 'slack',
    channel: '#crewmate-alerts',
    description: '',
    enabled: true,
    consecutive_failures: 0,
    created_at: '2026-10-01T00:00:00Z',
  };
  const off = {
    ...on,
    id: 'sub_2',
    events: ['deploy.*'],
    destination: 'webhook',
    adapter_id: undefined,
    channel: undefined,
    url: 'https://hooks.example.com/pando',
    enabled: false,
    disabled_reason: 'Pando turned this off after a day of failed deliveries.',
  };

  it('shows where each sends, what it sends, and whether it is on', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed({ subscriptions: [on, off] }));
    const text = screen.text();
    expect(text).toContain('Slack#crewmate-alerts');
    expect(text).toContain('deploy.failed');
    expect(text).toContain('deploy.*');
    expect(text).toContain('https://hooks.example.com/pando');
    expect(text).toContain('On');
    expect(text).toContain('Turned off');
    expect(text).toContain('Pando turned this off after a day of failed deliveries.');
    screen.unmount();
  });

  it('turns one Pando turned off back on', async () => {
    const calls: Array<{ url: string; method?: string; body?: string }> = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push({ url: String(url), method: init?.method, body: init?.body as string | undefined });
      return String(url).includes('/subscriptions/sub_2') ? reply({ ...off, enabled: true }) : unanswered();
    });
    const screen = mount(<AppEvents app={app} />, seed({ subscriptions: [on, off] }));
    // Only the card that is off offers it.
    expect([...document.querySelectorAll('button')].filter((b) => b.textContent === 'Turn on')).toHaveLength(1);
    await screen.press('Turn on');
    const patch = calls.find((c) => c.url.endsWith('/subscriptions/sub_2') && c.method === 'PATCH');
    expect(JSON.parse(patch?.body ?? '{}')).toEqual({ enabled: true });
    screen.unmount();
  });

  it('sends a test, and opens the deliveries and the subscription', async () => {
    const calls: string[] = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push(`${init?.method ?? 'GET'} ${String(url)}`);
      if (String(url).endsWith('/test')) return reply({ id: 'dlv_1', status: 'pending' });
      if (String(url).endsWith('/deliveries')) return reply({ deliveries: [] });
      if (String(url).endsWith('/subscriptions/sub_1')) return reply(on);
      return unanswered();
    });
    const screen = mount(<AppEvents app={app} />, seed({ subscriptions: [on] }));
    await screen.press('Send a test');
    expect(calls.some((c) => c.startsWith('POST') && c.endsWith('/subscriptions/sub_1/test'))).toBe(true);
    expect(screen.text()).toContain('Test sent.');

    await screen.press('Deliveries');
    await tick();
    expect(screen.text()).toContain('No deliveries yet.');
    await screen.press('Close');

    await screen.press('Edit');
    await tick();
    expect(calls.some((c) => c.startsWith('GET') && c.endsWith('/subscriptions/sub_1'))).toBe(true);
    expect(screen.text()).toContain('Turn off');
    screen.unmount();
  });

  it('says there are none', () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<AppEvents app={app} />, seed());
    expect(screen.text()).toContain('No subscriptions yet');
    screen.unmount();
  });
});
