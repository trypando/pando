// The inbox (R-377): what Pando told you on the console.
//
// A bell beside Settings, with the number unread, on the launcher and in the
// admin console alike — a notification is about the person, like Settings,
// and reaches people who only open apps as much as people who run them.
// Opening it lists what is unread, newest first; opening one marks it read,
// which takes it off that list, and goes where it points. Read ones are kept
// under All. Which notifications arrive here is chosen under Settings.

import { useState } from 'react';
import { createPortal } from 'react-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Badge, Button, Dialog, EmptyState, Icon, IconButton, Tabs, Tag } from '@design';

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

/** Which notifications the dialog lists: those still unread, or all of them. */
type InboxView = 'unread' | 'all';

const KEY = ['inbox'];
const COUNT_KEY = ['inbox', 'unread'];
const LIST_KEY = ['inbox', 'list'];

const listKey = (view: InboxView) => [...LIST_KEY, view];
const listPath = (view: InboxView) => (view === 'unread' ? '/me/notifications?unread=true' : '/me/notifications');

/** How often the bell asks for its count. */
const INBOX_COUNT_MS = 60_000;

export function InboxButton() {
  const [open, setOpen] = useState(false);
  // Unread first: a notification marked read leaves the list it was read
  // from, and the read ones are a tab away rather than in the way.
  const [view, setView] = useState<InboxView>('unread');
  // The count alone, asked every minute so it moves without a reload. The bell
  // is on every screen of every signed-in console, so it asks for a number
  // rather than a page of the inbox (issue #72); the list is read when the
  // bell is opened.
  const count = useQuery({
    queryKey: COUNT_KEY,
    queryFn: () => api.get<{ unread: number }>('/me/notifications/unread'),
    refetchInterval: INBOX_COUNT_MS,
    // A number, and the first thing somebody coming back to the tab looks at.
    refetchOnWindowFocus: true,
    // An account the inbox does not apply to (a token) gets a refusal; the
    // bell then shows no count rather than an error.
    retry: false,
  });
  const inbox = useQuery({
    queryKey: listKey(view),
    queryFn: () => api.get<Page>(listPath(view)),
    enabled: open,
    // Read fresh each time the bell opens, rather than whatever an earlier
    // opening left in the cache.
    staleTime: 0,
    retry: false,
  });
  // The list's count is the newer of the two while the bell is open.
  const unread = (open ? inbox.data?.unread : undefined) ?? count.data?.unread ?? 0;

  return (
    <>
      <span style={{ position: 'relative', display: 'inline-flex' }}>
        <IconButton
          label={unread > 0 ? `Notifications, ${unread} unread` : 'Notifications'}
          onClick={() => {
            setView('unread');
            setOpen(true);
          }}
        >
          <Icon name="bell" size={16} />
        </IconButton>
        {unread > 0 && (
          <span style={{ position: 'absolute', top: 'calc(var(--space-1) * -1)', right: 'calc(var(--space-1) * -1)', pointerEvents: 'none' }}>
            <Badge count={unread} />
          </span>
        )}
      </span>
      {/* Portaled to the body. The bell sits in a header that is its own
          stacking context (isolation, for the contour map), and a dialog
          rendered there is layered only within the header: the app tiles
          below it painted over the dialog. */}
      {open &&
        createPortal(
          <InboxDialog
            view={view}
            onView={setView}
            unread={unread}
            page={inbox.data}
            loading={inbox.isPending}
            error={inbox.error}
            onClose={() => setOpen(false)}
          />,
          document.body,
        )}
    </>
  );
}

function InboxDialog({
  view,
  onView,
  unread,
  page,
  loading,
  error,
  onClose,
}: {
  view: InboxView;
  onView: (view: InboxView) => void;
  unread: number;
  page?: Page;
  loading: boolean;
  error: unknown;
  onClose: () => void;
}) {
  const queries = useQueryClient();
  const refresh = () => void queries.invalidateQueries({ queryKey: KEY });

  // Marked read in the cache at once, so a notification leaves the unread
  // list as it is clicked rather than when the next read comes back.
  const markRead = (id?: string) => {
    const now = new Date().toISOString();
    const isMarked = (n: Notification) => id === undefined || n.id === id;
    const remaining = (left: number) => (id === undefined ? 0 : Math.max(0, left - 1));
    queries.setQueryData<Page>(listKey('unread'), (old) =>
      old && { notifications: old.notifications.filter((n) => !isMarked(n)), unread: remaining(old.unread) },
    );
    queries.setQueryData<Page>(listKey('all'), (old) =>
      old && {
        notifications: old.notifications.map((n) => (isMarked(n) && !n.read_at ? { ...n, read_at: now } : n)),
        unread: remaining(old.unread),
      },
    );
    queries.setQueryData<{ unread: number }>(COUNT_KEY, (old) => old && { unread: remaining(old.unread) });
  };

  const readOne = useMutation({
    mutationFn: (id: string) => api.post<unknown>(`/me/notifications/${id}/read`, {}),
    onMutate: (id) => markRead(id),
    onSettled: refresh,
  });
  const readAll = useMutation({
    mutationFn: () => api.post<unknown>('/me/notifications/read', {}),
    onMutate: () => markRead(),
    onSettled: refresh,
  });

  const open = async (n: Notification) => {
    const read = n.read_at ? Promise.resolve() : readOne.mutateAsync(n.id).then(
      () => undefined,
      // Still go where it points; the refresh shows it unread again.
      () => undefined,
    );
    if (!n.link) return;
    let target: URL;
    try {
      target = new URL(n.link, window.location.origin);
    } catch {
      return;
    }
    // Anything but this console is not ours to replace the page with. It opens
    // in a new tab now, during the click, while the browser still allows one;
    // this page stays, and so does its request.
    if (target.origin !== window.location.origin) {
      window.open(n.link, '_blank', 'noopener');
      return;
    }
    // A link to this console is followed in place, once the notification is
    // marked read: a navigation started first cancels the request, and the
    // notification comes back unread.
    await read;
    window.location.assign(target.pathname + target.search);
  };

  const list = page?.notifications ?? [];
  const empty = !loading && !error && list.length === 0;

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
          <Button variant="secondary" disabled={unread === 0 || readAll.isPending} onClick={() => readAll.mutate()}>
            Mark all read
          </Button>
        </>
      }
    >
      <div style={{ marginBottom: 'var(--space-3)' }}>
        <Tabs
          value={view}
          onChange={(v) => onView(v as InboxView)}
          items={[
            { value: 'unread', label: 'Unread', trailing: unread > 0 ? <Badge count={unread} /> : undefined },
            { value: 'all', label: 'All' },
          ]}
        />
      </div>
      {loading && <LineSkeleton width="28ch" />}
      {!loading && error !== null && error !== undefined && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{messageOf(error)}</p>
      )}
      {empty && view === 'unread' && (
        <EmptyState heading="Nothing unread">Notifications you have read are under All.</EmptyState>
      )}
      {empty && view === 'all' && (
        <EmptyState heading="No notifications">Choose what arrives under Settings.</EmptyState>
      )}
      <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column' }}>
        {list.map((n) => (
          <li key={n.id} style={{ borderTop: 'var(--border-width) solid var(--rule)' }}>
            <button
              onClick={() => void open(n)}
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
