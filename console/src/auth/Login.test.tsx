import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads, and the sign-in
// frame asks how wide the page is. Rendered on the server here, so there is no
// page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= {
    location: { pathname: '/' },
    matchMedia: () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }),
  };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { Login } from './Login';

function render(needed: boolean) {
  const client = new QueryClient();
  client.setQueryData(['setup'], { needed });
  return renderToString(
    <QueryClientProvider client={client}>
      <Login />
    </QueryClientProvider>,
  );
}

// R-046, issue #130: a new installation is set up with the token Pando
// printed to its log, so reaching this page first is not enough.
describe('setting up a new installation', () => {
  it('asks for the setup token first, and says where it is', () => {
    const page = render(true);
    expect(page).toContain('Set up Pando');
    expect(page).toContain('Setup token');
    expect(page).toContain('setup_token');
    expect(page).toContain('docker compose logs pando');
    expect(page.indexOf('Setup token')).toBeLessThan(page.indexOf('Username'));
  });

  it('is not shown once the installation is set up', () => {
    expect(render(false)).not.toContain('Setup token');
  });
});
