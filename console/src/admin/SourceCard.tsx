// Where the app comes from, what is running, and how it is deployed.
//
// One card: the source (a repository and branch, an image and tag, or files
// somebody sent), what is running from it with the way to its deploy log, a
// strip offering what is newer when there is something newer, and the four
// deploy settings as switches (DeploySwitches).

import { useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Icon, Input } from '@design';

import { api, RequestFailed } from '@api/client';
import type { App, AppSpec, Deployment, Revision } from '@api/types.gen';
import { messageOf } from '../install/Accounts';
import { relative } from '../ui/time';
import { APP_LIST_KEY } from './appList';
import { useOpenPanel } from './AppPanel';
import { DeploySwitches, shortRef, useAutoDeploy } from './DeploySettings';
import { imageUpdateLabel, useImageUpdate } from './ImageUpdateButton';
import { useNewestSpec } from './newestSpec';
import { filesFromDrop, filesFromInput, prepare } from './pack';
import { FilePicker, sendFiles } from './UploadSource';
import type { PickedFile } from './pack';
import { AppVerb, useCan } from './verbs';

type OnRefused = (message: string, remedy?: string, code?: string) => void;

export interface SourceCardProps {
  app: App;
  /** A refused deploy or upgrade, said under the header; empty clears it. */
  onRefused: OnRefused;
  /** A deploy started here is waiting for approval (R-154). */
  onWaiting: () => void;
  /** Settings, at what automatic deploys watch. */
  onOptions?: () => void;
}

/** A commit or digest, short enough to read. */
export function short(ref: string | undefined): string {
  if (!ref) return '';
  if (ref.startsWith('sha256:')) return ref.slice(0, 'sha256:'.length + 8);
  return ref.slice(0, 7);
}

/** The tag in an image reference, or "latest" when it names none. */
export function tagOf(image: string): string {
  const name = image.split('@')[0] ?? image;
  const slash = name.lastIndexOf('/');
  const colon = name.lastIndexOf(':');
  return colon > slash ? name.slice(colon + 1) : 'latest';
}

export function SourceCard({ app, onRefused, onWaiting, onOptions }: Readonly<SourceCardProps>) {
  const canEdit = useCan(AppVerb.SpecEdit);
  const [changing, setChanging] = useState(false);
  const type = app.source?.type;

  return (
    <section aria-labelledby="source" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 'var(--space-4)', flexWrap: 'wrap' }}>
        <h4 id="source" style={{ font: 'var(--type-h4)', margin: 0 }}>
          Source and deploys
        </h4>
        {canEdit && (type === 'git' || type === 'image' || type === 'upload') && (
          <Button variant="ghost" onClick={() => setChanging(true)}>
            Change source
          </Button>
        )}
      </div>
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-3)',
          padding: 'var(--space-4)',
          background: 'var(--paper-raised)',
          border: 'var(--border-width) solid var(--rule)',
          borderRadius: 'var(--radius-md)',
        }}
      >
        <SourceLine app={app} />
        {type === 'git' && <NewerCommit app={app} onRefused={onRefused} onWaiting={onWaiting} />}
        {type === 'image' && <NewerBuild app={app} onRefused={onRefused} onWaiting={onWaiting} />}
        {type === 'upload' && canEdit && <SendFiles appID={app.id} />}
        <div style={{ borderTop: 'var(--border-width) solid var(--rule)', paddingTop: 'var(--space-3)' }}>
          <DeploySwitches app={app} onOptions={onOptions} />
        </div>
      </div>
      {changing && <ChangeSource app={app} onClose={() => setChanging(false)} />}
    </section>
  );
}

/** The server's words, or packing's own (a folder too large), or the
 *  network's, said as Pando says it. */
function errorText(error: unknown): string {
  return error instanceof Error && !(error instanceof TypeError) ? error.message : messageOf(error);
}

const CAPTION: React.CSSProperties = { font: 'var(--type-caption)', color: 'var(--ink-secondary)' };
const MONO: React.CSSProperties = { font: 'var(--type-code-sm)', color: 'var(--ink)' };
const LINK: React.CSSProperties = {
  padding: 0,
  border: 0,
  background: 'none',
  font: 'var(--type-caption)',
  color: 'var(--ink-secondary)',
  textDecoration: 'underline',
  cursor: 'pointer',
};

/** The source, then what is running from it and the way to its deploy log. */
function SourceLine({ app }: Readonly<{ app: App }>) {
  const openDeployments = useOpenPanel('deployments');
  const deployments = useQuery({
    queryKey: ['apps', app.id, 'deployments'],
    queryFn: () => api.get<{ deployments: Deployment[] | null }>(`/apps/${app.id}/deployments`),
  });
  const list = deployments.data?.deployments ?? [];
  const ran = list.find((d) => d.status === 'succeeded');
  const source = app.source ?? { type: '' };
  const running = source.type === 'image' ? short(source.digest) : short(source.commit);

  return (
    <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 'var(--space-2) var(--space-5)' }}>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)', minWidth: 0, flexWrap: 'wrap' }}>
        {source.type === 'git' && (
          <>
            <Icon name="git-branch" aria-label="Repository" role="img" />
            <span style={{ ...MONO, overflowWrap: 'anywhere' }}>{source.url}</span>
            {source.ref && (
              <>
                <span style={CAPTION}>on</span>
                <code style={MONO}>{source.ref}</code>
              </>
            )}
          </>
        )}
        {source.type === 'image' && (
          <>
            <Icon name="package" aria-label="Published image" role="img" />
            <code style={{ ...MONO, overflowWrap: 'anywhere' }}>{source.image}</code>
          </>
        )}
        {source.type === 'upload' && (
          <>
            <Icon name="upload" />
            <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>Uploaded files</span>
          </>
        )}
      </span>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        {ran ? (
          <>
            <span style={CAPTION}>Running</span>
            {running && <code style={MONO}>{running}</code>}
            <span style={CAPTION}>deployed {relative(ran.finished_at ?? ran.started_at).toLowerCase()}</span>
          </>
        ) : (
          !deployments.isPending && <span style={CAPTION}>Not deployed yet</span>
        )}
        {openDeployments && list.length > 0 && (
          <span style={CAPTION}>
            ·{' '}
            <button type="button" style={LINK} onClick={() => openDeployments({ view: 'deployments', deployment: list[0]?.id })}>
              Deployment log
            </button>{' '}
            ·{' '}
            <button type="button" style={LINK} onClick={() => openDeployments({ view: 'deployments' })}>
              History
            </button>
          </span>
        )}
      </span>
    </div>
  );
}

/** The sunken strip that offers something newer. */
function Strip({ children, dashed = false }: Readonly<{ children: React.ReactNode; dashed?: boolean }>) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        flexWrap: 'wrap',
        gap: 'var(--space-2) var(--space-3)',
        padding: 'var(--space-2) var(--space-2) var(--space-2) var(--space-3)',
        borderRadius: 'var(--radius-sm)',
        background: dashed ? undefined : 'var(--paper-sunken)',
        border: dashed ? 'var(--border-width) dashed var(--field-border)' : undefined,
      }}
    >
      {children}
    </div>
  );
}

/**
 * A newer commit than the one running, as automatic deploys last found it
 * (GET /auto-deploy). Deploying it writes a revision that names the commit,
 * so what ships is what the strip said, then deploys that revision.
 */
function NewerCommit({ app, onRefused, onWaiting }: Readonly<Omit<SourceCardProps, 'onOptions'>>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const canDeploy = useCan(AppVerb.Deploy);
  const view = useAutoDeploy(app.id);
  const newest = useNewestSpec(app.id);
  const check = view.data?.last_check;
  const found = check?.found_commit;

  const deploy = useMutation({
    mutationFn: async () => {
      const spec = newest.spec as AppSpec;
      const rev = await api.post<Revision>(`/apps/${app.id}/specs`, { ...spec, source: { ...spec.source, commit: found } });
      return api.post<{ status?: string } | undefined>(`/apps/${app.id}/deployments`, { spec_revision: rev.revision });
    },
    onSuccess: (deployment) => {
      onRefused('', undefined, undefined);
      if (deployment?.status === 'awaiting_approval') onWaiting();
      void queries.invalidateQueries({ queryKey: ['apps', app.id], exact: true });
      for (const part of ['deployments', 'status', 'security', 'specs', 'notices', 'auto-deploy']) {
        void queries.invalidateQueries({ queryKey: ['apps', app.id, part] });
      }
      void queries.invalidateQueries({ queryKey: APP_LIST_KEY });
    },
    onError: (error) => {
      const failed = error instanceof RequestFailed ? error : null;
      onRefused(failed?.message ?? 'Pando couldn’t start a deploy. Try again.', failed?.remedy, failed?.code);
    },
  });

  if (!check || !found || found === app.source?.commit) return null;
  return (
    <Strip>
      <Icon name="circle-arrow-up" color="var(--contour-text)" />
      <span style={{ flex: '1 1 18rem', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
        <code style={MONO}>{shortRef(check.found_ref) || app.source?.ref}</code> has a newer commit,{' '}
        <code style={MONO}>{short(found)}</code>.
      </span>
      <span style={CAPTION}>Checked {relative(check.checked_at).toLowerCase()}</span>
      {canEdit && canDeploy && newest.spec && (
        <Button variant="secondary" disabled={deploy.isPending || app.state === 'deploying'} onClick={() => deploy.mutate()}>
          {`Deploy ${short(found)}`}
        </Button>
      )}
    </Strip>
  );
}

/**
 * A newer build of the image's tag (R-143a). A check is asked for, never made
 * on its own: it reads a registry. The strip appears when one finds something.
 */
function NewerBuild({ app, onRefused, onWaiting }: Readonly<Omit<SourceCardProps, 'onOptions'>>) {
  const canEdit = useCan(AppVerb.SpecEdit);
  const canDeploy = useCan(AppVerb.Deploy);
  const u = useImageUpdate(app, onRefused, onWaiting);
  // Upgrading writes a revision and deploys it: both verbs.
  if (!canEdit || !canDeploy) return null;
  const found = u.found;

  if (found?.update_available) {
    return (
      <Strip>
        <Icon name="circle-arrow-up" color="var(--contour-text)" />
        <span style={{ flex: '1 1 18rem', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
          Tag <code style={MONO}>{tagOf(found.image)}</code> now names a newer build,{' '}
          <code style={MONO}>{short(found.latest_digest)}</code>.
        </span>
        <span style={CAPTION}>
          Checked {relative(new Date(u.checkedAt).toISOString()).toLowerCase()} ·{' '}
          <button type="button" style={LINK} disabled={u.busy} onClick={u.check}>
            Check again
          </button>
        </span>
        <Button variant="secondary" disabled={u.busy} onClick={u.upgrade}>
          {u.upgrading ? 'Upgrading' : 'Upgrade'}
        </Button>
      </Strip>
    );
  }
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)' }}>
      <Button variant="ghost" disabled={u.busy} onClick={u.check}>
        {found ? 'Check again' : imageUpdateLabel(found, u.checking, u.upgrading)}
      </Button>
      {found && !u.checking && <span style={CAPTION}>Up to date.</span>}
    </span>
  );
}

function sendState(pending: boolean, sent: boolean): string {
  if (pending) return 'Sending.';
  return sent ? 'Sent. Pando is checking the app again.' : 'Drop the app’s folder here to send a new version.';
}

/** New files for an app sent from somebody's computer: dropped, or chosen. */
function SendFiles({ appID }: Readonly<{ appID: string }>) {
  const queries = useQueryClient();
  const input = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);
  const send = useMutation({
    mutationFn: (files: PickedFile[]) => sendFiles(appID, files),
    onSuccess: () => void queries.invalidateQueries({ queryKey: ['apps', appID] }),
  });
  const take = (picked: PickedFile[]) => {
    const files = prepare(picked);
    if (files.length > 0) send.mutate(files);
  };

  return (
    <div
      onDragOver={(e) => {
        e.preventDefault();
        setOver(true);
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setOver(false);
        if (!send.isPending) void filesFromDrop(e.dataTransfer.items).then(take);
      }}
      style={{ background: over ? 'var(--paper-sunken)' : undefined, borderRadius: 'var(--radius-sm)' }}
    >
      <Strip dashed>
        <Icon name="upload" />
        <span style={{ flex: '1 1 18rem', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
          {sendState(send.isPending, send.isSuccess)}{' '}
          <span style={CAPTION}>Pando checks the app again, then you deploy.</span>
        </span>
        <Button variant="secondary" disabled={send.isPending} onClick={() => input.current?.click()}>
          Choose files
        </Button>
        <input
          ref={input}
          type="file"
          multiple
          hidden
          onChange={(e) => {
            take(filesFromInput(e.target.files));
            e.target.value = '';
          }}
        />
      </Strip>
      {send.isError && (
        <div style={{ marginTop: 'var(--space-2)' }}>
          <Banner tone="failed">{errorText(send.error)}</Banner>
        </div>
      )}
    </div>
  );
}

/**
 * A different repository, branch or image, written as a revision the next
 * deploy ships; or, for files, new files. All app.spec.edit.
 */
function ChangeSource({ app, onClose }: Readonly<{ app: App; onClose: () => void }>) {
  const queries = useQueryClient();
  const newest = useNewestSpec(app.id);
  const spec = newest.spec;
  const type = app.source?.type;
  const [url, setUrl] = useState(spec?.source?.url ?? app.source?.url ?? '');
  const [ref, setRef] = useState(spec?.source?.ref ?? app.source?.ref ?? '');
  const [image, setImage] = useState(spec?.source?.image ?? app.source?.image ?? '');
  const [files, setFiles] = useState<PickedFile[]>([]);

  const save = useMutation({
    mutationFn: async () => {
      if (type === 'upload') return sendFiles(app.id, files);
      const s = spec as AppSpec;
      // A new place to read from names no commit or build yet: the next
      // deploy pins what it finds there (design 01 §2.1).
      const source =
        type === 'git'
          ? { ...s.source, url: url.trim(), ref: ref.trim(), commit: '' }
          : { ...s.source, image: image.trim(), digest: '' };
      await api.post(`/apps/${app.id}/specs`, { ...s, source });
    },
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', app.id] });
      onClose();
    },
  });

  const named = type === 'git' ? url.trim() !== '' : image.trim() !== '';
  const ready = type === 'upload' ? files.length > 0 : Boolean(spec) && named;
  const saveLabel = type === 'upload' ? 'Send files' : 'Save';

  return (
    <Dialog
      open
      onClose={onClose}
      title="Change source"
      description={type === 'upload' ? 'Pando checks the app again, then you deploy.' : 'Takes effect at the next deploy.'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!ready || save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving' : saveLabel}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {type === 'git' && (
          <>
            <Input label="Repository" mono autoComplete="off" spellCheck={false} value={url} onChange={(e) => setUrl(e.target.value)} />
            <Input
              label="Branch"
              mono
              autoComplete="off"
              spellCheck={false}
              value={ref}
              helper="Empty uses the repository’s default branch."
              onChange={(e) => setRef(e.target.value)}
            />
          </>
        )}
        {type === 'image' && (
          <Input
            label="Image"
            mono
            autoComplete="off"
            spellCheck={false}
            value={image}
            helper="For example ghcr.io/acme/web:1.4."
            onChange={(e) => setImage(e.target.value)}
          />
        )}
        {type === 'upload' && <FilePicker files={files} onPick={(picked) => setFiles(picked)} disabled={save.isPending} />}
        {save.isError && <Banner tone="failed">{errorText(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}
