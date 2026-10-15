import { afterEach, describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';

import { api, runningVersion } from '@api/client';
import { bugReportURL, FeedbackButton, IDEA_URL } from './Feedback';

describe('bugReportURL', () => {
  it('opens the bug form with the version filled in', () => {
    const url = new URL(bugReportURL('0.4.0'));
    expect(url.origin + url.pathname).toBe('https://github.com/trypando/pando/issues/new');
    expect(url.searchParams.get('template')).toBe('bug.yml');
    // The form asks for `pando version` output, which starts "pando".
    expect(url.searchParams.get('version')).toBe('pando 0.4.0');
  });

  it('leaves the version for the person when it is not known', () => {
    const url = new URL(bugReportURL(undefined));
    expect(url.searchParams.get('template')).toBe('bug.yml');
    expect(url.searchParams.has('version')).toBe(false);
  });
});

describe('IDEA_URL', () => {
  it('starts a discussion in the Ideas category, not an issue', () => {
    const url = new URL(IDEA_URL);
    expect(url.pathname).toBe('/trypando/pando/discussions/new');
    expect(url.searchParams.get('category')).toBe('ideas');
  });
});

describe('runningVersion', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('is the Pando-Version header of the last signed-in response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('{}', { status: 200, headers: { 'Pando-Version': '0.4.0' } })),
    );
    await api.get('/me');
    expect(runningVersion()).toBe('0.4.0');
  });
});

describe('FeedbackButton', () => {
  it('is a closed menu behind a labeled button', () => {
    const html = renderToString(<FeedbackButton />);
    expect(html).toContain('aria-label="Report a bug or suggest an idea"');
    expect(html).toContain('aria-expanded="false"');
    expect(html).not.toContain('role="menu"');
  });

  // Icon renders nothing for a name the vendored set lacks, which left this
  // button an empty square nobody could find.
  it('draws its icon', () => {
    const html = renderToString(<FeedbackButton />);
    expect(html).toMatch(/data-icon="bug"[^>]*><svg/);
  });
});
