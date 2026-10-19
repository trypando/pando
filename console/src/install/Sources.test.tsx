// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AppLogoDialog, AuthorizeDialog, needsAuthorizing, postForm, type SourceConnection } from './Sources';
import { optionLabel } from './AdapterDialog';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const app: SourceConnection = {
  id: 'src_gh',
  name: 'GitHub',
  kind: 'github',
  capabilities: { method: 'app', host: 'github.com', web_authorization: true },
};

const reply = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

let root: Root | undefined;
let container: HTMLDivElement | undefined;

afterEach(() => {
  if (root) act(() => root!.unmount());
  container?.remove();
  document.body.querySelectorAll('form').forEach((f) => f.remove());
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function render(source: SourceConnection) {
  container = document.createElement('div');
  document.body.append(container);
  root = createRoot(container);
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  act(() =>
    root!.render(
      <QueryClientProvider client={queries}>
        <AuthorizeDialog source={source} onClose={() => undefined} onAuthorized={() => undefined} />
      </QueryClientProvider>,
    ),
  );
}

async function press(text: string) {
  const found = [...document.body.querySelectorAll('button')].find((b) => b.textContent === text);
  expect(found, `a button "${text}"`).toBeDefined();
  await act(async () => {
    found!.click();
    await new Promise((r) => setTimeout(r, 0));
  });
}

describe('a GitHub App connection, R-091', () => {
  it('posts the manifest Pando returns to GitHub, as only a browser can', async () => {
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => undefined);
    const fetch = vi.fn(async () =>
      reply(200, {
        mode: 'web',
        authorize_url: 'https://pando.example/admin/system/adapters?authorize=src_gh',
        form: { action: 'https://github.com/settings/apps/new?state=s', fields: { manifest: '{"name":"Pando"}' } },
      }),
    );
    vi.stubGlobal('fetch', fetch);

    render(app);
    expect(document.body.textContent).toContain('Create the App on GitHub, then pick the repositories it reads.');
    expect(document.body.textContent).not.toContain('Show a code');
    await press('Continue to GitHub');

    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({ mode: 'web' });
    expect(submit).toHaveBeenCalledTimes(1);
    const form = document.body.querySelector('form')!;
    expect(form.method).toBe('post');
    expect(form.action).toBe('https://github.com/settings/apps/new?state=s');
    expect((form.querySelector('input[name="manifest"]') as HTMLInputElement).value).toBe('{"name":"Pando"}');
  });

  it('is opened straight away until it is created and installed', () => {
    expect(needsAuthorizing(app)).toBe(true);
    expect(needsAuthorizing({ ...app, capabilities: { ...app.capabilities, authorized: true } })).toBe(false);
    expect(needsAuthorizing({ ...app, problem: 'refused' })).toBe(false);
    expect(needsAuthorizing({ ...app, capabilities: { method: 'token' } })).toBe(false);
  });
});

describe('the App logo step, R-091', () => {
  it('offers Pando\'s logo and opens the App\'s settings page, where GitHub takes it', async () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    const installed = {
      ...app,
      capabilities: { ...app.capabilities, authorized: true, manage_url: 'https://github.com/organizations/acme/settings/apps/pando-acme' },
    };
    container = document.createElement('div');
    document.body.append(container);
    root = createRoot(container);
    act(() => root!.render(<AppLogoDialog source={installed} onClose={() => undefined} />));

    const download = [...document.body.querySelectorAll('a')].find((a) => a.textContent === 'Download the logo');
    expect(download?.getAttribute('download')).toBe('pando-logo.png');
    expect(download?.getAttribute('href')).toMatch(/pando-icon-ink\.png$/);
    expect(document.body.textContent).toContain('Upload a logo');
    await press('Open App settings');
    expect(open).toHaveBeenCalledWith(installed.capabilities.manage_url, '_blank', 'noopener,noreferrer');
  });
});

describe('postForm', () => {
  it('posts every field hidden', () => {
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => undefined);
    postForm({ action: 'https://example.com/x', fields: { a: '1', b: '2' } });
    const form = document.body.querySelector('form')!;
    expect(form.hidden).toBe(true);
    expect([...form.querySelectorAll('input')].map((i) => [i.type, i.name, i.value])).toEqual([
      ['hidden', 'a', '1'],
      ['hidden', 'b', '2'],
    ]);
    expect(submit).toHaveBeenCalledTimes(1);
  });
});

describe('optionLabel', () => {
  it('marks the recommended choice', () => {
    expect(optionLabel({ label: 'GitHub App', recommended: true })).toBe('GitHub App (recommended)');
    expect(optionLabel({ label: 'Deploy key' })).toBe('Deploy key');
  });
});
