// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';

import { AdapterDialog } from './AdapterDialog';
import type { AdapterKind } from './adapters';
import { mount, reply, serverRender } from '../ui/testing';
import type { Mounted } from '../ui/testing';

const tunnel: AdapterKind = {
  category: 'routing',
  kind: 'tunnel',
  name: 'Tunnel',
  description: 'Reaches apps through a tunnel.',
  id_prefix: 'rte_',
  fields: [
    { key: 'api_token', label: 'API token', type: 'string', credential: true, required: true },
    { key: 'zone', label: 'Zone', type: 'string', required: true },
    { key: 'other_zones', label: 'Other zones', type: 'string', list: true, placeholder: 'example.org' },
    { key: 'console_hostname', label: 'Console hostname', type: 'string', suggest: 'pando.{zone}' },
    { key: 'image', label: 'Image', type: 'string', default: 'tunnel:1', advanced: true },
  ],
  steps: [
    {
      id: 'token',
      label: 'API token',
      title: 'Create an API token',
      do: [
        { text: 'Open **API Tokens** and run `make-token`.' },
        { text: 'Add these:', table: { columns: ['Scope', 'Access'], rows: [['Zone', 'Edit']] } },
      ],
      links: [
        { label: 'Open tokens', url: 'https://dash.example.com/tokens' },
        { label: 'Open zone', url: 'https://{zone}/admin' },
      ],
      fields: ['api_token'],
    },
    { id: 'zones', label: 'Zones', title: 'Choose zones', lede: 'Apps go under `{zone}`.', fields: ['zone', 'other_zones', 'console_hostname'] },
  ],
};

const plain = (n: number): AdapterKind => ({
  category: 'routing',
  kind: `plain${n}`,
  name: `Plain ${n}`,
  description: `Plain number ${n}.`,
  id_prefix: 'rte_',
  fields: [],
});

const sink: AdapterKind = {
  category: 'audit_sink',
  kind: 'https',
  name: 'HTTPS',
  description: 'Posts events.',
  id_prefix: 'as_',
  fields: [
    { key: 'url', label: 'URL', type: 'string' },
    { key: 'token', label: 'Token', type: 'string', credential: true },
    { key: 'body', label: 'Body', type: 'string', default: 'ndjson' },
  ],
  presets: [{ id: 'splunk', label: 'Splunk', values: { body: 'splunk_hec', url: 'https://splunk:8088' } }],
  other_preset: 'Another SIEM',
  steps: [
    { id: 'splunk_token', label: 'Token', title: 'Create an HEC token', presets: ['splunk'], fields: ['token'] },
    { id: 'token', label: 'Token', title: 'Enter the token', presets: ['other'], fields: ['token', 'body'] },
    { id: 'url', label: 'URL', title: 'Enter the endpoint', fields: ['url'] },
  ],
};

let screen: Mounted | undefined;
afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

function open(kinds: AdapterKind[], category?: string) {
  const onSaved = vi.fn();
  screen = mount(<AdapterDialog category={category} adapters={[]} onClose={() => undefined} onSaved={onSaved} />, [
    [['adapter-kinds'], { kinds }],
  ]);
  return { s: screen, onSaved };
}

async function type(selector: string, value: string, key?: string) {
  const el = document.querySelector<HTMLInputElement>(selector)!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    if (key) el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
    await new Promise((r) => setTimeout(r, 0));
  });
}

describe('adding an adapter one step at a time', () => {
  it('walks a kind’s steps to a saved adapter, as the API is sent it', async () => {
    const fetch = vi.fn(async () => reply({ id: 'rte_tunnel', warnings: ['Check the hostname.'] }, 201));
    vi.stubGlobal('fetch', fetch);
    const { s, onSaved } = open([tunnel, plain(1)]);

    expect(s.text()).toContain('Add adapter');
    expect(s.text()).not.toContain('Takes effect');
    await s.press('Continue');
    expect(s.text()).toContain('Choose a category to continue.');
    await s.fill('Category', 'routing');
    expect(s.text()).toContain('Add routing adapter');
    await s.press('Continue');
    expect(s.text()).toContain('Choose an adapter to continue.');
    await s.click('Tunnel');
    // Still choosing: the title names the category until the kind is past choosing.
    expect(s.text()).toContain('Add routing adapter');
    await s.press('Continue');

    expect(s.text()).toContain('Add Tunnel');
    expect(s.text()).toContain('Create an API token');
    expect(s.text()).toContain('make-token');
    expect(s.text()).toContain('Zone');
    expect(s.text()).toContain('Open tokens');
    // A link naming a setting not yet answered is held back.
    expect(s.text()).not.toContain('Open zone');
    await s.press('Continue');
    expect(s.text()).toContain('API token is required.');
    await s.fill('API token (required)', 'tok');
    await s.press('Continue');

    expect(s.text()).toContain('Choose zones');
    await s.fill('Zone (required)', 'example.com');
    expect(s.text()).toContain('Apps go under example.com.');
    expect((document.getElementById('f-console-hostname') as HTMLInputElement).value).toBe('pando.example.com');
    await type('#list-other_zones', 'example.org', 'Enter');
    await type('#list-other_zones', 'example.net,');
    expect(s.text()).toContain('example.org');
    expect(s.text()).toContain('example.net');
    await s.press('Continue');

    expect(s.text()).toContain('Can’t be changed later.');
    await s.press('Continue');
    expect(s.text()).toContain('Review');
    expect(s.text()).toContain('pando.example.com');
    expect(s.text()).toContain('example.org, example.net');
    expect(s.text()).toContain('Advanced settings');

    // The step list goes back to a finished step.
    await s.press('API token');
    expect(s.text()).toContain('Create an API token');
    await s.press('Continue');
    await s.press('Continue');
    await s.press('Continue');
    await s.press('Add adapter');

    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toMatchObject({
      id: 'rte_tunnel',
      category: 'routing',
      kind: 'tunnel',
      config: { zone: 'example.com', other_zones: 'example.org, example.net', console_hostname: 'pando.example.com' },
      credentials: { api_token: 'tok' },
    });
    expect(onSaved).toHaveBeenCalledWith('Tunnel', 'routing', ['Check the hostname.'], 'rte_tunnel');
  });

  it('says why there is nothing to choose when the kinds can’t be read', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => reply({ code: 'INTERNAL', message: 'Pando couldn’t list adapter kinds.' }, 500)),
    );
    screen = mount(<AdapterDialog adapters={[]} onClose={() => undefined} onSaved={() => undefined} />);
    // The refusal arrives after the request settles, a few ticks in under load.
    for (let i = 0; i < 50 && !screen.text().includes('couldn’t'); i++) {
      await act(async () => {
        await new Promise((r) => setTimeout(r, 10));
      });
    }
    expect(screen.text()).toContain('Pando couldn’t list adapter kinds.');
    expect(screen.text()).not.toContain('Choose a category');
  });

  it('shows a problem with a setting no step asks on Review, and saves nothing', async () => {
    const fetch = vi.fn(async () => reply({ id: 'rte_tunnel' }, 201));
    vi.stubGlobal('fetch', fetch);
    const regional: AdapterKind = {
      ...tunnel,
      fields: [...(tunnel.fields ?? []), { key: 'region', label: 'Region', type: 'string', required: true }],
    };
    const { s } = open([regional], 'routing');
    await s.fill('API token (required)', 'tok');
    await s.press('Continue');
    await s.fill('Zone (required)', 'example.com');
    await s.press('Continue');
    await s.press('Continue');
    await s.press('Add adapter');

    expect(fetch).not.toHaveBeenCalled();
    expect(s.text()).toContain('Region is required.');
  });

  it('switches to every setting at once, keeping what was entered, and back', async () => {
    const { s } = open([tunnel], 'routing');
    expect(s.text()).not.toContain('Choose an adapter');
    expect(s.text()).toContain('Create an API token');
    await s.fill('API token (required)', 'tok');
    await s.press('Show all settings');
    expect(s.text()).toContain('Set up step by step');
    expect((document.getElementById('f-api-token-required-') as HTMLInputElement).value).toBe('tok');
    await s.press('Set up step by step');
    expect(s.text()).toContain('Create an API token');
  });

  it('counts the steps above the one being answered at phone width, instead of a rail beside it', async () => {
    vi.stubGlobal(
      'matchMedia',
      (query: string) =>
        ({ matches: true, media: query, addEventListener: () => {}, removeEventListener: () => {} }) as unknown as MediaQueryList,
    );
    const html = serverRender(
      <AdapterDialog category="routing" adapters={[]} onClose={() => undefined} onSaved={() => undefined} />,
      [[['adapter-kinds'], { kinds: [tunnel] }]],
    );
    expect(html).toContain('Step 1 of 4');
    expect(html).not.toContain('aria-label="Steps"');
    const { s } = open([tunnel], 'routing');
    expect(document.querySelector('nav[aria-label="Steps"]')).toBeNull();
    expect(s.text()).toContain('Step 1 of 4');
    await s.fill('API token (required)', 'tok');
    await s.press('Continue');
    expect(s.text()).toContain('Step 2 of 4');
    await s.press('Show all settings');
    expect(s.text()).toContain('Set up step by step');
  });

  it('can search a long list of adapters', async () => {
    const { s } = open([tunnel, ...[1, 2, 3, 4, 5, 6].map(plain)], 'routing');
    expect(s.text()).toContain('Plain 6');
    await s.fill('Search adapters', 'tun');
    expect(s.text()).not.toContain('Plain 6');
    expect(s.text()).toContain('Tunnel');
    await s.fill('Search adapters', 'zzz');
    expect(s.text()).toContain('Nothing matches “zzz”.');
  });

  it('takes chips back and away', async () => {
    const { s } = open([tunnel], 'routing');
    await s.fill('API token (required)', 'tok');
    await s.press('Continue');
    await type('#list-other_zones', 'a.org b.org');
    expect(s.text()).toContain('a.org');
    expect(s.text()).toContain('b.org');
    // Backspace in the empty field takes the last back to edit.
    await type('#list-other_zones', '', 'Backspace');
    expect((document.getElementById('list-other_zones') as HTMLInputElement).value).toBe('b.org');
    await act(async () => {
      document.querySelector<HTMLButtonElement>('button[aria-label="Remove a.org"]')!.click();
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(s.text()).not.toContain('a.org');
  });
});

describe('a kind with presets', () => {
  it('asks which provider first, and shows only that provider’s steps', async () => {
    const { s } = open([sink], 'audit_sink');
    expect(s.text()).toContain('Choose a provider');
    await s.press('Continue');
    expect(s.text()).toContain('Choose a provider to continue.');
    await s.click('Splunk');
    await s.press('Continue');
    expect(s.text()).toContain('Create an HEC token');
    expect(s.text()).not.toContain('Enter the token');
    await s.press('Continue');
    expect((document.getElementById('f-url') as HTMLInputElement).value).toBe('https://splunk:8088');

    await s.press('Provider');
    await s.click('Another SIEM');
    await s.press('Continue');
    expect(s.text()).toContain('Enter the token');
    await s.press('Continue');
    // Splunk's URL is taken back with Splunk.
    expect((document.getElementById('f-url') as HTMLInputElement).value).toBe('');
    await s.press('Continue');
    await s.press('Continue');
    expect(s.text()).toContain('Provider');
    expect(s.text()).toContain('Another SIEM');
  });
});
