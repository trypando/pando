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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, Input, Radio, Switch } from '@design';

import { api, RequestFailed } from '@api/client';
import type { App, AppSpec, AutoDeployCheck, AutoDeployView } from '@api/types.gen';
import { relative } from '../ui/time';
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
  const [trigger, setTrigger] = useState(
    spec.deploy?.auto_deploy?.trigger === 'release_tagged' ? 'release_tagged' : 'branch_updated',
  );
  const [branch, setBranch] = useState(spec.deploy?.auto_deploy?.branch ?? '');
  const [tagPattern, setTagPattern] = useState(spec.deploy?.auto_deploy?.tag_pattern ?? '');
  const [requireApproval, setRequireApproval] = useState(spec.deploy?.require_approval ?? false);
  // Both triggers watch a repository (R-141). An image's tags are R-143.
  const fromGit = spec.source?.type === 'git';

  const save = useMutation({
    mutationFn: () =>
      api.post(`/apps/${appID}/specs`, {
        ...spec,
        deploy: {
          ...spec.deploy,
          strategy: startThenSwap ? 'start_then_swap' : 'recreate',
          auto_rollback: autoRollback,
          auto_deploy: {
            enabled: autoDeploy,
            trigger,
            branch: trigger === 'branch_updated' ? branch.trim() : '',
            tag_pattern: trigger === 'release_tagged' ? tagPattern.trim() : '',
          },
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
        title="Deploy when the repository changes"
        control={
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
            <Checkbox
              checked={autoDeploy}
              onChange={(e) => setAutoDeploy(e.target.checked)}
              disabled={!canEdit || requireApproval || !fromGit}
              label="Deploy automatically"
            />
            {autoDeploy && (
              <AutoDeployTrigger
                trigger={trigger}
                onTrigger={setTrigger}
                branch={branch}
                onBranch={setBranch}
                deployedFrom={spec.source?.ref ?? ''}
                tagPattern={tagPattern}
                onTagPattern={setTagPattern}
                disabled={!canEdit}
              />
            )}
          </div>
        }
      >
        {fromGit
          ? 'Pando checks the repository every few minutes and deploys what it finds. Off by default, so nothing ships without someone asking for it. Not available while this app’s deploys need approval.'
          : 'Automatic deploys watch a git repository, and this app is deployed from an image or uploaded files. Deploy a new version by hand.'}
      </Setting>

      {fromGit && <AutoDeployActivity appID={appID} canEdit={canEdit} />}

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

/** What deploys automatically: each commit on a branch, or each new release (R-141). */
function AutoDeployTrigger({
  trigger,
  onTrigger,
  branch,
  onBranch,
  deployedFrom,
  tagPattern,
  onTagPattern,
  disabled,
}: {
  trigger: string;
  onTrigger: (t: 'branch_updated' | 'release_tagged') => void;
  branch: string;
  onBranch: (b: string) => void;
  deployedFrom: string;
  tagPattern: string;
  onTagPattern: (p: string) => void;
  disabled: boolean;
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <fieldset style={{ border: 0, padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
        <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
          What deploys
        </legend>
        <Radio
          name="auto_deploy_trigger"
          value="branch_updated"
          label="Each new commit on a branch"
          description="For a project that does not tag its releases. Every push to the branch ships."
          checked={trigger === 'branch_updated'}
          disabled={disabled}
          onChange={() => onTrigger('branch_updated')}
        />
        <Radio
          name="auto_deploy_trigger"
          value="release_tagged"
          label="Each new release"
          description="Pando deploys the newest release tag, and ignores commits in between."
          checked={trigger === 'release_tagged'}
          disabled={disabled}
          onChange={() => onTrigger('release_tagged')}
        />
      </fieldset>
      {trigger === 'branch_updated' ? (
        <Input
          label="Branch"
          mono
          autoComplete="off"
          spellCheck={false}
          value={branch}
          placeholder={deployedFrom || 'main'}
          helper={
            deployedFrom
              ? `Leave empty to follow ${deployedFrom}, the branch this app was deployed from.`
              : 'The branch whose commits deploy.'
          }
          disabled={disabled}
          onChange={(e) => onBranch(e.target.value)}
        />
      ) : (
        <Input
          label="Release tags"
          mono
          autoComplete="off"
          spellCheck={false}
          value={tagPattern}
          placeholder="v1.2.3"
          helper="Leave empty to count tags such as v1.2.3 as releases, without pre-releases. Or give a pattern such as release-*, where * matches any characters. The highest version wins."
          disabled={disabled}
          onChange={(e) => onTagPattern(e.target.value)}
        />
      )}
    </div>
  );
}

/**
 * What auto-deploy last found, and the webhook that makes it look sooner
 * (R-142). Read from GET /auto-deploy, which reports on the deployed settings.
 */
function AutoDeployActivity({ appID, canEdit }: { appID: string; canEdit: boolean }) {
  const queries = useQueryClient();
  const view = useQuery({
    queryKey: ['apps', appID, 'auto-deploy'],
    queryFn: () => api.get<AutoDeployView>(`/apps/${appID}/auto-deploy`),
  });
  // The secret is in this response and nowhere else, ever.
  const rotate = useMutation({
    mutationFn: () =>
      api.post<{ webhook_url: string; webhook_secret: string }>(`/apps/${appID}/auto-deploy/webhook-secret`),
    onSuccess: () => void queries.invalidateQueries({ queryKey: ['apps', appID, 'auto-deploy'] }),
  });
  const remove = useMutation({
    mutationFn: () => api.del(`/apps/${appID}/auto-deploy/webhook-secret`),
    onSuccess: () => {
      rotate.reset();
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'auto-deploy'] });
    },
  });

  const v = view.data;
  if (!v || !v.deployed.enabled) return null;
  const check = v.last_check;

  return (
    <Setting
      title="Checks"
      control={
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <Input label="Webhook URL" mono readOnly value={v.webhook_url} />
          {rotate.data && (
            <Input
              label="Webhook secret"
              mono
              readOnly
              value={rotate.data.webhook_secret}
              helper="Shown once. Paste it into the repository’s webhook settings with the URL above, content type application/json."
            />
          )}
          {canEdit && (
            <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
              <Button variant="secondary" onClick={() => rotate.mutate()} disabled={rotate.isPending}>
                {v.webhook_secret_set ? 'Replace webhook secret' : 'Make webhook secret'}
              </Button>
              {v.webhook_secret_set && (
                <Button variant="ghost" onClick={() => remove.mutate()} disabled={remove.isPending}>
                  Turn off webhook
                </Button>
              )}
            </div>
          )}
          {(rotate.isError || remove.isError) && <Failure error={rotate.error ?? remove.error} />}
        </div>
      }
    >
      {check ? lastCheck(check) : 'Pando has not checked this app yet.'}{' '}
      A webhook from the repository makes Pando check as soon as something is pushed, if the
      repository’s host can reach this installation. Without one, Pando still checks every few
      minutes.
    </Setting>
  );
}

/** The last check, as a sentence. */
function lastCheck(c: AutoDeployCheck): string {
  const when = `Last checked ${relative(c.checked_at).toLowerCase()}.`;
  if (c.error) return `${when} ${c.error}`;
  const found = c.found_commit
    ? ` Found ${shortRef(c.found_ref)} at ${c.found_commit.slice(0, 7)}.`
    : '';
  const tried =
    c.attempted_commit && c.attempted_commit !== c.found_commit
      ? ` Last deployed automatically: ${c.attempted_commit.slice(0, 7)}.`
      : '';
  return `${when}${found}${tried}`;
}

function shortRef(ref?: string): string {
  return (ref ?? '').replace(/^refs\/(heads|tags)\//, '');
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
