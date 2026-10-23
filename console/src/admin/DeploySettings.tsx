// Deploy settings (R-141, R-145, R-147, R-154, R-158).
//
// The four switches sit on the Overview, in the Source and deploys card: each
// one is a spec revision written the moment it flips, so app.spec.edit, and
// without it they are shown as they are, disabled. What auto-deploy watches —
// a branch or release tags — and its webhook are on Settings (DeploySection).
//
// Start-then-swap runs two copies of the app at once. For an app that writes to
// a local file or runs migrations on startup, that is data corruption, so
// turning it on shows R-145's sentence in full and asks before it saves.
//
// Auto-rollback is off by default because health is a nullable signal (R-147,
// design 05 §4): an app with no health check reports nothing, and rolling back
// on "no signal" would undo good deploys. So it cannot be turned on without one.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Input, Radio, Switch } from '@design';

import { api, RequestFailed } from '@api/client';
import type { App, AppSpec, AutoDeploy, AutoDeployCheck, AutoDeployView } from '@api/types.gen';
import { relative } from '../ui/time';
import { useAppStatus } from './appStatus';
import { MEASURE } from '../ui/layout';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

/** R-145, in full, where start-then-swap is turned on. */
export const START_FIRST_WARNING =
  'Two copies of your app run at the same time during a deploy. Do not enable this if your app writes to a local file or runs migrations on startup.';

/** Whether anything in the spec gives Pando a health signal (R-221). */
export function hasHealthCheck(spec: AppSpec): boolean {
  if ((spec.workloads ?? []).some((w) => w.healthcheck)) return true;
  const h = spec.health;
  return Boolean(h?.source && (h.path || h.port));
}

function autoDeployKey(appID: string) {
  return ['apps', appID, 'auto-deploy'];
}

export function useAutoDeploy(appID: string, enabled = true) {
  return useQuery({
    queryKey: autoDeployKey(appID),
    queryFn: () => api.get<AutoDeployView>(`/apps/${appID}/auto-deploy`),
    enabled,
  });
}

/** What a changed deploy setting invalidates: the revisions, and what is read from them. */
function invalidate(queries: ReturnType<typeof useQueryClient>, appID: string) {
  for (const part of ['specs', 'auto-deploy', 'notices', 'status']) {
    void queries.invalidateQueries({ queryKey: ['apps', appID, part] });
  }
}

/**
 * The four deploy settings as switches, for the Overview's Source and deploys
 * card. Each saves when it flips.
 */
export function DeploySwitches({ app, onOptions }: Readonly<{ app: App; onOptions?: () => void }>) {
  const appID = app.id;
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const newest = useNewestSpec(appID);
  const status = useAppStatus(app);
  const spec = newest.spec;
  const fromGit = (spec?.source?.type ?? app.source?.type) === 'git';
  const autoDeploy = useAutoDeploy(appID, fromGit);
  const [confirming, setConfirming] = useState(false);

  const write = useMutation({
    mutationFn: (deploy: Partial<AppSpec['deploy']>) =>
      api.post(`/apps/${appID}/specs`, { ...spec, deploy: { ...spec?.deploy, ...deploy } }),
    onSuccess: () => invalidate(queries, appID),
  });
  const setAuto = useMutation({
    mutationFn: (enabled: boolean) =>
      api.put(`/apps/${appID}/auto-deploy`, { ...(autoDeploy.data?.settings ?? spec?.deploy?.auto_deploy), enabled } satisfies AutoDeploy),
    onSuccess: () => invalidate(queries, appID),
  });

  if (!spec) return null;
  const busy = write.isPending || setAuto.isPending || !canEdit;
  const deploy = spec.deploy ?? ({} as AppSpec['deploy']);
  const requireApproval = deploy.require_approval ?? false;
  const startFirst = deploy.strategy === 'start_then_swap';
  const rollback = deploy.auto_rollback ?? false;
  const health = hasHealthCheck(spec);
  // R-158: approval stops automatic deploys, whether the app asks for it or
  // the installation's policy does.
  const paused = requireApproval || status.data?.auto_deploy_paused || autoDeploy.data?.paused;
  const settings = autoDeploy.data?.settings ?? deploy.auto_deploy;
  const watching =
    settings?.trigger === 'release_tagged' ? 'When a release is tagged' : `When ${settings?.branch || spec.source?.ref || 'the branch'} changes`;
  const failure = write.error ?? setAuto.error;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(13rem, 1fr))',
          gap: 'var(--space-3) var(--space-5)',
        }}
      >
        {fromGit && (
          <Hover text="Pando checks the repository every few minutes and deploys what it finds.">
            <Switch
              checked={Boolean(deploy.auto_deploy?.enabled) && !requireApproval}
              disabled={busy || Boolean(paused && !deploy.auto_deploy?.enabled)}
              label="Deploy automatically"
              description={paused ? 'Not while deploys need approval' : watching}
              onChange={(e) => setAuto.mutate(e.target.checked)}
            />
            {onOptions && (
              <button type="button" onClick={onOptions} style={LINK}>
                Options
              </button>
            )}
          </Hover>
        )}
        {/* R-154: turning it off is read from the running configuration too,
            so switching it off is itself a deploy somebody approves. */}
        <Hover text="Someone else approves each deploy. Turning it off needs one last approval. Installation policy can require it regardless.">
          <Switch
            checked={requireApproval}
            disabled={busy}
            label="Require approval"
            description={approvalNote(app.source?.type)}
            onChange={(e) =>
              write.mutate(
                // R-158: the two do not combine, and the server refuses a spec
                // asking for both. Turning approval on says so by doing it.
                e.target.checked
                  ? { require_approval: true, auto_deploy: { ...deploy.auto_deploy, enabled: false } }
                  : { require_approval: false },
              )
            }
          />
        </Hover>
        <Hover text={START_FIRST_WARNING}>
          <Switch
            checked={startFirst}
            disabled={busy}
            label="Start new version first"
            description="Two copies run during a deploy"
            onChange={(e) => {
              if (e.target.checked) setConfirming(true);
              else write.mutate({ strategy: 'recreate' });
            }}
          />
        </Hover>
        <Hover text="Pando deploys the last version that ran when a new one fails its health check.">
          <Switch
            checked={rollback}
            // Never on without a health check (R-147); always off again.
            disabled={busy || (!health && !rollback)}
            label="Roll back failed deploys"
            description={health ? 'When the health check fails' : 'Needs a health check'}
            onChange={(e) => write.mutate({ auto_rollback: e.target.checked })}
          />
        </Hover>
      </div>

      {failure && <Failure error={failure} />}

      {confirming && (
        <Dialog
          open
          onClose={() => setConfirming(false)}
          title="Start the new version first?"
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirming(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                onClick={() => {
                  setConfirming(false);
                  write.mutate({ strategy: 'start_then_swap' });
                }}
              >
                Turn on
              </Button>
            </>
          }
        >
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>{START_FIRST_WARNING}</p>
        </Dialog>
      )}
    </div>
  );
}

/** What approval covers, in a few words, by where the app comes from. */
function approvalNote(source: string | undefined): string {
  if (source === 'git') return 'Turns automatic deploys off';
  return source === 'image' ? 'Upgrades included' : 'For every deploy';
}

const LINK: React.CSSProperties = {
  alignSelf: 'flex-start',
  marginLeft: 'calc(var(--space-6) + var(--space-4))',
  padding: 0,
  border: 0,
  background: 'none',
  font: 'var(--type-caption)',
  color: 'var(--ink-secondary)',
  textDecoration: 'underline',
  cursor: 'pointer',
};

/** A switch with its full explanation as hover text. */
function Hover({ text, children }: Readonly<{ text: string; children: React.ReactNode }>) {
  return (
    <div title={text} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      {children}
    </div>
  );
}

/**
 * What auto-deploy watches and how it hears sooner, for an app deployed from a
 * repository: the Settings half of the deploy settings. Turning it on and off
 * is the Overview's switch.
 */
export function DeploySection({ app }: Readonly<{ app: App }>) {
  const appID = app.id;
  const newest = useNewestSpec(appID);
  if (!newest.spec || newest.spec.source?.type !== 'git') return null;
  return (
    <AutoDeployOptions
      // A new revision starts the form again from what it says.
      key={JSON.stringify(newest.spec.deploy?.auto_deploy ?? {})}
      appID={appID}
      spec={newest.spec}
    />
  );
}

function AutoDeployOptions({ appID, spec }: Readonly<{ appID: string; spec: AppSpec }>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const current = spec.deploy?.auto_deploy;
  const [trigger, setTrigger] = useState(current?.trigger === 'release_tagged' ? 'release_tagged' : 'branch_updated');
  const [branch, setBranch] = useState(current?.branch ?? '');
  const [tagPattern, setTagPattern] = useState(current?.tag_pattern ?? '');

  const save = useMutation({
    mutationFn: () =>
      api.put(`/apps/${appID}/auto-deploy`, {
        enabled: current?.enabled ?? false,
        trigger,
        branch: trigger === 'branch_updated' ? branch.trim() : '',
        tag_pattern: trigger === 'release_tagged' ? tagPattern.trim() : '',
      } satisfies AutoDeploy),
    onSuccess: () => invalidate(queries, appID),
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)', maxWidth: MEASURE }}>
      <Setting
        title="Automatic deploys"
        control={
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
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
            {canEdit && (
              <Button variant="secondary" onClick={() => save.mutate()} disabled={save.isPending} style={{ alignSelf: 'flex-start' }}>
                Save
              </Button>
            )}
            {save.isError && <Failure error={save.error} />}
          </div>
        }
      >
        What deploys when automatic deploys are on. Turn them on from the Overview.
      </Setting>
      <AutoDeployActivity appID={appID} canEdit={canEdit} />
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
          Trigger
        </legend>
        <Radio
          name="auto_deploy_trigger"
          value="branch_updated"
          label="New commits on a branch"
          checked={trigger === 'branch_updated'}
          disabled={disabled}
          onChange={() => onTrigger('branch_updated')}
        />
        <Radio
          name="auto_deploy_trigger"
          value="release_tagged"
          label="New releases"
          description="Deploys the newest release tag."
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
          helper={deployedFrom ? `Defaults to ${deployedFrom}.` : undefined}
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
          helper="Defaults to version tags such as v1.2.3, without pre-releases. Accepts a pattern such as release-*. The highest version deploys."
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
  const view = useAutoDeploy(appID);
  // The secret is in this response and nowhere else, ever.
  const rotate = useMutation({
    mutationFn: () =>
      api.post<{ webhook_url: string; webhook_secret: string }>(`/apps/${appID}/auto-deploy/webhook-secret`),
    onSuccess: () => void queries.invalidateQueries({ queryKey: autoDeployKey(appID) }),
  });
  const remove = useMutation({
    mutationFn: () => api.del(`/apps/${appID}/auto-deploy/webhook-secret`),
    onSuccess: () => {
      rotate.reset();
      void queries.invalidateQueries({ queryKey: autoDeployKey(appID) });
    },
  });

  const v = view.data;
  if (!v || !v.deployed.enabled) return null;
  const check = v.last_check;

  return (
    <Setting
      title="Webhook"
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
                {v.webhook_secret_set ? 'Replace webhook secret' : 'Generate webhook secret'}
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
      {check ? lastCheck(check) : 'Not checked yet.'} Pando checks every few minutes, or on each push
      with a webhook.
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

export function shortRef(ref?: string): string {
  return (ref ?? '').replace(/^refs\/(heads|tags)\//, '');
}

export function Failure({ error }: { error: unknown }) {
  const failed = error instanceof RequestFailed ? error : null;
  return (
    <Banner tone="failed">
      {failed?.message ?? 'Pando couldn’t save these settings. Try again.'}
      {failed?.remedy ? ` ${failed.remedy}` : ''}
    </Banner>
  );
}
