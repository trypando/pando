import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { ImageRegistry, setAtStartup } from './ImageRegistry';
import type { RegistryView } from './ImageRegistry';

function render(view: Partial<RegistryView>, verbs: string[] = ['install.view', 'install.adapters.manage']) {
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queries.setQueryData(['me'], { verbs });
  queries.setQueryData(['image-registry'], {
    configured: false, url: '', username: '', kind: 'basic', layout: 'per_app',
    insecure: false, always: false, password_set: false, fixed: [], ...view,
  });
  return renderToString(
    <QueryClientProvider client={queries}>
      <ImageRegistry />
    </QueryClientProvider>,
  );
}

describe('the image registry settings (issue #72, R-271, R-194)', () => {
  it('locks a field set at startup and says where it is set', () => {
    const html = render({
      configured: true,
      url: 'https://registry.internal:5000',
      fixed: [{ key: 'url', value: 'https://registry.internal:5000', source: { kind: 'env', name: 'PANDO_REGISTRY_URL' } }],
    });
    const tag = html.match(/<input[^>]*value="https:\/\/registry.internal:5000"[^>]*>/)?.[0] ?? '';
    expect(tag).toContain('disabled');
    expect(html).toContain('Set at startup by PANDO_REGISTRY_URL');
    // A field the startup configuration leaves alone stays editable.
    const user = html.match(/<input[^>]*autoComplete="off"[^>]*>|<input[^>]*autocomplete="off"[^>]*>/)?.[0] ?? '';
    expect(user).not.toContain('disabled');
  });

  it('never shows the password, only that one is set', () => {
    const html = render({ configured: true, url: 'https://r', username: 'pando', password_set: true });
    expect(html).toContain('Set. Type a new one to replace it.');
    expect(html).toContain('Remove the stored password');
    const field = html.match(/<input[^>]*type="password"[^>]*>/)?.[0] ?? '';
    expect(field).toContain('value=""');
  });

  it('is read-only without install.adapters.manage', () => {
    const html = render({ configured: true, url: 'https://r' }, ['install.view']);
    expect(html).not.toContain('Save registry');
    const url = html.match(/<input[^>]*value="https:\/\/r"[^>]*>/)?.[0] ?? '';
    expect(url).toContain('disabled');
  });

  it('says where a file setting is', () => {
    expect(setAtStartup({ kind: 'file', name: '/etc/pando.yaml', key: 'registry.url' })).toContain('/etc/pando.yaml, at registry.url');
  });
});
