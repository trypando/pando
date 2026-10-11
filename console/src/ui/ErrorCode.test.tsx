import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';

import { RequestFailed } from '@api/client';
import { FailedBanner } from './ErrorCode';
import { AIPrompt } from './AskAI';
import { errorDocsURL } from './docs';

const refused = new RequestFailed(
  422,
  {
    code: 'PLAN_SLOT_UNFILLED',
    message: 'This app needs a postgres database and none is configured.',
    remedy: 'Configure a services adapter that provides postgres.',
    request_id: 'req_x',
  },
  'The request failed (422).',
);

// R-105: the message is the server's, word for word; the code links to what it means.
describe('a failed request', () => {
  it('links its code to the errors reference, in a new tab', () => {
    const html = renderToString(<FailedBanner error={refused} />);
    expect(html).toContain('href="https://trypando.ai/docs/errors#PLAN_SLOT_UNFILLED"');
    expect(html).toContain('target="_blank"');
    expect(html).toContain('This app needs a postgres database and none is configured.');
    expect(html).not.toContain('Configure a services adapter');
  });

  it('adds the remedy when asked to', () => {
    const html = renderToString(<FailedBanner error={refused} remedy />);
    expect(html).toContain('Configure a services adapter that provides postgres.');
  });

  it('shows no code when the server never answered', () => {
    const html = renderToString(<FailedBanner error={new TypeError('fetch failed')} />);
    expect(html).toContain('Pando could not reach the server.');
    expect(html).not.toContain('docs/errors');
  });

  it('escapes a code into its anchor', () => {
    expect(errorDocsURL('A B')).toBe('https://trypando.ai/docs/errors#A%20B');
  });
});

// R-346 and the other AI functions: a refused request says why, and links its code.
describe('a refused AI request', () => {
  it('links its code under the prompt', () => {
    const html = renderToString(
      <AIPrompt label="What you want to do" pending={false} error={refused} onAsk={() => {}} />,
    );
    expect(html).toContain('href="https://trypando.ai/docs/errors#PLAN_SLOT_UNFILLED"');
    expect(html).toContain('Configure a services adapter that provides postgres.');
  });
});
