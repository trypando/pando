// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { App } from '@api/types.gen';
import { mount, reply, serverRender, type Mounted, type Seed } from '../ui/testing';
import { AppSettings } from './AppSettings';
import { Egress } from './Egress';
import { SettingsDependencies } from './SettingsDependencies';
import { SettingsGeneral } from './SettingsGeneral';
import { SettingsStorage } from './SettingsStorage';
import { SettingsVariables } from './SettingsVariables';
import { AppVerb } from './verbs';

// An app's Settings tab: its pages, and what each lets somebody do.

const app = { id: 'app_1', name: 'crewmate', pinned_spec_id: 'spec_1', state: 'running' } as App;

/** The newest revision, as every page that edits one reads it. */
const newest = (body: unknown): Seed => [
  [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
  [['apps', 'app_1', 'spec', 1], { id: 'spec_1', revision: 1, body }],
];

interface Call {
  method: string;
  path: string;
  body?: unknown;
}

/** Answers every request with `answer`, and keeps what was sent. */
function recording(answer: (call: Call) => unknown = () => ({})) {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    const call: Call = { method: init?.method ?? 'GET', path: String(url).replace(/^.*\/api\/v1/, '') };
    if (init?.body) call.body = JSON.parse(String(init.body));
    calls.push(call);
    return reply(answer(call));
  });
  return calls;
}

const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET');

let screen: Mounted | undefined;
const open = (...args: Parameters<typeof mount>) => (screen = mount(...args));

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

const input = (label: string) => document.querySelector<HTMLInputElement>(`input[aria-label="${label}"]`);
const button = (label: string) => document.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);

describe('the settings pages', () => {
  const pages = (page: 'general' | 'variables' | 'storage', onPage = vi.fn()) => {
    recording();
    open(
      <AppSettings app={app} page={page} onPage={onPage} hrefOf={(p) => `/admin/apps/app_1/settings/${p}`} />,
      newest({ workloads: [{ name: 'web' }] }),
    );
    return onPage;
  };

  it('lists every page as a link to its own address, the open one marked', () => {
    pages('variables');
    const links = [...document.querySelectorAll<HTMLAnchorElement>('nav[aria-label="Settings"] a')];
    expect(links.map((a) => a.textContent)).toEqual(['General', 'Variables', 'Dependencies', 'Storage', 'Egress']);
    expect(links.map((a) => a.getAttribute('href'))).toEqual([
      '/admin/apps/app_1/settings/general',
      '/admin/apps/app_1/settings/variables',
      '/admin/apps/app_1/settings/dependencies',
      '/admin/apps/app_1/settings/storage',
      '/admin/apps/app_1/settings/egress',
    ]);
    expect(document.querySelector('a[aria-current="page"]')?.textContent).toBe('Variables');
  });

  it('shows one page at a time', () => {
    pages('storage');
    expect(screen!.text()).toContain('Kept across deployments and backed up daily');
    expect(screen!.text()).not.toContain('Ignored notices');
  });

  it('opens a page in place on a plain click', async () => {
    const onPage = pages('general');
    await screen!.click('Storage');
    expect(onPage).toHaveBeenCalledWith('storage');
  });
});

describe('the variable editor on Settings', () => {
  const spec = {
    workloads: [
      {
        name: 'web',
        env: [
          { key: 'NODE_ENV', value: 'production' },
          { key: 'SESSION_SECRET', secret_ref: 'SESSION_SECRET' },
          { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' },
        ],
      },
      { name: 'worker', env: [{ key: 'STRIPE_KEY', value: '' }] },
    ],
  };
  const seed: Seed = [...newest(spec), [['slots', 'app_1'], { slots: [{ key: 'DATABASE_URL', type: 'postgres' }] }]];
  const editor = (verbs: string[]) => {
    const calls = recording();
    open(<SettingsVariables appID="app_1" />, seed, verbs);
    return calls;
  };

  it('shows a dependency’s value as where it comes from, with nothing to change', () => {
    editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    expect(screen!.text()).toContain('From PostgreSQL');
    expect(input('Value of DATABASE_URL')).toBeNull();
    expect(button('Remove DATABASE_URL')).toBeNull();
  });

  it('outlines an empty value, and never shows a stored secret', () => {
    editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    expect(input('Value of STRIPE_KEY')!.closest('.pando-vfield')!.className).toContain('pando-vfield--empty');
    expect(input('Value of STRIPE_KEY')!.placeholder).toBe('No value');
    const stored = input('Value of SESSION_SECRET')!;
    expect(stored.value).toBe('');
    expect(stored.placeholder).toMatch(/^•+$/);
    expect(stored.type).toBe('password');
    expect(stored.closest('.pando-vfield')!.className).not.toContain('empty');
  });

  it('turns the lock, and counts the change', async () => {
    editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    const lock = button('Secret: NODE_ENV')!;
    expect(lock.getAttribute('aria-pressed')).toBe('false');
    expect(lock.title).toBe('Plain value. Click to store as a secret');
    await screen!.toggle('Secret: NODE_ENV');
    expect(button('Secret: NODE_ENV')!.getAttribute('aria-pressed')).toBe('true');
    expect(input('Value of NODE_ENV')!.type).toBe('password');
    expect(screen!.text()).toContain('1 unsaved change');
  });

  it('generates a value, kept as a secret', async () => {
    editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    const generate = [...document.querySelectorAll<HTMLButtonElement>('button[aria-label="Generate a random value"]')];
    // NODE_ENV, SESSION_SECRET, STRIPE_KEY: every value somebody can change.
    expect(generate).toHaveLength(3);
    await screen!.toggle('Generate a random value');
    expect(input('Value of STRIPE_KEY')!.value).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(button('Secret: STRIPE_KEY')!.getAttribute('aria-pressed')).toBe('true');
  });

  it('adds a variable to the workload whose card it was added in, and saves it with a secret first', async () => {
    const calls = editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    const adds = [...document.querySelectorAll('button')].filter((b) => b.textContent?.trim() === 'Add variable');
    expect(adds).toHaveLength(2);
    await screen!.press('Add variable');
    await screen!.type('Name', 'API_TOKEN');
    expect(screen!.text()).toContain('Looks like a credential.');
    await screen!.type('Value of API_TOKEN', 'tok-1');
    await screen!.toggle('Secret: API_TOKEN');
    await screen!.type('Value of NODE_ENV', 'development');
    expect(screen!.text()).toContain('2 unsaved changes');
    await screen!.press('Save changes');

    const sent = writes(calls);
    expect(sent.map((c) => `${c.method} ${c.path}`)).toEqual(['PUT /apps/app_1/secrets/API_TOKEN', 'POST /apps/app_1/specs']);
    expect(sent[0]!.body).toEqual({ value: 'tok-1' });
    const body = sent[1]!.body as { workloads: Array<{ name: string; env: unknown[] }> };
    expect(body.workloads[0]!.env).toEqual([
      { key: 'NODE_ENV', value: 'development', source: 'user' },
      { key: 'SESSION_SECRET', secret_ref: 'SESSION_SECRET' },
      { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' },
    ]);
    expect(body.workloads[1]!.env).toEqual([
      { key: 'STRIPE_KEY', value: '' },
      { key: 'API_TOKEN', secret_ref: 'API_TOKEN', source: 'user' },
    ]);
  });

  it('removes a variable, and discards the change', async () => {
    editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    await screen!.toggle('Remove NODE_ENV');
    expect(input('Value of NODE_ENV')).toBeNull();
    expect(screen!.text()).toContain('1 unsaved change');
    await screen!.press('Discard');
    expect(input('Value of NODE_ENV')).not.toBeNull();
    expect(screen!.text()).not.toContain('unsaved change');
  });

  it('keeps a secret’s row fixed without app.secrets.write, and plain ones changeable', () => {
    editor([AppVerb.SpecEdit]);
    expect(input('Value of SESSION_SECRET')!.disabled).toBe(true);
    expect(button('Remove SESSION_SECRET')).toBeNull();
    expect(input('Value of NODE_ENV')!.disabled).toBe(false);
    // The lock is shown, and cannot be turned.
    expect(button('Secret: NODE_ENV')!.disabled).toBe(true);
    expect(button('Secret: NODE_ENV')!.title).toBe('Plain value');
  });

  it('changes nothing without app.spec.edit', () => {
    editor([]);
    expect(input('Value of NODE_ENV')!.disabled).toBe(true);
    expect(screen!.text()).not.toContain('Add variable');
    expect(screen!.text()).not.toContain('Generate');
  });

  it('asks for a stored secret’s value before it becomes plain', async () => {
    editor([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    await screen!.toggle('Secret: SESSION_SECRET');
    expect(screen!.text()).toContain('Type SESSION_SECRET\'s value to store it as a plain value.');
    const save = [...document.querySelectorAll('button')].find((b) => b.textContent === 'Save changes')!;
    expect(save.disabled).toBe(true);
  });
});

describe('dependencies, filled one of three ways (R-131)', () => {
  const slots = [
    { key: 'DATABASE_URL', type: 'postgres', required: true },
    { key: 'SMTP_URL', type: 'smtp' },
  ];

  it('has Pando run one at a click', async () => {
    const calls = recording();
    open(<SettingsDependencies appID="app_1" />, [[['slots', 'app_1'], { slots }]], [AppVerb.SpecEdit]);
    const ways = [...document.querySelectorAll<HTMLButtonElement>('[aria-label="How DATABASE_URL is filled"] [role="radio"]')];
    await screen!.settle(() => ways[0]!.click());
    expect(writes(calls)[0]).toEqual({ method: 'PUT', path: '/apps/app_1/slots/DATABASE_URL', body: { mode: 'provisioned' } });
  });

  it('points one at an existing one, saved with its address', async () => {
    const calls = recording();
    open(<SettingsDependencies appID="app_1" />, [[['slots', 'app_1'], { slots }]], [AppVerb.SpecEdit]);
    const ways = [...document.querySelectorAll<HTMLButtonElement>('[role="radiogroup"][aria-label="How SMTP_URL is filled"] [role="radio"]')];
    expect(ways.map((w) => w.disabled)).toEqual([true, false, true]);
    await screen!.settle(() => ways[1]!.click());
    await screen!.type('Address of the Mail server for SMTP_URL', 'smtp://mail:587');
    await screen!.press('Save');
    expect(writes(calls)[0]).toEqual({
      method: 'PUT',
      path: '/apps/app_1/slots/SMTP_URL',
      body: { mode: 'bound', target: 'smtp://mail:587' },
    });
  });

  it('stores a value as a secret', async () => {
    const calls = recording();
    open(<SettingsDependencies appID="app_1" />, [[['slots', 'app_1'], { slots }]], [AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    const ways = [...document.querySelectorAll<HTMLButtonElement>('[aria-label="How SMTP_URL is filled"] [role="radio"]')];
    await screen!.settle(() => ways[2]!.click());
    expect(input('Value of SMTP_URL')!.type).toBe('password');
    await screen!.type('Value of SMTP_URL', 'smtp://u:p@mail');
    await screen!.press('Save');
    expect(writes(calls)[0]!.body).toEqual({ mode: 'literal', value: 'smtp://u:p@mail' });
  });
});

describe('storage (R-204, R-403)', () => {
  const spec = {
    resources: { disk_bytes: 10 * 2 ** 30 },
    slots: [{ key: 'DATABASE_URL', type: 'postgres', resolution: { mode: 'provisioned' } }],
    volumes: [
      { id: 'vol_up', name: 'uploads', declared: 'user' },
      { id: 'vol_img', name: 'data', declared: 'image' },
    ],
    workloads: [
      { name: 'web', mounts: [{ volume_id: 'vol_up', path: '/app/uploads' }, { volume_id: 'vol_img', path: '/data' }] },
    ],
  };
  const reading = {
    supported: true,
    workloads: [
      {
        name: 'web',
        disk_bytes: 2 ** 30 / 5,
        volumes: [
          { id: 'vol_up', name: 'uploads', path: '/app/uploads', bytes: 340 * 2 ** 20 },
          { id: 'vol_img', name: 'data', path: '/data', bytes: 2 ** 30 },
        ],
      },
    ],
  };
  const usageKey = ['apps', 'app_1', 'usage', 'running', 'spec_1'];
  const seed = (usage: unknown): Seed => [
    ...newest(spec),
    [usageKey, usage],
    [['volumes', 'app_1'], { volumes: [{ id: 'vol_up' }, { id: 'vol_img' }, { id: 'vol_pg', handle: 'pgdata', adapter_ref: 'docker' }] }],
  ];

  it('shows disk use against the limit, and each volume with its workload, path and size', () => {
    const html = serverRender(<SettingsStorage app={app} />, seed(reading), [AppVerb.SpecEdit]);
    expect(html).toContain('Disk use');
    expect(html).toContain('1.5 of 10 GiB');
    expect(html).toContain('role="meter"');
    expect(html).toContain('Includes storage and what each workload writes outside it.');
    expect(html).toContain('uploads');
    expect(html).toContain('/app/uploads');
    expect(html).toContain('340 MiB');
    // A volume a service Pando runs keeps, tagged with the service.
    expect(html).toContain('pgdata');
    expect(html).toContain('PostgreSQL');
  });

  it('offers Remove only on a volume somebody added', () => {
    const html = serverRender(<SettingsStorage app={app} />, seed(reading), [AppVerb.SpecEdit]);
    expect(html.match(/>Remove</g)).toHaveLength(1);
    expect(serverRender(<SettingsStorage app={app} />, seed(reading))).not.toContain('>Remove<');
  });

  it('says nothing of disk use when the runtime can’t report it', () => {
    expect(serverRender(<SettingsStorage app={app} />, seed({ supported: false }))).not.toContain('Disk use');
  });

  it('removes a volume from the spec, and what mounts it', async () => {
    const calls = recording();
    open(<SettingsStorage app={app} />, seed(reading), [AppVerb.SpecEdit]);
    await screen!.press('Remove');
    expect(screen!.text()).toContain('Remove uploads?');
    await screen!.press('Remove');
    const body = writes(calls)[0]!.body as typeof spec;
    expect(body.volumes.map((v) => v.id)).toEqual(['vol_img']);
    expect(body.workloads[0]!.mounts).toEqual([{ volume_id: 'vol_img', path: '/data' }]);
  });
});

describe('egress, edited in place (R-182 – R-184)', () => {
  const allowlist = {
    install: { mode: 'allowlist', list: ['registry.npmjs.org'], loosening: 'approval' },
    spec: { mode: 'inherit' },
    effective: { mode: 'allowlist', list: [], block_private: false, restricted: true },
  };
  const seed = (data: unknown, egress: unknown = { mode: 'inherit' }): Seed => [[['apps', 'app_1', 'egress'], data], ...newest({ egress })];

  it('marks an address that loosens the host’s rules before it is allowed, then saves it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const calls = recording((c) =>
      c.path.includes('dry_run')
        ? { dry_run: true, egress: {}, egress_changed: true, new_loosenings: [{ kind: 'allowlist_add', entry: 'api.stripe.com', message: 'Adds it.' }], approval: { required: true, reasons: [] } }
        : {},
    );
    open(<Egress appID="app_1" />, seed(allowlist), [AppVerb.EgressLoosen]);
    await screen!.type('Address', 'api.stripe.com');
    await screen!.settle(() => vi.advanceTimersByTime(500));
    await screen!.settle(() => undefined);
    vi.useRealTimers();
    expect(screen!.text()).toContain('Needs approval to deploy');
    expect(calls.some((c) => c.path === '/apps/app_1/specs?dry_run=true')).toBe(true);
    await screen!.press('Allow');
    const saved = writes(calls).filter((c) => c.path === '/apps/app_1/specs');
    expect(saved).toHaveLength(1);
    expect((saved[0]!.body as { egress: unknown }).egress).toEqual({ mode: 'inherit', add: ['api.stripe.com'] });
  });

  it('removes an address the app added', async () => {
    const calls = recording();
    open(<Egress appID="app_1" />, seed(allowlist, { mode: 'inherit', add: ['api.stripe.com'] }), [AppVerb.EgressTighten]);
    await screen!.press('Remove');
    expect((writes(calls)[0]!.body as { egress: unknown }).egress).toEqual({ mode: 'inherit' });
  });

  it('restricts an unrestricted app with a list of its own, allowed or blocked', async () => {
    const calls = recording();
    const open_ = { install: { mode: 'allow_all' }, spec: { mode: 'inherit' }, effective: { mode: 'allow_all', block_private: false, restricted: false } };
    open(<Egress appID="app_1" />, seed(open_), [AppVerb.EgressTighten]);
    await screen!.press('Restrict this app');
    expect(screen!.text()).toContain('This host allows any address.');
    await screen!.type('Address', 'tracker.example.com');
    await screen!.press('Block');
    const egress = (writes(calls).find((c) => c.path === '/apps/app_1/specs')!.body as { egress: unknown }).egress;
    expect(egress).toEqual({ mode: 'denylist', list: ['tracker.example.com'] });
  });

  it('turns blocking private addresses on for the app', async () => {
    const calls = recording();
    const open_ = { install: { mode: 'allow_all' }, spec: { mode: 'inherit' }, effective: { mode: 'allow_all', block_private: false, restricted: false } };
    open(<Egress appID="app_1" />, seed(open_), [AppVerb.EgressTighten]);
    await screen!.toggle('Block private addresses');
    expect((writes(calls)[0]!.body as { egress: unknown }).egress).toEqual({ mode: 'inherit', block_private: true });
  });
});

describe('ignored notices (R-412)', () => {
  const notices = (list: unknown[]): Seed => [
    [['apps', 'app_1', 'notices'], { notices: list }],
    ...newest({ workloads: [{ name: 'web' }] }),
  ];

  it('says so when nothing is ignored', () => {
    const html = serverRender(<SettingsGeneral app={app} />, notices([{ key: 'a', message: 'Shown.', fingerprint: 'f', ignored: false }]));
    expect(html).toContain('Nothing ignored.');
    expect(html).not.toContain('Shown.');
  });

  it('restores one, and it leaves the list', async () => {
    const calls = recording();
    open(<SettingsGeneral app={app} />, notices([{ key: 'backup.not_taken', message: 'The last backup was not taken.', fingerprint: 'f', ignored: true }]));
    expect(screen!.text()).toContain('The last backup was not taken.');
    await screen!.press('Restore');
    expect(writes(calls)).toEqual([{ method: 'DELETE', path: '/apps/app_1/notices/backup.not_taken/ignore' }]);
    expect(screen!.text()).toContain('Nothing ignored.');
  });
});

describe('the Build card', () => {
  const spec = {
    build: { strategy: 'railpack', generated_files: { Dockerfile: 'FROM node:22' } },
    workloads: [
      { name: 'web', primary: true, command: ['node', 'dist/server.js'], ports: [{ number: 3000 }] },
      { name: 'worker', command: ['node', 'dist/worker.js'] },
    ],
  };
  const detection = (over: object = {}) => ({
    status: 'ready',
    answers: {},
    commit: 'a3f9c21deadbeef',
    updated_at: '2026-10-20T00:00:00Z',
    detection: {
      winning_bid: { detector: 'railpack', strategy: 'railpack', confidence: 0.9, evidence: ['package.json names a start script.'] },
      runners_up: [{ detector: 'dockerfile', strategy: 'dockerfile', confidence: 0.4, evidence: ['A Dockerfile is present.'] }],
      questions: [{ key: 'port', prompt: 'Pando could not determine which port this app serves HTTP on. Valid answer: a port number, such as 3000.', kind: 'port' }],
      draft_spec: { warnings: [] },
    },
    ...over,
  });
  const seed = (d: unknown): Seed => [...newest({ ...spec, created_at: '2026-10-21T00:00:00Z' }), [['apps', 'app_1', 'detection'], d]];

  it('lists what Pando found, and offers the plan and another look', () => {
    const html = serverRender(<SettingsGeneral app={app} />, seed(detection()), [AppVerb.SpecEdit]);
    expect(html).toContain('Built with');
    expect(html).toContain('node dist/server.js');
    expect(html).toContain('node dist/worker.js');
    expect(html).toContain('3000');
    expect(html).toContain('from a3f9c21');
    expect(html).toContain('View build plan');
    expect(html).toContain('Run detection again');
    expect(html).toContain('How Pando decided');
    // Not newer than the configuration: nothing to replace.
    expect(html).not.toContain('Replace configuration');
  });

  it('shows how Pando decided, each question as written with its copy button', async () => {
    recording();
    open(<SettingsGeneral app={app} />, seed(detection()), [AppVerb.SpecEdit]);
    await screen!.press('How Pando decided');
    expect(screen!.text()).toContain('package.json names a start script.');
    expect(screen!.text()).toContain('Pando could not determine which port this app serves HTTP on. Valid answer: a port number, such as 3000.');
    expect(button('Copy this question')).not.toBeNull();
    expect(screen!.text()).toContain('Other candidates (1)');
  });

  it('offers a newer detection as a replacement, once its questions are answered', () => {
    const newer = serverRender(<SettingsGeneral app={app} />, seed(detection({ updated_at: '2026-10-22T00:00:00Z' })), [AppVerb.SpecEdit]);
    expect(newer).toContain('A newer detection has a question to answer.');
    const answered = serverRender(
      <SettingsGeneral app={app} />,
      seed(detection({ updated_at: '2026-10-22T00:00:00Z', answers: { port: '3000' } })),
      [AppVerb.SpecEdit],
    );
    expect(answered).toContain('A newer detection isn’t applied yet.');
    expect(answered).toContain('Replace configuration');
  });

  it('offers no other look to somebody who can only read it', () => {
    expect(serverRender(<SettingsGeneral app={app} />, seed(detection()))).not.toContain('Run detection again');
  });

  it('opens the build plan, and runs detection again', async () => {
    const calls = recording((c) => (c.path.endsWith('/detection') ? detection() : {}));
    open(<SettingsGeneral app={app} />, seed(detection()), [AppVerb.SpecEdit]);
    await screen!.press('View build plan');
    expect(screen!.text()).toContain('FROM node:22');
    await screen!.press('Edit');
    expect(screen!.text()).toContain('Saved as a new revision.');
    await screen!.press('Discard');
    await screen!.press('Close');
    await screen!.press('Run detection again');
    expect(writes(calls)).toEqual([{ method: 'POST', path: '/apps/app_1/detection/rerun' }]);
  });

  it('answers a question, and replaces the configuration with a newer detection', async () => {
    const newer = detection({ updated_at: '2026-10-22T00:00:00Z', answers: { port: '3000' } });
    const calls = recording((c) => (c.path.endsWith('/detection') ? newer : {}));
    open(<SettingsGeneral app={app} />, seed(newer), [AppVerb.SpecEdit]);
    await screen!.press('How Pando decided');
    await screen!.fill('Answer', '8080');
    await screen!.press('Save answer');
    await screen!.press('Replace configuration');
    expect(screen!.text()).toContain('Variables, dependencies and storage you set are kept.');
    await screen!.press('Replace it');
    expect(writes(calls)).toEqual([
      { method: 'POST', path: '/apps/app_1/detection/answers', body: { answers: { port: '8080' } } },
      { method: 'POST', path: '/apps/app_1/detection/accept', body: { confirm: true } },
    ]);
  });

  it('says why a detection failed, with a way to look again', async () => {
    const failed = detection({ status: 'failed', detection: { error: { message: 'Pando could not clone the repository.' } } });
    const calls = recording((c) => (c.path.endsWith('/detection') ? failed : {}));
    open(<SettingsGeneral app={app} />, seed(failed), [AppVerb.SpecEdit]);
    expect(screen!.text()).toContain('Pando could not clone the repository.');
    await screen!.press('Try again');
    expect(writes(calls)).toEqual([{ method: 'POST', path: '/apps/app_1/detection/rerun' }]);
  });

  it('says why Pando can’t run what it found', () => {
    const blocked = detection({ status: 'blocked', detection: { blocked: { message: 'The compose file uses network_mode: host.' } } });
    expect(serverRender(<SettingsGeneral app={app} />, seed(blocked))).toContain('The compose file uses network_mode: host.');
  });

  it('says it is reading while detection runs', () => {
    expect(serverRender(<SettingsGeneral app={app} />, seed(detection({ status: 'running' })))).toContain('Reading the repository.');
  });

  it('shows a carried file', async () => {
    recording();
    const files = { ...spec, workloads: [{ name: 'web', files: [{ path: '/etc/caddy/Caddyfile', content: ':80' }] }] };
    open(<SettingsGeneral app={app} />, [...newest(files), [['apps', 'app_1', 'detection'], detection()]]);
    await screen!.press('/etc/caddy/Caddyfile');
    expect(screen!.text()).toContain('Placed in web each time it starts.');
  });
});

describe('the cards General shows where they apply', () => {
  it('shows what automatic deployments watch for an app from a repository', () => {
    const html = serverRender(<SettingsGeneral app={app} />, newest({ source: { type: 'git', ref: 'main' }, deploy: {} }));
    expect(html).toContain('Automatic deploys');
  });

  it('shows the registry credential for an app from an image', () => {
    const html = serverRender(
      <SettingsGeneral app={{ ...app, source: { type: 'image' } } as App} />,
      [[['apps', 'app_1', 'registry-credential'], { set: false }]],
    );
    expect(html).toContain('Registry credential');
  });
});

describe('adding storage', () => {
  it('declares a path in the chosen workload', async () => {
    const calls = recording();
    open(<SettingsStorage app={app} />, newest({ workloads: [{ name: 'web' }, { name: 'worker' }] }), [AppVerb.SpecEdit]);
    expect(screen!.text()).toContain('No storage.');
    await screen!.press('Add storage');
    await screen!.fill('Name', 'uploads');
    await screen!.fill('Path', '/app/uploads');
    await screen!.fill('Workload', 'worker');
    await screen!.press('Add storage');
    expect(writes(calls)).toEqual([
      { method: 'POST', path: '/apps/app_1/volumes', body: { name: 'uploads', path: '/app/uploads', workload: 'worker' } },
    ]);
  });
});
