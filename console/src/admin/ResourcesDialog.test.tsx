// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { AppSpec } from '@api/types.gen';
import { ResourcesDialog, coreNumber } from './ResourcesDialog';
import { raisedAppLimits, withWorkloadResources } from './resourceSpec';
import { InstallVerb } from '../app/principal';
import { mount, reply, serverRender, unanswered, type Mounted } from '../ui/testing';

const MIB = 1 << 20;

/** An app at 512 MiB and one core: web with nothing of its own, worker
 *  pinned at 256 MiB, and a migrator with its own CPU. */
const spec = {
  revision: 3,
  resources: { memory_bytes: 512 * MIB, cpu_millis: 1000, disk_bytes: 0, overridden: false },
  runtime: { adapter_ref: 'rt_local', isolation_floor: 0 },
  workloads: [
    { name: 'web', exposed: true, primary: true },
    { name: 'worker', exposed: false, primary: false, resources: { memory_bytes: 256 * MIB } },
    { name: 'migrate', exposed: false, primary: false, resources: { cpu_millis: 500 } },
  ],
} as unknown as AppSpec;

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('the spec a resources change writes', () => {
  it('TestR241a_RaisingOneWorkloadPastTheAppRaisesTheAppAndPinsTheRest', () => {
    const written = withWorkloadResources(spec, 'web', { memory_bytes: 1024 * MIB });
    // The app's limit becomes the workload's, as an override (R-241).
    expect(written.resources).toEqual({ memory_bytes: 1024 * MIB, cpu_millis: 1000, disk_bytes: 0, overridden: true });
    const by = Object.fromEntries((written.workloads ?? []).map((w) => [w.name, w.resources]));
    expect(by.web).toEqual({ memory_bytes: 1024 * MIB });
    // The worker had its own and keeps it.
    expect(by.worker).toEqual({ memory_bytes: 256 * MIB });
    // The migrator ran at the app's memory, and is written that limit as its own.
    expect(by.migrate).toEqual({ cpu_millis: 500, memory_bytes: 512 * MIB });
  });

  it('changes only the workload when it stays within the app', () => {
    const written = withWorkloadResources(spec, 'worker', { memory_bytes: 384 * MIB, cpu_millis: 250 });
    expect(written.resources).toEqual(spec.resources);
    const by = Object.fromEntries((written.workloads ?? []).map((w) => [w.name, w.resources]));
    expect(by.worker).toEqual({ memory_bytes: 384 * MIB, cpu_millis: 250 });
    expect(by.web).toBeUndefined();
    expect(by.migrate).toEqual({ cpu_millis: 500 });
  });

  it('takes a workload back to the app’s limits when nothing is asked', () => {
    const written = withWorkloadResources(spec, 'worker', {});
    expect(written.workloads?.find((w) => w.name === 'worker')?.resources).toBeUndefined();
  });

  it('raises CPU on its own, pinning only CPU', () => {
    const written = withWorkloadResources(spec, 'web', { cpu_millis: 2000 });
    expect(raisedAppLimits(spec, { cpu_millis: 2000 })).toEqual({ cpu_millis: { from: 1000, to: 2000 } });
    const by = Object.fromEntries((written.workloads ?? []).map((w) => [w.name, w.resources]));
    expect(by.worker).toEqual({ memory_bytes: 256 * MIB, cpu_millis: 1000 });
    expect(by.migrate).toEqual({ cpu_millis: 500 });
  });

  it('has nothing to raise on an app with no limit', () => {
    const open = { ...spec, resources: { ...spec.resources, memory_bytes: 0 } };
    expect(raisedAppLimits(open, { memory_bytes: 4096 * MIB })).toEqual({});
  });

  it('reads cores as a plain number', () => {
    expect(coreNumber(500)).toBe('0.5');
    expect(coreNumber(1000)).toBe('1');
    expect(coreNumber(40)).toBe('0.04');
  });
});

describe('the resources dialog', () => {
  const dialog = () => (
    <ResourcesDialog
      appID="app_1"
      appName="crewmate"
      workload="web"
      spec={spec}
      using={{ memory_bytes: 471 * MIB, cpu_millis: 40 }}
      onClose={() => {}}
    />
  );

  it('says each default and what is in use', () => {
    const html = serverRender(dialog());
    expect(html).toContain('Resources for web');
    expect(html).toContain('Default 512 MiB. Using 471 MiB now.');
    expect(html).toContain('Default 1. Using 0.04 now.');
  });

  it('says what the app’s limit becomes before it saves, then writes the revision', async () => {
    const posted: unknown[] = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && String(url).endsWith('/apps/app_1/specs')) {
        posted.push(JSON.parse(String(init.body)));
        return reply({ revision: 4 }, 201);
      }
      return unanswered();
    });
    const closed = vi.fn();
    screen = mount(
      <ResourcesDialog appID="app_1" appName="crewmate" workload="web" spec={spec} onClose={closed} />,
    );
    expect(screen.text()).not.toContain('This raises');
    await screen.fill('Memory (MiB)', '1024');
    expect(screen.text()).toContain('This raises crewmate’s memory limit from 512 MiB to 1024 MiB.');
    await screen.press('Save');
    expect(posted).toEqual([withWorkloadResources(spec, 'web', { memory_bytes: 1024 * MIB })]);
    expect(closed).toHaveBeenCalled();
  });

  it('shows the server’s refusal (R-105)', async () => {
    vi.stubGlobal('fetch', async () =>
      reply({ code: 'PLAN_CAPACITY_EXCEEDED', message: 'This host has 2 GiB of memory left and this app asks for 4 GiB.' }, 422),
    );
    screen = mount(<ResourcesDialog appID="app_1" appName="crewmate" workload="web" spec={spec} onClose={() => {}} />);
    await screen.fill('Memory (MiB)', '4096');
    await screen.press('Save');
    expect(screen.text()).toContain('This host has 2 GiB of memory left and this app asks for 4 GiB.');
  });

  it('refuses a value that is not a number', async () => {
    vi.stubGlobal('fetch', unanswered);
    screen = mount(dialog());
    await screen.fill('CPU (cores)', 'lots');
    expect(screen.text()).toContain('Enter a number of cores, such as 0.5.');
  });

  it('says how much of the host is unallocated, to somebody who may see it', () => {
    const capacity = { runtimes: [{ adapter_ref: 'rt_local', status: 'ok', total_memory_bytes: 8 * 2 ** 30, allocated_memory_bytes: 2 * 2 ** 30, total_cpu_millis: 4000, allocated_cpu_millis: 500 }] };
    const html = serverRender(dialog(), [
      [['me'], { verbs: [InstallVerb.View] }],
      [['capacity'], capacity],
    ]);
    expect(html).toContain('This host has 6.0 GiB of memory and 3.5 CPU not yet allocated to apps.');
    expect(serverRender(dialog(), [[['capacity'], capacity]])).not.toContain('not yet allocated');
  });
});
