// The image on an app's launcher tile (R-340).
//
// Setting it needs app.spec.edit, the same as renaming. Somebody without it
// sees the image and no Upload or Remove, rather than buttons the server
// would refuse.
//
// With no image, the preview is the terrain the launcher generates from the
// app's ID — what everyone sees until somebody uploads one.
//
// The preview loads the same URL the launcher does, which is gated on the data
// plane. Somebody who administers this app but may not open it gets a
// not-found for it, and sees the generated terrain instead of a broken image —
// the upload still works for them.

import { useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { invalidateAppLists } from './appList';
import { Button } from '@design';

import { api, base } from '@api/client';
import type { App } from '@api/types.gen';
import { TopoTile } from '../ui/TopoBackground';
import { AppVerb, keepVerbs, useCan } from './verbs';
import { isSVG, svgToPng } from './rasterize';

// SVG is offered too, and converted to a PNG before it leaves the browser (see
// rasterize.ts); the server itself never accepts one.
const ACCEPT = 'image/png,image/jpeg,image/webp,image/gif,image/svg+xml,.svg';

export function AppImage({ app }: { app: App }) {
  const queries = useQueryClient();
  const picker = useRef<HTMLInputElement>(null);
  const [broken, setBroken] = useState<string | null>(null);
  // The same verb as the endpoints: PUT and DELETE /icon are app.spec.edit.
  const canEdit = useCan(AppVerb.SpecEdit);

  const refresh = (updated: App) => {
    queries.setQueryData(['apps', app.id], keepVerbs(updated));
    void invalidateAppLists(queries);
    void queries.invalidateQueries({ queryKey: ['me', 'apps'] });
  };

  const upload = useMutation({
    mutationFn: async (file: File) =>
      api.putFile<App>(`/apps/${app.id}/icon`, isSVG(file) ? await svgToPng(file) : file),
    onSuccess: refresh,
  });
  const remove = useMutation({
    mutationFn: () => api.del<App>(`/apps/${app.id}/icon`),
    onSuccess: refresh,
  });

  const src = app.icon_updated_at
    ? `${base}/apps/${app.id}/icon?v=${encodeURIComponent(app.icon_updated_at)}`
    : null;
  const showImage = src !== null && broken !== src;
  const busy = upload.isPending || remove.isPending;
  const failure = upload.error ?? remove.error;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
        <div
          style={{
            width: '6ch',
            aspectRatio: '1 / 1',
            borderRadius: 'var(--radius-md)',
            border: 'var(--border-width) solid var(--rule)',
            overflow: 'hidden',
            flex: '0 0 auto',
          }}
        >
          {showImage ? (
            <img
              src={src}
              alt=""
              onError={() => setBroken(src)}
              style={{ width: '100%', height: '100%', objectFit: 'cover', display: 'block' }}
            />
          ) : (
            <TopoTile seed={app.id} />
          )}
        </div>

        {canEdit && (
          <>
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
              {upload.isPending ? 'Uploading' : src ? 'Replace' : 'Upload'}
            </Button>
            {src && (
              <Button variant="secondary" disabled={busy} onClick={() => remove.mutate()}>
                {remove.isPending ? 'Removing' : 'Remove'}
              </Button>
            )}
          </>
        )}
      </div>

      {failure ? (
        // The server's message, verbatim (R-105): it names the limit that was
        // hit and what to do about it.
        <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{failure.message}</span>
      ) : (
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          Shown on this app&rsquo;s tile in the launcher. Without one, the tile shows a map generated for this
          app.{canEdit && ' PNG, JPEG, WebP, GIF or SVG, up to 256 KB.'}
        </span>
      )}
    </div>
  );
}
