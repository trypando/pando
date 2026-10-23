// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { AppVerbs } from './verbs';
import { mount, reply, type Mounted } from '../ui/testing';
import { IdleSection, choiceOf, idleValue, outlook } from './IdleSettings';
import type { IdleReport } from './IdleSettings';

const app = { id: 'app_x', name: 'ledger' } as unknown as App;

const report: IdleReport = {
  stop_days: null,
  delete_days: 0,
  install_stop_days: 30,
  install_delete_days: 90,
  effective_stop_days: 30,
  effective_delete_days: 0,
  last_activity_at: '2026-10-01T12:00:00Z',
  stops_at: '2026-10-31T12:00:00Z',
  stopped_for_idle: false,
};

function render(r: IdleReport | undefined, verbs: string[] = []) {
  const client = new QueryClient();
  if (r) client.setQueryData(['apps', app.id, 'idle'], r);
  return renderToString(
    <QueryClientProvider client={client}>
      <AppVerbs.Provider value={verbs}>
        <IdleSection app={app} />
      </AppVerbs.Provider>
    </QueryClientProvider>,
  );
}

// R-397: null is the installation's, 0 is never, a number is the app's own.
describe('an idle setting', () => {
  it('reads an app value as the choice it stands for', () => {
    expect(choiceOf(null)).toBe('install');
    expect(choiceOf(0)).toBe('off');
    expect(choiceOf(14)).toBe('days');
  });

  it('sends what the choice means, whole days and at least one', () => {
    expect(idleValue('install', '30')).toBeNull();
    expect(idleValue('off', '30')).toBe(0);
    expect(idleValue('days', '45')).toBe(45);
    expect(idleValue('days', '2.6')).toBe(3);
    expect(idleValue('days', '')).toBe(1);
    expect(idleValue('days', '-4')).toBe(1);
  });
});

// R-395, R-396: what will happen is said in dates before any control.
describe('the outlook', () => {
  it('says when Pando stops and deletes the app', () => {
    const said = outlook({ ...report, deletes_at: '2026-12-30T12:00:00Z' });
    expect(said).toMatch(/^If nobody uses it, Pando stops it on .*\. Pando deletes it on .* if nobody uses it by then\.$/);
  });

  it('says nothing when nothing will happen', () => {
    expect(outlook({ ...report, stops_at: undefined })).toBeNull();
  });

  it('still says when a stopped app will be deleted', () => {
    const said = outlook({ ...report, stopped_for_idle: true, stops_at: undefined, deletes_at: '2026-12-30T12:00:00Z' });
    expect(said).toMatch(/^Pando stopped this app for being idle\. Start it to use it again\. Pando deletes it on /);
    expect(outlook({ ...report, stopped_for_idle: true })).toBe(
      'Pando stopped this app for being idle. Start it to use it again.',
    );
  });
});

describe('the idle section', () => {
  it('renders nothing until the settings arrive', () => {
    expect(render(undefined)).toBe('');
  });

  it('shows two switches as they stand, fixed for somebody who cannot edit', () => {
    const html = render(report);
    expect(html).toContain('Stop when unused');
    expect(html).toContain('After 30 days without a visit');
    expect(html).toContain('Delete when unused');
    expect(html).toContain('>Off<');
    expect(html).toContain('If nobody uses it, Pando stops it on');
    expect(html.match(/role="switch"[^>]*disabled=""/g)).toHaveLength(2);
    expect(html).not.toContain('>Change<');
  });

  it('says one day, not one days', () => {
    expect(render({ ...report, effective_stop_days: 1 })).toContain('After 1 day without a visit');
  });

  it('offers a change of days to somebody who may edit the app', () => {
    const html = render({ ...report, stop_days: 14, effective_stop_days: 14 }, ['app.spec.edit']);
    expect(html).toContain('After 14 days without a visit');
    expect(html).toContain('>Change<');
    expect(html).not.toMatch(/role="switch"[^>]*disabled=""/);
  });
});

describe('flipping an idle switch (R-397)', () => {
  let screen: Mounted | undefined;
  afterEach(() => {
    screen?.unmount();
    vi.unstubAllGlobals();
  });

  const sent = () => {
    const calls: unknown[] = [];
    vi.stubGlobal('fetch', async (_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') calls.push(JSON.parse(String(init.body)));
      return reply(report);
    });
    return calls;
  };

  it('turns stopping off, keeping the delete setting', async () => {
    const calls = sent();
    screen = mount(<IdleSection app={app} />, [[['apps', app.id, 'idle'], report]], ['app.spec.edit']);
    await screen.toggle('Stop when unused');
    expect(calls).toEqual([{ stop_days: 0, delete_days: 0 }]);
  });

  it('turns deleting on at the installation’s days', async () => {
    const calls = sent();
    screen = mount(<IdleSection app={app} />, [[['apps', app.id, 'idle'], report]], ['app.spec.edit']);
    await screen.toggle('Delete when unused');
    expect(calls).toEqual([{ stop_days: null, delete_days: null }]);
  });

  it('turns one on at the app’s own days where the installation has none', async () => {
    const calls = sent();
    const none = { ...report, install_delete_days: 0 };
    screen = mount(<IdleSection app={app} />, [[['apps', app.id, 'idle'], none]], ['app.spec.edit']);
    await screen.toggle('Delete when unused');
    expect(calls).toEqual([{ stop_days: null, delete_days: 90 }]);
  });

  it('sets the app’s own days', async () => {
    const calls = sent();
    screen = mount(<IdleSection app={app} />, [[['apps', app.id, 'idle'], report]], ['app.spec.edit']);
    await screen.press('Change');
    await screen.fill('Days before stopping', '14');
    await screen.press('Save');
    expect(calls).toEqual([{ stop_days: 14, delete_days: 0 }]);
  });
});
