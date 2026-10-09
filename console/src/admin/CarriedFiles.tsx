// Configuration files the app carries (R-020).
//
// A compose service that bind-mounts `./Caddyfile` gets that file copied into
// the spec at detection and placed in the container at every start. It is a
// snapshot: the repository can change underneath it, and nothing here changes
// until somebody re-detects. That is worth a screen of its own, because
// otherwise the only record of what the app is actually running is a warning on
// the review page that somebody read once.

import { useState } from 'react';
import { Button, CodeBlock, Dialog } from '@design';

import type { AppSpec, File as CarriedFile, Workload } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { Table } from '../ui/Table';
import { useNewestSpec } from './newestSpec';

interface Row {
  path: string;
  workload: string;
  size: string;
  content: string;
}

export function CarriedFiles({ appID }: { appID: string }) {
  const [showing, setShowing] = useState<Row | null>(null);

  const { spec, error } = useNewestSpec(appID);

  const rows = fileRows(spec);
  if (rows.length === 0) return null;

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Configuration files</h4>
      <Quiet>
        Files Pando copies into the app when it starts, taken from the repository when this app was
        read. Editing them in the repository changes nothing here until the app is read again.
      </Quiet>

      {error && <Quiet>{messageOf(error)}</Quiet>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          columns={[
            { key: 'path', header: 'Where it goes', width: 'minmax(0,38ch)', mono: true },
            { key: 'workload', header: 'Part of the app', width: '18ch', muted: true },
            { key: 'size', header: 'Size', width: '10ch', muted: true },
            {
              key: 'actions',
              header: '',
              width: '12ch',
              align: 'right',
              render: (row: Row) => (
                <Button variant="secondary" onClick={() => setShowing(row)}>
                  View
                </Button>
              ),
            },
          ]}
          rows={rows}
        />
      </div>

      {showing && (
        <Dialog
          open
          onClose={() => setShowing(null)}
          title={showing.path}
          description={`Placed in ${showing.workload} each time it starts.`}
          footer={
            <Button variant="ghost" onClick={() => setShowing(null)}>
              Close
            </Button>
          }
        >
          <CodeBlock lines={showing.content} />
        </Dialog>
      )}
    </section>
  );
}

function fileRows(body?: AppSpec): Row[] {
  const out: Row[] = [];
  for (const w of (body?.workloads ?? []) as Workload[]) {
    for (const f of (w.files ?? []) as CarriedFile[]) {
      out.push({
        path: f.path,
        workload: w.name,
        size: `${Math.max(1, Math.round(f.content.length / 1024))} KB`,
        content: f.content,
      });
    }
  }
  return out;
}
