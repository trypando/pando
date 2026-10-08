// Deploy settings (R-145, R-147).
//
// Both settings here are off by default and both explain themselves **in body
// text at the point of enabling, not in a tooltip**. Design 08 §1.3 is explicit
// about that, and the reason is that a tooltip is not read by the person who
// most needs it: someone scanning a settings page for the thing that will make
// deploys faster.
//
// Start-then-swap runs two copies of the app at once. For an app that writes to
// a local file or runs migrations on startup, that is data corruption, and the
// person enabling it is the only one who knows which kind of app theirs is.
//
// Auto-rollback is off because health is a nullable signal (R-147, design 05
// §4): an app with no health check reports nothing, and rolling back on "no
// signal" would undo good deploys.
//
// Saving is a spec revision, so app.spec.edit. Without it the settings are
// shown as they are, disabled, and there is no Save.

import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, Switch } from '@design';

import { api, RequestFailed } from '@api/client';
import type { App, AppSpec } from '@api/types.gen';
import { useAppStatus } from './Parts';
import { MEASURE } from '../ui/layout';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

/**
 * The deploy settings of an app, on its newest revision, with the status that
 * says whether approval has paused its auto-deploy (R-158).
 */
export function DeploySection({ app }: { app: App }) {
  const appID = app.id;
  const newest = useNewestSpec(appID);
  // The same query Parts reads, so it is one request.
  const status = useAppStatus(app);
  if (!newest.spec) return null;
  return (
    <DeploySettings
      // A new revision starts the form again from what it says.
      key={JSON.stringify(newest.spec.deploy ?? {})}
      appID={appID}
      spec={newest.spec}
      autoDeployPaused={status.data?.auto_deploy_paused ?? false}
    />
  );
}

export function DeploySettings({
  appID,
  spec,
  autoDeployPaused = false,
}: {
  appID: string;
  spec: AppSpec;
  /** The pinned spec deploys automatically, and approval now stops it (R-158). */
  autoDeployPaused?: boolean;
}) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const [startThenSwap, setStartThenSwap] = useState(
    spec.deploy?.strategy === 'start_then_swap',
  );
  const [autoRollback, setAutoRollback] = useState(spec.deploy?.auto_rollback ?? false);
  const [autoDeploy, setAutoDeploy] = useState(spec.deploy?.auto_deploy?.enabled ?? false);
  const [requireApproval, setRequireApproval] = useState(spec.deploy?.require_approval ?? false);

  const save = useMutation({
    mutationFn: () =>
      api.post(`/apps/${appID}/specs`, {
        ...spec,
        deploy: {
          ...spec.deploy,
          strategy: startThenSwap ? 'start_then_swap' : 'recreate',
          auto_rollback: autoRollback,
          auto_deploy: { ...spec.deploy?.auto_deploy, enabled: autoDeploy },
          require_approval: requireApproval,
        },
      }),
    onSuccess: () => void queries.invalidateQueries({ queryKey: ['apps', appID] }),
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)', maxWidth: MEASURE }}>
      {/* R-158: said on the app, because otherwise a branch moves and nothing
          happens, and nobody is told why. */}
      {autoDeployPaused && (
        <Banner tone="info">
          Auto-deploy is paused. This app’s deploys now need approval, and a deploy that waits for
          somebody can’t start on its own. Deploy by hand to ask for approval.
        </Banner>
      )}

      <Setting
        title="Deploy when the branch changes"
        control={
          <Checkbox
            checked={autoDeploy}
            onChange={(e) => setAutoDeploy(e.target.checked)}
            disabled={!canEdit || requireApproval}
            label="Deploy automatically"
          />
        }
      >
        Pando checks the branch every few minutes and deploys when it moves. Off by default, so
        nothing ships without someone asking for it. Not available while this app’s deploys need
        approval.
      </Setting>

      <Setting
        title="Require approval for this app’s deploys"
        control={
          <Switch
            checked={requireApproval}
            onChange={(e) => {
              setRequireApproval(e.target.checked);
              // R-158: the two do not combine, and the server refuses a spec
              // asking for both. Turning approval on says so by doing it.
              if (e.target.checked) setAutoDeploy(false);
            }}
            disabled={!canEdit}
            label="Wait for approval before each deploy"
          />
        }
      >
        {/* R-154: turning it off is read from the running configuration too,
            so switching it off is itself a deploy somebody approves. */}
        Each deploy waits until somebody allowed to approve it says yes. Turning this on turns
        auto-deploy off, because a request for every push is a backlog nobody reads. Turning it off
        again needs one last approval. An administrator can also require approval for an app, and
        then it applies whatever this says.
      </Setting>

      <Setting
        title="Keep the app up during a deploy"
        control={
          <Switch
            checked={startThenSwap}
            onChange={(e) => setStartThenSwap(e.target.checked)}
            disabled={!canEdit}
            label="Start the new version before stopping the old one"
          />
        }
      >
        {/* Body text, not a tooltip. This is the sentence that stops someone
            corrupting their own data. */}
        Two copies of your app run at the same time during a deploy. Do not enable this if your app
        writes to a local file or runs migrations on startup.
      </Setting>

      <Setting
        title="Undo a deploy that doesn't come up"
        control={
          <Switch
            checked={autoRollback}
            onChange={(e) => setAutoRollback(e.target.checked)}
            disabled={!canEdit}
            label="Roll back automatically"
          />
        }
      >
        Off by default, because Pando can only roll back on a signal it trusts. An app without a
        health check reports nothing, and Pando would undo deploys that were fine.
      </Setting>

      {canEdit && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
          {/* Secondary: Deploy, in the app's header, is the one primary
              button on this screen. */}
          <Button
            variant="secondary"
            onClick={() => save.mutate()}
            disabled={save.isPending}
            style={{ alignSelf: 'flex-start' }}
          >
            Save settings
          </Button>
          {save.isError && <Failure error={save.error} />}
          {save.isSuccess && (
            <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
              Saved as a new configuration revision. It takes effect on the next deploy.
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function Setting({
  title,
  children,
  control,
}: {
  title: string;
  children: React.ReactNode;
  control: React.ReactNode;
}) {
  return (
    <section
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-2)',
        paddingBottom: 'var(--space-5)',
        borderBottom: 'var(--border-width) solid var(--rule)',
      }}
    >
      <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>{title}</h4>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
        {children}
      </p>
      <div style={{ marginTop: 'var(--space-2)' }}>{control}</div>
    </section>
  );
}

function Failure({ error }: { error: unknown }) {
  const failed = error instanceof RequestFailed ? error : null;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--marker-deep)', margin: 0 }}>
        {failed?.message ?? 'Pando couldn’t save these settings. Try again.'}
      </p>
      {failed?.remedy && (
        <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
          {failed.remedy}
        </p>
      )}
    </div>
  );
}
