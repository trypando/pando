// The inbox (R-377): what Pando told you on the console.
//
// A bell beside Settings, with the number unread, on the launcher and in the
// admin console alike — a notification is about the person, like Settings,
// and reaches people who only open apps as much as people who run them.
// Opening it lists the newest first; opening one marks it read and goes where
// it points. Which notifications arrive here is chosen under Settings.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Badge, Button, Dialog, EmptyState, Icon, IconButton, Tag } from '@design';

import { api } from '@api/client';
import { messageOf } from '../install/Accounts';
import { LineSkeleton } from './Loading';
import { relative } from './time';

interface Notification {
  id: string;
  kind: string;
  app_name?: string;
  subject: string;
  body?: string;
  link?: string;
  read_at?: string;
  created_at: string;
}

interface Page {
  notifications: Notification[];
  unread: number;
}

const KEY = ['inbox'];

export function InboxButton() {
  const [open, setOpen] = useState(false);
  const inbox = useQuery({
    queryKey: KEY,
    queryFn: () => api.get<Page>('/me/notifications'),
    // Asked again every minute, so the count moves without a reload.
    refetchInterval: 60_000,
    // An account the inbox does not apply to (a token) gets a refusal; the
    // bell then shows no count rather than an error.
    retry: false,
  });
  const unread = inbox.data?.unread ?? 0;

  return (
    <>
      <span style={{ position: 'relative', display: 'inline-flex' }}>
        <IconButton label={unread > 0 ? `Notifications, ${unread} unread` : 'Notifications'} onClick={() => setOpen(true)}>
          <Icon name="bell" size={16} />
        </IconButton>
        {unread > 0 && (
          <span style={{ position: 'absolute', top: 'calc(var(--space-1) * -1)', right: 'calc(var(--space-1) * -1)', pointerEvents: 'none' }}>
            <Badge count={unread} />
          </span>
        )}
      </span>
      {open && <InboxDialog page={inbox.data} loading={inbox.isPending} error={inbox.error} onClose={() => setOpen(false)} />}
    </>
  );
}

function InboxDialog({
  page,
  loading,
  error,
  onClose,
}: {
  page?: Page;
  loading: boolean;
  error: unknown;
  onClose: () => void;
}) {
  const queries = useQueryClient();
  const refresh = () => void queries.invalidateQueries({ queryKey: KEY });

  const readOne = useMutation({
    mutationFn: (id: string) => api.post<unknown>(`/me/notifications/${id}/read`, {}),
    onSuccess: refresh,
  });
  const readAll = useMutation({
    mutationFn: () => api.post<unknown>('/me/notifications/read', {}),
    onSuccess: refresh,
  });

  const open = (n: Notification) => {
    if (!n.read_at) readOne.mutate(n.id);
    if (!n.link) return;
    // A link to this console is followed in place; anything else is not ours
    // to replace the page with.
    try {
      const target = new URL(n.link, window.location.origin);
      if (target.origin === window.location.origin) {
        window.location.assign(target.pathname + target.search);
        return;
      }
    } catch {
      return;
    }
    window.open(n.link, '_blank', 'noopener');
  };

  const list = page?.notifications ?? [];

  return (
    <Dialog
      open
      width={640}
      title="Notifications"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
          <Button
            variant="secondary"
            disabled={(page?.unread ?? 0) === 0 || readAll.isPending}
            onClick={() => readAll.mutate()}
          >
            Mark all read
          </Button>
        </>
      }
    >
      {loading && <LineSkeleton width="28ch" />}
      {!loading && error !== null && error !== undefined && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{messageOf(error)}</p>
      )}
      {!loading && !error && list.length === 0 && (
        <EmptyState heading="No notifications">
          Pando tells you here when an app of yours fails, a deploy needs your approval, and the like. Choose what
          arrives under Settings.
        </EmptyState>
      )}
      <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column' }}>
        {list.map((n) => (
          <li key={n.id} style={{ borderTop: 'var(--border-width) solid var(--rule)' }}>
            <button
              onClick={() => open(n)}
              style={{
                width: '100%',
                textAlign: 'left',
                border: 'none',
                background: 'transparent',
                padding: 'var(--space-3) 0',
                cursor: 'pointer',
                display: 'flex',
                flexDirection: 'column',
                gap: 'var(--space-1)',
              }}
            >
              <span style={{ display: 'flex', alignItems: 'baseline', gap: 'var(--space-2)' }}>
                {!n.read_at && <Tag tone="contour">New</Tag>}
                <span
                  style={{
                    font: n.read_at ? 'var(--type-body-ui)' : 'var(--type-label)',
                    color: 'var(--ink)',
                    flex: 1,
                  }}
                >
                  {n.subject}
                </span>
                <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', whiteSpace: 'nowrap' }}>
                  {relative(n.created_at)}
                </span>
              </span>
              {n.body && (
                <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{n.body}</span>
              )}
            </button>
          </li>
        ))}
      </ul>
    </Dialog>
  );
}
