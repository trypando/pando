import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountAppLimit, describeLimit, parseMaxApps } from './AppLimit';
import type { Limit } from './AppLimit';

const finance: Limit = { limit: 5, source: 'group', group_name: 'Finance', owned: 3, max_apps: null };

// R-244: 0 is unlimited, so nothing that is not a whole number may become 0.
describe('parseMaxApps', () => {
  it('reads empty as not set and a whole number as itself', () => {
    expect(parseMaxApps('')).toEqual({ ok: true, value: null });
    expect(parseMaxApps('  ')).toEqual({ ok: true, value: null });
    expect(parseMaxApps('0')).toEqual({ ok: true, value: 0 });
    expect(parseMaxApps(' 12 ')).toEqual({ ok: true, value: 12 });
  });

  it('refuses anything else rather than reading it as unlimited', () => {
    for (const bad of ['-1', '1e', '2.5', 'five', 'NaN']) expect(parseMaxApps(bad)).toEqual({ ok: false });
  });
});

describe('describeLimit', () => {
  it('says how many of how many, and where the limit comes from', () => {
    expect(describeLimit(finance)).toBe('3 of 5, through Finance');
    expect(describeLimit({ ...finance, source: 'user', max_apps: 5 })).toBe('3 of 5, set on this account');
    expect(describeLimit({ limit: 0, source: 'policy', owned: 9, max_apps: null })).toBe(
      "Unlimited, the installation's default",
    );
  });
});

function render(limit: Limit | undefined, manage: boolean) {
  const client = new QueryClient();
  if (limit) client.setQueryData(['users', 'usr_x', 'app-limit'], limit);
  return renderToString(
    <QueryClientProvider client={client}>
      <AccountAppLimit userID="usr_x" manage={manage} />
    </QueryClientProvider>,
  );
}

describe('the account page row', () => {
  it('shows the limit, with Change only for somebody who manages accounts', () => {
    expect(render(finance, true)).toContain('3 of 5, through Finance');
    expect(render(finance, true)).toContain('Change');
    expect(render(finance, false)).not.toContain('Change');
  });

  it('holds its place until the limit arrives', () => {
    expect(render(undefined, true)).not.toContain('through');
  });
});
