// Backups — the screen behind install.backup.manage (Sequence D).
//
// The one thing this screen has to get right is R-214: Pando never stores the
// passphrase, and a lost one makes the bundle unusable. The requirement says
// that must be stated **at creation, not in documentation** — so it is in the
// dialog, above the field, in body text. Not a tooltip, not a help link, and
// not a line in a readme nobody opens until the disaster.
//
// Restore is deliberately awkward. It replaces every app, account and secret in
// the installation, and a screen that made it feel routine would be lying about
// what it does.

import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, EmptyState, Input, StatusIndicator, Tag } from '@design';

import { api } from '@api/client';
import type { BackupAttempt } from '@api/types.gen';
import { Quiet, Screen, messageOf } from './Accounts';
import { Table } from '../ui/Table';
import { ShowMore, usePaged, type PageOf } from '../ui/paged';
import { relative } from '../ui/time';
import { attemptDetail, attemptLabel, attemptSymbol } from '../ui/backupAttempt';

interface BackupRow {
  id: string;
  kind: string;
  adapter_ref: string;
  size_bytes: number;
  created_at: string;
  retain_until?: string;
  manifest?: { counts?: Record<string, number>; created_at?: string };
}

export function Backups() {
  const [taking, setTaking] = useState(false);
  const [acting, setActing] = useState<{ row: BackupRow; mode: 'verify' | 'restore' } | null>(null);

  // Newest first, a page at a time (issue #72). The first page carries the
  // most recent apps' last scheduled attempts.
  const paged = usePaged<{ backups: BackupRow[] | null; attempts?: BackupAttempt[] } & PageOf, BackupRow>({
    key: ['backups'],
    path: '/backups',
    rows: (p) => p.backups,
  });
  const backups = paged.query;

  const rows = paged.rows;
  const attempts = backups.data?.pages[0]?.attempts ?? [];

  return (
    <Screen
      heading="Backups"
      action={
        <Button variant="primary" onClick={() => setTaking(true)}>
          Take a backup
        </Button>
      }
    >
      {backups.isError && <Quiet>{messageOf(backups.error)}</Quiet>}

      <Table
        loading={backups.isPending}
        // An installation that has never been backed up is the state this
        // screen most needs to be clear about, and it was the one it showed as
        // a row of column headers over blank paper.
        empty={
          <EmptyState
            heading="No backups yet"
            action={
              <Button variant="primary" onClick={() => setTaking(true)}>
                Take a backup
              </Button>
            }
          >
            A backup holds every app, account and secret in this installation.
          </EmptyState>
        }
        columns={[
          {
            key: 'created_at',
            header: 'Taken',
            width: '22ch',
            render: (row: BackupRow) => new Date(row.created_at).toLocaleString(),
          },
          {
            key: 'kind',
            header: 'Contents',
            width: '18ch',
            render: (row: BackupRow) => (
              <Tag>{row.kind === 'dr_bundle' ? 'Whole installation' : row.kind}</Tag>
            ),
          },
          { key: 'adapter_ref', header: 'Destination', width: 'minmax(0,18ch)', mono: true },
          {
            key: 'size_bytes',
            header: 'Size',
            width: '12ch',
            align: 'right',
            muted: true,
            render: (row: BackupRow) => humanSize(row.size_bytes),
          },
          {
            key: 'retain_until',
            header: 'Kept until',
            width: '18ch',
            muted: true,
            render: (row: BackupRow) =>
              row.retain_until ? new Date(row.retain_until).toLocaleDateString() : 'Until discarded',
          },
          {
            key: 'actions',
            header: '',
            width: '22ch',
            align: 'right',
            render: (row: BackupRow) => (
              <div style={{ display: 'flex', gap: 'var(--space-2)', justifyContent: 'flex-end' }}>
                {/* Check a backup before it is needed, rather than at the
                    moment of disaster (R-216). */}
                <Button variant="secondary" onClick={() => setActing({ row, mode: 'verify' })}>
                  Check
                </Button>
                <Button variant="secondary" onClick={() => setActing({ row, mode: 'restore' })}>
                  Restore
                </Button>
              </div>
            ),
          },
        ]}
        rows={rows}
      />
      <ShowMore query={backups} label="Show more backups" />

      {/* Each app's last daily backup, including the ones that did not happen
          (R-211, issue #87). The table above lists what exists; a backup that
          was skipped or failed leaves nothing there, and used to leave nothing
          anywhere but the server log. */}
      {attempts.length > 0 && (
        <section
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-3)',
            marginTop: 'var(--space-6)',
          }}
        >
          <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Daily app backups</h4>
          <Quiet>
            Pando copies the data of every running app that keeps any, once a day. This is the last
            attempt for each app.
          </Quiet>
          <Table
            columns={[
              {
                key: 'app_name',
                header: 'App',
                width: 'minmax(0,22ch)',
                render: (row: BackupAttempt) => row.app_name || row.app_id,
              },
              {
                key: 'outcome',
                header: 'Last attempt',
                width: '24ch',
                render: (row: BackupAttempt) => (
                  <StatusIndicator
                    status={attemptSymbol(row)}
                    label={`${attemptLabel(row)} ${relative(row.attempted_at).toLowerCase()}`}
                  />
                ),
              },
              {
                key: 'message',
                header: 'Details',
                width: 'minmax(0,1fr)',
                muted: true,
                render: (row: BackupAttempt) => attemptDetail(row) || '—',
              },
            ]}
            rows={attempts.map((a) => ({ ...a, id: a.app_id }))}
          />
        </section>
      )}

      {taking && <TakeBackup onClose={() => setTaking(false)} />}
      {acting && <UseBackup {...acting} onClose={() => setActing(null)} />}
    </Screen>
  );
}

function TakeBackup({ onClose }: { onClose: () => void }) {
  const [passphrase, setPassphrase] = useState('');
  const [again, setAgain] = useState('');
  const [retainDays, setRetainDays] = useState('30');
  const queries = useQueryClient();

  const mismatch = again !== '' && passphrase !== again;

  const take = useMutation({
    mutationFn: () =>
      api.post<unknown>('/backups', {
        passphrase,
        retain_days: Number(retainDays) || 0,
      }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['backups'] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      title="Take a backup"
      description="A copy of this whole installation: every app's configuration, its data, and the key that reads its secrets."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={take.isPending || passphrase.length < 16 || passphrase !== again}
            onClick={() => take.mutate()}
          >
            {take.isPending ? 'Taking backup' : 'Take backup'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {/* R-214, said at the point it matters. Not a tooltip: this is the one
            fact about backups that costs an installation if it is missed, and
            a person reads it here or not at all. */}
        <Banner tone="info">
          Pando doesn&rsquo;t keep this passphrase. If you lose it, nothing in this backup can be
          read again — not by you, and not by anyone who takes the file.
        </Banner>

        <Input
          label="Passphrase"
          type="password"
          value={passphrase}
          autoComplete="new-password"
          helper="At least 16 characters. Write it down somewhere that will still exist if this machine doesn't."
          onChange={(e) => setPassphrase(e.target.value)}
        />
        <Input
          label="Passphrase again"
          type="password"
          value={again}
          autoComplete="new-password"
          onChange={(e) => setAgain(e.target.value)}
          error={
            mismatch
              ? 'These two are different.'
              : take.isError
                ? messageOf(take.error)
                : undefined
          }
        />
        <Input
          label="Keep for"
          type="number"
          value={retainDays}
          helper="Days. Leave it at 0 to keep this backup until you discard it."
          onChange={(e) => setRetainDays(e.target.value)}
        />
      </div>
    </Dialog>
  );
}

function UseBackup({
  row,
  mode,
  onClose,
}: {
  row: BackupRow;
  mode: 'verify' | 'restore';
  onClose: () => void;
}) {
  const [passphrase, setPassphrase] = useState('');
  const [typed, setTyped] = useState('');
  const queries = useQueryClient();

  const restoring = mode === 'restore';

  // One mutation for both, because the dialog is the same shape and only the
  // consequence differs. The response bodies differ too, so the type is the
  // union rather than either one.
  const run = useMutation<{ volumes?: number; verified?: boolean }>({
    mutationFn: () =>
      restoring
        ? api.post<{ volumes: number }>(`/backups/${row.id}/restore`, { passphrase, confirm: true })
        : api.post<{ verified: boolean }>(`/backups/${row.id}/verify`, { passphrase }),
    onSuccess: () => void queries.invalidateQueries(),
  });

  // Typing the word is the confirmation. A second "are you sure" dialog trains
  // people to click through both; typing cannot be done by muscle memory.
  const confirmed = !restoring || typed === 'replace';

  return (
    <Dialog
      open
      title={restoring ? 'Restore this installation' : 'Check this backup'}
      description={
        restoring
          ? 'Everything here is replaced by what was in the backup. Anything created since then is gone.'
          : 'Reads the backup and checks it is complete. Nothing here changes.'
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant={restoring ? 'destructive' : 'primary'}
            disabled={run.isPending || passphrase === '' || !confirmed}
            onClick={() => run.mutate()}
          >
            {run.isPending
              ? restoring
                ? 'Restoring'
                : 'Checking'
              : restoring
                ? 'Replace this installation'
                : 'Check backup'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {run.isSuccess && !restoring && (
          <Banner tone="running">This backup is complete and can be restored.</Banner>
        )}
        {run.isSuccess && restoring && (
          <Banner tone="running">Restored. Apps are being brought back to their pinned setup.</Banner>
        )}

        <Input
          label="Passphrase"
          type="password"
          value={passphrase}
          autoComplete="off"
          onChange={(e) => setPassphrase(e.target.value)}
          error={run.isError ? messageOf(run.error) : undefined}
        />

        {restoring && (
          <Input
            label="Type replace to confirm"
            value={typed}
            autoComplete="off"
            onChange={(e) => setTyped(e.target.value)}
          />
        )}
      </div>
    </Dialog>
  );
}

/** Sizes in the shortest honest unit, like durations elsewhere. */
function humanSize(bytes: number): string {
  if (!bytes) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let n = bytes;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}
