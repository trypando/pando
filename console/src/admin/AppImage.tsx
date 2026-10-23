// The image on an app's launcher tile (R-340), as the tile in the app's header.
//
// Setting it needs app.spec.edit, the same as renaming. For somebody with it
// the tile carries a pencil badge and opens a dialog to upload, replace or
// remove the image; anyone else sees the tile and nothing that offers a change
// the server would refuse.
//
// With no image, the tile is the terrain the launcher generates from the app's
// ID — what everyone sees until somebody uploads one.
//
// The tile loads the same URL the launcher does, which is gated on the data
// plane. Somebody who administers this app but may not open it gets a
// not-found for it, and sees the generated terrain instead of a broken image —
// the upload still works for them.

import { useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { invalidateAppLists } from './appList';
import { Button, Dialog, Icon } from '@design';

import { api, base } from '@api/client';
import type { App } from '@api/types.gen';
import { TopoTile } from '../ui/TopoBackground';
import { AppVerb, keepVerbs, useCan } from './verbs';
import { isSVG, svgToPng } from './rasterize';

// SVG is offered too, and converted to a PNG before it leaves the browser (see
// rasterize.ts); the server itself never accepts one.
const ACCEPT = 'image/png,image/jpeg,image/webp,image/gif,image/svg+xml,.svg';

const TILE: React.CSSProperties = {
  position: 'relative',
  width: 'var(--space-8)',
  height: 'var(--space-8)',
  flex: '0 0 auto',
  padding: 0,
  borderRadius: 'var(--radius-md)',
  border: 'var(--border-width) solid var(--rule)',
  background: 'var(--paper-sunken)',
};

function iconSrc(app: App): string | null {
  return app.icon_updated_at ? `${base}/apps/${app.id}/icon?v=${encodeURIComponent(app.icon_updated_at)}` : null;
}

/** The image, or the terrain, filling its box. */
function Picture({ app }: Readonly<{ app: App }>) {
  const src = iconSrc(app);
  const [broken, setBroken] = useState<string | null>(null);
  return (
    <span style={{ display: 'block', width: '100%', height: '100%', overflow: 'hidden', borderRadius: 'var(--radius-md)' }}>
      {src !== null && broken !== src ? (
        <img src={src} alt="" onError={() => setBroken(src)} style={{ width: '100%', height: '100%', objectFit: 'cover', display: 'block' }} />
      ) : (
        <TopoTile seed={app.id} />
      )}
    </span>
  );
}

export function AppImage({ app }: { app: App }) {
  // The same verb as the endpoints: PUT and DELETE /icon are app.spec.edit.
  const canEdit = useCan(AppVerb.SpecEdit);
  const [open, setOpen] = useState(false);

  if (!canEdit) {
    return (
      <span style={TILE}>
        <Picture app={app} />
      </span>
    );
  }

  return (
    <>
      <button type="button" aria-label="Change image" title="Change image" onClick={() => setOpen(true)} style={{ ...TILE, cursor: 'pointer' }}>
        <Picture app={app} />
        <span
          aria-hidden="true"
          style={{
            position: 'absolute',
            right: 'calc(var(--space-2) * -0.75)',
            bottom: 'calc(var(--space-2) * -0.75)',
            width: 'calc(var(--space-5) - var(--space-1) / 2)',
            height: 'calc(var(--space-5) - var(--space-1) / 2)',
            borderRadius: 'var(--radius-pill)',
            background: 'var(--paper-raised)',
            border: 'var(--border-width) solid var(--rule-strong)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          <Icon name="pencil" size={12} color="var(--ink)" />
        </span>
      </button>
      {open && <ChangeImage app={app} onClose={() => setOpen(false)} />}
    </>
  );
}

function uploadLabel(pending: boolean, set: boolean): string {
  if (pending) return 'Uploading';
  return set ? 'Replace' : 'Upload';
}

function ChangeImage({ app, onClose }: Readonly<{ app: App; onClose: () => void }>) {
  const queries = useQueryClient();
  const picker = useRef<HTMLInputElement>(null);

  const refresh = (updated: App) => {
    queries.setQueryData(['apps', app.id], keepVerbs(updated));
    void invalidateAppLists(queries);
    void queries.invalidateQueries({ queryKey: ['me', 'apps'] });
  };

  const upload = useMutation({
    mutationFn: async (file: File) => api.putFile<App>(`/apps/${app.id}/icon`, isSVG(file) ? await svgToPng(file) : file),
    onSuccess: refresh,
  });
  const remove = useMutation({
    mutationFn: () => api.del<App>(`/apps/${app.id}/icon`),
    onSuccess: refresh,
  });

  // The header passes the record as it now is, so the preview follows an upload.
  const src = iconSrc(app);
  const busy = upload.isPending || remove.isPending;
  const failure = upload.error ?? remove.error;

  return (
    <Dialog
      open
      onClose={onClose}
      title="Launcher image"
      description="The picture on this app’s tile in the launcher."
      footer={
        <Button variant="ghost" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
          <span style={TILE}>
            <Picture key={src ?? ''} app={app} />
          </span>
          <input
            ref={picker}
            type="file"
            accept={ACCEPT}
            hidden
            onChange={(e) => {
              const file = e.target.files?.[0];
              e.target.value = '';
              if (file) upload.mutate(file);
            }}
          />
          <Button variant="secondary" disabled={busy} onClick={() => picker.current?.click()}>
            {uploadLabel(upload.isPending, src !== null)}
          </Button>
          {src && (
            <Button variant="secondary" disabled={busy} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Removing' : 'Remove'}
            </Button>
          )}
        </div>
        {failure ? (
          // The server's message, verbatim (R-105): it names the limit that
          // was hit and what to do about it.
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{failure.message}</span>
        ) : (
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>PNG, JPEG, WebP, GIF or SVG, up to 256 KB.</span>
        )}
      </div>
    </Dialog>
  );
}
