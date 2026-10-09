import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { ApprovalRequest } from './ApprovalRequest';
import type { ApprovalDeployment } from './approval';
import { Egress } from './Egress';
import type { EgressResponse } from './appEgress';
import { AppVerbs } from './verbs';

function render(node: React.ReactNode, seed?: (q: QueryClient) => void, verbs: string[] = []) {
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  seed?.(queries);
  return renderToString(
    <QueryClientProvider client={queries}>
      <AppVerbs.Provider value={verbs}>{node}</AppVerbs.Provider>
    </QueryClientProvider>,
  );
}

const waiting: ApprovalDeployment = {
  id: 'dep_01',
  app_id: 'app_01',
  spec_id: 'spec_01',
  trigger: 'manual',
  status: 'awaiting_approval',
  started_at: '2026-10-01T00:00:00Z',
  created_by: 'usr_01',
  approvals_required: 2,
  approval_reasons: [{ reason: 'app_spec', message: 'This app asks for approval of its own deploys.' }],
  approvals: [{ principal_id: 'usr_02', principal_name: 'Ada', decision: 'approve', comment: 'Looks fine', decided_at: '2026-10-01T00:10:00Z' }],
};

describe('a deploy waiting for approval (R-154 – R-156)', () => {
  it('shows why, how far it has got, and who has answered', () => {
    const html = render(<ApprovalRequest deployment={{ ...waiting, can_decide: true }} />);
    expect(html).toContain('Waiting for approval');
    expect(html).toContain('This app asks for approval of its own deploys.');
    expect(html).toContain('1 of 2 approvals.');
    expect(html).toContain('Ada');
    expect(html).toContain('Looks fine');
    expect(html).toContain('Approve');
    expect(html).toContain('Reject');
  });

  // R-155: only somebody the server says may decide is offered the buttons.
  it('offers no answer to somebody who may not give one', () => {
    const html = render(<ApprovalRequest deployment={{ ...waiting, can_decide: false }} />);
    expect(html).not.toContain('>Approve<');
    expect(html).toContain('You can’t answer this request.');
  });
});

describe("an app's egress, merged (R-187, R-188)", () => {
  const data: EgressResponse = {
    install: { mode: 'allowlist', list: ['api.example.com'], block_private: true, loosening: 'approval' },
    spec: { mode: 'inherit', add: ['extra.example.com'] },
    effective: {
      mode: 'allowlist',
      list: [
        { entry: 'api.example.com', from: 'install' },
        { entry: 'extra.example.com', from: 'app' },
      ],
      block_private: true,
      block_private_from: 'install',
      loosenings: [{ kind: 'allowlist_add', entry: 'extra.example.com', message: "Adds extra.example.com to the installation's allowlist, so this app can reach it." }],
      gate: 'approval',
      unused: ['Removing x.example changes nothing: the installation\'s list does not have it.'],
      restricted: true,
    },
  };
  const seed = (q: QueryClient) => q.setQueryData(['apps', 'app_01', 'egress'], data);

  it('shows each entry with where it came from, the loosening with what it needs, and the gateway note', () => {
    const html = render(<Egress appID="app_01" />, seed);
    expect(html).toContain('Only the destinations listed');
    expect(html).toContain('api.example.com');
    expect(html).toContain('Installation');
    expect(html).toContain('This app');
    expect(html).toContain('Needs a deploy approval');
    expect(html).toContain('changes nothing');
    expect(html).toContain('Only HTTP and HTTPS sent through Pando’s egress gateway leave this app.');
  });

  it('says nothing is in the way when nothing is restricted', () => {
    const html = render(<Egress appID="app_01" />, (q) =>
      q.setQueryData(['apps', 'app_01', 'egress'], {
        install: { mode: 'allow_all' },
        spec: { mode: 'inherit' },
        effective: { mode: 'allow_all', list: [], block_private: false, restricted: false },
      }),
    );
    expect(html).toContain('Anywhere');
    expect(html).toContain('Nothing is restricted');
    expect(html).not.toContain('egress gateway');
  });
});
