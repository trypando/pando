// The General page of an app's Settings: how Pando builds it, what happens to
// it when nobody uses it, the notices this person ignored, and — where they
// apply — what automatic deployments watch and the registry credential.
//
// The Build card is what was the Configuration tab after onboarding: what
// detection found, how it decided (evidence, an AI adapter's changes, the
// questions it asked and the runners-up, R-102), and a way to look again
// (R-022). A question is shown as Pando wrote it, with its copy button
// (R-105), wherever one is shown.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, CodeBlock, Dialog, Tag } from '@design';

import { api } from '@api/client';
import type { App, AppSpec, Notice, Proposal } from '@api/types.gen';
import { Disclosure } from '../ui/Disclosure';
import { InlineWarning } from '../ui/InlineWarning';
import { LineSkeleton, Loading } from '../ui/Loading';
import { relative } from '../ui/time';
import { invalidateApp } from './appList';
import { noticesKey } from './Attention';
import { BuildPlan } from './BuildPlan';
import { DeploySection } from './DeploySettings';
import {
  Blocked,
  DetectionFailed,
  Failure,
  QuestionCard,
  RunnersUp,
  fetchDetection,
  refetchWhileRunning,
  type DetectionResponse,
} from './DetectionReview';
import { primaryWorkload } from './onboarding';
import { startCommand, strategyLabel } from './discovery';
import { IdleSection } from './IdleSettings';
import { Screening } from './Screening';
import { CardHead, PageHead, Quiet } from './settingsParts';
import { RegistryCredential } from './RegistryCredential';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

export function SettingsGeneral({ app }: Readonly<{ app: App }>) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <PageHead title="General" />
      <BuildCard appID={app.id} />
      <Card tone="paper" padding="md">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
          <CardHead title="Inactivity" />
          <IdleSection app={app} />
        </div>
      </Card>
      <IgnoredNotices appID={app.id} />
      <AutoDeployCard app={app} />
      <SourceCredentialCard app={app} />
    </div>
  );
}

// --- build ------------------------------------------------------------------

function BuildCard({ appID }: Readonly<{ appID: string }>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const { spec, pending } = useNewestSpec(appID);
  const [viewing, setViewing] = useState(false);

  const detection = useQuery({
    queryKey: ['apps', appID, 'detection'],
    queryFn: () => fetchDetection(appID, queries.getQueryData<DetectionResponse>(['apps', appID, 'detection'])),
    refetchInterval: refetchWhileRunning,
  });

  // R-022: looking again is explicit, never automatic.
  const rerun = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/detection/rerun`),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] }),
  });

  const d = detection.data;
  const running = d?.status === 'running';
  const hasPlan = Object.keys(spec?.build?.generated_files ?? {}).length > 0;
  let line: string | undefined;
  if (running) line = 'Reading the repository.';
  else if (d?.updated_at) line = `Detected ${relative(d.updated_at).toLowerCase()}${d.commit ? ` from ${d.commit.slice(0, 7)}` : ''}`;

  return (
    <Card tone="paper" padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <CardHead
          title="Build"
          line={line}
          action={
            <>
              {hasPlan && (
                <Button variant="secondary" onClick={() => setViewing(true)}>
                  View build plan
                </Button>
              )}
              {canEdit && (
                <Button variant="secondary" disabled={running || rerun.isPending} onClick={() => rerun.mutate()}>
                  Run detection again
                </Button>
              )}
            </>
          }
        />

        {pending ? (
          <Loading>
            <LineSkeleton width="30ch" />
            <LineSkeleton width="40ch" />
          </Loading>
        ) : (
          spec && <BuildFacts spec={spec} proposal={d?.detection} />
        )}

        {rerun.isError && <Failure error={rerun.error} />}
        {detection.isError && <Failure error={detection.error} />}
        {d?.detection && !running && <Decided appID={appID} detection={d} spec={spec} />}
      </div>

      {viewing && (
        <Dialog
          open
          width={720}
          onClose={() => setViewing(false)}
          title="Build plan"
          footer={
            <Button variant="ghost" onClick={() => setViewing(false)}>
              Close
            </Button>
          }
        >
          <BuildPlan appID={appID} />
        </Dialog>
      )}
    </Card>
  );
}

/** What the app is built and run with, as label and value. */
function BuildFacts({ spec, proposal }: Readonly<{ spec: AppSpec; proposal?: Partial<Proposal> }>) {
  const [file, setFile] = useState<{ path: string; workload: string; content: string } | null>(null);
  const workloads = spec.workloads ?? [];
  const primary = workloads.find((w) => w.name === primaryWorkload(spec)) ?? workloads[0];
  const port = (primary?.ports ?? [])[0]?.number;
  const files = workloads.flatMap((w) => (w.files ?? []).map((f) => ({ path: f.path, workload: w.name, content: f.content })));
  const build = spec.build;

  const rows: Array<[string, React.ReactNode]> = [['Built with', strategyLabel(build?.strategy)]];
  if (build?.dockerfile) rows.push(['Dockerfile', <Mono key="d">{build.dockerfile}</Mono>]);
  if (build?.static_dir) rows.push(['Static directory', <Mono key="s">{build.static_dir}</Mono>]);
  if (workloads.length > 0) {
    rows.push([
      workloads.length === 1 ? 'Start command' : 'Start commands',
      <span key="c" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        {workloads.map((w) => {
          const run = startCommand(proposal ?? {}, spec, w);
          return (
            <span key={w.name} style={{ overflowWrap: 'anywhere' }}>
              {workloads.length > 1 && (
                <>
                  <Mono>{w.name}</Mono>
                  <span style={{ color: 'var(--ink-secondary)' }}> · </span>
                </>
              )}
              {run.text ? run.value : <Mono>{run.value}</Mono>}
            </span>
          );
        })}
      </span>,
    ]);
  }
  if (port) {
    rows.push([
      'Port',
      <span key="p">
        <Mono>{String(port)}</Mono>
        {workloads.length > 1 && primary && <span style={{ color: 'var(--ink-secondary)' }}> on {primary.name}</span>}
      </span>,
    ]);
  }
  if (files.length > 0) {
    rows.push([
      'Carried files',
      <span key="f" style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2) var(--space-3)' }}>
        {files.map((f) => (
          <button
            key={f.workload + f.path}
            type="button"
            className="pando-link"
            style={{ font: 'var(--type-code-sm)' }}
            onClick={() => setFile(f)}
          >
            {f.path}
          </button>
        ))}
      </span>,
    ]);
  }

  return (
    <>
      <dl
        style={{
          display: 'grid',
          gridTemplateColumns: 'minmax(7rem, 9rem) minmax(0, 1fr)',
          gap: 'var(--space-2) var(--space-4)',
          margin: 0,
          font: 'var(--type-body-ui)',
        }}
      >
        {rows.map(([label, value]) => (
          <div key={label} style={{ display: 'contents' }}>
            <dt style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', paddingTop: 'calc(var(--space-1) / 2)' }}>{label}</dt>
            <dd style={{ margin: 0, color: 'var(--ink)', minWidth: 0 }}>{value}</dd>
          </div>
        ))}
      </dl>
      {file && (
        <Dialog
          open
          onClose={() => setFile(null)}
          title={file.path}
          description={`Placed in ${file.workload} each time it starts.`}
          footer={
            <Button variant="ghost" onClick={() => setFile(null)}>
              Close
            </Button>
          }
        >
          <CodeBlock lines={file.content} />
        </Dialog>
      )}
    </>
  );
}

function Mono({ children }: Readonly<{ children: React.ReactNode }>) {
  return <span style={{ font: 'var(--type-code)' }}>{children}</span>;
}

/**
 * How Pando decided, a click away: the evidence, an AI adapter's changes, the
 * questions it asked, the warnings and what else bid. A detection that ran
 * after the configuration was last set is offered as a replacement, which
 * keeps the variables, dependencies and storage set here.
 */
function Decided({ appID, detection, spec }: Readonly<{ appID: string; detection: DetectionResponse; spec?: AppSpec }>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const [replacing, setReplacing] = useState(false);
  const { status, detection: proposal, answers } = detection;

  const answer = useMutation({
    mutationFn: (given: Record<string, string>) => api.post(`/apps/${appID}/detection/answers`, { answers: given }),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] }),
  });
  const accept = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/detection/accept`, { confirm: true }),
    onSuccess: () => invalidateApp(queries, appID),
  });
  const rerun = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/detection/rerun`),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] }),
  });

  if (status === 'failed') {
    return (
      <DetectionFailed error={proposal.error} onRetry={canEdit ? () => rerun.mutate() : undefined} retrying={rerun.isPending} />
    );
  }
  if (status === 'blocked') return <Blocked proposal={proposal} />;

  const asked = (proposal.questions ?? []).filter((q) => !q.deferred);
  const unanswered = asked.filter((q) => !(answers ?? {})[q.key]);
  const warnings = proposal.draft_spec?.warnings ?? [];
  // Newer than the configuration: something to apply, and questions to answer first.
  const newer = Boolean(detection.updated_at && spec?.created_at && detection.updated_at > spec.created_at);

  return (
    <>
      {newer && canEdit && (
        <div
          style={{
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
            flexWrap: 'wrap',
            gap: 'var(--space-3)',
            paddingTop: 'var(--space-3)',
            borderTop: 'var(--border-width) solid var(--rule)',
          }}
        >
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
            {unanswered.length > 0
              ? `A newer detection has ${unanswered.length === 1 ? 'a question' : `${unanswered.length} questions`} to answer.`
              : 'A newer detection isn’t applied yet.'}
          </span>
          <Button variant="secondary" disabled={unanswered.length > 0 || accept.isPending} onClick={() => setReplacing(true)}>
            Replace configuration
          </Button>
        </div>
      )}
      {accept.isError && <Failure error={accept.error} />}

      <Disclosure show="How Pando decided" hide="Hide how Pando decided" forceOpen={newer && unanswered.length > 0}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          {(proposal.winning_bid?.evidence ?? []).length > 0 && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
              <span>
                <Tag mono>{proposal.winning_bid.strategy}</Tag>
              </span>
              <ul style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', margin: 0, paddingLeft: 'var(--space-4)' }}>
                {(proposal.winning_bid.evidence ?? []).map((e) => (
                  <li key={e} style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
                    {e}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <Screening outcome={proposal.screening} />
          {asked.map((question) => (
            <QuestionCard
              key={question.key}
              question={question}
              answer={(answers ?? {})[question.key]}
              onAnswer={canEdit ? (value) => answer.mutate({ [question.key]: value }) : undefined}
              saving={answer.isPending}
            />
          ))}
          {answer.isError && <Failure error={answer.error} />}
          {warnings.map((w) => (
            <InlineWarning key={w.code + w.message} code={w.code}>
              {w.message}
            </InlineWarning>
          ))}
          <RunnersUp candidates={proposal.runners_up ?? []} />
        </div>
      </Disclosure>

      <Dialog
        open={replacing}
        title="Replace this app's configuration?"
        description="Replaces the build and workloads. Variables, dependencies and storage you set are kept."
        onClose={() => setReplacing(false)}
        footer={
          <>
            <Button variant="ghost" onClick={() => setReplacing(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              onClick={() => {
                accept.mutate();
                setReplacing(false);
              }}
            >
              Replace it
            </Button>
          </>
        }
      />
    </>
  );
}

// --- ignored notices --------------------------------------------------------

/**
 * The notices this person ignored on the Overview (R-412), each with Restore.
 * Ignoring is theirs alone, so the list is too.
 */
function IgnoredNotices({ appID }: Readonly<{ appID: string }>) {
  const queries = useQueryClient();
  const notices = useQuery({
    queryKey: noticesKey(appID),
    queryFn: () => api.get<{ notices: Notice[] | null }>(`/apps/${appID}/notices`),
  });
  const restore = useMutation({
    mutationFn: (key: string) => api.del(`/apps/${appID}/notices/${encodeURIComponent(key)}/ignore`),
    onSuccess: (_, key) =>
      queries.setQueryData<{ notices: Notice[] | null }>(noticesKey(appID), (old) =>
        old ? { notices: (old.notices ?? []).map((n) => (n.key === key ? { ...n, ignored: false } : n)) } : old,
      ),
  });
  const ignored = (notices.data?.notices ?? []).filter((n) => n.ignored);

  return (
    <Card tone="paper" padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <CardHead title="Ignored notices" line={ignored.length > 0 ? 'Hidden from the overview for you' : undefined} />
        {notices.isPending ? (
          <Loading>
            <LineSkeleton width="36ch" />
          </Loading>
        ) : ignored.length === 0 ? (
          <Quiet>Nothing ignored.</Quiet>
        ) : (
          <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column' }}>
            {ignored.map((n, i) => (
              <li
                key={n.key}
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                  flexWrap: 'wrap',
                  gap: 'var(--space-3)',
                  padding: 'var(--space-2) 0',
                  borderTop: i === 0 ? undefined : 'var(--border-width) solid var(--rule)',
                }}
              >
                <span style={{ flex: '1 1 20rem', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{n.message}</span>
                <Button
                  variant="ghost"
                  disabled={restore.isPending && restore.variables === n.key}
                  onClick={() => restore.mutate(n.key)}
                >
                  Restore
                </Button>
              </li>
            ))}
          </ul>
        )}
        {restore.isError && <Failure error={restore.error} />}
      </div>
    </Card>
  );
}

// --- where they apply -------------------------------------------------------

/** What automatic deployments watch, for an app deployed from a repository. */
function AutoDeployCard({ app }: Readonly<{ app: App }>) {
  const newest = useNewestSpec(app.id);
  if (newest.spec?.source?.type !== 'git') return null;
  return (
    <Card tone="paper" padding="md">
      <DeploySection app={app} />
    </Card>
  );
}

/** How Pando signs in to pull the image, for an app deployed from one. */
function SourceCredentialCard({ app }: Readonly<{ app: App }>) {
  if (app.source?.type !== 'image') return null;
  return (
    <Card tone="paper" padding="md">
      <RegistryCredential appID={app.id} />
    </Card>
  );
}
