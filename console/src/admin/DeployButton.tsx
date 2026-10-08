// Deploy, in the app's header.
//
// It used to sit at the bottom of the overview, under the status card, the
// security score and every warning — so deploying meant scrolling past
// everything on the page to reach the one thing somebody came to do. It is an
// action on the app rather than a part of the overview, which is what the
// header is for, and from there it is reachable from every tab.
//
// The primary button of the view, and the only one (the brand's rule): Delete
// beside it is destructive, and nothing else on an app's screen is primary.
//
// It owns its own query of the app's revisions rather than taking them as a
// prop. The key is the one the overview already uses, so asking twice costs one
// request, and the alternative — passing the revision down — is how a button
// ends up shipping a spec that changed while somebody was reading the page.

import { useEffect } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button } from '@design';

import { api, RequestFailed } from '@api/client';
import type { App } from '@api/types.gen';
import { APP_LIST_KEY } from './appList';

interface SpecRevision {
  id: string;
  revision: number;
}

interface PastDeploy {
  status: string;
}

export function DeployButton({
  app,
  onRefused,
  onWaiting,
}: {
  app: App;
  /** The refusal to show, or an empty message to clear one. */
  onRefused: (message: string, remedy?: string, code?: string) => void;
  /** The deploy was accepted and is waiting for approval (R-154): it has not
   *  started, and the screen has to say so rather than "Deploying". */
  onWaiting?: () => void;
}) {
  const queries = useQueryClient();

  const specs = useQuery({
    queryKey: ['apps', app.id, 'specs'],
    queryFn: () =>
      api.get<{ revisions: SpecRevision[] | null; pinned_spec_id: string }>(`/apps/${app.id}/specs`),
    enabled: Boolean(app.pinned_spec_id),
  });

  // Whether there is a running version this would replace.
  //
  // The same query key the overview and the logs tab use, so it costs one
  // request between them. A deployment that succeeded, rather than the app's
  // state: a first deploy that failed at the build leaves an app that has
  // never run, and offering to re-deploy it would be describing something that
  // never happened.
  const deployments = useQuery({
    queryKey: ['apps', app.id, 'deployments'],
    queryFn: () => api.get<{ deployments: PastDeploy[] | null }>(`/apps/${app.id}/deployments`),
    enabled: Boolean(app.pinned_spec_id),
  });
  const shipped = (deployments.data?.deployments ?? []).some((d) => d.status === 'succeeded');

  const revisions = (specs.data?.revisions ?? []).slice().sort((a, b) => b.revision - a.revision);
  const newest = revisions[0];
  const pinned = revisions.find((r) => r.id === app.pinned_spec_id);

  // Editing anything writes a new revision and leaves the pinned one alone
  // (R-152), so Deploy ships the newest when there is one — which is what the
  // banner on the overview is telling somebody about.
  const unshipped = Boolean(newest && pinned && newest.revision > pinned.revision);

  const deploy = useMutation({
    mutationFn: () =>
      api.post<{ status?: string } | undefined>(
        `/apps/${app.id}/deployments`,
        unshipped ? { spec_revision: newest?.revision } : {},
      ),
    // What a deploy changes, and nothing else (issue #72): this app's record,
    // deploys, status and security report, and the paged list of apps so its
    // row follows it through building to running. Not every ['apps', …]
    // query, which was every other app's record and every list in the cache.
    onSuccess: (deployment) => {
      if (deployment?.status === 'awaiting_approval') onWaiting?.();
      void queries.invalidateQueries({ queryKey: ['apps', app.id], exact: true });
      for (const part of ['deployments', 'status', 'security']) {
        void queries.invalidateQueries({ queryKey: ['apps', app.id, part] });
      }
      void queries.invalidateQueries({ queryKey: APP_LIST_KEY });
      void queries.invalidateQueries({ queryKey: ['approvals'] });
    },
  });

  const failed = deploy.error instanceof RequestFailed ? deploy.error : null;

  // The refusal is handed to the caller rather than drawn under the button.
  //
  // A deploy is refused with a paragraph — "none of this app's workloads is
  // marked as the primary one…" — and a paragraph inside a row of buttons
  // widens the row and shoves them around, which is what happened. It belongs
  // where the app's other banners are: across the page, under the header.
  useEffect(() => {
    if (deploy.isError) {
      onRefused(
        failed?.message ?? 'Pando couldn’t start a deploy. Try again.',
        failed?.remedy,
        failed?.code,
      );
    }
    if (deploy.isSuccess) {
      onRefused('', undefined, undefined);
    }
  }, [deploy.isError, deploy.isSuccess, failed?.message, failed?.remedy, failed?.code]);

  return (
    <Button
      variant="primary"
      onClick={() => deploy.mutate()}
      disabled={deploy.isPending || app.state === 'deploying'}
    >
      {/* One action, and the label says whether something is already running
          that it will replace. The word it produces is still "Deployed" — in
          the log, in the deploy list, and in the app's state — because it is
          the same act either way. */}
      {app.state === 'deploying'
        ? shipped
          ? 'Re-deploying'
          : 'Deploying'
        : shipped
          ? 'Re-deploy'
          : 'Deploy'}
    </Button>
  );
}
