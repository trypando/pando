// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AdapterDialog } from './AdapterDialog';
import type { ConfiguredAdapter } from './AdapterDialog';
import type { AdapterKind } from './adapters';

// Rendered into a document so Save can be pressed: making an adapter the
// default of a category that has one is asked first (issue 220).
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const cloudflare: AdapterKind = {
  category: 'routing',
  kind: 'cloudflare',
  name: 'Cloudflare',
  description: 'Reaches apps through a Cloudflare tunnel.',
  id_prefix: 'rte_',
  fields: [{ key: 'zone', label: 'Zone', type: 'string' }],
};

const existing: ConfiguredAdapter = {
  id: 'rte_cloudflare',
  category: 'routing',
  kind: 'cloudflare',
  name: 'Cloudflare',
  is_default: false,
  enabled: true,
  config: { zone: 'bemeek.io' },
};
const loopback: ConfiguredAdapter = { id: 'rte_loopback', category: 'routing', kind: 'loopback', name: 'Localhost', is_default: true };

let root: Root;
let container: HTMLDivElement;
let onSaved: ReturnType<typeof vi.fn<(name: string, category: string) => void>>;

beforeEach(() => {
  container = document.createElement('div');
  document.body.append(container);
  onSaved = vi.fn<(name: string, category: string) => void>();
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queries.setQueryData(['adapter-kinds'], { kinds: [cloudflare] });
  root = createRoot(container);
  act(() =>
    root.render(
      <QueryClientProvider client={queries}>
        <AdapterDialog existing={existing} adapters={[loopback, existing]} onClose={() => undefined} onSaved={onSaved} />
      </QueryClientProvider>,
    ),
  );
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
});

function button(text: string): HTMLButtonElement {
  const found = [...container.querySelectorAll('button')].find((b) => b.textContent === text);
  expect(found, `a button "${text}"`).toBeDefined();
  return found!;
}

async function press(text: string) {
  await act(async () => {
    button(text).click();
    await new Promise((r) => setTimeout(r, 0));
  });
}

function checkDefault() {
  const box = [...container.querySelectorAll('label')]
    .find((l) => l.textContent?.includes('Use as the default routing adapter'))
    ?.querySelector('input');
  expect(box).toBeTruthy();
  act(() => box!.click());
}

const reply = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

describe('making an adapter the default in place of another, issue 220', () => {
  it('asks first, naming both, and saves only on Make default', async () => {
    const fetch = vi.fn(async () => reply(201, { id: 'rte_cloudflare', replaced_default: 'rte_loopback' }));
    vi.stubGlobal('fetch', fetch);

    checkDefault();
    await press('Save adapter');
    expect(container.textContent).toContain('Make Cloudflare the default?');
    expect(container.textContent).toContain('This makes Cloudflare the default routing adapter in place of Localhost.');
    expect(fetch).not.toHaveBeenCalled();

    await press('Cancel');
    expect(container.textContent).not.toContain('Make Cloudflare the default?');

    await press('Save adapter');
    await press('Make default');
    expect(fetch).toHaveBeenCalledTimes(1);
    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toMatchObject({ id: 'rte_cloudflare', is_default: true });
    expect(onSaved).toHaveBeenCalledWith('Cloudflare', 'routing');
  });

  it('shows why a handover was refused', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        reply(400, {
          code: 'VALID_INVALID',
          message: 'Another routing adapter became the default while rte_cloudflare was being saved, and each category has one default.',
          remedy: 'Save it again to make it the default in place of that one.',
        }),
      ),
    );

    checkDefault();
    await press('Save adapter');
    await press('Make default');
    expect(container.textContent).toContain('became the default while rte_cloudflare was being saved');
    expect(onSaved).not.toHaveBeenCalled();
  });

  it('does not ask when the default is left as it is', async () => {
    const fetch = vi.fn(async () => reply(201, { id: 'rte_cloudflare' }));
    vi.stubGlobal('fetch', fetch);

    await press('Save adapter');
    expect(container.textContent).not.toContain('Make Cloudflare the default?');
    expect(fetch).not.toHaveBeenCalled();
  });
});
