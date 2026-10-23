// One app: what needs doing, where it comes from, how each workload is, and
// its security score.
//
// What the app is — its image, name, status, score and address — is the
// header's (AdminConsole). Every deploy's log and the app's own output are in
// the side panel (AppPanel), opened from here: both are histories, and a
// history on the screen you deploy from is a screen that scrolls forever.

import { useEffect, useRef } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Banner, Button, Card } from '@design';

import { api } from '@api/client';
import type { App, BackupAttempt } from '@api/types.gen';
import { MEASURE } from '../ui/layout';
import { deploymentsInterval } from '../ui/polling';
import { Attention } from './Attention';
import { Security } from './Security';
import { SourceCard } from './SourceCard';
import { Workloads } from './Workloads';
import { useOpenPanel } from './AppPanel';
import { ApprovalRequest } from './ApprovalRequest';
import { isAwaiting } from './approval';
import type { ApprovalDeployment } from './approval';

/** The Security section's element id, which the header's score scrolls to. */
export const SECURITY_ID = 'app-security';

/** Brings the Security section into view, where there is one. */
export function scrollToSecurity() {
  document.getElementById(SECURITY_ID)?.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

const ignore = () => undefined;

export function AppOverview({
  app,
  onGo,
  focus,
  onRefused = ignore,
  onWaiting = ignore,
}: {
  app: App & { last_backup?: BackupAttempt };
  /** Where a notice's fix lives: a tab, and the section on it. Output and
   *  deploy logs open in the side panel instead (useOpenPanel). */
  onGo: (tab: string, focus?: string) => void;
  /** A section to open at: `security`. */
  focus?: string;
  /** A deploy or upgrade started here was refused; an empty message clears it. */
  onRefused?: (message: string, remedy?: string, code?: string) => void;
  /** A deploy started here is waiting for approval (R-154). */
  onWaiting?: () => void;
}) {
  // Output and deployments both need app.logs.read; the opener is absent
  // without it, and so is every control that would use it.
  const openPanel = useOpenPanel('output');
  const security = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (focus === 'security') security.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [focus]);

  const deployments = useQuery({
    queryKey: ['apps', app.id, 'deployments'],
    queryFn: () => api.get<{ deployments: ApprovalDeployment[] | null }>(`/apps/${app.id}/deployments`),
    // While a deploy waits for approval, somebody else's answer is what moves
    // it, so it is asked again now and then rather than left until a reload.
    // While one runs it is not asked at all: the app screen reads it again
    // when the app's status says the deploy has finished
    // (useRecordFollowsStatus), and the deploy's own log streams.
    refetchInterval: (query) => deploymentsInterval(query.state.data?.deployments),
  });
  // A newer request supersedes an older one (R-156), so there is at most one.
  const waiting = (deployments.data?.deployments ?? []).find(isAwaiting);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)' }}>
      {app.state === 'failed' && (
        // The one place red belongs on this screen. A banner, not stacked with
        // anything else. "Fix what's wrong" needs a way to find out what is
        // wrong, and that is the app's own output, in the side panel.
        <Banner
          tone="failed"
          action={
            openPanel && (
              <Button variant="secondary" onClick={() => openPanel({ view: 'output' })}>
                Open logs
              </Button>
            )
          }
        >
          Pando has stopped trying to start this app. Fix what&rsquo;s wrong and deploy again.
        </Banner>
      )}

      {app.state === 'deploying' && <Banner tone="building">Deploying.</Banner>}

      {/* A deploy waiting for approval has not started, and the app keeps
          running what it ran (R-154). Said here, with the request itself,
          because this is where the person who pressed Deploy is looking. */}
      {waiting && (
        <div style={{ maxWidth: MEASURE }}>
          <Card padding="md">
            <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>A deploy is waiting for approval</h4>
            <ApprovalRequest deployment={waiting} />
          </Card>
        </div>
      )}

      <Attention appID={app.id} onGo={onGo} onFindings={scrollToSecurity} />

      {app.pinned_spec_id && (
        <SourceCard app={app} onRefused={onRefused} onWaiting={onWaiting} onOptions={() => onGo('settings', 'auto-deploy')} />
      )}

      <Workloads app={app} />

      <div id={SECURITY_ID} ref={security}>
        <Security appID={app.id} watching={app.state === 'deploying'} />
      </div>
    </div>
  );
}
