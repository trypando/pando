// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { PanelOpener } from './AppPanel';
import { AppVerb } from './verbs';
import { addReading, nearLimit, usageKey, workloadLine, Workloads, type PartUsage, type Reading } from './Workloads';
import { points } from './Sparkline';
import { mount, reply, serverRender, unanswered, type Mounted, type Seed } from '../ui/testing';

const app = { id: 'app_x', name: 'crewmate', state: 'running', pinned_spec_id: 'spec_x' } as unknown as App;

const use = (name: string, extra: Partial<PartUsage> = {}): PartUsage => ({
  name,
  primary: name === 'web',
  running: true,
  cpu_millis: 120,
  cpu_limit_millis: 1000,
  memory_bytes: 212 << 20,
  memory_limit_bytes: 512 << 20,
  disk_bytes: 84 << 20,
  volumes: [],
  ...extra,
});

const reading = (at: string, workloads: PartUsage[]): Reading => ({ supported: true, reported_at: at, workloads });

const part = (name: string, extra: object = {}) => ({
  name,
  primary: name === 'web',
  present: true,
  running: true,
  restarting: false,
  restart_count: 0,
  healthy: null,
  ...extra,
});

/** The app's status, its usage, and its newest spec declaring web and worker. */
const seed = (workloads: PartUsage[], parts: object[]): Seed => [
  [[...usageKey(app)], reading('2026-10-23T10:00:00Z', workloads)],
  [['apps', 'app_x', 'status'], { state: 'running', desired_state: 'running', workloads: parts }],
  [['apps', 'app_x', 'specs'], { revisions: [{ id: 'spec_x', revision: 1 }], pinned_spec_id: 'spec_x' }],
  [
    ['apps', 'app_x', 'spec', 1],
    { id: 'spec_x', revision: 1, body: { resources: { memory_bytes: 512 << 20, cpu_millis: 1000 }, workloads: [{ name: 'web' }, { name: 'worker' }] } },
  ],
];

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('a workload card', () => {
  const cards = (verbs: string[], parts = [part('web'), part('worker', { running: false, present: true, exit_code: 1 }), part('postgres')]) =>
    serverRender(
      <PanelOpener value={() => {}}>
        <Workloads app={app} />
      </PanelOpener>,
      seed([use('web'), use('worker', { running: false }), use('postgres')], parts),
      verbs,
    );
  const count = (html: string, s: string) => html.split(s).length - 1;

  it('offers Logs, Terminal and Resources to somebody with every verb', () => {
    const html = cards([AppVerb.LogsRead, AppVerb.Exec, AppVerb.ResourcesOverride, AppVerb.SpecEdit]);
    expect(count(html, '>Logs<')).toBe(3);
    // Not on the worker, which is not running.
    expect(count(html, '>Terminal<')).toBe(2);
    // Not on postgres, which Pando provides and the spec does not declare.
    expect(count(html, '>Resources<')).toBe(2);
  });

  it('offers nothing a person may not use', () => {
    const html = cards([AppVerb.View]);
    expect(html).not.toContain('>Logs<');
    expect(html).not.toContain('>Terminal<');
    expect(html).not.toContain('>Resources<');
  });

  it('asks for the resources verb, not only the spec edit', () => {
    expect(cards([AppVerb.SpecEdit])).not.toContain('>Resources<');
    expect(cards([AppVerb.ResourcesOverride])).not.toContain('>Resources<');
  });

  it('says what each workload is, and how it is', () => {
    const html = cards([]);
    expect(html).toContain('Serves the address · No restarts');
    expect(html).toContain('Provided by Pando · No restarts');
    expect(html).toContain('Exited (1)');
    expect(html).toContain('212.0 MB of 512.0 MB');
    expect(html).toContain('0.12 of 1');
  });
});

describe('changing a workload’s resources', () => {
  it('opens the dialog for that workload, with what it uses now', async () => {
    vi.stubGlobal('fetch', unanswered);
    screen = mount(<Workloads app={app} />, seed([use('web')], [part('web')]), [AppVerb.ResourcesOverride, AppVerb.SpecEdit]);
    await screen.press('Resources');
    expect(screen.text()).toContain('Resources for web');
    expect(screen.text()).toContain('Using 212 MiB now.');
    await screen.press('Cancel');
    expect(screen.text()).not.toContain('Resources for web');
  });

  it('says so when the runtime cannot report usage', () => {
    const html = serverRender(<Workloads app={app} />, [
      [[...usageKey(app)], { supported: false }],
      [['apps', 'app_x', 'status'], { state: 'running', workloads: [part('web', { restarting: true, restart_count: 4 })] }],
    ]);
    expect(html).toContain('doesn’t report usage');
    expect(html).toContain('Restarting, 4 times');
    expect(html).toContain('4 restarts');
  });
});

describe('the readings this page has taken (R-245)', () => {
  it('accumulate, one per reading, for the parts that are running', () => {
    let history = addReading({}, reading('t1', [use('web'), use('worker', { running: false })]));
    history = addReading(history, reading('t2', [use('web', { memory_bytes: 300 << 20 })]));
    expect(history.web?.map((s) => s.at)).toEqual(['t1', 't2']);
    expect(history.web?.[1]?.memory).toBe(300 << 20);
    expect(history.worker).toBeUndefined();
  });

  it('do not count the same reading twice', () => {
    const once = addReading({}, reading('t1', [use('web')]));
    expect(addReading(once, reading('t1', [use('web')]))).toBe(once);
  });

  it('keep at most so many', () => {
    let history = {};
    for (let i = 0; i < 5; i++) history = addReading(history, reading(`t${i}`, [use('web')]), 3);
    expect((history as Record<string, unknown[]>).web).toHaveLength(3);
  });

  it('take nothing from a runtime that cannot report', () => {
    const h = {};
    expect(addReading(h, { supported: false })).toBe(h);
  });

  it('grow on screen as the page asks again', async () => {
    let n = 1;
    vi.stubGlobal('fetch', async (url: string) => {
      if (String(url).endsWith('/usage')) {
        n += 1;
        return reply(reading(`t${n}`, [use('web', { memory_bytes: (200 + n * 10) << 20 })]));
      }
      if (String(url).endsWith('/status')) return reply({ state: 'running', workloads: [part('web')] });
      return unanswered();
    });
    screen = mount(<Workloads app={app} />, seed([use('web')], [part('web')]));
    const memory = () => document.querySelectorAll('polyline')[0]!.getAttribute('points')!.split(' ');
    // Memory, CPU and disk, each a line; one reading is drawn flat across.
    expect(document.querySelectorAll('polyline')).toHaveLength(3);
    expect(memory()).toHaveLength(2);
    await screen.press('Refresh');
    await screen.press('Refresh');
    expect(memory()).toHaveLength(3);
  });

  it('draws memory near its limit in the warning color', () => {
    expect(nearLimit(471, 512)).toBe(true);
    expect(nearLimit(212, 512)).toBe(false);
    expect(nearLimit(900, 0)).toBe(false);
    const html = serverRender(<Workloads app={app} />, seed([use('web', { memory_bytes: 480 << 20 })], [part('web')]));
    expect(html).toContain('stroke="var(--contour-text)"');
    // CPU and disk stay in the ordinary color.
    expect(html).toContain('stroke="var(--water)"');
    // Memory and CPU have a limit line; disk does not.
    expect(html.split('data-limit').length - 1).toBe(2);
  });

  it('place each reading against the scale', () => {
    expect(points([], 10)).toEqual([]);
    expect(points([5], 10)).toEqual([
      [0, 18.8],
      [200, 18.8],
    ]);
    expect(points([0, 10, 20], 10).map(([, y]) => y)).toEqual([36, 1.5, 1.5]);
  });
});

describe('a workload line', () => {
  it('names the part that serves the address only among several', () => {
    expect(workloadLine(part('web') as never, undefined, true, false)).toBe('Serves the address · No restarts');
    expect(workloadLine(part('web') as never, undefined, false, false)).toBe('No restarts');
    expect(workloadLine(part('worker', { restart_count: 2 }) as never, undefined, true, false)).toBe('2 restarts');
    expect(workloadLine(part('worker', { restart_count: 1 }) as never, undefined, true, false)).toBe('1 restart');
    expect(workloadLine(undefined, use('pg'), true, true)).toBe('Provided by Pando');
  });
});

describe('the usage key, after a deploy finishes', () => {
  const deploying = { id: 'app_x', state: 'deploying', pinned_spec_id: 'spec_x' };
  const running = { ...deploying, state: 'running' };

  it('asks again when the app goes from deploying to running, or pins another spec', () => {
    expect(usageKey(running)).not.toEqual(usageKey(deploying));
    expect(usageKey({ ...running, pinned_spec_id: 'spec_y' })).not.toEqual(usageKey(running));
  });

  it('keeps the reading while nothing has changed', () => {
    expect(usageKey({ ...running })).toEqual(usageKey(running));
    expect(usageKey({ id: 'app_x', state: 'draft' })).toEqual(['apps', 'app_x', 'usage', 'draft', '']);
  });
});
