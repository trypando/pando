// Approvals: every deploy waiting for somebody's sign-off (R-154 – R-156).
//
// The approver's inbox. `GET /approvals` answers with the waiting requests on
// apps the caller can view, each saying whether this caller may decide it, so
// the screen is the same for an administrator holding install.deploys.approve
// and for somebody holding app.deploy.approve on two apps: what they may answer
// has buttons, and the rest is shown read-only.

import { usePolledHead, type HeadShape } from '../ui/headPoll';
import { Button, EmptyState } from '@design';

import { api } from '@api/client';
import { Quiet, Screen, messageOf } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { LineSkeleton, Loading } from '../ui/Loading';
import { ShowMore, withParams } from '../ui/paged';
import { ApprovalRequest } from './ApprovalRequest';
import type { ApprovalRow } from './approval';

/**
 * The waiting requests. One query key for the sidebar's count and this screen,
 * so the two cannot disagree. A caller the endpoint refuses has nothing waiting
 * for them, which is an answer rather than an error, so it is not retried.
 */
type ApprovalsPage = { approvals: ApprovalRow[] | null; next_cursor?: string };

const APPROVALS_SHAPE: HeadShape<ApprovalsPage, ApprovalRow> = {
  rowsOf: (p) => p.approvals ?? [],
  withRows: (p, approvals) => ({ ...p, approvals }),
  idOf: (r) => r.id,
  hasMore: (p) => Boolean(p.next_cursor),
};

export function useApprovals(enabled: boolean) {
  const query = usePolledHead({
    key: ['approvals'],
    headKey: ['approvals-head'],
    fetchPage: (cursor) => api.get<ApprovalsPage>(withParams('/approvals', { cursor })),
    // A page at a time (issue #72). A page can be short and still have a
    // next one, when the requests it read were on apps this caller can't see.
    nextParam: (last) => last.next_cursor || undefined,
    shape: APPROVALS_SHAPE,
    enabled,
    retry: false,
    // Somebody else's answer, or a new request, changes this list. Every open
    // console asks, and each ask checks access app by app, so not often: a
    // request waits hours for an answer, not seconds. The first page only —
    // a request answered further down goes when this screen is next opened.
    interval: 60_000,
  });
  return { query, rows: query.data?.pages.flatMap((p) => p.approvals ?? []) ?? [] };
}

export function Approvals({ onOpenApp }: { onOpenApp: (appID: string) => void }) {
  const { query: approvals, rows } = useApprovals(true);

  return (
    <Screen heading="Approvals">
      <div style={{ maxWidth: MEASURE, display: 'flex', flexDirection: 'column' }}>
        <Quiet>
          Deploys waiting for somebody to approve them. A deploy runs once it has enough approvals,
          and any one rejection ends it. Until then the app keeps running what it runs now.
        </Quiet>

        {approvals.isPending && (
          <Loading gap="var(--space-3)">
            <LineSkeleton width="24ch" font="var(--type-h4)" />
            <LineSkeleton width="52ch" />
            <LineSkeleton width="40ch" />
          </Loading>
        )}
        {approvals.isError && <Quiet>{messageOf(approvals.error)}</Quiet>}

        {approvals.isSuccess && rows.length === 0 && !approvals.hasNextPage && (
          <EmptyState heading="Nothing is waiting for approval">
            When a deploy needs approval, it is listed here until somebody answers it.
          </EmptyState>
        )}

        {rows.map((row) => (
          <section
            key={row.id}
            style={{ padding: 'var(--space-5) 0', borderTop: 'var(--border-width) solid var(--rule)' }}
          >
            <ApprovalRequest
              deployment={row}
              heading={
                <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'baseline', gap: 'var(--space-3)' }}>
                  <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>{row.app_name}</h4>
                  <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink-secondary)' }}>
                    {row.spec_revision ? `Revision ${row.spec_revision}` : row.spec_id}
                  </span>
                  <Button variant="ghost" onClick={() => onOpenApp(row.app_id)}>
                    Open app
                  </Button>
                </div>
              }
            />
          </section>
        ))}
        <ShowMore query={approvals} label="Show more requests" />
      </div>
    </Screen>
  );
}
