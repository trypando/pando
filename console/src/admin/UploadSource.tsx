// Sending an app's files from somebody's computer (R-262).
//
// For the person R-005 describes, "push it to a repository first" is the whole
// obstacle: they have a folder, or one index.html an assistant wrote for them.
// So the console takes the folder as it is — chosen, or dropped — packs it the
// way `pando deploy ./` does, and Pando works out how to run it like any other
// source. Nothing about what happens after differs from a repository.

import { useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button } from '@design';

import { api } from '@api/client';
import { Quiet, messageOf } from '../install/Accounts';
import { filesFromDrop, filesFromInput, folderOf, pack, prepare, totalBytes, type PickedFile } from './pack';
import { RegistryCredential } from './RegistryCredential';
import { AppVerb, useCan } from './verbs';

/** The most Pando accepts, compressed (maxUploadBytes in the server). */
export const MAX_UPLOAD_BYTES = 256 * 1024 * 1024;

/** Packs files and sends them as an app's source, then asks Pando to read them. */
export async function sendFiles(appID: string, files: readonly PickedFile[]): Promise<void> {
  const archive = await pack(files);
  if (archive.size > MAX_UPLOAD_BYTES) {
    throw new Error(
      `These files are ${Math.ceil(archive.size / 1024 / 1024)} MB compressed, which is more than the 256 MB Pando accepts. ` +
        'Leave out build output and downloaded dependencies, which Pando rebuilds.',
    );
  }
  await api.postFile(`/apps/${appID}/source`, archive);
  await api.post(`/apps/${appID}/detection/rerun`);
}

export function describeFiles(files: readonly PickedFile[]): string {
  if (files.length === 0) return '';
  const size = totalBytes(files);
  const amount = size < 1024 * 1024 ? `${Math.max(1, Math.round(size / 1024))} KB` : `${(size / 1024 / 1024).toFixed(1)} MB`;
  return files.length === 1 ? `${files[0]!.path}, ${amount}` : `${files.length} files, ${amount}`;
}

/** Choose or drop files or a folder. */
export function FilePicker({
  files,
  onPick,
  disabled,
}: {
  files: readonly PickedFile[];
  /** folder is the chosen folder's name, '' when files were chosen. */
  onPick: (files: PickedFile[], folder: string) => void;
  disabled?: boolean;
}) {
  const filesInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);

  const take = (picked: PickedFile[]) => onPick(prepare(picked), folderOf(picked));

  return (
    <div
      onDragOver={(e) => {
        e.preventDefault();
        if (!disabled) setOver(true);
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setOver(false);
        if (disabled) return;
        void filesFromDrop(e.dataTransfer.items).then(take);
      }}
      style={{
        border: `var(--border-width) dashed ${over ? 'var(--rule-strong)' : 'var(--field-border)'}`,
        borderRadius: 'var(--radius-md)',
        background: over ? 'var(--paper-sunken)' : 'var(--paper)',
        padding: 'var(--space-5)',
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-3)',
      }}
    >
      <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>Files</span>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
        {files.length > 0
          ? describeFiles(files)
          : 'Drop a folder or files here, or choose them. A single index.html is enough for a web page.'}
      </p>
      <div style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap' }}>
        <Button variant="secondary" disabled={disabled} onClick={() => folderInput.current?.click()}>
          Choose a folder
        </Button>
        <Button variant="secondary" disabled={disabled} onClick={() => filesInput.current?.click()}>
          Choose files
        </Button>
      </div>
      <input
        ref={filesInput}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          take(filesFromInput(e.target.files));
          e.target.value = '';
        }}
      />
      <input
        ref={folderInput}
        type="file"
        hidden
        // A folder picker: not in React's types, and every current browser
        // supports it.
        {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
        onChange={(e) => {
          take(filesFromInput(e.target.files));
          e.target.value = '';
        }}
      />
      <Quiet>
        Pando leaves out .git, node_modules and other folders it rebuilds. Up to 256 MB compressed.
      </Quiet>
    </div>
  );
}

/** The files section of an app that was sent from somebody's computer. */
export function UploadedFiles({ appID }: { appID: string }) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const [files, setFiles] = useState<PickedFile[]>([]);

  const send = useMutation({
    mutationFn: () => sendFiles(appID, files),
    onSuccess: () => {
      setFiles([]);
      void queries.invalidateQueries({ queryKey: ['apps', appID] });
    },
  });

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Files</h4>
      <Quiet>
        This app runs files sent from a computer rather than a repository. Sending new ones replaces
        them, and Pando works out again how to run the app. Nothing changes until you accept what it
        finds.
      </Quiet>
      {canEdit && (
        <div style={{ marginTop: 'var(--space-4)', display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <FilePicker files={files} onPick={setFiles} disabled={send.isPending} />
          {send.isError && <Quiet>{messageOf(send.error)}</Quiet>}
          {send.isSuccess && <Quiet>Sent. Pando is working out how to run them.</Quiet>}
          <div>
            <Button variant="primary" disabled={files.length === 0 || send.isPending} onClick={() => send.mutate()}>
              {send.isPending ? 'Sending' : 'Send files'}
            </Button>
          </div>
        </div>
      )}
    </section>
  );
}

/** Whichever source section the app's source calls for. */
export function SourceSection({ appID }: { appID: string }) {
  const app = useQuery({
    queryKey: ['apps', appID],
    queryFn: () => api.get<{ source?: { type?: string } }>(`/apps/${appID}`),
  });
  switch (app.data?.source?.type) {
    case 'upload':
      return <UploadedFiles appID={appID} />;
    case 'image':
      return <RegistryCredential appID={appID} />;
    default:
      return null;
  }
}
