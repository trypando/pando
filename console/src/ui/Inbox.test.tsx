import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { InboxButton } from './Inbox';

describe('InboxButton', () => {
  // R-377: the count unread beside Settings on every screen. Every open
  // console asks for it, so it is the count alone (issue #72).
  it('shows the unread count and reads no page of the inbox while closed', () => {
    const client = new QueryClient();
    client.setQueryData(['inbox', 'unread'], { unread: 3 });
    const html = renderToString(
      <QueryClientProvider client={client}>
        <InboxButton />
      </QueryClientProvider>,
    );
    expect(html).toContain('Notifications, 3 unread');
    const list = client.getQueryCache().find({ queryKey: ['inbox', 'list'] });
    expect(list?.state.data).toBeUndefined();
    expect(list?.state.fetchStatus ?? 'idle').toBe('idle');
  });
});
