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
import { AppVerb, AppVerbs } from './verbs';

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
    expect(html).toContain('Answering needs app.deploy.approve');
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
  // The newest revision, which an edit builds on.
  const newest = (q: QueryClient, egress: unknown) => {
    q.setQueryData(['apps', 'app_01', 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' });
    q.setQueryData(['apps', 'app_01', 'spec', 1], { id: 'spec_1', revision: 1, body: { egress } });
  };
  const seed = (q: QueryClient) => {
    q.setQueryData(['apps', 'app_01', 'egress'], data);
    newest(q, data.spec);
  };

  it('lists the host’s addresses locked, the app’s with Remove, a loosening with what it needs, and the gateway note', () => {
    const html = render(<Egress appID="app_01" />, seed, [AppVerb.EgressTighten]);
    expect(html).toContain('This host only allows the addresses below.');
    expect(html).toContain('api.example.com');
    expect(html).toContain('Set by this host');
    expect(html).toContain('data-icon="lock"');
    expect(html).toContain('extra.example.com');
    expect(html).toContain('Added for this app');
    expect(html).toContain('Needs approval to deploy');
    expect(html).toContain('changes nothing');
    expect(html).toContain('Only HTTP and HTTPS through Pando’s egress gateway');
    // One Remove, for the app's address; the host's has none.
    expect(html.match(/>Remove</g)).toHaveLength(1);
    // The host blocks private addresses for every app.
    expect(html).toContain('On for every app on this host');
    expect(html).toMatch(/disabled=""[^>]*aria-label="Block private addresses"/);
  });

  it('offers nothing to change without an egress verb', () => {
    const html = render(<Egress appID="app_01" />, seed);
    expect(html).not.toContain('>Remove<');
    expect(html).not.toContain('>Allow<');
  });

  it('says the app is unrestricted when nothing is set anywhere', () => {
    const html = render(
      <Egress appID="app_01" />,
      (q) => {
        q.setQueryData(['apps', 'app_01', 'egress'], {
          install: { mode: 'allow_all' },
          spec: { mode: 'inherit' },
          effective: { mode: 'allow_all', list: [], block_private: false, restricted: false },
        });
        newest(q, { mode: 'inherit' });
      },
      [AppVerb.EgressTighten],
    );
    expect(html).toContain('Unrestricted');
    expect(html).toContain('This app can connect anywhere.');
    expect(html).toContain('Restrict this app');
    expect(html).not.toContain('egress gateway');
    expect(html).not.toContain('On for every app on this host');
  });
});
