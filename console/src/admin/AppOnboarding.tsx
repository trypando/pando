// Onboarding a new app: Pando works out how to run it, live, and the page then
// turns into the review of that plan (R-102, R-103, R-105; design 08 §1.3).
//
// One page in three phases, and the layout morphs between them rather than
// cutting. While detection runs ("discovering") a terrain profile draws across
// the top, the headline says what Pando is doing, and each step lands in a list
// with what it found. When it finishes the headline becomes "Plan ready" and
// the summit mark lands ("ready"); a moment later the column widens, the steps
// fold behind a toggle, and the review rises below with a sticky bar to reject,
// accept, or accept and deploy ("done"). Someone opening an app whose detection
// already finished lands on "done" with nothing animating.
//
// AI appears only where it did something (R-336): an AI step in the list when
// an adapter was called, questions it answered grouped under "Check what AI
// filled in" with its reason and a way back to the suggestion, and a star on
// whatever else it changed. Everything it changed is shown with its reason, and
// what Pando refused is shown too (R-331, R-334). A failed trial run's output
// stays on the page whatever a repair did (R-107).
//
// Reading is app.view. Answering and accepting are app.spec.edit; deploying is
// app.deploy; rejecting is app.delete; a secret value is app.secrets.write.
// Without a verb its control is absent or read-only (R-261).

import './onboarding.css';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Badge,
  Button,
  Card,
  Checkbox,
  CodeBlock,
  Dialog,
  Icon,
  IconButton,
  Input,
  Radio,
  Skeleton,
  SkeletonText,
  StatusIndicator,
  StatusSymbol,
  Tag,
  Tooltip,
} from '@design';
import { AiStar } from '../ui/AiStar';
import { AiGlyph, AiThinking, useReducedMotion } from '../ui/AiThinking';

import { api, RequestFailed } from '@api/client';
import type { Amendment, AppSpec, Question, Report, Source, Turn } from '@api/types.gen';
import { Loading } from '../ui/Loading';
import { ScoreBadge } from '../ui/ScoreBadge';
import { Security } from './Security';
import { deletePath } from './delete-app';
import { invalidateAppLists } from './appList';
import {
  Blocked,
  DetectionFailed,
  Failure,
  fetchDetection,
  refetchWhileRunning,
  type DetectionResponse,
} from './DetectionReview';
import {
  COMPOSE_REWRITTEN,
  aiFrom,
  answerFor,
  composeNote,
  answerState,
  askedQuestions,
  currentStep,
  discoverySteps,
  dwellFor,
  finishedCount,
  groupOf,
  isService,
  listNames,
  paced,
  planSpec,
  progress,
  questionName,
  serviceImage,
  serviceProvision,
  slotLabel,
  startCommand,
  stillNeeded,
  stops,
  strategyLabel,
  type DiscoveryStep,
  type Finding,
} from './discovery';
import {
  SCREENING_ADVISORY,
  envKey,
  mergeRows,
  neededValues,
  absoluteAddress,
  randomSecret,
  primaryWorkload,
  rowProblems,
  suggestions,
  valuesPayload,
  variableRows,
  type RowEdit,
  type Suggestions,
  type VariableRow,
} from './onboarding';
import { PHRASE_MS, phrasesFor } from './onboardingPhrases';
import { describeAmendment } from './screeningText';
import { Terrain } from './Terrain';
import { AppVerb, can, type AppWithVerbs } from './verbs';

export interface DeployRefusal {
  message: string;
  remedy?: string;
  code?: string;
}

type Phase = 'discovering' | 'ready' | 'done';

/** How long "Plan ready" holds before the review rises. */
const READY_HOLD_MS = 1_600;

/** A detection that finished this recently is still played through. */
const RECENT_MS = 60_000;

export function AppOnboarding({
  app,
  onBack,
  onDeployRefused,
}: {
  app: AppWithVerbs;
  /** Back to the list: the back button, and where a rejected app leaves to. */
  onBack: () => void;
  /**
   * A deploy refused after the accept went through. The app is configured by
   * then and this page gives way to the ordinary app screen, so the refusal is
   * handed to that screen to show under its header, with the way to the fix.
   */
  onDeployRefused: (refusal: DeployRefusal | null) => void;
}) {
  const appID = app.id;
  const queries = useQueryClient();
  const verbs = app.verbs ?? [];
  const canEdit = can(verbs, AppVerb.SpecEdit);
  const canDeploy = can(verbs, AppVerb.Deploy);
  const canDelete = can(verbs, AppVerb.Delete);
  const canSecrets = can(verbs, AppVerb.SecretsWrite);
  const reduced = useReducedMotion();

  // The same key DetectionReview uses, so the two never disagree about one app.
  const detection = useQuery({
    queryKey: ['apps', appID, 'detection'],
    queryFn: () =>
      fetchDetection(appID, queries.getQueryData<DetectionResponse>(['apps', appID, 'detection'])),
    // Waited on while running, a stage at a time: the page builds from
    // whatever has been found so far.
    refetchInterval: refetchWhileRunning,
  });

  // The variables form. Edits are held by variable, not as a copy of the rows,
  // so a poll that brings in more of the proposal never drops what somebody
  // typed (onboarding.ts mergeRows).
  const [edits, setEdits] = useState<Record<string, RowEdit>>({});
  const [added, setAdded] = useState<VariableRow[]>([]);
  const nextRow = useRef(0);

  // Slots the person said the app runs without, by slot key. Sent with the
  // accept (spec.MarkSlotOptional); asked about first, because if they are
  // wrong the deploy breaks.
  const [optional, setOptional] = useState<Set<string>>(() => new Set());
  const [confirmingOptional, setConfirmingOptional] = useState<VariableRow | null>(null);
  const setOptionalKey = (key: string, on: boolean) =>
    setOptional((all) => {
      const next = new Set(all);
      if (on) next.add(key);
      else next.delete(key);
      return next;
    });

  // The hostname the person chose for the app, where its routing serves one
  // (subdomain mode). Empty keeps the one detection assigned; sent with the
  // accept (spec.SetHostname).
  const [hostname, setHostname] = useState('');

  // Answers typed but not yet saved, by question key.
  const [drafts, setDrafts] = useState<Record<string, string>>({});

  const answer = useMutation({
    mutationFn: (answers: Record<string, string>) => api.post(`/apps/${appID}/detection/answers`, { answers }),
    onSuccess: (_, answers) => {
      setDrafts((all) => {
        const next = { ...all };
        for (const key of Object.keys(answers)) delete next[key];
        return next;
      });
      return queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] });
    },
  });

  // R-022: looking again is explicit. Offered on a failure, which is when
  // somebody has just fixed what caused it.
  const rerun = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/detection/rerun`),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] }),
  });

  const [rejecting, setRejecting] = useState(false);
  const [viewingScan, setViewingScan] = useState(false);
  const reject = useMutation({
    mutationFn: async () => {
      try {
        await api.del(deletePath(appID, 'none'));
      } catch (error) {
        // A just-added app has deployed nothing, so no storage of it holds any
        // data. If the server still asks for R-204's backup decision — storage
        // was declared, so it cannot tell — the answer is to discard, because
        // there is nothing to back up. Asked only when the server requires it,
        // so an app with no storage is not recorded as a forced delete.
        if (error instanceof RequestFailed && error.code === 'STATE_BACKUP_DECISION_REQUIRED') {
          await api.del(deletePath(appID, 'discard'));
          return;
        }
        throw error;
      }
    },
    onSuccess: () => {
      // As DeleteApp does: the lists only, then remove, so they refetch and the
      // deleted app's own record is not asked for on the way out.
      void invalidateAppLists(queries);
      queries.removeQueries({ queryKey: ['apps', appID] });
      onBack();
    },
  });

  const data = detection.data;
  // While fetching, the body may carry nothing but the stage — or, from a
  // server that has not written one yet, nothing at all. Both are an empty
  // proposal still being found, not a failure.
  const proposal = data ? (data.detection ?? ({} as DetectionResponse['detection'])) : undefined;
  const status = data?.status ?? 'running';
  const running = status === 'running';
  const failed = status === 'failed';
  const blocked = status === 'blocked';
  const saved = data?.answers ?? null;

  // --- Phase ---------------------------------------------------------------
  // "Ready" is held only when this page watched detection finish. Opening an
  // app that finished earlier goes straight to the review, and so does one
  // under reduced motion.
  //
  // "Watching" also covers a detection that finished moments before the page
  // first heard about it. A small repository is read faster than the page's
  // first request comes back, and that app arrived as a finished review with
  // nothing shown of how Pando got there. The first answer decides: running,
  // or finished within the last minute, and the steps play.
  const [watching, setWatching] = useState(false);
  const decided = useRef(false);
  useEffect(() => {
    if (!data || reduced) return;
    if (running) {
      decided.current = true;
      setWatching(true);
      return;
    }
    if (decided.current) return;
    decided.current = true;
    const finishedAt = Date.parse(data.updated_at ?? '');
    if (!Number.isNaN(finishedAt) && Date.now() - finishedAt < RECENT_MS) setWatching(true);
  }, [data, running, reduced]);

  // --- Steps -----------------------------------------------------------------
  // The source scan runs during detection, so its report is there once the
  // scan stage has passed. An install with no scanner answers with an error,
  // which means no scan step and no badge — not a failure worth showing.
  const scanned = !running || proposal?.stage === 'screening';
  const security = useQuery({
    queryKey: ['apps', appID, 'security'],
    queryFn: () => api.get<Report>(`/apps/${appID}/security`),
    enabled: Boolean(data) && scanned,
    retry: false,
  });
  const report = security.data?.scan ? security.data : undefined;

  const real = useMemo(
    () =>
      proposal
        ? discoverySteps({
            status,
            stage: proposal.stage,
            proposal,
            source: app.source,
            commit: data?.commit,
            security: report,
          })
        : [],
    [proposal, status, app.source, data?.commit, report],
  );

  // Paced while watching: one step at a time, each held long enough to read
  // even when the server finished it in a blink (discovery.ts paced). Never
  // ahead of the server — only a step it has finished is revealed.
  const [revealed, setRevealed] = useState(0);
  const finished = finishedCount(real);
  const pacing = watching && revealed < real.length;
  const steps = watching ? paced(real, Math.min(revealed, finished)) : real;
  const current = currentStep(steps);

  // When the shown step became the current one, for its dwell and for easing
  // the terrain forward.
  const since = useRef<{ id: string; at: number }>({ id: '', at: Date.now() });
  if (current && since.current.id !== current.id) since.current = { id: current.id, at: Date.now() };

  useEffect(() => {
    if (!watching || revealed >= finished) return;
    const step = real[revealed];
    if (!step) return;
    const wait = Math.max(0, dwellFor(step) - (Date.now() - since.current.at));
    const timer = window.setTimeout(() => setRevealed((n) => n + 1), wait);
    return () => window.clearTimeout(timer);
  }, [watching, revealed, finished, real]);

  // "Plan ready" is held once the last step has been shown, then the review.
  const discovering = !data || running || pacing;
  const [holding, setHolding] = useState(false);
  const held = useRef(false);
  useEffect(() => {
    if (!watching || discovering || held.current || failed || blocked) return;
    held.current = true;
    setHolding(true);
    const timer = window.setTimeout(() => setHolding(false), READY_HOLD_MS);
    return () => window.clearTimeout(timer);
  }, [watching, discovering, failed, blocked]);
  const phase: Phase = discovering ? 'discovering' : holding ? 'ready' : 'done';

  const fraction = useCallback(() => {
    if (!discovering) return failed ? progress(steps, 0) : 1;
    // A step the server already finished eases across its dwell; one still
    // running eases by how long that stage usually takes.
    const paceStep = current && real.find((s) => s.id === current.id);
    const expected = paceStep?.state === 'done' ? dwellFor(paceStep) / 2 : undefined;
    return progress(steps, Date.now() - since.current.at, expected);
  }, [discovering, failed, steps, current, real]);

  const edit = (row: VariableRow, patch: RowEdit & { key?: string }) => {
    if (row.original === undefined) {
      setAdded((list) => list.map((r) => (r.id === row.id ? { ...r, ...patch } : r)));
    } else {
      setEdits((all) => ({ ...all, [row.id]: { ...all[row.id], ...patch } }));
    }
  };

  const spec = proposal?.draft_spec as AppSpec | undefined;
  const asked = proposal ? askedQuestions(proposal) : [];
  const missing = stillNeeded(asked, saved, drafts);

  // The spec accepting would pin: the reading the build-method answer picks.
  // Variables, tallies and the plan all describe this one. The variables form
  // was built from the winner's draft while the plan showed the adopted
  // reading, so a value typed for the winner's workload was sent with a name
  // the accepted spec did not have, and dropped.
  const effective: Record<string, string> = {};
  for (const q of asked) effective[q.key] = answerFor(q, saved, drafts[q.key]);
  const plan = (proposal && planSpec(proposal, effective)) ?? spec;

  // Where the app will be served: the hostname the person chose, in subdomain
  // mode, else the address the server worked out from the plan's routing.
  const address =
    hostname.trim() && plan?.routing?.mode === 'subdomain'
      ? `https://${hostname.trim()}`
      : absoluteAddress(data?.address, typeof window === 'undefined' ? 'http:' : window.location.protocol);
  const detected = variableRows(plan, address).map((row) => (canSecrets ? row : { ...row, secret: false }));
  const rows = mergeRows(detected, edits, added);
  const problems = rowProblems(rows);

  // Answers typed and not yet saved go with the accept, so nobody has to know
  // that a text answer saves when the field loses focus.
  const unsaved = () => {
    const out: Record<string, string> = {};
    for (const q of asked) {
      const typed = drafts[q.key];
      if (typed !== undefined && typed.trim() && typed !== answerFor(q, saved)) out[q.key] = typed.trim();
    }
    return out;
  };

  const accept = useMutation({
    mutationFn: async (andDeploy: boolean) => {
      const pending = unsaved();
      if (Object.keys(pending).length > 0) {
        await api.post(`/apps/${appID}/detection/answers`, { answers: pending });
      }
      await api.post(`/apps/${appID}/detection/accept`, {
        values: valuesPayload(rows),
        optional: [...optional],
        hostname: hostname.trim() || undefined,
      });
      if (!andDeploy) return;
      // Accepting pins a spec and does not deploy it (Sequence A). The deploy is
      // its own request, made only once the accept has gone through, so a
      // refused deploy leaves a configured app rather than nothing.
      try {
        await api.post(`/apps/${appID}/deployments`, {});
        onDeployRefused(null);
      } catch (error) {
        const refused = error instanceof RequestFailed ? error : null;
        onDeployRefused({
          message:
            refused?.message ??
            'Pando saved the configuration but couldn’t start a deploy. Deploy it from the app’s overview.',
          remedy: refused?.remedy,
          code: refused?.code,
        });
      }
    },
    // ['apps']: the app leaves draft, which changes both this screen — it gives
    // way to the ordinary one, with tabs — and the app's row in the list.
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps'] }),
  });

  const rejectDialog = (
    <Dialog
      open={rejecting}
      title={`Reject ${app.name}?`}
      description="Pando removes the app and everything it found. Nothing was deployed."
      onClose={() => setRejecting(false)}
      footer={
        <>
          <Button variant="ghost" onClick={() => setRejecting(false)}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={reject.isPending} onClick={() => reject.mutate()}>
            {reject.isPending ? 'Rejecting' : 'Reject plan'}
          </Button>
        </>
      }
    >
      {reject.isError && <Failure error={reject.error} />}
    </Dialog>
  );

  // The app's name, above its source. No back button: the sidebar is the way
  // back to the list.
  const back = <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>{app.name}</span>;

  // Reject is the destructive choice in the bar, so it reads in marker red
  // while staying a quiet ghost beside the two ways forward.
  const rejectButton = (
    <Button variant="ghost" style={{ color: 'var(--marker-deep)' }} onClick={() => setRejecting(true)}>
      Reject plan
    </Button>
  );

  // Before the first answer: the terrain's place and the column's shape.
  if (detection.isPending) {
    return (
      <div>
        <div className="pando-terrain" aria-hidden="true" />
        <div className="pando-onboard-column">
          {back}
          <Loading gap="var(--space-5)">
            <Skeleton width="16rem" height="2rem" radius="sm" />
            <SkeletonText lines={4} />
          </Loading>
        </div>
      </div>
    );
  }

  if (detection.isError || !data || !proposal) {
    return (
      <div>
        <div className="pando-onboard-column">
          {back}
          <Failure error={detection.error} />
        </div>
        {canDelete && (
          <ActionBar status="failed" label="Pando couldn’t load this app" detail="Reject it, or reload the page to try again.">
            {rejectButton}
          </ActionBar>
        )}
        {rejectDialog}
      </div>
    );
  }

  const outcome = proposal.screening;
  const marks = suggestions(outcome, spec);
  const done = phase === 'done';
  const aiSteps = steps.filter((s) => s.ai).length;
  const seed = `${app.source?.url ?? app.source?.image ?? app.id}@${data.commit || proposal.commit || ''}`;

  const headline = failed
    ? 'Pando stopped'
    : blocked
      ? 'This can’t run as written'
      : phase === 'discovering'
        ? (current?.active ?? 'Reading the repo')
        : 'Plan ready';

  const shownDone = (id: string) => steps.some((s) => s.id === id && s.state === 'done');

  return (
    <div>
      <Terrain
        seed={seed}
        fraction={fraction}
        aiFrom={aiFrom(steps)}
        stops={stops(steps)}
        summit={!discovering && !failed && !blocked}
        moving={discovering}
      />

      <div className="pando-onboard-column" data-phase={done ? 'done' : 'working'}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', minWidth: 0 }}>
          {back}
          <SourceTags source={app.source} commit={data.commit || proposal.commit} />
          <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3) var(--space-5)' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', flex: '1 1 auto', minWidth: 0 }}>
              {discovering && current?.ai && <AiStar size={18} />}
              <h2 className="pando-onboard-headline" data-big={!discovering && !failed && !blocked}>
                {headline}
              </h2>
            </div>
            {!discovering && report && <SecurityScore report={report} onView={() => setViewingScan(true)} />}
          </div>
          {discovering && <WorkingLine step={current} startedAt={data.started_at} reduced={reduced} />}
          {!discovering && !failed && !blocked && (
            <p
              className="pando-onboard-enter"
              style={{ margin: 0, font: 'var(--type-body)', color: 'var(--ink-secondary)', maxWidth: '60ch' }}
            >
              {subline(outcome, asked, missing.length, marks)}
            </p>
          )}
        </div>

        <Segments steps={steps} hidden={done} failedFixed={Boolean(outcome?.ran && outcome.function === 'repair_plan')} />

        <Tallies
          spec={done ? plan : spec}
          discovering={discovering}
          steps={steps}
          asked={asked.length}
          byAI={asked.filter((q) => q.suggested).length}
          runs={shownDone('runs')}
          vars={shownDone('vars')}
          questions={!discovering}
        />

        <section style={{ display: 'flex', flexDirection: 'column' }}>
          {done ? (
            <StepLog steps={steps} aiSteps={aiSteps} />
          ) : (
            <StepList steps={steps.filter((s) => s.state !== 'pending')} />
          )}
        </section>

        {failed && (
          <div className="pando-onboard-enter">
            <DetectionFailed
              error={proposal.error}
              onRetry={canEdit ? () => rerun.mutate() : undefined}
              retrying={rerun.isPending}
            />
          </div>
        )}
        {blocked && (
          <div className="pando-onboard-enter">
            <Blocked proposal={proposal} />
          </div>
        )}

        {done && !failed && !blocked && (
          <div
            className="pando-onboard-enter"
            style={{ '--stagger': 1, display: 'flex', flexDirection: 'column', gap: 'var(--space-7)' } as React.CSSProperties}
          >

            {(['needs', 'ai'] as const).map((group) => {
              const items = asked.filter((q) => groupOf(q) === group);
              if (items.length === 0) return null;
              return (
                <section key={group} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
                  <GroupHeading
                    title={group === 'ai' ? 'Check what AI filled in' : 'Needs your answer'}
                    count={items.length}
                    ai={group === 'ai'}
                  >
                    {group === 'ai'
                      ? 'AI answered these from the code. Change anything that looks wrong.'
                      : outcome?.ran
                        ? 'Pando and AI couldn’t work these out from the code. Copy a question into the tool that wrote this app and paste its answer back.'
                        : 'Pando couldn’t work these out from the code. Copy a question into the tool that wrote this app and paste its answer back.'}
                  </GroupHeading>
                  {items.map((q) => (
                    <QuestionCard
                      key={q.key}
                      question={q}
                      value={answerFor(q, saved, drafts[q.key])}
                      saving={answer.isPending && Boolean(answer.variables?.[q.key] !== undefined)}
                      error={answer.isError && answer.variables?.[q.key] !== undefined ? answer.error : undefined}
                      onType={canEdit ? (value) => setDrafts((all) => ({ ...all, [q.key]: value })) : undefined}
                      onSave={
                        canEdit
                          ? (value) => {
                              if (value.trim() && value !== ((saved ?? {})[q.key] ?? q.suggested?.value)) {
                                answer.mutate({ [q.key]: value.trim() });
                              }
                            }
                          : undefined
                      }
                    />
                  ))}
                </section>
              );
            })}

            {plan && (
              <Variables
                rows={rows}
                spec={plan}
                problems={problems}
                marks={marks}
                canEdit={canEdit}
                canSecrets={canSecrets}
                onEdit={edit}
                onAdd={() => {
                  nextRow.current += 1;
                  setAdded((list) => [
                    ...list,
                    { id: `added-${nextRow.current}`, workload: '', key: '', value: '', secret: false },
                  ]);
                }}
                onRemove={(row) => setAdded((list) => list.filter((r) => r.id !== row.id))}
                optional={optional}
                onOptional={setOptionalKey}
                onAskOptional={(row) => setConfirmingOptional(row)}
              />
            )}

            {/* After the questions and variables, which are what somebody acts
                on first, and just before AI's notes, which say the same kind of
                thing. The plan's warnings, not the winner's: a warning about the
                reading nobody picked describes an app that won't be deployed. */}
            <Notices proposal={proposal} spec={plan} marks={marks} />

            {outcome?.ran && <AiNotes notes={outcome.notes ?? []} />}

            {plan && (
              <ThePlan
                proposal={proposal}
                spec={plan}
                marks={marks}
                answers={effective}
                source={app.source}
                commit={data.commit}
                address={address}
                hostname={hostname}
                onHostname={canEdit ? setHostname : undefined}
              />
            )}

            {aiAvailable(outcome) && (
              <AskAI appID={appID} conversation={proposal.conversation ?? []} canAsk={canEdit} />
            )}
          </div>
        )}
      </div>

      {done && !failed && !blocked && (canEdit || canDelete) && (
        <ActionBar
          {...barStatus(missing, Object.keys(problems).length, neededValues(rows, optional), canDeploy, canEdit)}
          error={accept.isError ? accept.error : undefined}
        >
          {canDelete && rejectButton}
          {canEdit && (
            <Button
              variant={canDeploy ? 'secondary' : 'primary'}
              disabled={missing.length > 0 || Object.keys(problems).length > 0 || accept.isPending}
              onClick={() => accept.mutate(false)}
            >
              {accept.isPending && accept.variables === false ? 'Accepting' : 'Accept plan'}
            </Button>
          )}
          {canEdit && canDeploy && (
            <Button
              variant="primary"
              // A required value left empty refuses the deploy (R-132), so
              // deploying waits for it; accepting does not.
              disabled={
                missing.length > 0 ||
                Object.keys(problems).length > 0 ||
                neededValues(rows, optional).length > 0 ||
                accept.isPending
              }
              onClick={() => accept.mutate(true)}
            >
              {accept.isPending && accept.variables === true ? 'Accepting' : 'Accept and deploy'}
            </Button>
          )}
        </ActionBar>
      )}

      {(failed || blocked) && canDelete && (
        <ActionBar
          status="failed"
          label={failed ? 'Detection stopped' : 'Pando can’t run this app'}
          detail={failed ? 'Fix what stopped it and try again, or reject the app.' : 'Change what’s described above, or reject the app.'}
        >
          {rejectButton}
        </ActionBar>
      )}

      {rejectDialog}

      {/* Marking a required value optional is the person's call — detection
          can be wrong — but a wrong call breaks the deploy, so it is asked. */}
      <Dialog
        open={confirmingOptional !== null}
        title={`Mark ${confirmingOptional?.key ?? 'this'} as not required?`}
        description={`Pando found ${confirmingOptional?.key ?? 'this variable'} as something the app needs. If the app does need it, deploying without a value will fail, or the app will start and not work. Mark it not required only if you know the app runs without it.`}
        onClose={() => setConfirmingOptional(null)}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmingOptional(null)}>
              Keep it required
            </Button>
            <Button
              variant="destructive"
              onClick={() => {
                if (confirmingOptional?.slot) setOptionalKey(confirmingOptional.slot.key, true);
                setConfirmingOptional(null);
              }}
            >
              Mark not required
            </Button>
          </>
        }
      />

      {/* The findings, on this page: a draft app has no overview tab to send
          anybody to. The same panel the overview shows, so they read alike. */}
      <Dialog
        open={viewingScan}
        title="Security scan"
        width={880}
        onClose={() => setViewingScan(false)}
        footer={
          <Button variant="secondary" onClick={() => setViewingScan(false)}>
            Close
          </Button>
        }
      >
        {viewingScan && <Security appID={appID} everything />}
      </Dialog>
    </div>
  );
}

/**
 * The scan's score beside "Plan ready": the badge, and a way to the findings
 * when there are any. The number is the first signal and the color the second
 * (R-320), as everywhere else the score appears.
 */
function SecurityScore({ report, onView }: { report: Report; onView: () => void }) {
  const counts = report.counts;
  const total = counts.critical + counts.high + counts.medium + counts.low + counts.unknown;
  return (
    <div
      className="pando-onboard-enter"
      style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 'var(--space-2)', marginLeft: 'auto' }}
    >
      <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)' }}>Security score</span>
      <ScoreBadge
        score={report.scan?.score}
        verdict={report.standing.verdict as never}
        threshold={report.standing.threshold}
        full
        size={28}
      />
      {total > 0 ? (
        <button type="button" className="pando-link" style={{ font: 'var(--type-body-ui)' }} onClick={onView}>
          {total === 1 ? 'View 1 vulnerability' : `View ${total} vulnerabilities`}
        </button>
      ) : (
        <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>No known vulnerabilities</span>
      )}
    </div>
  );
}

// --- Header ------------------------------------------------------------------

function SourceTags({ source, commit }: { source: Source | undefined; commit?: string }) {
  if (!source) return null;
  const short = commit ? commit.slice(0, 7) : undefined;
  const repo = source.url ? repoName(source.url) : undefined;
  const href = source.url ? repoPage(source.url) : undefined;
  const repoTag = repo && (
    <Tag mono icon={<Icon name="git-branch" size={12} />} title={source.url}>
      {repo}
    </Tag>
  );
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2)' }}>
      {source.type === 'image' && source.image && <Tag mono>{source.image}</Tag>}
      {source.type === 'upload' && <Tag>An uploaded archive</Tag>}
      {repoTag &&
        (href ? (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            title={`Open ${repo} in a new tab`}
            style={{ display: 'inline-flex', lineHeight: 0, textDecoration: 'none', color: 'inherit' }}
          >
            {repoTag}
          </a>
        ) : (
          repoTag
        ))}
      {source.ref && <Tag mono>{source.ref}</Tag>}
      {source.subdir && <Tag mono>{source.subdir}</Tag>}
      {short && <Tag mono>{short}</Tag>}
    </div>
  );
}

/**
 * The repository's page, for a link: an https clone URL without its `.git`,
 * and an SSH one (`git@host:owner/repo`) turned into the host's https address.
 * Anything else — a local path, a scheme a browser won't open — has no link.
 */
export function repoPage(url: string): string | undefined {
  const trimmed = url.trim().replace(/\.git$/, '').replace(/\/+$/, '');
  if (/^https?:\/\//i.test(trimmed)) return trimmed;
  const ssh = /^(?:ssh:\/\/)?git@([^:/]+)[:/](.+)$/.exec(trimmed);
  if (ssh) return `https://${ssh[1]}/${ssh[2]}`;
  return undefined;
}

/** `owner/repo` from a clone URL, which is how people name a repository. */
function repoName(url: string): string {
  const cleaned = url.replace(/\.git$/, '').replace(/\/+$/, '');
  const parts = cleaned.split(/[/:]/).filter(Boolean);
  return parts.length >= 2 ? `${parts[parts.length - 2]}/${parts[parts.length - 1]}` : cleaned;
}

/**
 * What Pando is doing, live: a ripple, a phrase that turns over, and how long
 * it has been at it. Water blue while AI works. Announced politely; under
 * reduced motion the phrase holds still.
 */
function WorkingLine({
  step,
  startedAt,
  reduced,
}: {
  step: DiscoveryStep | undefined;
  startedAt?: string;
  reduced: boolean;
}) {
  const [now, setNow] = useState(() => Date.now());
  const [tick, setTick] = useState(0);
  const id = step?.ai ? (step.name.startsWith('Review') ? 'repair' : 'answer') : step?.id;
  const phrases = phrasesFor(id);

  useEffect(() => {
    const clock = window.setInterval(() => setNow(Date.now()), 1_000);
    if (reduced) return () => window.clearInterval(clock);
    const phrase = window.setInterval(() => setTick((n) => n + 1), PHRASE_MS);
    return () => {
      window.clearInterval(clock);
      window.clearInterval(phrase);
    };
  }, [reduced]);

  // Each step starts from its first, literal phrase.
  useEffect(() => setTick(0), [id]);

  const began = startedAt ? Date.parse(startedAt) : NaN;
  const seconds = Number.isNaN(began) ? undefined : Math.max(0, Math.floor((now - began) / 1_000));
  const phrase = phrases[(reduced ? 0 : tick) % phrases.length];
  const color = step?.ai ? 'var(--water)' : 'var(--ink-secondary)';

  return (
    <div role="status" aria-live="polite" style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
      {step?.ai ? (
        // AI at work looks the same wherever it is.
        <AiGlyph />
      ) : (
        <span className="pando-ripple" aria-hidden="true">
          <span />
          <span />
          <span />
          <i />
        </span>
      )}
      <span key={phrase} className="pando-onboard-enter" style={{ font: 'var(--type-body-ui)', color }}>
        {phrase}
        <span className="pando-dots" aria-hidden="true">
          <span>.</span>
          <span>.</span>
          <span>.</span>
        </span>
      </span>
      {seconds !== undefined && (
        <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink-muted)' }}>{seconds}s</span>
      )}
    </div>
  );
}

function subline(
  outcome: DetectionResponse['detection']['screening'],
  asked: Question[],
  missing: number,
  marks: Suggestions,
): string {
  const parts: string[] = [];
  if (outcome?.ran) {
    const answered = asked.filter((q) => q.suggested).length;
    const changed = marks.count - Object.keys(outcome.answers ?? {}).length;
    if (outcome.function === 'repair_plan') {
      parts.push(
        changed > 0
          ? `AI changed ${changed === 1 ? 'one thing' : `${changed} things`} after the plan failed.`
          : 'AI looked at the failed plan and found nothing it could change.',
      );
    } else if (answered > 0) {
      parts.push(`AI answered ${answered === 1 ? 'one question' : `${answered} questions`}.`);
    }
  }
  parts.push(
    missing > 0
      ? `Pando needs ${missing === 1 ? 'one answer' : `${missing} answers`} from you before it can deploy.`
      : 'Everything’s answered. Nothing runs until you accept.',
  );
  return parts.join(' ');
}

/** One segment per step: ink when done, water for AI, the rule for what's left. */
function Segments({ steps, hidden, failedFixed }: { steps: DiscoveryStep[]; hidden: boolean; failedFixed: boolean }) {
  return (
    <div className="pando-segments" data-hidden={hidden} aria-hidden="true">
      {steps.map((s) => (
        <span
          key={s.id}
          style={{
            background:
              s.state === 'done'
                ? s.failed
                  ? failedFixed
                    ? 'var(--contour-text)'
                    : 'var(--status-failed)'
                  : s.ai
                    ? 'var(--water)'
                    : 'var(--ink)'
                : s.state === 'current'
                  ? 'var(--rule-strong)'
                  : 'var(--rule)',
          }}
        />
      ))}
    </div>
  );
}

/**
 * Four counts between rules. Each fills in when the step that found it is
 * shown, so they climb with the list rather than arriving before it.
 */
function Tallies({
  spec,
  discovering,
  steps,
  asked,
  byAI,
  runs,
  vars,
  questions,
}: {
  spec: AppSpec | undefined;
  discovering: boolean;
  steps: DiscoveryStep[];
  /** Every question detection asked, answered or not. */
  asked: number;
  /** How many of them an AI adapter answered. */
  byAI: number;
  runs: boolean;
  vars: boolean;
  questions: boolean;
}) {
  // Everything that runs: the app's own services and the ones Pando runs
  // beside it. A compose app with app, proxy and a Postgres database is three,
  // whichever side of the plan each sits on.
  const services = runs
    ? (spec?.workloads ?? []).length + (spec?.slots ?? []).filter(isService).length
    : 0;
  const storage = runs ? new Set((spec?.workloads ?? []).flatMap((w) => (w.mounts ?? []).map((m) => m.path))).size : 0;
  const variables = vars
    ? new Set((spec?.workloads ?? []).flatMap((w) => (w.env ?? []).map((e) => e.key))).size
    : 0;
  const trial = steps.find((s) => s.id === 'trial');
  const broken = discovering && trial?.state === 'done' && trial.failed;
  const count = questions ? asked : 0;
  const tallies: Array<{ label: string; value: number; hot?: boolean }> = [
    { label: services === 1 ? 'Service' : 'Services', value: services },
    { label: variables === 1 ? 'Variable' : 'Variables', value: variables },
    { label: 'Storage', value: storage },
    broken
      ? { label: 'Run failed', value: 1, hot: true }
      : {
          label: `${count === 1 ? 'Question' : 'Questions'}${questions && byAI ? `, ${byAI} answered by AI` : ''}`,
          value: count,
        },
  ];
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'repeat(4, minmax(0, 1fr))',
        borderTop: 'var(--border-width) solid var(--rule)',
        borderBottom: 'var(--border-width) solid var(--rule)',
      }}
    >
      {tallies.map((t) => (
        <div
          key={t.label}
          style={{ display: 'flex', flexDirection: 'column', gap: 'calc(var(--space-1) / 2)', padding: 'var(--space-3) var(--space-3) var(--space-3) 0' }}
        >
          <span
            style={{
              font: 'var(--type-h3)',
              fontVariantNumeric: 'tabular-nums',
              color: t.hot ? 'var(--status-failed)' : t.value ? 'var(--ink)' : 'var(--ink-muted)',
            }}
          >
            {t.value}
          </span>
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{t.label}</span>
        </div>
      ))}
    </div>
  );
}

// --- Steps -------------------------------------------------------------------

/** Newest on top while Pando works: the step under way is where the eye is. */
function StepList({ steps }: { steps: DiscoveryStep[] }) {
  return (
    <ol style={{ display: 'flex', flexDirection: 'column-reverse', margin: 0, padding: 0, listStyle: 'none' }}>
      {steps.map((s) => (
        <StepRow key={s.id} step={s} />
      ))}
    </ol>
  );
}

function StepLog({ steps, aiSteps }: { steps: DiscoveryStep[]; aiSteps: number }) {
  const [open, setOpen] = useState(false);
  const summary = `${steps.length} steps${aiSteps ? `, ${aiSteps} by AI` : ''}`;
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 'var(--space-3)',
          border: 'none',
          background: 'transparent',
          padding: '0 0 var(--space-2)',
          cursor: 'pointer',
          textAlign: 'left',
          color: 'var(--ink)',
        }}
      >
        <span style={{ font: 'var(--type-label)' }}>How Pando got here</span>
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{summary}</span>
        <span
          style={{ marginLeft: 'auto', display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)', color: 'var(--ink-secondary)' }}
        >
          {open ? 'Hide' : 'Show'}
          <Icon name={open ? 'chevron-up' : 'chevron-down'} size={16} />
        </span>
      </button>
      {open && <StepList steps={steps} />}
    </>
  );
}

function StepRow({ step }: { step: DiscoveryStep }) {
  const reached = step.state !== 'pending';
  const nameColor = step.ai ? 'var(--water)' : reached ? 'var(--ink)' : 'var(--ink-muted)';
  return (
    <li className="pando-step pando-onboard-enter" data-ai={Boolean(step.ai)} aria-current={step.state === 'current' ? 'step' : undefined}>
      <div className="pando-step-head">
        <span style={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          {step.ai ? (
            <AiStar size={16} />
          ) : (
            <StatusSymbol status={step.state === 'current' ? 'building' : step.failed ? 'failed' : 'running'} size={8} />
          )}
        </span>
        <span
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 'var(--space-2)',
            minWidth: 0,
            font: step.state === 'current' ? 'var(--type-label)' : 'var(--type-body-ui)',
            color: nameColor,
          }}
        >
          {step.name}
          {step.ai && <span className="pando-ai-label">AI</span>}
        </span>
        {step.result && (
          <span
            className="pando-onboard-enter"
            style={{ font: 'var(--type-caption)', color: step.failed ? 'var(--ink)' : 'var(--ink-secondary)', textAlign: 'right' }}
          >
            {step.result}
          </span>
        )}
      </div>
      {step.findings.length > 0 && (
        <div className="pando-step-findings">
          {step.findings.map((f, i) => (
            <FindingLine key={`${f.key}-${i}`} finding={f} ai={Boolean(step.ai)} stagger={i} />
          ))}
        </div>
      )}
    </li>
  );
}

function FindingLine({ finding, ai, stagger }: { finding: Finding; ai: boolean; stagger: number }) {
  return (
    <div className="pando-finding pando-onboard-enter" style={{ '--stagger': stagger } as React.CSSProperties}>
      <span style={{ font: 'var(--type-caption)', lineHeight: 'var(--space-5)', color: ai ? 'var(--water)' : 'var(--ink-secondary)' }}>
        {finding.key}
      </span>
      <span
        style={{
          font: finding.text ? 'var(--type-body-ui)' : 'var(--type-code-sm)',
          lineHeight: 'var(--space-5)',
          color: 'var(--ink)',
          overflowWrap: 'anywhere',
        }}
      >
        {finding.value}
      </span>
    </div>
  );
}

// --- Review --------------------------------------------------------------------

/**
 * What somebody should know before accepting, one row each between rules: what
 * the trial run showed and its output (R-107), what AI changed and what Pando
 * refused of it (R-334), an AI adapter that did not run, and the plan's
 * warnings. No tinted banners.
 */
function Notices({
  proposal,
  spec,
  marks,
}: {
  proposal: DetectionResponse['detection'];
  spec: AppSpec | undefined;
  marks: Suggestions;
}) {
  const outcome = proposal.screening;
  const trial = proposal.trial;
  const rows: React.ReactNode[] = [];
  const changed = (outcome?.applied ?? []).filter((a) => a.amendment.kind !== 'answer_question');

  if (outcome?.ran && outcome.function === 'repair_plan' && changed.length > 0) {
    rows.push(
      <NoticeRow key="repair" ai title="AI changed the plan after it failed.">
        {`${changed.map((c) => describeAmendment(c.amendment)).join('. ')}. Each change is marked below.`}
      </NoticeRow>,
    );
  }

  if (trial?.ran) {
    const ports = trial.observed_ports ?? [];
    rows.push(
      <NoticeRow
        key="trial"
        status={trial.crashed || !trial.started ? 'failed' : 'running'}
        title={trial.crashed ? 'The trial run failed.' : !trial.started ? 'The app didn’t start in a trial run.' : 'The app started in a trial run.'}
        detail={proposal.trial_log ? <CodeBlock title="Trial run" lines={proposal.trial_log} /> : undefined}
        detailLabel="output"
      >
        {trial.crashed
          ? 'Pando started the app and it exited with an error. Its output shows why.'
          : ports.length
            ? `It opened ${ports.length === 1 ? 'port' : 'ports'} ${ports.join(', ')}.`
            : 'It opened no ports.'}
      </NoticeRow>,
    );
  }

  if (outcome && !outcome.ran && outcome.skip_code && !['not_configured', 'not_needed', 'blocked'].includes(outcome.skip_code)) {
    rows.push(
      <NoticeRow key="skipped" status="info" title="AI didn’t run.">
        {outcome.skipped}
      </NoticeRow>,
    );
  }

  // The compose importer's rewrites, as one row: each is a construct Pando
  // handled its own way, and eight paragraphs of that is a wall nobody reads.
  // The list is one click away, one line each, the full sentence on hover.
  const rewrites = (spec?.warnings ?? []).filter((w) => w.code === COMPOSE_REWRITTEN).map((w) => composeNote(w.message));
  if (rewrites.length > 0) {
    rows.push(
      <NoticeRow
        key="compose"
        status="info"
        title={
          rewrites.length === 1
            ? 'Pando adapted one setting from the compose file to run here.'
            : `Pando adapted ${rewrites.length} settings from the compose file to run here.`
        }
        detail={
          <ul style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', margin: 0, padding: 0, listStyle: 'none' }}>
            {rewrites.map((note, i) => (
              <li
                key={`${note.full}-${i}`}
                title={note.full}
                style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 14rem) minmax(0, 1fr)', gap: 'var(--space-3)' }}
              >
                <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink)', overflowWrap: 'anywhere' }}>
                  {note.service ? `${note.service} · ${note.construct}` : ''}
                </span>
                <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{note.gist}</span>
              </li>
            ))}
          </ul>
        }
        detailLabel="what changed"
      >
        Each is listed with what Pando does instead.
      </NoticeRow>,
    );
  }

  for (const warning of spec?.warnings ?? []) {
    if (warning.code === COMPOSE_REWRITTEN) continue;
    const ai = warning.code === SCREENING_ADVISORY;
    const amendment = ai ? marks.warnings[warning.message.trim()] : undefined;
    rows.push(
      <NoticeRow key={warning.code + warning.message} ai={ai} status="info" title="">
        {warning.message}
        {amendment?.reason ? ` ${amendment.reason}` : ''}
      </NoticeRow>,
    );
  }

  const refused = outcome?.ran ? (outcome.refused ?? []) : [];
  if (refused.length > 0) {
    rows.push(
      <NoticeRow
        key="refused"
        ai
        title={refused.length === 1 ? 'Pando didn’t apply one AI suggestion.' : `Pando didn’t apply ${refused.length} AI suggestions.`}
        detail={
          <ul style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', margin: 0, padding: 0, listStyle: 'none' }}>
            {refused.map((r, i) => (
              <li key={`${r.amendment.kind}-${i}`} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
                <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{describeAmendment(r.amendment)}</span>
                <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{r.reason}</span>
              </li>
            ))}
          </ul>
        }
        detailLabel="them"
      >
        None of them are in the plan.
      </NoticeRow>,
    );
  }

  if (rows.length === 0) return null;
  return <div style={{ display: 'flex', flexDirection: 'column' }}>{rows}</div>;
}

function NoticeRow({
  ai,
  status,
  title,
  detail,
  detailLabel,
  children,
}: {
  ai?: boolean;
  status?: 'running' | 'failed' | 'info';
  title: string;
  detail?: React.ReactNode;
  detailLabel?: string;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'var(--space-4) minmax(0, 1fr)',
        gap: 'var(--space-3)',
        padding: 'var(--space-3) 0',
        borderTop: 'var(--border-width) solid var(--rule)',
        alignItems: 'baseline',
      }}
    >
      <span style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: 'var(--space-5)' }}>
        {ai ? <AiStar size={14} /> : <StatusSymbol status={status ?? 'info'} size={8} />}
      </span>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', minWidth: 0 }}>
        <span style={{ color: 'var(--ink-secondary)', textWrap: 'pretty' } as React.CSSProperties}>
          {title && <span style={{ color: 'var(--ink)', fontWeight: 500 }}>{title} </span>}
          {children}
          {detail && (
            <>
              {' '}
              <button type="button" className="pando-link" onClick={() => setOpen(!open)} aria-expanded={open}>
                {open ? `Hide ${detailLabel}` : `Show ${detailLabel}`}
              </button>
            </>
          )}
        </span>
        {open && detail}
      </div>
    </div>
  );
}

function GroupHeading({
  title,
  count,
  ai,
  children,
}: {
  title: string;
  count: number;
  ai?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
        {ai && <AiStar size={20} />}
        <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>{title}</h3>
        <Badge count={count} />
      </div>
      <span style={{ color: 'var(--ink-secondary)', textWrap: 'pretty' } as React.CSSProperties}>{children}</span>
    </div>
  );
}

/**
 * One question. The prompt is verbatim with a copy button beside it (R-105).
 * A choice saves the moment it is picked; typed text saves when the field is
 * left, or with the accept. An AI answer shows its reason and the files it
 * rests on, and once changed offers the suggestion back (R-338).
 */
function QuestionCard({
  question,
  value,
  saving,
  error,
  onType,
  onSave,
}: {
  question: Question;
  value: string;
  saving: boolean;
  error?: unknown;
  /** Absent for somebody who may read the question but not answer it. */
  onType?: (value: string) => void;
  onSave?: (value: string) => void;
}) {
  const [copied, setCopied] = useState(false);
  const state = answerState(question, value);
  const suggested = question.suggested;
  const label = saving
    ? 'Saving'
    : state === 'needed'
      ? 'Needs an answer'
      : state === 'ai'
        ? 'AI answered'
        : 'Answered';
  const tone = saving || state === 'needed' ? 'building' : state === 'ai' ? 'info' : 'running';

  async function copy() {
    // The prompt exactly, and nothing else. Anything prepended would arrive in
    // the assistant as part of the question.
    await navigator.clipboard.writeText(question.prompt);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2_000);
  }

  const choice = question.kind === 'choice' && (question.options ?? []).length > 0;

  return (
    <Card tone="paper" padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 'var(--space-4)' }}>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
            {/* Verbatim. Not shortened, not re-worded, not split up. */}
            <span style={{ font: 'var(--type-label)', color: 'var(--ink)', textWrap: 'pretty' } as React.CSSProperties}>
              {question.prompt}
            </span>
            {question.why && (
              <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', textWrap: 'pretty' } as React.CSSProperties}>
                {question.why}
              </span>
            )}
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', flex: '0 0 auto' }}>
            <StatusIndicator status={tone} label={label} />
            <IconButton label={copied ? 'Copied' : 'Copy this question'} onClick={copy}>
              <Icon name={copied ? 'check' : 'copy'} size={16} />
            </IconButton>
          </div>
        </div>

        {choice ? (
          <div role="radiogroup" aria-label={question.prompt} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            {(question.options ?? []).map((option) => (
              <Radio
                key={option}
                name={question.key}
                value={option}
                label={option}
                description={question.key === 'build_strategy' || question.key === 'build_method' ? strategyLabel(option) : undefined}
                checked={value === option}
                disabled={!onSave}
                onChange={() => {
                  onType?.(option);
                  onSave?.(option);
                }}
              />
            ))}
          </div>
        ) : (
          <Input
            aria-label={question.prompt}
            mono={question.kind !== 'text'}
            value={value}
            placeholder={question.kind === 'port' ? '3000' : undefined}
            disabled={!onType}
            onChange={(e) => onType?.(e.target.value)}
            onBlur={(e) => onSave?.(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') onSave?.((e.target as HTMLInputElement).value);
            }}
          />
        )}

        {Boolean(error) && <Failure error={error} />}

        {suggested && state === 'ai' && (
          <div
            style={{
              display: 'grid',
              gridTemplateColumns: 'var(--space-4) minmax(0, 1fr)',
              gap: 'var(--space-2)',
              alignItems: 'start',
              paddingTop: 'var(--space-3)',
              borderTop: 'var(--border-width) solid var(--rule)',
            }}
          >
            <span style={{ display: 'flex', height: 'var(--space-4)', alignItems: 'center' }}>
              <AiStar size={14} />
            </span>
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', textWrap: 'pretty' } as React.CSSProperties}>
              <span style={{ color: 'var(--water)', fontWeight: 500 }}>Why AI picked this.</span>{' '}
              {suggested.reason}
              {(suggested.evidence ?? []).length > 0 && (
                <> Based on {(suggested.evidence ?? []).map((f, i) => (
                  <span key={f}>
                    {i > 0 && ', '}
                    <code style={{ font: 'var(--type-code-sm)' }}>{f}</code>
                  </span>
                ))}.</>
              )}
            </span>
          </div>
        )}
        {suggested && state !== 'ai' && (
          <div
            style={{
              display: 'flex',
              flexWrap: 'wrap',
              gap: 'var(--space-2)',
              alignItems: 'center',
              paddingTop: 'var(--space-3)',
              borderTop: 'var(--border-width) solid var(--rule)',
              font: 'var(--type-caption)',
              color: 'var(--ink-secondary)',
            }}
          >
            <span>You changed the AI suggestion.</span>
            {onSave && (
              <button
                type="button"
                className="pando-link"
                onClick={() => {
                  onType?.(suggested.value);
                  onSave(suggested.value);
                }}
              >
                Use suggestion
              </button>
            )}
          </div>
        )}
      </div>
    </Card>
  );
}

function Variables({
  rows,
  spec,
  problems,
  marks,
  canEdit,
  canSecrets,
  onEdit,
  onAdd,
  onRemove,
  optional,
  onOptional,
  onAskOptional,
}: {
  rows: VariableRow[];
  spec: AppSpec;
  problems: Record<string, string>;
  marks: Suggestions;
  canEdit: boolean;
  canSecrets: boolean;
  onEdit: (row: VariableRow, patch: RowEdit & { key?: string }) => void;
  onAdd: () => void;
  onRemove: (row: VariableRow) => void;
  /** Slots the person marked not required, by slot key. */
  optional: Set<string>;
  onOptional: (slot: string, optional: boolean) => void;
  /** Ask before marking a required value optional: it may break the deploy. */
  onAskOptional: (row: VariableRow) => void;
}) {
  const workloads = spec.workloads ?? [];
  const many = workloads.length > 1;
  // Rows Pando would fill that the person has chosen to point elsewhere, by
  // row id: their field is shown even before anything is typed in it.
  const [overriding, setOverriding] = useState<Set<string>>(() => new Set());
  // Values Pando generated, by row id, so the row can say so — until the
  // person changes the value, when it is simply theirs.
  const [generated, setGenerated] = useState<Record<string, string>>({});
  // How many times each row has been generated, to replay its wash; and the
  // row whose button says "Generated" for a moment.
  const [flash, setFlash] = useState<Record<string, number>>({});
  const [justGenerated, setJustGenerated] = useState<string | null>(null);
  useEffect(() => {
    if (!justGenerated) return;
    const timer = window.setTimeout(() => setJustGenerated(null), 1_600);
    return () => window.clearTimeout(timer);
  }, [justGenerated, flash]);
  if (rows.length === 0 && !canEdit) return null;

  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>Variables</h3>
        <span style={{ color: 'var(--ink-secondary)' }}>
          {rows.length === 0
            ? 'Pando found no variables this app reads. Add any it needs.'
            : 'Change any value. Pando uses these on every deploy.'}
        </span>
      </div>

      {rows.length > 0 && (
        <Card tone="paper" padding="none">
          {rows.map((row, i) => {
            const isNew = row.original === undefined;
            const ai = isNew ? undefined : marks.env[envKey(row.workload || primaryWorkload(spec), row.key)];
            const changed = !isNew && row.value !== row.original;
            const slot = row.slot;
            const service = slot?.service ? slotLabel(slot.type) : undefined;
            // Pando fills it, and nobody has chosen to point it elsewhere: no
            // field at all, so it cannot be overwritten by accident.
            const pandoFills = Boolean(slot && service && slot.provisioned && !changed && !overriding.has(row.id));
            let source: React.ReactNode;
            let placeholder = row.secret ? 'Kept secret' : 'No value yet';
            if (slot && service && slot.provisioned) {
              // Pando creates the service at the first deploy and writes its
              // address here then. A value typed now points the app at one the
              // person already runs instead.
              placeholder = `Your ${service} URL`;
              source = pandoFills ? (
                <span>{`From ${service}, which Pando creates`}</span>
              ) : (
                <>
                  <span>{`Your own ${service}. Pando won’t create one.`}</span>
                  {canEdit && (
                    <button
                      type="button"
                      className="pando-link"
                      onClick={() => {
                        onEdit(row, { value: '' });
                        setOverriding((all) => {
                          const next = new Set(all);
                          next.delete(row.id);
                          return next;
                        });
                      }}
                    >
                      {`Use Pando’s ${service}`}
                    </button>
                  )}
                </>
              );
            } else if (slot) {
              const markedOptional = slot.required && optional.has(slot.key);
              placeholder = slot.required && !markedOptional ? 'Needed to deploy' : 'Optional';
              source = changed ? (
                <span>Your value</span>
              ) : markedOptional ? (
                <span>You marked it optional. It stays unset if empty.</span>
              ) : (
                <span style={{ color: slot.required ? 'var(--ink)' : undefined }}>
                  {slot.required ? 'Needs a value' : 'Optional, no value yet'}
                </span>
              );
            } else if (isNew) source = <span>Added by you</span>;
            else if (changed) {
              source = (
                <>
                  <span>Your value</span>
                  {canEdit && (
                    <button type="button" className="pando-link" onClick={() => onEdit(row, { value: row.original })}>
                      {row.original ? 'Use Pando’s value' : 'Clear'}
                    </button>
                  )}
                </>
              );
            } else if (ai) {
              source = (
                <span title={ai.reason} style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)' }}>
                  <AiStar size={12} />
                  Suggested by AI
                </span>
              );
            } else if (row.original) source = <span>Found in the repo</span>;
            else source = <span style={{ color: 'var(--ink)' }}>No value yet</span>;

            // Provenance for a value nobody here typed, most specific last.
            const aiValue = isNew ? undefined : aiMarkFor(marks, row, spec);
            if (row.fromAddress) {
              source = <span>From the app’s address</span>;
            } else if (aiValue && !changed && row.value !== '') {
              source = (
                <span title={aiValue.reason} style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)' }}>
                  <AiStar size={12} />
                  Filled by AI
                </span>
              );
            }
            if (row.value !== '' && generated[row.id] === row.value) {
              // Said, not offered again here: the Generate button beside the
              // value does that.
              source = <span>Generated by Pando</span>;
            }

            return (
              <VariableLine
                key={row.id}
                first={i === 0}
                name={
                  isNew ? (
                    <Input
                      aria-label="Name"
                      placeholder="NAME"
                      mono
                      value={row.key}
                      error={problems[row.id]}
                      onChange={(e) => onEdit(row, { key: e.target.value })}
                    />
                  ) : (
                    row.key
                  )
                }
                workload={many && !isNew ? row.workload : undefined}
                required={
                  slot?.required && !pandoFills
                    ? {
                        optional: optional.has(slot.key),
                        filled: row.value.trim() !== '',
                        onToggle: canEdit
                          ? optional.has(slot.key)
                            ? () => onOptional(slot.key, false)
                            : () => onAskOptional(row)
                          : undefined,
                      }
                    : undefined
                }
                source={source}
              >
                {pandoFills ? (
                  <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2) var(--space-4)' }}>
                    <StatusIndicator status="info" label="Filled in by Pando" />
                    {canEdit && (
                      <button
                        type="button"
                        className="pando-link"
                        onClick={() => setOverriding((all) => new Set(all).add(row.id))}
                      >
                        {`Use your own ${service}`}
                      </button>
                    )}
                  </div>
                ) : (
                  <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
                    <div
                      // Re-keyed on each generation, so the wash replays every
                      // time: the value visibly changed, even when it is
                      // hidden as a secret.
                      key={`${row.id}-${flash[row.id] ?? 0}`}
                      className={flash[row.id] ? 'pando-generated' : undefined}
                      style={{ flex: 1, minWidth: 0, borderRadius: 'var(--radius-sm)' }}
                    >
                      <Input
                        aria-label={`Value of ${row.key || 'the new variable'}`}
                        mono
                        type={row.secret ? 'password' : 'text'}
                        autoComplete="off"
                        placeholder={placeholder}
                        value={row.value}
                        disabled={!canEdit}
                        onChange={(e) => onEdit(row, { value: e.target.value })}
                      />
                    </div>
                    {canEdit && (
                      <GenerateValue
                        justDone={justGenerated === row.id}
                        onGenerate={() => {
                          const value = randomSecret();
                          setGenerated((all) => ({ ...all, [row.id]: value }));
                          setFlash((all) => ({ ...all, [row.id]: (all[row.id] ?? 0) + 1 }));
                          setJustGenerated(row.id);
                          // A generated value is a secret by nature; keep it
                          // one unless the person says otherwise afterwards.
                          onEdit(row, { value, secret: canSecrets ? true : row.secret });
                        }}
                      />
                    )}
                    <Divider />
                    {/* A default, never a lock: every value can be kept as a
                        secret or left readable. */}
                    <Checkbox
                      label="Secret"
                      checked={row.secret}
                      disabled={!canEdit || !canSecrets}
                      onChange={(e) => onEdit(row, { secret: e.target.checked })}
                    />
                    {isNew && canEdit && (
                      <IconButton label="Remove this variable" onClick={() => onRemove(row)}>
                        <Icon name="x" size={16} />
                      </IconButton>
                    )}
                  </div>
                )}
              </VariableLine>
            );
          })}
        </Card>
      )}

      {canEdit && (
        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 'var(--space-2)' }}>
          <Button variant="secondary" onClick={onAdd}>
            Add variable
          </Button>
          <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
            {canSecrets
              ? 'A secret is stored apart from the configuration and never appears in an export. Use it for keys and passwords.'
              : 'You can’t store secrets on this app, so every value here is saved in the configuration. Leave keys and passwords empty for somebody who can.'}
          </p>
        </div>
      )}
    </section>
  );
}

/**
 * The AI amendment that set a variable, if one did. Keyed by workload and
 * name; an amendment naming no workload is filed under the primary of the
 * spec it was read against, which is not always the reading shown, so a
 * variable of the same name elsewhere is the fallback.
 */
function aiMarkFor(marks: Suggestions, row: VariableRow, spec: AppSpec): Amendment | undefined {
  const direct = marks.env[envKey(row.workload || primaryWorkload(spec), row.key)];
  if (direct) return direct;
  return Object.entries(marks.env).find(([key]) => key.endsWith(`/${row.key}`))?.[1];
}

/**
 * Fill a value with a random one: for a variable that just needs a secret
 * nobody will type — an encryption key, a session secret. The info mark says
 * what it is, so nobody wonders whether Pando looked something up.
 */
function GenerateValue({ onGenerate, justDone }: { onGenerate: () => void; justDone: boolean }) {
  // The explanation is the button's own tooltip — hovering the thing is how
  // somebody asks what it does. It is a short paragraph, so it sets its own
  // width and wraps; Tooltip itself is one line, for labels.
  return (
    <Tooltip
      side="top"
      content={
        <span style={{ display: 'block', width: '16rem', whiteSpace: 'normal', lineHeight: 1.4 }}>
          Fills in a random, secure string — essentially a password. Use it when the app just needs a
          secret value and you don’t have one of your own.
        </span>
      }
    >
      {/* The label never changes, only the icon (the same size), so the
          button keeps its width and the value beside it does not jump. */}
      <Button
        variant="ghost"
        icon={<Icon name={justDone ? 'check' : 'dices'} size={16} />}
        onClick={onGenerate}
        aria-label={justDone ? 'Generated a random value. Generate another.' : 'Generate a random value'}
      >
        Generate
      </Button>
    </Tooltip>
  );
}

/**
 * A full-height rule between two controls that should not read as one, with
 * room on both sides: the Generate button acts on the value, the Secret box
 * on how it is stored.
 */
function Divider() {
  return (
    <span
      aria-hidden="true"
      style={{
        alignSelf: 'stretch',
        flex: '0 0 auto',
        width: 'var(--border-width)',
        background: 'var(--rule-strong)',
        margin: '0 var(--space-2)',
      }}
    />
  );
}

/**
 * "Required" or "Optional", as a tag that is also the switch between them.
 * The ordinary Tag, same size, recolored: marker red while a required value is
 * missing — it blocks the deploy, one of the few things on this page that
 * should catch the eye. Clicking Required asks before making it optional
 * (a wrong call breaks the deploy); clicking Optional makes it required again.
 */
function RequirementBadge({
  optional,
  filled,
  onToggle,
}: {
  optional: boolean;
  filled: boolean;
  onToggle?: () => void;
}) {
  const red = !optional && !filled;
  const tag = (
    <Tag style={red ? { color: 'var(--marker-deep)', borderColor: 'var(--marker)', background: 'transparent' } : undefined}>
      {optional ? 'Optional' : 'Required'}
    </Tag>
  );
  if (!onToggle) return tag;
  return (
    <button
      type="button"
      onClick={onToggle}
      title={optional ? 'Mark this required again' : 'The app needs this. Click if it doesn’t.'}
      aria-label={optional ? `Optional. Mark required again.` : `Required. Mark not required.`}
      style={{ display: 'inline-flex', border: 'none', background: 'transparent', padding: 0, cursor: 'pointer' }}
    >
      {tag}
    </button>
  );
}

function VariableLine({
  first,
  name,
  workload,
  required,
  source,
  children,
}: {
  first: boolean;
  name: React.ReactNode;
  workload?: string;
  /**
   * Whether the app can deploy without a value (R-132): said beside the name
   * as a tag that is also the switch (RequirementBadge).
   */
  required?: { optional: boolean; filled: boolean; onToggle?: () => void };
  source: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div
      style={{
        display: 'flex',
        flexWrap: 'wrap',
        alignItems: 'center',
        gap: 'var(--space-2) var(--space-4)',
        padding: 'var(--space-3) var(--space-4)',
        borderTop: first ? 'none' : 'var(--border-width) solid var(--rule)',
      }}
    >
      <div style={{ flex: '1 1 15rem', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 'calc(var(--space-1) / 2)' }}>
        <span style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2)', font: 'var(--type-code-sm)', color: 'var(--ink)', overflowWrap: 'anywhere' }}>
          {name}
          {required && (
            <RequirementBadge optional={required.optional} filled={required.filled} onToggle={required.onToggle} />
          )}
          {workload && <Tag mono>{workload}</Tag>}
        </span>
        <span
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 'var(--space-2)',
            flexWrap: 'wrap',
            font: 'var(--type-caption)',
            color: 'var(--ink-secondary)',
          }}
        >
          {source}
        </span>
      </div>
      <div style={{ flex: '2 1 20rem', minWidth: 0 }}>{children}</div>
    </div>
  );
}

/**
 * What the AI adapter said that changes nothing (design 10 §2: notes are never
 * warnings). After the variables and before the plan: read once the person has
 * settled the answers and values, as context for the plan below.
 */
function AiNotes({ notes }: { notes: string[] }) {
  if (notes.length === 0) return null;
  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
        <AiStar size={20} />
        <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>Notes from AI</h3>
      </div>
      <div style={{ display: 'flex', flexDirection: 'column' }}>
        {notes.map((note) => (
          <p
            key={note}
            style={{
              margin: 0,
              padding: 'var(--space-3) 0',
              borderTop: 'var(--border-width) solid var(--rule)',
              color: 'var(--ink)',
              textWrap: 'pretty',
            } as React.CSSProperties}
          >
            {note}
          </p>
        ))}
      </div>
    </section>
  );
}

/**
 * Whether an AI adapter is there to ask. Every finished detection records an
 * outcome; these skip codes say there is no adapter, host policy forbids it, or
 * it cannot revise. Anything else — it ran, or was not needed — means it is.
 */
export function aiAvailable(outcome: DetectionResponse['detection']['screening']): boolean {
  if (!outcome) return false;
  return !['not_configured', 'policy', 'unsupported'].includes(outcome.skip_code ?? '');
}

/**
 * Talking to the AI adapter about the plan (R-336's third trigger, design 10
 * §4.3). The person says what is wrong; the adapter checks the repository,
 * changes what it can show, and replies. What it changed and what Pando would
 * not do are listed under each reply (R-334). Somebody who may read the plan
 * but not change it sees the conversation and no box to type in.
 */
function AskAI({ appID, conversation, canAsk }: { appID: string; conversation: Turn[]; canAsk: boolean }) {
  const queries = useQueryClient();
  const [message, setMessage] = useState('');
  const ask = useMutation({
    mutationFn: (text: string) => api.post<DetectionResponse>(`/apps/${appID}/detection/revise`, { message: text }),
    onSuccess: (updated) => {
      setMessage('');
      queries.setQueryData(['apps', appID, 'detection'], updated);
    },
  });

  const send = () => {
    const text = message.trim();
    if (text && !ask.isPending) ask.mutate(text);
  };

  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
          <AiStar size={20} />
          <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>Ask AI about this plan</h3>
        </div>
        <span style={{ color: 'var(--ink-secondary)', textWrap: 'pretty' } as React.CSSProperties}>
          Say what’s wrong — a missing variable, the right port, a database it missed. AI checks the
          repository and changes what it can.
        </span>
      </div>

      {(conversation.length > 0 || ask.isPending) && (
        <ol style={{ display: 'flex', flexDirection: 'column', margin: 0, padding: 0, listStyle: 'none' }}>
          {conversation.map((turn, i) => (
            <TurnRow key={`${turn.at}-${i}`} turn={turn} />
          ))}
          {ask.isPending && (
            <>
              <TurnRow turn={{ from: 'person', text: ask.variables ?? '', at: '' }} />
              <li style={{ padding: 'var(--space-3) 0', borderTop: 'var(--border-width) solid var(--rule)' }}>
                <AiThinking phrases={phrasesFor('ask')} />
              </li>
            </>
          )}
        </ol>
      )}

      {canAsk && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
          <Input
            as="textarea"
            rows={2}
            aria-label="What should change about this plan"
            placeholder="It also needs Redis, reached at REDIS_URL."
            value={message}
            disabled={ask.isPending}
            onChange={(e) => setMessage(e.target.value)}
            onKeyDown={(e) => {
              // Enter with a modifier sends; plain Enter is a new line.
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                send();
              }
            }}
          />
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
            <Button variant="secondary" disabled={!message.trim() || ask.isPending} onClick={send}>
              {ask.isPending ? 'Asking' : 'Ask AI'}
            </Button>
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
              AI reads files from the repository to check what you say.
            </span>
          </div>
          {ask.isError && <Failure error={ask.error} />}
        </div>
      )}
    </section>
  );
}

function TurnRow({ turn }: { turn: Turn }) {
  const ai = turn.from === 'ai';
  return (
    <li
      className="pando-onboard-enter"
      style={{
        display: 'grid',
        gridTemplateColumns: 'var(--space-6) minmax(0, 1fr)',
        gap: 'var(--space-3)',
        padding: 'var(--space-3) 0',
        borderTop: 'var(--border-width) solid var(--rule)',
      }}
    >
      <span style={{ display: 'flex', alignItems: 'flex-start', paddingTop: 'calc(var(--space-1) / 2)' }}>
        {ai ? <AiStar size={14} /> : <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>You</span>}
      </span>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', minWidth: 0 }}>
        <span style={{ color: 'var(--ink)', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{turn.text}</span>
        {(turn.changes ?? []).length > 0 && (
          <ul style={{ margin: 0, paddingLeft: 'var(--space-4)', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
            {(turn.changes ?? []).map((change) => (
              <li key={change} style={{ font: 'var(--type-caption)', color: 'var(--water)' }}>
                {`Changed: ${change}`}
              </li>
            ))}
          </ul>
        )}
        {(turn.refused ?? []).length > 0 && (
          <ul style={{ margin: 0, paddingLeft: 'var(--space-4)', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
            {(turn.refused ?? []).map((refusal) => (
              <li key={refusal} style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
                {`Pando didn’t apply: ${refusal}`}
              </li>
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}

/** How the app will be built and run, and what runs beside it. */
function ThePlan({
  proposal,
  spec,
  marks,
  answers,
  source,
  commit,
  address,
  hostname,
  onHostname,
}: {
  proposal: DetectionResponse['detection'];
  spec: AppSpec;
  marks: Suggestions;
  answers: Record<string, string>;
  source: Source | undefined;
  commit?: string;
  /** Where the app will be reachable once deployed, as a full URL. */
  address?: string;
  hostname: string;
  /** Absent where the address can't be chosen here, or by who can't edit. */
  onHostname?: (hostname: string) => void;
}) {
  const workloads = spec.workloads ?? [];
  const primaryName = primaryWorkload(spec);
  const primary = workloads.find((w) => w.name === primaryName) ?? workloads[0];
  const start = answers.start_command
    ? { value: answers.start_command }
    : primary
      ? startCommand(proposal, spec, primary)
      : undefined;
  const port = answers.primary_port || (primary?.ports ?? [])[0]?.number?.toString();
  const build = spec.build;
  const health = spec.health;
  const short = commit ? commit.slice(0, 7) : undefined;

  const buildRows: Array<{ k: string; v: string; text?: boolean; ai?: Amendment }> = [
    { k: 'Approach', v: strategyLabel(build?.strategy), text: true },
  ];
  if (build?.dockerfile) buildRows.push({ k: 'Built from', v: build.dockerfile, ai: marks.build.dockerfile });
  if (build?.context && build.context !== '.') buildRows.push({ k: 'Build context', v: build.context, ai: marks.build.context });
  if (build?.static_dir) buildRows.push({ k: 'Serves', v: build.static_dir, ai: marks.build.static_dir });
  if (start) {
    buildRows.push({
      k: workloads.length > 1 ? `Start ${primary!.name}` : 'Start',
      v: start.value,
      text: start.text,
      ai: primary ? marks.command[primary.name] : undefined,
    });
  }
  if (port) buildRows.push({ k: 'Port', v: port, ai: primary ? marks.port[primary.name] : undefined });
  if (health?.source === 'http' && health.path) {
    buildRows.push({ k: 'Health check', v: `GET ${health.path} on :${health.port}`, ai: marks.health });
  }
  if (source?.type === 'git') {
    buildRows.push({
      k: 'Deploys',
      v: short ? `${short}${source.ref ? ` on ${source.ref}` : ''}` : (source.ref ?? 'The default branch'),
      text: !short,
    });
  }

  // Services only. A slot of type `unknown` is a value somebody sets, and is
  // listed with the variables (discovery.ts isService).
  const services = (spec.slots ?? []).filter(isService);
  const mounts = workloads.flatMap((w) => (w.mounts ?? []).map((m) => ({ workload: w.name, path: m.path })));

  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>The plan</h3>
      <AvailableAt routing={spec.routing} address={address} hostname={hostname} onHostname={onHostname} />
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 21rem), 1fr))',
          gap: 'var(--space-5) var(--space-7)',
          alignItems: 'start',
        }}
      >
        <div style={{ display: 'flex', flexDirection: 'column' }}>
          <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)', paddingBottom: 'var(--space-2)' }}>
            Build and run
          </span>
          {buildRows.map((r) => (
            <PlanRow key={r.k} label={r.k}>
              <span style={{ font: r.text ? 'var(--type-body-ui)' : 'var(--type-code-sm)', color: 'var(--ink)', overflowWrap: 'anywhere' }}>
                {r.v}
              </span>
              {r.ai && (
                <span title={r.ai.reason ? `Changed by AI. ${r.ai.reason}` : 'Changed by AI'}>
                  <AiStar size={14} />
                </span>
              )}
            </PlanRow>
          ))}
        </div>
        <div style={{ display: 'flex', flexDirection: 'column' }}>
          <span style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)', paddingBottom: 'var(--space-2)' }}>
            Processes and services
          </span>
          {workloads.map((w) => {
            const runs = startCommand(proposal, spec, w);
            return (
              <ProcessRow
                key={w.name}
                name={w.name}
                detail={w.image && !(w.command ?? []).length ? w.image : runs.value}
                text={Boolean(runs.text) && !w.image}
                status="running"
                state={workloads.length > 1 && w.name === primaryName ? 'Runs, serves the app' : 'Runs'}
              />
            );
          })}
          {services.map((s) => {
            const provision = serviceProvision(s);
            return (
              <ProcessRow
                key={s.key}
                name={slotLabel(s.type)}
                detail={serviceImage(s) ?? `Reached at ${s.key}`}
                status={provision.status}
                state={provision.label}
                ai={Boolean(marks.slots[s.key])}
              />
            );
          })}
          {mounts.map((m) => (
            <ProcessRow
              key={m.workload + m.path}
              name="Storage"
              detail={m.path}
              status="info"
              state="Kept across deploys"
              ai={Boolean(marks.volumes[m.path])}
            />
          ))}
        </div>
      </div>
    </section>
  );
}

/**
 * Where the app will be once deployed, first in the plan: most people deploying
 * a generated app are asked for "the domain" by it and don't know what that
 * is, and Pando does. The address comes from the plan's routing — the routing
 * adapter the install uses and the mode it serves apps in — and can be chosen
 * here where that mode serves a hostname; elsewhere Pando assigns it.
 */
function AvailableAt({
  routing,
  address,
  hostname,
  onHostname,
}: {
  routing: AppSpec['routing'] | undefined;
  address?: string;
  hostname: string;
  onHostname?: (hostname: string) => void;
}) {
  const mode = routing?.mode;
  const how =
    mode === 'subdomain'
      ? 'at its own hostname'
      : mode === 'port'
        ? 'on a port of its own on this host, which Pando assigns'
        : mode === 'path'
          ? 'under a path on Pando’s address'
          : undefined;
  const editable = mode === 'subdomain' && onHostname;

  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-2)',
        padding: 'var(--space-3) var(--space-4)',
        border: 'var(--border-width) solid var(--rule)',
        borderRadius: 'var(--radius-md)',
        background: 'var(--paper-raised)',
      }}
    >
      <span style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'baseline', gap: 'var(--space-2)' }}>
        <span style={{ color: 'var(--ink-secondary)' }}>Once deployed, available at</span>
        {address ? (
          <a
            href={address}
            target="_blank"
            rel="noopener noreferrer"
            style={{ font: 'var(--type-code)', color: 'var(--ink)', overflowWrap: 'anywhere' }}
          >
            {address}
          </a>
        ) : (
          <span style={{ color: 'var(--ink)' }}>an address Pando assigns on the first deploy</span>
        )}
      </span>
      {how && (
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          {`Served ${how}${routing?.adapter_ref ? `, by the ${routing.adapter_ref} routing adapter` : ''}. Variables naming the app’s own URL or domain are filled from this address.`}
        </span>
      )}
      {editable && (
        <div style={{ maxWidth: '28rem' }}>
          <Input
            label="Hostname"
            mono
            placeholder={routing?.hostname}
            value={hostname}
            onChange={(e) => onHostname(e.target.value)}
            helper="Leave empty to keep the one Pando assigned. Point this name’s DNS at Pando before deploying."
          />
        </div>
      )}
    </div>
  );
}

function PlanRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: '7.5rem minmax(0, 1fr)',
        gap: 'var(--space-3)',
        padding: 'var(--space-2) 0',
        borderTop: 'var(--border-width) solid var(--rule)',
        alignItems: 'baseline',
      }}
    >
      <span style={{ color: 'var(--ink-secondary)' }}>{label}</span>
      <span style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', minWidth: 0 }}>{children}</span>
    </div>
  );
}

function ProcessRow({
  name,
  detail,
  text,
  status,
  state,
  ai,
}: {
  name: string;
  detail: string;
  /** A description rather than a command or an image: the UI face, not mono. */
  text?: boolean;
  status: 'running' | 'info' | 'building' | 'stopped';
  state: string;
  ai?: boolean;
}) {
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: '6rem minmax(0, 1fr) auto',
        gap: 'var(--space-3)',
        padding: 'var(--space-2) 0',
        borderTop: 'var(--border-width) solid var(--rule)',
        alignItems: 'baseline',
      }}
    >
      <span style={{ font: 'var(--type-label)', display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)' }}>
        {name}
        {ai && <AiStar size={12} />}
      </span>
      <span
        style={{
          font: text ? 'var(--type-caption)' : 'var(--type-code-sm)',
          color: 'var(--ink-secondary)',
          overflowWrap: 'anywhere',
        }}
      >
        {detail}
      </span>
      <StatusIndicator status={status} label={state} />
    </div>
  );
}

// --- The action bar -------------------------------------------------------------

function barStatus(
  missing: Question[],
  problems: number,
  values: VariableRow[],
  canDeploy: boolean,
  canEdit: boolean,
): { status: 'building' | 'running' | 'info'; label: string; detail: string } {
  if (!canEdit) {
    return { status: 'info', label: 'Waiting for review', detail: 'Accepting this plan needs permission to change the app.' };
  }
  if (missing.length > 0) {
    return {
      status: 'building',
      label: missing.length === 1 ? '1 answer needed' : `${missing.length} answers needed`,
      detail: `Still needed: ${listNames(missing.map((q) => questionName(q.key)))}.`,
    };
  }
  if (problems > 0) {
    return { status: 'building', label: 'A variable needs a name', detail: 'Name the variable you added, or remove it.' };
  }
  if (values.length > 0) {
    return {
      status: 'building',
      label: values.length === 1 ? '1 value needed to deploy' : `${values.length} values needed to deploy`,
      detail: `Set ${listNames(values.map((r) => r.key))} to deploy now, or accept and set ${values.length === 1 ? 'it' : 'them'} later in Settings.`,
    };
  }
  return {
    status: 'running',
    label: canDeploy ? 'Ready to deploy' : 'Ready to accept',
    detail: 'Nothing runs until you accept.',
  };
}

function ActionBar({
  status,
  label,
  detail,
  error,
  children,
}: {
  status: 'building' | 'running' | 'info' | 'failed';
  label: string;
  detail: string;
  error?: unknown;
  children: React.ReactNode;
}) {
  return (
    <div className="pando-actionbar pando-onboard-enter" style={{ '--stagger': 2 } as React.CSSProperties}>
      <div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'calc(var(--space-1) / 2)', minWidth: 0 }}>
          <StatusIndicator status={status} label={label} />
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', paddingLeft: 'var(--space-4)' }}>
            {detail}
          </span>
          {Boolean(error) && (
            <div style={{ paddingLeft: 'var(--space-4)' }}>
              <Failure error={error} />
            </div>
          )}
        </div>
        <div className="pando-actionbar-buttons">{children}</div>
      </div>
    </div>
  );
}

// --- Pieces ------------------------------------------------------------------------

