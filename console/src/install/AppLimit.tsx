// How many apps a person may own (R-244, issue #131): the limit in force on an
// account and where it comes from, and the value an administrator sets on an
// account or a group. Empty is "not set here"; zero is unlimited. Somebody who
// may not create apps at all is somebody without the permission, not somebody
// with a limit of zero, which is why zero is not offered as "none".

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Input } from '@design';

import { api } from '@api/client';
import { LineSkeleton } from '../ui/Loading';
import { refusal } from './Accounts';

/** GET /users/{id}/app-limit. */
export interface Limit {
  limit: number;
  source: 'user' | 'group' | 'policy';
  group_id?: string;
  group_name?: string;
  owned: number;
  max_apps: number | null;
}

/**
 * What the field says: null when empty (not set here), a whole number, or
 * not ok. Never coerced: 0 is unlimited, so a mistake read as 0 would remove
 * the limit it was meant to set.
 */
export function parseMaxApps(value: string): { ok: true; value: number | null } | { ok: false } {
  const v = value.trim();
  if (v === '') return { ok: true, value: null };
  if (!/^\d+$/.test(v)) return { ok: false };
  return { ok: true, value: Number(v) };
}

/** What an account's limit says, as the account page shows it. */
export function describeLimit(l: Limit): string {
  return l.limit === 0 ? `Unlimited, ${from(l)}` : `${l.owned} of ${l.limit}, ${from(l)}`;
}

/** Where a limit comes from, as a phrase after the number. */
function from(l: Limit): string {
  switch (l.source) {
    case 'user':
      return 'set on this account';
    case 'group':
      return `through ${l.group_name ?? 'a group'}`;
    default:
      return "the installation's default";
  }
}

/** The limit in force on an account, with a change for someone who manages accounts. */
export function AccountAppLimit({ userID, manage }: Readonly<{ userID: string; manage: boolean }>) {
  const limit = useQuery({
    queryKey: ['users', userID, 'app-limit'],
    queryFn: () => api.get<Limit>(`/users/${userID}/app-limit`),
  });
  const [open, setOpen] = useState(false);

  if (limit.isPending) return <LineSkeleton width="20ch" />;
  if (limit.isError || !limit.data) return <>—</>;
  const l = limit.data;

  return (
    <span style={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'baseline', gap: 'var(--space-3)' }}>
      <span>
        {describeLimit(l)}
      </span>
      {manage && (
        <Button variant="ghost" onClick={() => setOpen(true)}>
          Change
        </Button>
      )}
      {open && (
        <MaxAppsDialog
          title="Apps this account may own"
          description="A number here applies whatever this person's groups say. Leave it empty to use the most generous of their groups, or the installation's default."
          current={l.max_apps}
          path={`/users/${userID}/app-limit`}
          invalidate={[['users', userID, 'app-limit']]}
          onClose={() => setOpen(false)}
        />
      )}
    </span>
  );
}

/** A group's own limit, changed from the groups table. */
export function GroupAppLimit({ groupID, name, onClose }: Readonly<{ groupID: string; name: string; onClose: () => void }>) {
  const current = useQuery({
    queryKey: ['groups', groupID, 'app-limit'],
    queryFn: () => api.get<{ max_apps: number | null }>(`/groups/${groupID}/app-limit`),
  });
  const title = `Apps each person in ${name} may own`;
  if (current.isPending) return null;
  // Not an empty editor: Save from one would clear the limit the group has,
  // which nobody could see because it was never read.
  if (current.isError || !current.data) {
    return (
      <Dialog
        open
        title={title}
        onClose={onClose}
        footer={
          <>
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button variant="primary" disabled={current.isFetching} onClick={() => void current.refetch()}>
              {current.isFetching ? 'Trying again' : 'Try again'}
            </Button>
          </>
        }
      >
        <Banner tone="failed">{refusal(current.error)}</Banner>
      </Dialog>
    );
  }
  return (
    <MaxAppsDialog
      title={title}
      description="Somebody in several groups gets the most generous of them. A number set on their own account replaces every group's."
      current={current.data.max_apps}
      path={`/groups/${groupID}/app-limit`}
      // Every member's limit may have changed.
      invalidate={[['groups', groupID, 'app-limit'], ['users']]}
      onClose={onClose}
    />
  );
}

function MaxAppsDialog({
  title,
  description,
  current,
  path,
  invalidate,
  onClose,
}: Readonly<{
  title: string;
  description: string;
  current: number | null;
  path: string;
  invalidate: string[][];
  onClose: () => void;
}>) {
  const queries = useQueryClient();
  const [value, setValue] = useState(current === null ? '' : String(current));
  const parsed = parseMaxApps(value);

  const save = useMutation({
    mutationFn: () => api.put<unknown>(path, { max_apps: parsed.ok ? parsed.value : null }),
    onSuccess: () => {
      for (const key of invalidate) void queries.invalidateQueries({ queryKey: key });
      onClose();
    },
  });

  return (
    <Dialog
      open
      title={title}
      description={description}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={save.isPending || !parsed.ok} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving' : 'Save'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {/* Text, not type="number": a number field reports "1e" as empty,
            which would read as clearing the limit rather than as a mistake. */}
        <Input
          label="Apps"
          inputMode="numeric"
          value={value}
          placeholder="Not set"
          helper="Zero means unlimited. Leave it empty to remove it. Lowering it deletes nothing; it stops new apps past the limit."
          error={parsed.ok ? undefined : 'Use a whole number of apps, 0 for unlimited, or leave it empty.'}
          onChange={(e) => {
            if (save.isError) save.reset();
            setValue(e.target.value);
          }}
        />
        {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}
