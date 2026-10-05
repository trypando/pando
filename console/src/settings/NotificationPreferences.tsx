// Which of Pando's own notifications reach you, and on which channel (R-373).
//
// Pando's own notifications are the ones it sends to particular people without
// anybody subscribing: a deploy waiting for your approval, an app of yours that
// failed. Each one can be turned off per channel that reaches people — the
// console, and email when an administrator has set it up. Channels that post to
// a room (Slack, Teams, Discord, ntfy) are not here: they hear only from event
// subscriptions, because a message meant for you must not land in a room.
//
// "An app was shared with you" is off by default (R-266): the launcher tile is
// the notification, and this is for someone who wants an email as well.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Checkbox } from '@design';

import { api } from '@api/client';
import { messageOf } from '../install/Accounts';
import { LineSkeleton } from '../ui/Loading';

interface View {
  kinds: { kind: string; label: string; description: string; default_on: boolean }[];
  channels: { id: string; kind: string }[];
  choices: { kind: string; channel: string; enabled: boolean }[];
}

const CHANNEL_NAMES: Record<string, string> = { console: 'Console', smtp: 'Email' };

export function NotificationPreferences() {
  const queries = useQueryClient();
  const view = useQuery({
    queryKey: ['notification-preferences'],
    queryFn: () => api.get<View>('/notification-preferences'),
  });
  const set = useMutation({
    mutationFn: (choice: { kind: string; channel: string; enabled: boolean }) =>
      api.put<View>('/notification-preferences', { choices: [choice] }),
    onSuccess: (next) => queries.setQueryData(['notification-preferences'], next),
  });

  const muted = { font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-3)' };

  if (view.isPending) return <LineSkeleton width="28ch" />;
  if (view.isError) return <p style={muted}>{messageOf(view.error)}</p>;

  const { kinds, channels, choices } = view.data;
  const on = (kind: string, channel: string) =>
    choices.find((c) => c.kind === kind && c.channel === channel)?.enabled ?? false;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <p style={muted}>
        What Pando tells you without being asked. To hear about anything else, such as every deploy of an
        app, make a subscription under Events.
      </p>
      {set.isError && <p style={{ ...muted, color: 'var(--status-failed)' }}>{messageOf(set.error)}</p>}
      {kinds.map((k) => (
        <fieldset
          key={k.kind}
          style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}
        >
          <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-1)' }}>
            {k.label}
          </legend>
          <p style={{ ...muted, margin: 0 }}>{k.description}</p>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-4)' }}>
            {channels.map((ch) => (
              <Checkbox
                key={ch.id}
                label={CHANNEL_NAMES[ch.kind] ?? ch.id}
                checked={on(k.kind, ch.id)}
                disabled={set.isPending}
                onChange={(e) => set.mutate({ kind: k.kind, channel: ch.id, enabled: e.target.checked })}
              />
            ))}
          </div>
        </fieldset>
      ))}
    </div>
  );
}
