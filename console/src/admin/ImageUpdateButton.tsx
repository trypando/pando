// Check for a newer build of an image app's tag, then upgrade to it (R-143a).
//
// A check writes nothing, and the upgrade sends the digest that check
// returned, so what ships is the build the person was shown. Used by the
// Overview's Source and deploys card.

import { useEffect } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api, RequestFailed } from '@api/client';
import type { App, ImageCheck } from '@api/types.gen';
import { APP_LIST_KEY } from './appList';

export function imageUpdateLabel(check: ImageCheck | undefined, checking: boolean, upgrading: boolean): string {
  if (upgrading) return 'Upgrading';
  if (checking) return 'Checking';
  if (!check) return 'Check for update';
  return check.update_available ? 'Upgrade' : 'Up to date';
}

type OnRefused = (message: string, remedy?: string, code?: string) => void;

/** The check and the upgrade, for the button and for the Overview's update strip. */
export function useImageUpdate(app: App, onRefused: OnRefused, onWaiting?: () => void) {
  const queries = useQueryClient();
  const key = ['apps', app.id, 'image-update'];

  // Fetched only when asked: a check reads a registry, which can be slow or rate limited.
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
      for (const part of ['deployments', 'status', 'security', 'specs', 'notices']) {
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
  return {
    found,
    checkedAt: check.dataUpdatedAt,
    checking: check.isFetching,
    upgrading: upgrade.isPending,
    busy: check.isFetching || upgrade.isPending || app.state === 'deploying',
    check: () => {
      upgrade.reset();
      void check.refetch();
    },
    upgrade: () => {
      if (found) upgrade.mutate(found.latest_digest);
    },
  };
}
