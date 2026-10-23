// An app's own events (R-378): what happened to it lately, and the
// subscriptions that send its events somewhere.
//
// Anyone who can see the app reads the feed and may subscribe to it (R-368);
// the feed is the same events a webhook would receive, described as a person
// reads them. It holds what the event outbox keeps — 30 days (R-366) — and all
// of it is read, a page at a time as the box scrolls, because the audit log,
// not this, is the history: there is nothing older here to show.

import { useEffect, useRef, useState, type CSSProperties } from 'react';
import { Button, Dialog, EmptyState, StatusSymbol } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { format } from '../app/route';
import { InstallVerb, useInstallVerb, usePrincipal } from '../app/principal';
import { messageOf } from '../install/Accounts';
import { NO_FILTERS, linkQuery } from '../install/audit';
import { Subscriptions } from '../install/Events';
import { usePolledHead, type HeadShape } from '../ui/headPoll';
import { LineSkeleton } from '../ui/Loading';
import { RouteLink, followInPlace } from '../ui/RouteLink';
import { useOpenPanel } from './AppPanel';
import {
  actorName,
  byDay,
  deploymentOf,
  fieldOf,
  nearEnd,
  outcomeOf,
  timeOf,
  type FeedItem,
  type FeedPage,
} from './eventFeed';
import './events.css';

const FEED_SHAPE: HeadShape<FeedPage, FeedItem> = {
  rowsOf: (p) => p.events ?? [],
  withRows: (p, events) => ({ ...p, events }),
  idOf: (r) => r.id,
  hasMore: (p) => Boolean(p.next_before),
};

const heading: CSSProperties = { font: 'var(--type-h4)', color: 'var(--ink)', margin: 0 };
const caption: CSSProperties = { font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 };
const truncate: CSSProperties = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const cell: CSSProperties = {
  ...truncate,
  padding: 'var(--space-2) var(--space-3)',
  borderBottom: 'var(--border-width) solid var(--rule)',
  font: 'var(--type-body-ui)',
  color: 'var(--ink)',
  textAlign: 'left',
};
const headCell: CSSProperties = {
  ...cell,
  font: 'var(--type-label)',
  color: 'var(--ink-secondary)',
  background: 'var(--paper-sunken)',
};

export function AppEvents({ app }: { app: App }) {
  const [creating, setCreating] = useState(false);
  const canManageAll = useInstallVerb(InstallVerb.EventsManage);

  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-start', gap: 'var(--space-7)' }}>
      <Feed app={app} />

      <aside
        aria-labelledby="app-subscriptions"
        style={{
          flex: '1 1 20rem',
          minWidth: 0,
          maxWidth: '26rem',
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-3)',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 'var(--space-3)' }}>
          <h4 id="app-subscriptions" style={heading}>
            Subscriptions
          </h4>
          <Button variant="secondary" onClick={() => setCreating(true)}>
            New subscription
          </Button>
        </div>
        <p style={caption}>Send this app&rsquo;s events to a webhook, Slack, Teams, Discord, email or ntfy.</p>
        <Subscriptions
          apps={[app]}
          app={app}
          layout="cards"
          canManageAll={canManageAll}
          creating={creating}
          onCreating={setCreating}
        />
      </aside>
    </div>
  );
}

/** The table of the last 30 days, newest first, in a box that scrolls. */
function Feed({ app }: { app: App }) {
  const canReadAudit = useInstallVerb(InstallVerb.AuditRead);
  const me = usePrincipal().data;
  // Holders of app.logs.read open a deploy in the side panel; for anyone else
  // a deploy is shown like any other event.
  const openPanel = useOpenPanel('deployments');
  const [shown, setShown] = useState<FeedItem | null>(null);
  const box = useRef<HTMLDivElement>(null);

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
  const rows = feed.data?.pages.flatMap((p) => p.events ?? []) ?? [];

  // The next page is read as the box nears its end — and at once while the
  // box is not yet full, since a box with nothing to scroll is never scrolled.
  const more = () => {
    const el = box.current;
    if (!el || !feed.hasNextPage || feed.isFetchingNextPage || feed.isFetchNextPageError) return;
    if (nearEnd(el)) void feed.fetchNextPage();
  };
  useEffect(more, [rows.length, feed.hasNextPage, feed.isFetchingNextPage, feed.isFetchNextPageError]);

  const open = (item: FeedItem) => {
    const deployment = deploymentOf(item);
    if (deployment && openPanel) openPanel({ view: 'deployments', deployment });
    else setShown(item);
  };

  const auditHref = format({ view: 'admin', section: 'audit', query: linkQuery({ ...NO_FILTERS, app: app.id }) });

  return (
    <section
      aria-labelledby="app-events"
      style={{ flex: '999 1 40rem', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}
    >
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: 'var(--space-3)' }}>
        <h4 id="app-events" style={heading}>
          Events
        </h4>
        <span style={caption}>The last 30 days</span>
      </div>
      {feed.isError && <p style={caption}>{messageOf(feed.error)}</p>}

      <div
        ref={box}
        onScroll={more}
        // A box that scrolls on its own is reached by the keyboard too.
        tabIndex={0}
        aria-label="Events from the last 30 days"
        style={{ maxHeight: '34rem', overflowY: 'auto', borderBottom: 'var(--border-width) solid var(--rule)' }}
      >
        <table className="pando-events" style={{ width: '100%', tableLayout: 'fixed', borderCollapse: 'collapse' }}>
          <thead>
            <tr>
              <th style={{ ...headCell, width: '9ch' }}>Time</th>
              <th style={headCell}>Event</th>
              <th style={{ ...headCell, width: '18ch' }}>Actor</th>
            </tr>
          </thead>
          <tbody>
            {feed.isPending &&
              [0, 1, 2].map((n) => (
                <tr key={n}>
                  <td colSpan={3} style={cell}>
                    <LineSkeleton width={`${60 - n * 12}%`} />
                  </td>
                </tr>
              ))}
            {byDay(rows).map((line) =>
              line.kind === 'day' ? (
                <tr key={line.key}>
                  <th
                    colSpan={3}
                    scope="colgroup"
                    style={{ ...cell, font: 'var(--type-label)', color: 'var(--ink-secondary)', paddingTop: 'var(--space-4)' }}
                  >
                    {line.label}
                  </th>
                </tr>
              ) : (
                <EventRow key={line.key} item={line.item} actor={actorName(line.item.actor, me, line.item.actor_name)} onOpen={open} />
              ),
            )}
            {feed.isFetchingNextPage && (
              <tr>
                <td colSpan={3} style={{ ...cell, color: 'var(--ink-secondary)' }}>
                  Loading older events
                </td>
              </tr>
            )}
          </tbody>
        </table>
        {feed.isSuccess && rows.length === 0 && <EmptyState heading="No events in the last 30 days" />}
      </div>

      <p style={caption}>
        Anything older than 30 days is in the{' '}
        {canReadAudit ? (
          <RouteLink
            href={auditHref}
            onFollow={() => followInPlace(auditHref)}
            style={{ color: 'var(--ink-secondary)', textDecoration: 'underline' }}
          >
            audit log
          </RouteLink>
        ) : (
          'audit log'
        )}
        .
      </p>

      {shown && <EventDetail item={shown} actor={actorName(shown.actor, me, shown.actor_name)} onClose={() => setShown(null)} />}
    </section>
  );
}

function EventRow({ item, actor, onOpen }: { item: FeedItem; actor: string; onOpen: (item: FeedItem) => void }) {
  const outcome = outcomeOf(item.type);
  const detail = item.body && item.body !== item.subject ? item.body : '';
  return (
    <tr
      className="pando-event"
      tabIndex={0}
      data-event={item.type}
      onClick={() => onOpen(item)}
      onKeyDown={(e) => {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        e.preventDefault();
        onOpen(item);
      }}
    >
      <td style={{ ...cell, color: 'var(--ink-secondary)', fontVariantNumeric: 'tabular-nums' }}>
        <time dateTime={item.occurred_at}>{timeOf(item.occurred_at)}</time>
      </td>
      <td style={cell}>
        <span style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', minWidth: 0 }}>
          {/* The same width with or without a symbol, so every subject starts
              at one edge. Only a success or a failure has one. */}
          <span style={{ display: 'inline-flex', flex: '0 0 auto', width: 'var(--space-3)' }}>
            {outcome && (
              <StatusSymbol status={outcome === 'success' ? 'running' : 'failed'} />
            )}
          </span>
          <span style={{ ...truncate, minWidth: 0 }}>
            {item.subject}
            {detail && <span style={{ color: 'var(--ink-secondary)' }}> — {detail}</span>}
          </span>
        </span>
      </td>
      <td style={{ ...cell, color: 'var(--ink-secondary)' }}>{actor}</td>
    </tr>
  );
}

/** Everything the feed says about one event. */
function EventDetail({ item, actor, onClose }: { item: FeedItem; actor: string; onClose: () => void }) {
  const term: CSSProperties = { color: 'var(--ink-secondary)' };
  const value: CSSProperties = { margin: 0, overflowWrap: 'anywhere' };
  return (
    <Dialog
      open
      title={item.subject}
      description={item.body && item.body !== item.subject ? item.body : undefined}
      onClose={onClose}
      footer={
        <Button variant="secondary" onClick={onClose}>
          Close
        </Button>
      }
    >
      <dl
        style={{
          display: 'grid',
          gridTemplateColumns: 'max-content 1fr',
          gap: 'var(--space-2) var(--space-4)',
          margin: 0,
          font: 'var(--type-body-ui)',
        }}
      >
        <dt style={term}>Time</dt>
        <dd style={value}>{new Date(item.occurred_at).toLocaleString()}</dd>
        <dt style={term}>Event</dt>
        <dd style={{ ...value, font: 'var(--type-code-sm)' }}>{item.type}</dd>
        <dt style={term}>Actor</dt>
        <dd style={value}>{actor}</dd>
        {(item.fields ?? []).map(fieldOf).map((f) => (
          <Field key={f.label} label={f.label} value={f.value} term={term} style={value} />
        ))}
      </dl>
    </Dialog>
  );
}

function Field({ label, value, term, style }: { label: string; value: string; term: CSSProperties; style: CSSProperties }) {
  return (
    <>
      <dt style={term}>{label}</dt>
      <dd style={style}>{value}</dd>
    </>
  );
}
