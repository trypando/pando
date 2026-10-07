// Deleting an app (R-204, R-205).
//
// The console is the interactive surface, so this is where R-204's question is
// actually asked: keep a final backup of the app's storage, or discard it. The
// API takes the answer as a query parameter and refuses to choose on the
// caller's behalf; the CLI answers with a flag, this asks a person.
//
// The question is only worth asking when there is storage. An app that keeps
// nothing gets a plain confirmation, because a backup choice over an empty set
// is a decision that looks consequential and isn't.
//
// It lives in the app's header rather than on a tab, because the app most
// likely to be deleted is the one whose tabs are missing: an app whose source
// could not be fetched never gets past detection, has no pinned spec, and shows
// one tab. A delete on the settings tab is a delete that app cannot reach.
//
// The verb is checked where this is placed, not here. The app's header shows
// it only on app.delete, from the verbs `GET /apps/{id}` returns. The screen
// for an app whose record would not load has no verbs to go on, and offers it
// anyway: that is the app somebody is most likely trying to remove, and the
// server refuses anybody who may not.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Radio } from '@design';

import { api } from '@api/client';
import { Quiet, messageOf } from '../install/Accounts';
import { deletePath, type StorageDecision } from './delete-app';
import { invalidateAppLists } from './appList';

export function DeleteApp({
  appID,
  appName,
  onDeleted,
}: {
  appID: string;
  appName: string;
  onDeleted: () => void;
}) {
  const queries = useQueryClient();
  const [open, setOpen] = useState(false);
  const [keepBackup, setKeepBackup] = useState(true);

  // The same query key the Storage section uses, so opening an app asks once
  // and both places agree about what it keeps.
  const volumes = useQuery({
    queryKey: ['volumes', appID],
    queryFn: () => api.get<{ volumes: Array<{ id: string }> | null }>(`/apps/${appID}/volumes`),
  });

  // Unknown counts as "ask". This screen is reachable for an app whose own
  // record would not load, and an unanswered request is not evidence that an
  // app keeps nothing — treating it as such would quietly discard data.
  const hasStorage = !volumes.isSuccess || (volumes.data?.volumes ?? []).length > 0;
  const decision: StorageDecision = !hasStorage ? 'none' : keepBackup ? 'backup' : 'discard';

  const remove = useMutation({
    mutationFn: () => api.del(deletePath(appID, decision)),
    onSuccess: () => {
      // The lists only, then remove. `['apps', appID]` sits under the
      // `['apps']` prefix, so a plain invalidate refetches the app that was
      // just deleted and puts a 404 on the screen on the way out of it.
      void invalidateAppLists(queries);
      queries.removeQueries({ queryKey: ['apps', appID] });
      onDeleted();
    },
  });

  return (
    <>
      <Button variant="destructive" onClick={() => setOpen(true)}>
        Delete app
      </Button>

      {open && (
        <Dialog
          open
          onClose={() => setOpen(false)}
          title={`Delete ${appName}`}
          description="This stops the app and removes it from Pando. It can’t be undone."
          footer={
            <>
              <Button variant="ghost" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button
                variant="destructive"
                disabled={remove.isPending}
                onClick={() => remove.mutate()}
              >
                {remove.isPending ? 'Deleting' : 'Delete app'}
              </Button>
            </>
          }
        >
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
            {hasStorage ? (
              // R-204 in the one place it is decided. Two options, both stated,
              // with the keeping one selected — the same default the CLI takes
              // (R-205), so the two surfaces do not disagree about what happens
              // when nobody thinks about it.
              <>
                <Radio
                  name="app-storage"
                  value="backup"
                  checked={keepBackup}
                  onChange={() => setKeepBackup(true)}
                  label="Keep a final backup of its storage"
                  description="Kept until you discard it. It doesn’t age out."
                />
                <Radio
                  name="app-storage"
                  value="discard"
                  checked={!keepBackup}
                  onChange={() => setKeepBackup(false)}
                  label="Delete its storage too"
                  description="The data goes with the app and can’t be recovered."
                />
              </>
            ) : (
              <Quiet>This app keeps no storage, so there is nothing to back up.</Quiet>
            )}

            {remove.isError && (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
                {/* The message is the server's, verbatim (R-105): a failed
                    backup means the app has not been deleted, and that is the
                    sentence a person needs. Its remedy is not shown — it names
                    `force=true`, which is the CLI's way of answering the
                    question the radio above already asks. */}
                <Banner tone="failed">{messageOf(remove.error)}</Banner>
                {keepBackup && hasStorage && (
                  <p
                    style={{
                      font: 'var(--type-caption)',
                      color: 'var(--ink-secondary)',
                      margin: 0,
                    }}
                  >
                    To delete it without a backup, choose &ldquo;Delete its storage too&rdquo;.
                  </p>
                )}
              </div>
            )}
          </div>
        </Dialog>
      )}
    </>
  );
}
