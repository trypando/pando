// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import { answering, mount, type Mounted, type Seed } from '../ui/testing';
import { Installation } from './Installation';
import { addableKinds, isManaged, type AdapterKind } from './adapters';

// A kind Pando sets up itself, such as its BuildKit, is listed with its status
// and is neither added nor edited from the console. Which kinds those are is
// the server's to say (`managed`), not a name this file knows.

const buildkit: AdapterKind = {
  category: 'builder',
  kind: 'buildkit',
  name: 'BuildKit',
  description: 'Builds images from source in a rootless BuildKit that Pando runs itself.',
  id_prefix: 'bld_',
  managed: true,
  fields: [{ key: 'cache_max_bytes', label: 'Build service cache', type: 'int', advanced: true, default: '10737418240 (10 GiB)' }],
};
const loopback: AdapterKind = {
  category: 'routing',
  kind: 'loopback',
  name: 'Localhost',
  description: 'Reaches apps on this machine.',
  id_prefix: 'rte_',
  fields: null,
};

const seed = (adapters: object[]): Seed => [
  [['me'], { verbs: ['install.adapters.manage'] }],
  [['adapters'], { adapters }],
  [['adapter-kinds'], { kinds: [buildkit, loopback] }],
  [['sources'], { sources: [] }],
];
const builderRow = { id: 'bld_buildkit', category: 'builder', kind: 'buildkit', name: 'BuildKit', is_default: true, healthy: false };
const routingRow = { id: 'rte_loopback', category: 'routing', kind: 'loopback', name: 'Localhost', is_default: true, healthy: true };

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

/** The buttons in the adapters table row whose ID cell reads `id`. */
function rowButtons(id: string): string[] {
  const cell = [...document.querySelectorAll('[role="row"] span')].find((el) => el.textContent === id);
  const row = cell?.closest('[role="row"]');
  expect(row, `a row for ${id}`).toBeTruthy();
  return [...row!.querySelectorAll('button')].map((b) => String(b.textContent));
}

describe('a kind Pando sets up itself', () => {
  it('is told apart by the server’s marker, not by its name', () => {
    expect(addableKinds([buildkit, loopback])).toEqual([loopback]);
    expect(isManaged([buildkit, loopback], builderRow)).toBe(true);
    expect(isManaged([buildkit, loopback], routingRow)).toBe(false);
    expect(isManaged([{ ...buildkit, managed: false }], builderRow)).toBe(false);
  });

  it('is listed with its status and offers no Edit, while other rows do', () => {
    vi.stubGlobal('fetch', answering({}));
    screen = mount(<Installation />, seed([builderRow, routingRow]));
    expect(screen.text()).toContain('BuildKit');
    expect(screen.text()).toContain('bld_buildkit');
    expect(rowButtons('bld_buildkit')).not.toContain('Edit');
    expect(rowButtons('rte_loopback')).toContain('Edit');
  });

  it('is not offered in Add adapter', async () => {
    vi.stubGlobal('fetch', answering({}));
    screen = mount(<Installation />, seed([routingRow]));
    // A category whose only kind is managed is not "not set up".
    expect(screen.text()).not.toContain('Add a builder adapter');
    await screen.press('Add adapter');
    const categories = [...document.querySelectorAll('#s-category option')].map((o) => o.textContent);
    expect(categories).toContain('Routing');
    expect(categories).not.toContain('Builder');
  });
});
