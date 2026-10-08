// An app's own events (R-378): what happened to it lately, and the
// subscriptions that send its events somewhere.
//
// Anyone who can see the app reads the feed and may subscribe to it (R-368);
// the feed is the same events a webhook would receive, described as a person
// reads them. It holds what the event outbox keeps — about a month — because
// the audit log, not this, is the history.

import { useState } from 'react';
import { usePolledHead, type HeadShape } from '../ui/headPoll';
import { Button, EmptyState } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { messageOf } from '../install/Accounts';
import { Subscriptions } from '../install/Events';
import { Table } from '../ui/Table';
import { relative } from '../ui/time';

interface FeedItem {
  id: string;
  type: string;
  occurred_at: string;
  subject: string;
  body?: string;
  actor: { kind: string; id?: string };
}

interface FeedPage {
  events: FeedItem[];
  next_before: string;
}

const FEED_SHAPE: HeadShape<FeedPage, FeedItem> = {
  rowsOf: (p) => p.events ?? [],
  withRows: (p, events) => ({ ...p, events }),
  idOf: (r) => r.id,
  hasMore: (p) => Boolean(p.next_before),
};

export function AppEvents({ app }: { app: App }) {
  const [creating, setCreating] = useState(false);
  const canManageAll = useInstallVerb(InstallVerb.EventsManage);

  const feed = usePolledHead({
    key: ['app-events', app.id],
    headKey: ['app-events-head', app.id],
    fetchPage: (before) =>
      api.get<FeedPage>(`/apps/${app.id}/events${before ? `?before=${encodeURIComponent(before)}` : ''}`),
    nextParam: (last) => last.next_before || undefined,
    shape: FEED_SHAPE,
    // New events arrive while the tab is open: the newest page is asked for
    // again and what is new goes on top, rather than every page read so far.
    interval: 15_000,
  });
  const rows = feed.data?.pages.flatMap((p) => p.events) ?? [];

  const heading = { font: 'var(--type-h4)', margin: 0 };
  const quiet = { font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)' }}>
      <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <h4 style={heading}>Recent events</h4>
        {feed.isError && <p style={quiet}>{messageOf(feed.error)}</p>}
        <Table
          loading={feed.isPending}
          empty={
            <EmptyState heading="Nothing yet">
              Deploys, state changes, scans, shares and backups of {app.name} appear here as they happen.
            </EmptyState>
          }
          columns={[
            {
              key: 'occurred_at',
              header: 'When',
              width: '16ch',
              muted: true,
              render: (row: FeedItem) => relative(row.occurred_at),
            },
            { key: 'type', header: 'Event', width: 'minmax(0,22ch)', mono: true },
            {
              key: 'subject',
              header: 'What happened',
              width: 'minmax(0,1fr)',
              render: (row: FeedItem) => (
                <span style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
                  <span>{row.subject}</span>
                  {row.body && row.body !== row.subject && <span style={quiet}>{row.body}</span>}
                </span>
              ),
            },
          ]}
          rows={rows}
        />
        {feed.hasNextPage && (
          <div>
            <Button variant="secondary" disabled={feed.isFetchingNextPage} onClick={() => void feed.fetchNextPage()}>
              {feed.isFetchingNextPage ? 'Loading' : 'Show older events'}
            </Button>
          </div>
        )}
      </section>

      <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: 'var(--space-3)' }}>
          <h4 style={heading}>Subscriptions</h4>
          <Button variant="secondary" onClick={() => setCreating(true)}>
            New subscription
          </Button>
        </div>
        <p style={quiet}>
          Send {app.name}&rsquo;s events to a webhook, or to Slack, Teams, Discord, email or ntfy.
        </p>
        <Subscriptions
          apps={[app]}
          app={app}
          canManageAll={canManageAll}
          creating={creating}
          onCreating={setCreating}
        />
      </section>
    </div>
  );
}
