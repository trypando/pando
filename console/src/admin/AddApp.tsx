// Adding an app — the screen R-002 is actually about.
//
// "Setup cost is paid once, at the host. Deploying the tenth app must feel like
// nothing." Every other screen in the console administers apps that already
// exist; until this one there was no way to make one except `pando app add` or
// a POST by hand, which fails R-005 outright — the person this is for may not
// know what a port is, and will certainly not be running curl.
//
// Design 08's principle governs the shape: **the default path shows almost
// nothing — name, source, deploy.** Everything with a sane default lives behind
// Advanced and is never surfaced during setup. A field here has to justify
// being a blocker rather than configuration.
//
// So the whole form is one input. The name is derived from the URL and shown as
// a filled field somebody may correct, rather than asked for — asking is how a
// two-field form becomes a four-field form.
//
// Three sources, one flow (issue #41): a repository, an image that is already
// built, or files from this computer. An image may be private, which is the one
// extra question — asked only when somebody says so. Files are packed here and
// sent once the app exists; if sending fails, the app is kept and sending again
// goes to it rather than making a second one.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { invalidateAppLists } from './appList';
import { Banner, Button, Checkbox, Dialog, Input, Select } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { refusal } from '../install/Accounts';
import { covers, ready as sourceReady, useSources } from '../install/Sources';
import { useSettled } from '../ui/paged';
import { CredentialFields, credentialBody, credentialComplete, emptyCredential, type CredentialDraft } from './RegistryCredential';
import { FilePicker, sendFiles } from './UploadSource';
import type { PickedFile } from './pack';

type Kind = 'git' | 'image' | 'upload';

export function AddApp({ onAdded, onClose }: { onAdded: (app: App) => void; onClose: () => void }) {
  const queries = useQueryClient();

  const [kind, setKind] = useState<Kind>('git');
  const [url, setUrl] = useState('');
  const [image, setImage] = useState('');
  const [files, setFiles] = useState<PickedFile[]>([]);
  const [name, setName] = useState('');
  const [named, setNamed] = useState(false);
  const [isPrivate, setPrivate] = useState(false);
  const [credential, setCredential] = useState<CredentialDraft>(emptyCredential);

  // An upload app made on a first attempt whose files did not arrive. The next
  // attempt sends to it.
  const [created, setCreated] = useState<App | null>(null);

  // A repository may be picked from a source connection that can list them
  // (R-091) rather than typed. Typing still works for any of them: the
  // connection covering an address is found by the server.
  const sources = useSources(kind === 'git');
  const listable = (sources.data?.sources ?? []).filter((s) => sourceReady(s) && s.capabilities.list_repositories);
  const [from, setFrom] = useState('');
  const [search, setSearch] = useState('');
  const settled = useSettled(search.trim());
  const repos = useQuery({
    queryKey: ['source-repositories', from, settled],
    queryFn: () =>
      api.get<{ repositories: { url: string; full_name: string }[] | null }>(
        `/sources/${encodeURIComponent(from)}/repositories${settled ? `?q=${encodeURIComponent(settled)}` : ''}`,
      ),
    enabled: kind === 'git' && from !== '',
  });

  const ready =
    name.trim() !== '' &&
    (kind === 'git'
      ? url.trim() !== ''
      : kind === 'image'
        ? image.trim() !== '' && (!isPrivate || credentialComplete(credential))
        : files.length > 0);

  const create = useMutation({
    mutationFn: async () => {
      if (kind === 'upload') {
        const app = created ?? (await api.post<App>('/apps', { name: name.trim(), source: { type: 'upload' } }));
        setCreated(app);
        await sendFiles(app.id, files);
        return app;
      }
      const source =
        kind === 'git'
          ? { type: 'git', url: url.trim(), ...(from ? { connection: from } : {}) }
          : {
              type: 'image',
              image: image.trim(),
              ...(isPrivate ? { credential: credentialBody(credential) } : {}),
            };
      return api.post<App>('/apps', { name: name.trim(), source });
    },
    onSuccess: (app) => {
      void invalidateAppLists(queries);
      onAdded(app);
    },
  });

  // The name follows the source until somebody types one. Deriving it silently
  // and showing the result is the difference between one question and two.
  const edit = (set: (v: string) => void) => (value: string) => {
    if (create.isError) create.reset();
    set(value);
    if (!named) setName(nameFrom(value));
  };

  const error = create.isError ? refusal(create.error) : undefined;

  return (
    <Dialog
      open
      onClose={onClose}
      title="Add an app"
      // What happens next, said before it happens. Pando works out how to build
      // and run it, and shows you what it found before anything deploys
      // (R-102: ask, never guess).
      description="Pando works out how to build and run it, then shows you what it found. Nothing deploys until you accept."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={create.isPending || !ready} onClick={() => create.mutate()}>
            {create.isPending ? (kind === 'upload' ? 'Sending' : 'Adding') : 'Add app'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        {kind === 'git' && listable.length > 0 && (
          <Select
            label="Repository from"
            value={from}
            options={[
              { value: '', label: 'An address I enter' },
              ...listable.map((s) => ({ value: s.id, label: `${s.name} (${covers(s)})` })),
            ]}
            onChange={(e) => {
              if (create.isError) create.reset();
              setFrom(e.target.value);
              setSearch('');
            }}
          />
        )}
        {kind === 'git' && from !== '' && (
          <>
            <Input
              label="Find a repository"
              value={search}
              placeholder="api"
              helper="Part of its name. Pando lists the repositories this connection can read."
              onChange={(e) => setSearch(e.target.value)}
            />
            {repos.isError ? (
              <Banner tone="failed">{refusal(repos.error)}</Banner>
            ) : (
              <Select
                label="Repository"
                value={url}
                disabled={repos.isPending}
                options={[
                  ...(url ? [] : [{ value: '', label: repos.isPending ? 'Loading repositories' : 'Choose a repository' }]),
                  ...(repos.data?.repositories ?? []).map((r) => ({ value: r.url, label: r.full_name })),
                ]}
                onChange={(e) => edit(setUrl)(e.target.value)}
              />
            )}
            {error && <Banner tone="failed">{error}</Banner>}
          </>
        )}
        {kind === 'git' && from === '' && (
          <Input
            label="Repository"
            mono
            autoFocus
            value={url}
            placeholder="https://github.com/acme/notes"
            helper="Pando reads it to work out how to build and run the app, with the source connection that covers it if it is private. Nothing is read from it at deploy time."
            error={error}
            onChange={(e) => edit(setUrl)(e.target.value)}
          />
        )}
        {kind === 'image' && (
          <Input
            label="Image"
            mono
            autoFocus
            value={image}
            placeholder="ghcr.io/acme/web:1.4"
            helper="An image that is already built. Pando runs it as it is, pinned to the build the tag names now."
            error={error}
            onChange={(e) => edit(setImage)(e.target.value)}
          />
        )}
        {kind === 'upload' && (
          <>
            <FilePicker
              files={files}
              disabled={create.isPending}
              onPick={(picked, folder) => {
                if (create.isError) create.reset();
                setFiles(picked);
                if (!named && picked.length > 0) setName(nameFromFiles(picked, folder));
              }}
            />
            {error && <Banner tone="failed">{error}</Banner>}
          </>
        )}

        <Input
          label="Name"
          value={name}
          helper="What you will call it here. Taken from the source; change it if you like."
          onChange={(e) => {
            // Typing a name stops it following the source. Somebody who names
            // an app and then corrects the URL should not have their name
            // silently replaced.
            setNamed(true);
            setName(e.target.value);
          }}
        />

        {/* R-101's escape hatch: supply an image and skip detection. Never the
            default, because the whole product is the first option working. */}
        <Select
          label="Where it comes from"
          value={kind}
          disabled={created !== null}
          options={[
            { value: 'git', label: 'A Git repository' },
            { value: 'upload', label: 'Files on this computer' },
            { value: 'image', label: 'An image that is already built' },
          ]}
          onChange={(e) => {
            if (create.isError) create.reset();
            setKind(e.target.value as Kind);
          }}
        />

        {kind === 'image' && (
          <>
            <Banner tone="info">
              Pando reads the port, storage and health check the image declares, and asks about anything
              it cannot tell.
            </Banner>
            <Checkbox
              checked={isPrivate}
              onChange={(e) => setPrivate(e.target.checked)}
              label="The image is private"
            />
            {isPrivate && <CredentialFields value={credential} onChange={setCredential} />}
          </>
        )}
      </div>
    </Dialog>
  );
}

/** A name from a repository URL or an image reference. */
export function nameFrom(source: string): string {
  const trimmed = source.trim().replace(/\/+$/, '');
  if (trimmed === '') return '';

  // An image reference: drop the registry and the tag or digest.
  if (!trimmed.includes('://') && (trimmed.includes(':') || !trimmed.includes('.'))) {
    const last = trimmed.split('/').pop() ?? '';
    return (last.split('@')[0] ?? '').split(':')[0] ?? '';
  }

  const last = trimmed.split('/').pop() ?? '';
  return last.replace(/\.git$/, '');
}

/**
 * A name from chosen files: the folder's, when a folder was chosen, or the one
 * file's without its extension. index.html names nothing, so it gives way to
 * "site".
 */
export function nameFromFiles(files: readonly PickedFile[], folder?: string): string {
  if (folder) return folder;
  if (files.length !== 1) return 'site';
  const base = (files[0]!.path.split('/').pop() ?? '').replace(/\.[^.]+$/, '');
  return base === '' || base === 'index' ? 'site' : base;
}
