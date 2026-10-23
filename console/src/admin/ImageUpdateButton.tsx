// Check for a newer build of an image app's tag, then upgrade to it (R-143a).
//
// One button that changes with what it found: a check writes nothing, and the
// upgrade sends the digest that check returned, so what ships is the build the
// person was shown. Secondary, beside Deploy, which stays the view's primary.

import { useEffect } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button } from '@design';

import { api, RequestFailed } from '@api/client';
import type { App, ImageCheck } from '@api/types.gen';
import { APP_LIST_KEY } from './appList';

export function imageUpdateLabel(check: ImageCheck | undefined, checking: boolean, upgrading: boolean): string {
  if (upgrading) return 'Upgrading';
  if (checking) return 'Checking';
  if (!check) return 'Check for update';
  return check.update_available ? 'Upgrade' : 'Up to date';
}

export function ImageUpdateButton({
  app,
  onRefused,
  onWaiting,
}: {
  app: App;
  /** The refusal to show, or an empty message to clear one. */
  onRefused: (message: string, remedy?: string, code?: string) => void;
  /** The upgrade's deploy is waiting for approval (R-154). */
  onWaiting?: () => void;
}) {
  const queries = useQueryClient();
  const key = ['apps', app.id, 'image-update'];

  // Fetched only when pressed: a check reads a registry, which can be slow or rate limited.
  const check = useQuery({
    queryKey: key,
    queryFn: () => api.get<ImageCheck>(`/apps/${app.id}/image-update`),
    enabled: false,
    retry: false,
  });

  const upgrade = useMutation({
    mutationFn: (digest: string) =>
      api.post<{ status?: string } | undefined>(`/apps/${app.id}/image-update`, { digest }),
    // What a deploy changes, as Deploy invalidates it, plus the new revision.
    onSuccess: (deployment) => {
      if (deployment?.status === 'awaiting_approval') onWaiting?.();
      void queries.resetQueries({ queryKey: key });
      void queries.invalidateQueries({ queryKey: ['apps', app.id], exact: true });
      for (const part of ['deployments', 'status', 'security', 'specs']) {
        void queries.invalidateQueries({ queryKey: ['apps', app.id, part] });
      }
      void queries.invalidateQueries({ queryKey: APP_LIST_KEY });
      void queries.invalidateQueries({ queryKey: ['approvals'] });
    },
    // A refused upgrade, most often a tag that moved, needs a fresh check: drop the stale one.
    onError: () => void queries.resetQueries({ queryKey: key }),
  });

  // Refusals go where Deploy's do, across the page under the header.
  const failed = upgrade.error ?? check.error;
  const refusal = failed instanceof RequestFailed ? failed : null;
  useEffect(() => {
    if (failed) {
      onRefused(refusal?.message ?? 'Pando couldn’t check the image. Try again.', refusal?.remedy, refusal?.code);
    } else if (check.isSuccess || upgrade.isSuccess) {
      onRefused('', undefined, undefined);
    }
  }, [failed, check.isSuccess, upgrade.isSuccess, refusal?.message, refusal?.remedy, refusal?.code]);

  const found = check.data;
  return (
    <Button
      variant="secondary"
      disabled={check.isFetching || upgrade.isPending || app.state === 'deploying'}
      onClick={() => {
        if (found?.update_available) upgrade.mutate(found.latest_digest);
        else {
          upgrade.reset();
          void check.refetch();
        }
      }}
    >
      {imageUpdateLabel(found, check.isFetching, upgrade.isPending)}
    </Button>
  );
}
