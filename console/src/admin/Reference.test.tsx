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

import { Reference } from './Reference';

function render() {
  return renderToString(
    <QueryClientProvider client={new QueryClient()}>
      <Reference />
    </QueryClientProvider>,
  );
}

// R-261: the reference is published from the code; the console links to it
// rather than drawing a second copy, and keeps what only it can do — tokens.
describe('API and tools', () => {
  it('links to each published reference', () => {
    const html = render();
    for (const page of ['api', 'cli', 'mcp', 'errors', 'events']) {
      expect(html).toContain(`href="https://trypando.ai/docs/${page}"`);
    }
    expect(html).toContain('API reference');
    expect(html).toContain('CLI reference');
    expect(html).toContain('MCP server');
  });

  it('says where this installation is, which the docs cannot', () => {
    expect(render()).toContain('https://pando.internal.test');
  });

  it('has no Connect tab and still offers tokens', () => {
    const html = render();
    expect(html).not.toContain('>Connect<');
    expect(html).toContain('Your tokens');
  });
});
