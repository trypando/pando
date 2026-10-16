// @vitest-environment jsdom
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { mount, reply, serverRender, unanswered, type Mounted, type Seed } from '../ui/testing';
import { AppAddress } from './AppAddress';
import { Egress } from './Egress';
import { Environment } from './Environment';
import { Lifecycle } from './Lifecycle';
import { Reference } from './Reference';
import { Resources } from './Resources';
import { Security } from './Security';
import { AppVerb } from './verbs';

// What a screen says as it opens over the data it reads, and once somebody
// opens a dialog in it or waits on what they asked for.

const app = { id: 'app_1', name: 'inventory', slug: 'inventory', pinned_spec_id: 'spec_1' } as App;

/** The newest spec revision, as the screens that edit one read it. */
const newest = (body: unknown): Seed => [
  [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
  [['apps', 'app_1', 'spec', 1], { id: 'spec_1', revision: 1, body }],
];

/** Seven findings on the app's newest scan, more than the top five. */
const findings = Array.from({ length: 7 }, (_, i) => ({ id: `CVE-2026-${i}`, severity: 'low', package: 'zlib' }));
const scanned: Seed = [
  [['apps', 'app_1', 'security'], { standing: { verdict: 'ok' }, scan: { score: 90, findings }, counts: {}, ignoring_unfixable: false }],
];
const answers = (on: boolean): Seed => [[['ai-functions'], { functions: [{ function: 'answer_reference', on }] }]];

let screen: Mounted | undefined;
const open = (...args: Parameters<typeof mount>) => (screen = mount(...args));

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('a screen as it opens', () => {
  const screens: Array<[string, ReactNode, Seed, string, string]> = [
    ['the top five findings', <Security appID="app_1" />, scanned, 'Show all 7 findings', 'CVE-2026-5'],
    ['what deleting the app does to storage', <Resources app={app} />, [[['slots', 'app_1'], { slots: [] }]], 'Deleting the app offers to keep a final backup.', 'Add storage'],
    ['an app with no variables', <Environment appID="app_1" />, newest({ workloads: [{ name: 'web' }] }), 'No variables.', 'Add variable'],
    ['asking AI, where it answers', <Reference />, answers(true), 'Ask AI', 'Ask another question'],
    ['no asking AI, where it doesn’t', <Reference />, answers(false), 'API and tools', 'Ask AI'],
  ];
  it.each(screens)('shows %s', (_, view, data, said, unsaid) => {
    const html = serverRender(view, data);
    expect(html).toContain(said);
    expect(html).not.toContain(unsaid);
  });
});

describe('changing an app’s address', () => {
  const option = (name: string, extra: object = {}) => ({
    adapter_ref: `rte_${name.toLowerCase()}`,
    name,
    modes: ['subdomain', 'path', 'port'],
    default_mode: 'subdomain',
    is_default: false,
    ...extra,
  });
  const change = async (options: unknown[], verbs: string[]) => {
    const view = { routing: { adapter_ref: 'rte_alpha', mode: 'subdomain', hostname: 'inventory.example.com' }, options };
    const s = open(<AppAddress app={app} />, [[['apps', 'app_1', 'routing'], view]], [AppVerb.SpecEdit, ...verbs]);
    await s.press('Change');
    return s.text();
  };

  it('names the default adapter among a few, and says where a port comes from', async () => {
    const text = await change([option('Alpha', { is_default: true }), option('Beta')], [AppVerb.RoutingOverride]);
    expect(text).toContain('Alpha (default)');
    expect(text).toContain('Beta');
    expect(text).toContain('Assigned from the installation’s range.');
    expect(text).not.toContain('Needs permission');
  });

  it('says what a port shares with Pando, as it says what a path does', async () => {
    const view = { routing: { adapter_ref: 'rte_alpha', mode: 'port' }, options: [option('Alpha', { is_default: true })] };
    const s = open(<AppAddress app={app} />, [[['apps', 'app_1', 'routing'], view]], [AppVerb.SpecEdit, AppVerb.RoutingOverride]);
    await s.press('Change');
    expect(s.text()).toContain('browsers keep cookies apart by host, not by port');
    expect(s.text()).not.toContain('At a path, this app shares Pando’s address');
  });

  it('says what a path shares with Pando', async () => {
    const view = { routing: { adapter_ref: 'rte_alpha', mode: 'path', path_prefix: '/inventory' }, options: [option('Alpha', { is_default: true })] };
    const s = open(<AppAddress app={app} />, [[['apps', 'app_1', 'routing'], view]], [AppVerb.SpecEdit, AppVerb.RoutingOverride]);
    await s.press('Change');
    expect(s.text()).toContain('At a path, this app shares Pando’s address');
    expect(s.text()).not.toContain('not by port');
  });

  it('names the default adapter among many, and which modes need permission', async () => {
    const text = await change([option('Alpha'), option('Beta', { is_default: true }), option('Gamma'), option('Delta')], []);
    expect(text).toContain('Beta (default)');
    expect(text).toContain('Needs permission to override routing.');
  });
});

describe('adding an environment variable', () => {
  const add = async (verbs: string[]) => {
    const s = open(<Environment appID="app_1" />, newest({ workloads: [{ name: 'web' }] }), [AppVerb.SpecEdit, ...verbs]);
    await s.press('Add variable');
    return s;
  };

  it('says why the type is fixed for somebody who can’t store secrets', async () => {
    const s = await add([]);
    expect(s.text()).toContain('You can’t store secrets on this app.');
    expect(s.text()).not.toContain('Never shown again.');
  });

  it('says a secret is never shown again', async () => {
    const s = await add([AppVerb.SecretsWrite]);
    await s.fill('Type', 'secret');
    expect(s.text()).toContain('Never shown again.');
    expect(s.text()).not.toContain('You can’t store secrets');
  });
});

describe('editing egress', () => {
  const edit = async (install: string, egress: object) => {
    const data = { install: { mode: install }, effective: { mode: install, block_private: false, restricted: false }, spec: egress };
    const s = open(<Egress appID="app_1" />, [[['apps', 'app_1', 'egress'], data], ...newest({ egress })], [AppVerb.EgressTighten]);
    await s.press('Change');
    return s.text();
  };

  it.each([
    ['allowlist', 'Allowed destinations'],
    ['denylist', 'Blocked destinations'],
  ])('names the app’s own %s', async (mode, label) => {
    expect(await edit('denylist', { mode, list: ['example.com'] })).toContain(label);
  });

  it('keeps additions to an installation with no list, to be cleared', async () => {
    expect(await edit('allow_all', { add: ['example.com'] })).toContain('No effect without an installation list.');
  });
});

describe('a fill for a dependency Pando can’t provision', () => {
  it.each([
    ['smtp', 'Pando doesn’t provision email.'],
    ['kafka', 'Pando doesn’t provision kafka.'],
  ])('names a %s dependency', async (type, said) => {
    const s = open(<Resources app={app} />, [[['slots', 'app_1'], { slots: [{ key: 'URL', type }] }]], [AppVerb.SpecEdit]);
    await s.press('Change');
    expect(s.text()).toContain(said);
  });
});

describe('the security findings', () => {
  it('shows the top five, then all of them', async () => {
    const s = open(<Security appID="app_1" />, scanned);
    await s.press('Show all 7 findings');
    expect(s.text()).toContain('Show top five');
  });
});

describe('stopping an app', () => {
  it('says it is stopping while it waits', async () => {
    vi.stubGlobal('fetch', unanswered);
    const s = open(<Lifecycle app={{ ...app, desired_state: 'running' } as App} />);
    await s.press('Stop');
    await s.press('Stop app');
    expect(s.text()).toContain('Stopping');
  });
});

describe('asking how to do something', () => {
  it('asks for another question once one is answered', async () => {
    vi.stubGlobal('fetch', async () => reply({ answer: 'Grant the group the viewer role.', cites: [] }));
    const s = open(<Reference />, answers(true));
    await s.press('Ask AI');
    expect(s.text()).toContain('Question');
    await s.fill('Question', 'How do I share an app?');
    await s.submit('Question');
    expect(s.text()).toContain('Ask another question');
  });
});
