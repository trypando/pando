import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { AppVerbs } from './verbs';
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
    expect(said).toMatch(/^Pando stopped this app because nobody had used it\. Start it to use it again\. Pando deletes it on /);
    expect(outlook({ ...report, stopped_for_idle: true })).toBe(
      'Pando stopped this app because nobody had used it. Start it to use it again.',
    );
  });
});

describe('the idle section', () => {
  it('renders nothing until the settings arrive', () => {
    expect(render(undefined)).toBe('');
  });

  it("shows the installation's settings and no Save for somebody who cannot edit", () => {
    const html = render(report);
    expect(html).toContain('When nobody uses it');
    expect(html).toContain('The installation&#x27;s, 30 days');
    expect(html).toContain('If nobody uses it, Pando stops it on');
    expect(html).not.toContain('>Save<');
  });

  it('offers Save to somebody who may edit the app', () => {
    expect(render({ ...report, stop_days: 14 }, ['app.spec.edit'])).toContain('>Save<');
  });
});
