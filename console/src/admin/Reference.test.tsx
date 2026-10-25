import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads, and the screen names
// this installation's. Rendered on the server here, so there is no page: give
// it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= {
    location: { pathname: '/', origin: 'https://pando.internal.test' },
  };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { Reference, tokenLine } from './Reference';

function render() {
  return renderToString(
    <QueryClientProvider client={new QueryClient()}>
      <Reference />
    </QueryClientProvider>,
  );
}

// R-261: the reference is published from the code; the console links to it
// rather than drawing a second copy, and keeps what only it can do — how to
// connect to this installation, and tokens.
describe('API and tools', () => {
  it('links to each published reference', () => {
    const html = render();
    for (const page of ['api', 'cli', 'mcp', 'errors', 'events']) {
      expect(html).toContain(`href="https://trypando.ai/docs/${page}"`);
    }
    expect(html).toContain('API reference');
  });

  // R-413: the CLI signs in to this installation, and an MCP client is added
  // by the address Pando serves MCP at.
  it('says how to connect to this installation', () => {
    const html = render();
    expect(html).toContain('>Connect<');
    expect(html).toContain('pando login https://pando.internal.test');
    expect(html).toContain('https://pando.internal.test/api/v1/mcp');
    expect(html).toContain('https://pando.internal.test/api/v1');
    expect(html).not.toContain('This installation is at');
  });

  it('offers tokens', () => {
    expect(render()).toContain('Your tokens');
  });
});

describe('tokenLine', () => {
  it('says when a token expires and when it was last used', () => {
    expect(tokenLine({ expires_at: undefined, last_used_at: undefined })).toBe('Never expires · never used');
    const line = tokenLine({ expires_at: '2099-01-22T12:00:00Z', last_used_at: '2020-10-24T12:02:00Z' });
    expect(line).toMatch(/^Expires Jan 2[123], 2099 · used Oct 2[345], 2020 \d\d:\d\d$/);
  });
});
