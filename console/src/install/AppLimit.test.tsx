import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountAppLimit, GroupAppLimit, describeLimit, parseMaxApps } from './AppLimit';
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

function renderGroup(maxApps: number | null | undefined) {
  const client = new QueryClient();
  if (maxApps !== undefined) client.setQueryData(['groups', 'grp_x', 'app-limit'], { max_apps: maxApps });
  return renderToString(
    <QueryClientProvider client={client}>
      <GroupAppLimit groupID="grp_x" name="Finance" onClose={() => undefined} />
    </QueryClientProvider>,
  );
}

describe("a group's limit", () => {
  it('opens on the value the group has, and says what a group limit means', () => {
    const html = renderGroup(5);
    expect(html).toContain('Apps each person in Finance may own');
    expect(html).toContain('the most generous');
    expect(html).toContain('value="5"');
    expect(html).toContain('Zero means unlimited');
  });

  it('offers no editor when the value could not be read, so Save cannot clear it', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, retryOnMount: false } } });
    await client
      .fetchQuery({
        queryKey: ['groups', 'grp_x', 'app-limit'],
        queryFn: () => Promise.reject(new Error('Pando could not be reached.')),
      })
      .catch(() => undefined);
    const html = renderToString(
      <QueryClientProvider client={client}>
        <GroupAppLimit groupID="grp_x" name="Finance" onClose={() => undefined} />
      </QueryClientProvider>,
    );
    expect(html).toContain('Try again');
    expect(html).not.toContain('placeholder="Not set"');
    expect(html).not.toContain('>Save<');
  });

  it('opens empty for a group that sets none, and waits for the value', () => {
    expect(renderGroup(null)).toContain('placeholder="Not set"');
    expect(renderGroup(undefined)).toBe('');
  });
});
