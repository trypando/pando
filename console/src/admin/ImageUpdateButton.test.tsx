import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { App } from '@api/types.gen';
import { ImageUpdateButton, imageUpdateLabel } from './ImageUpdateButton';

const check = {
  image: 'ghcr.io/acme/web:latest',
  current_digest: 'sha256:a1',
  latest_digest: 'sha256:b2',
  update_available: true,
  revision: 3,
};

// R-143a: the button checks first, and only offers the upgrade a check found.
describe('the image update button', () => {
  it('says what pressing it does next', () => {
    expect(imageUpdateLabel(undefined, false, false)).toBe('Check for update');
    expect(imageUpdateLabel(undefined, true, false)).toBe('Checking');
    expect(imageUpdateLabel({ ...check, update_available: false }, false, false)).toBe('Up to date');
    expect(imageUpdateLabel(check, false, false)).toBe('Upgrade');
    expect(imageUpdateLabel(check, false, true)).toBe('Upgrading');
  });

  it('starts by offering a check', () => {
    const app = { id: 'app_x', state: 'running' } as unknown as App;
    const html = renderToString(
      <QueryClientProvider client={new QueryClient()}>
        <ImageUpdateButton app={app} onRefused={() => undefined} />
      </QueryClientProvider>,
    );
    expect(html).toContain('Check for update');
  });
});
