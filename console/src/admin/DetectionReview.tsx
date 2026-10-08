// Detection review (R-102, R-103, R-105).
//
// Three things here are requirements rather than presentation, and each is easy
// to lose without noticing:
//
// **Question text is rendered verbatim.** Design 08 §1.3: "the console does not
// paraphrase it, or the R-105 guarantee is lost in the UI layer." R-105 makes
// every question answerable by something that cannot see the repository,
// because the intended workflow is pasting it into the assistant that wrote the
// app. A console that shortened "Pando could not determine which port this
// Node.js app serves HTTP on. Valid answer: a port number, such as 3000." to
// "Port?" would destroy that guarantee while looking tidier.
//
// **Every question has a copy button**, for the same reason. It copies the
// prompt exactly, with nothing added.
//
// **The runners-up are shown.** R-102 is "ask, never guess", and showing what
// else bid is how that becomes visible rather than asserted — the user sees the
// auction instead of a verdict.
//
// Reading all of it is app.view. Answering, accepting and looking again are
// app.spec.edit, so without that verb the proposal and any answers already
// given are shown, and nothing offers to change them.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { invalidateApp } from './appList';
import {
  Button,
  Card,
  CodeBlock,
  Dialog,
  Icon,
  IconButton,
  Input,
  Select,
  SkeletonText,
  StatusIndicator,
  Tag,
} from '@design';

import { api, RequestFailed } from '@api/client';
import type { Candidate, Proposal, Question } from '@api/types.gen';
import { InlineWarning } from '../ui/InlineWarning';
import { rejectedEntries } from './rejections';
import { Screening } from './Screening';
import { Table } from '../ui/Table';
import { LineSkeleton } from '../ui/Loading';
import { AppVerb, useCan } from './verbs';

export interface DetectionResponse {
  status: string;
  detection: Proposal & {
    // Set only when detection failed outright — a repository that could not be
    // cloned, a trial run that could not start. Distinct from `blocked`, which
    // is a proposal that succeeded and found an app Pando cannot run: one is
    // "Pando never got to look", the other is "Pando looked and the answer is
    // no". They read differently and lead to different next steps.
    error?: { message?: string; remedy?: string };
    // Where a running detection has got to: fetching, detecting, trying,
    // screening. Set only while status is 'running', and the proposal fields
    // are present only once that stage has produced them — so while running,
    // any of them may be absent (see AppOnboarding.tsx).
    stage?: string;
  };
  answers: Record<string, string> | null;
  commit: string;
  /** When this detection began, RFC 3339: the onboarding page's clock. */
  started_at?: string;
  /** When it last changed: for a finished one, when it finished. */
  updated_at?: string;
  /**
   * Where the app will be reachable once deployed, from the proposal's
   * routing (spec.Address): https://host/, //host:port/ or /slug/.
   */
  address?: string;
}

/**
 * How long each read of a running detection waits for it to move on. Under the
 * server's own limit of 60 seconds (detection.MaxWait).
 */
const DETECTION_WAIT_SECONDS = 25;

/**
 * Reads an app's detection. While the last reading was still running, it is a
 * long poll: GET /detection?wait= is answered as soon as detection reaches a
 * new stage or finishes — the same wait `pando app detection --wait` and
 * pando_get_detection use (R-261, issue #80) — so a stage shows as it
 * happens rather than up to a polling interval late.
 */
export function fetchDetection(appID: string, previous: DetectionResponse | undefined) {
  const wait = previous?.status === 'running' ? `?wait=${DETECTION_WAIT_SECONDS}` : '';
  return api.get<DetectionResponse>(`/apps/${appID}/detection${wait}`);
}

/**
 * Asks again straight after each answer while detection runs: each ask waits
 * on the server, so this only has to be short. A tick that lands while one is
 * still waiting joins it rather than sending another. Stops once finished.
 */
export const refetchWhileRunning = (query: { state: { data?: DetectionResponse } }) =>
  query.state.data?.status === 'running' ? 500 : false;

export function DetectionReview({ appID, reviewed }: { appID: string; reviewed: boolean }) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);

  const detection = useQuery({
    queryKey: ['apps', appID, 'detection'],
    queryFn: () =>
      fetchDetection(appID, queries.getQueryData<DetectionResponse>(['apps', appID, 'detection'])),
    // Detection runs in the background after an app is created, so this
    // waits until it settles rather than asking the user to reload.
    refetchInterval: refetchWhileRunning,
  });

  const answer = useMutation({
    mutationFn: (answers: Record<string, string>) =>
      api.post(`/apps/${appID}/detection/answers`, { answers }),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] }),
  });

  const [replacing, setReplacing] = useState(false);

  const accept = useMutation({
    // confirm only when there is something to replace. The server refuses an
    // unconfirmed accept on a configured app, and that refusal is the real
    // guard — this just means the console does not walk into it.
    mutationFn: () => api.post(`/apps/${appID}/detection/accept`, reviewed ? { confirm: true } : {}),
    // This app and the lists, not this app alone: accepting pins a spec and moves the app
    // out of draft, which changes both this screen and its row in the list.
    // The narrower key refreshes the screen and leaves the list saying draft.
    onSuccess: () => invalidateApp(queries, appID),
  });

  // R-022: re-detection is explicit, never automatic. Offered here because a
  // failure is exactly when somebody has just fixed the thing that caused it —
  // made the repository reachable, corrected the address — and wants Pando to
  // look again.
  const rerun = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/detection/rerun`),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'detection'] }),
  });

  // The winning bid's card, in outline. Not "Reading the repository": that is
  // what detection says while it runs, and for an app detected last week it
  // would claim work that is not happening.
  if (detection.isPending) {
    // SkeletonText announces itself as loading, so no wrapper says it again.
    return (
      <Card padding="md">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          <LineSkeleton width="26ch" font="var(--type-h4)" />
          <SkeletonText lines={3} />
        </div>
      </Card>
    );
  }
  if (detection.isError) return <Failure error={detection.error} />;

  const { status, detection: proposal, answers } = detection.data;

  if (status === 'running') return <Quiet>Reading the repository.</Quiet>;

  // A failure left the screen saying "Reading the repository." for ever.
  //
  // The server records the reason precisely so somebody coming back later can
  // find out what happened, and nothing read it: the status was neither
  // 'running' nor 'blocked', so this fell through to rendering a proposal that
  // does not exist. An app stuck on a progress message is worse than an error,
  // because there is nothing to act on and no reason to stop waiting.
  if (status === 'failed') {
    return (
      <DetectionFailed
        error={detection.data.detection.error}
        onRetry={canEdit ? () => rerun.mutate() : undefined}
        retrying={rerun.isPending}
      />
    );
  }

  if (status === 'blocked') {
    return <Blocked proposal={proposal} />;
  }

  const asked = (proposal.questions ?? []).filter((q) => !q.deferred);
  const unanswered = asked.filter((q) => !(answers ?? {})[q.key]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)' }}>
      <WinningBid candidate={proposal.winning_bid} />

      {/* After the winning bid, before the questions: it changed the plan the
          person is about to accept, and it may have answered some questions. */}
      <Screening outcome={proposal.screening} />

      {asked.length > 0 && (
        <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          <div>
            <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>
              {asked.length === 1 ? 'One question' : `${asked.length} questions`}
            </h4>
            <p
              style={{
                font: 'var(--type-body-ui)',
                color: 'var(--ink-secondary)',
                margin: 'var(--space-1) 0 0',
              }}
            >
              Pando asks only when it genuinely can&rsquo;t work something out. Copy a question into
              the tool that wrote this app and paste its answer back.
            </p>
          </div>

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
        </section>
      )}

      <Dependencies proposal={proposal} />

      <Variables proposal={proposal} />

      <Warnings proposal={proposal} />

      <RunnersUp candidates={proposal.runners_up ?? []} />

      {canEdit && (
        <section
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-3)',
            paddingTop: 'var(--space-5)',
            borderTop: 'var(--border-width) solid var(--rule)',
          }}
        >
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            {reviewed
              ? 'This app is already configured. Using this takes the build and the workloads from the repository and keeps what you set yourself.'
              : 'Accepting saves this as the app’s configuration. It doesn’t deploy anything.'}
          </p>
          <Button
            variant="primary"
            disabled={unanswered.length > 0 || accept.isPending}
            // An app that is already configured gets a confirmation rather than
            // a one-click replace. This used to accept immediately, and the
            // configuration it discarded — environment variables, dependencies,
            // storage — was gone from the pinned spec with nothing said.
            onClick={() => (reviewed ? setReplacing(true) : accept.mutate())}
            style={{ alignSelf: 'flex-start' }}
          >
            {reviewed ? 'Replace configuration' : 'Accept configuration'}
          </Button>
          {unanswered.length > 0 && (
            <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
              {unanswered.length === 1
                ? 'One question still needs an answer.'
                : `${unanswered.length} questions still need answers.`}
            </p>
          )}
          {accept.isError && <Failure error={accept.error} />}
        </section>
      )}

      <Dialog
        open={replacing}
        title="Replace this app's configuration?"
        // Says what actually happens. It used to say the opposite — that
        // settings were not carried over — which was true until accepting
        // started merging them, and a dialog describing the old behavior is
        // worse than no dialog: somebody reads it and does not press the
        // button they needed.
        description="Pando will use what it just found for the build and the workloads. Environment variables you set, how each dependency is filled, and storage you added are kept."
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
    </div>
  );
}

function WinningBid({ candidate }: { candidate: Candidate }) {
  return (
    <Card padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
          <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>How Pando will build this</h4>
          <Tag mono>{candidate.strategy}</Tag>
        </div>

        {/* Evidence is why Pando thinks what it thinks (R-102). Shown as the
            API worded it — this is the reasoning, not a summary of it. */}
        <ul
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-1)',
            margin: 0,
            paddingLeft: 'var(--space-4)',
          }}
        >
          {(candidate.evidence ?? []).map((line) => (
            <li key={line} style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
              {line}
            </li>
          ))}
        </ul>
      </div>
    </Card>
  );
}

export function QuestionCard({
  question,
  answer,
  onAnswer,
  saving,
}: {
  question: Question;
  answer: string | undefined;
  /** Absent for somebody who may read the question but not answer it. */
  onAnswer?: (value: string) => void;
  saving: boolean;
}) {
  const [value, setValue] = useState(answer ?? '');
  const [copied, setCopied] = useState(false);

  async function copy() {
    // The prompt exactly, and nothing else. Anything prepended would arrive in
    // the assistant as part of the question.
    await navigator.clipboard.writeText(question.prompt);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2_000);
  }

  return (
    <Card padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
          {/* Verbatim. Not shortened, not re-worded, not split up. */}
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0, flex: 1 }}>
            {question.prompt}
          </p>
          <IconButton label={copied ? 'Copied' : 'Copy this question'} onClick={copy}>
            <Icon name={copied ? 'check' : 'copy'} size={16} />
          </IconButton>
        </div>

        {question.why && (
          <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
            {question.why}
          </p>
        )}

        <div style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-end' }}>
          {question.kind === 'choice' ? (
            <Select
              label="Answer"
              options={[
                { value: '', label: 'Choose one' },
                ...(question.options ?? []).map((option) => ({ value: option, label: option })),
              ]}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              disabled={!onAnswer}
              style={{ flex: 1 }}
            />
          ) : (
            <Input
              label="Answer"
              mono={question.kind === 'port' || question.kind === 'path'}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              disabled={!onAnswer}
              style={{ flex: 1 }}
            />
          )}
          {onAnswer && (
            <Button onClick={() => onAnswer(value)} disabled={!value || saving}>
              Save answer
            </Button>
          )}
        </div>
      </div>
    </Card>
  );
}

/**
 * What this app needs beside itself, before anybody accepts it.
 *
 * A required slot with nothing filling it refuses the deploy (R-132) — and
 * until now the first anybody heard of it was that refusal, after accepting,
 * from a button on another screen. It belongs in the review: this is the page
 * where somebody reads how their app will run, and "it needs a PostgreSQL
 * database, and Pando will run one inside it" is part of how it will run.
 */
function Dependencies({ proposal }: { proposal: Proposal }) {
  const slots = proposal.draft_spec?.slots ?? [];
  if (slots.length === 0) return null;

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>What this app needs</h4>
      <Quiet>
        Things the app expects to find beside it. Anything required and unfilled is refused at
        deploy rather than started and left to fail on a connection nobody made.
      </Quiet>

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          columns={[
            { key: 'key', header: 'Variable', width: 'minmax(0,26ch)', mono: true },
            {
              key: 'type',
              header: 'Kind',
              width: '18ch',
              render: (row: Slot) => SLOT_NAMES[row.type] ?? row.type,
            },
            {
              key: 'resolution',
              header: 'Filled by',
              width: 'minmax(0,30ch)',
              render: (row: Slot) => fills(row),
            },
          ]}
          rows={slots}
        />
      </div>
    </section>
  );
}

export const SLOT_NAMES: Record<string, string> = {
  postgres: 'PostgreSQL',
  mysql: 'MySQL',
  redis: 'Redis',
  s3: 'object storage',
  smtp: 'email',
  unknown: 'a value',
};

/** The same sentences the settings screen uses, so one app reads one way. */
export function fills(slot: Slot) {
  if (!slot.resolution) {
    return slot.required ? (
      <StatusIndicator status="failed" label="You choose after accepting" />
    ) : (
      <StatusIndicator status="stopped" label="Nothing, and optional" />
    );
  }
  switch (slot.resolution.mode) {
    case 'provisioned':
      return 'Pando runs one inside this app';
    case 'bound':
      return slot.resolution.target ?? 'Something already running';
    case 'literal':
      return 'A value you set';
    default:
      return slot.resolution.mode;
  }
}

export interface Slot {
  key: string;
  type: string;
  required?: boolean;
  resolution?: { mode: string; target?: string };
}

/**
 * The variables this app reads, which Pando read the names of and not the
 * values.
 *
 * `.env.example` is a list of names: an app's own author writing down what it
 * reads. The values are that app's keys and passwords, and Pando has none of
 * them. Saying so here, before anybody accepts, is the difference between
 * knowing what is left to do and finding eight empty rows later.
 */
function Variables({ proposal }: { proposal: Proposal }) {
  const rows = (proposal.draft_spec?.workloads ?? []).flatMap((w) =>
    (w.env ?? [])
      .filter((e) => !e.secret_ref && !e.slot_ref && (e.value ?? '') === '')
      .map((e) => ({ key: e.key, workload: w.name })),
  );
  if (rows.length === 0) return null;

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>
        Variables with no value
      </h4>
      <Quiet>
        Declared by the app and not filled in. Pando reads the names from the repository and cannot
        know the values. Set them under Settings, on the app&rsquo;s environment, after accepting —
        a key or a password is stored as a secret and never appears in an exported configuration.
      </Quiet>

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          columns={[
            { key: 'key', header: 'Name', width: 'minmax(0,30ch)', mono: true },
            { key: 'workload', header: 'Part of the app', width: '18ch', muted: true },
          ]}
          rows={rows}
        />
      </div>
    </section>
  );
}

function Warnings({ proposal }: { proposal: Proposal }) {
  const warnings = proposal.draft_spec?.warnings ?? [];
  if (warnings.length === 0) return null;

  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      {warnings.map((warning) => (
        <InlineWarning key={warning.code + warning.message} code={warning.code}>
          {warning.message}
        </InlineWarning>
      ))}
    </section>
  );
}

export function RunnersUp({ candidates }: { candidates: Candidate[] }) {
  const [open, setOpen] = useState(false);
  if (candidates.length === 0) return null;

  return (
    <section>
      <button
        onClick={() => setOpen(!open)}
        style={{
          border: 'none',
          background: 'transparent',
          padding: 0,
          cursor: 'pointer',
          font: 'var(--type-body-ui)',
          color: 'var(--ink-secondary)',
        }}
      >
        {open ? 'Hide' : 'Show'} what else Pando considered ({candidates.length})
      </button>

      {open && (
        <div
          style={{
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-3)',
            marginTop: 'var(--space-3)',
          }}
        >
          {candidates.map((candidate) => (
            <div
              key={candidate.detector + candidate.strategy}
              style={{
                display: 'flex',
                flexDirection: 'column',
                gap: 'var(--space-1)',
                paddingBottom: 'var(--space-3)',
                borderBottom: 'var(--border-width) solid var(--rule)',
              }}
            >
              <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
                <Tag mono>{candidate.strategy}</Tag>
              </div>
              {(candidate.evidence ?? []).map((line) => (
                <span
                  key={line}
                  style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}
                >
                  {line}
                </span>
              ))}
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

export function Blocked({ proposal }: { proposal: Proposal }) {
  // R-099: the reason is the most useful thing Pando has, and it is shown
  // rather than replaced with a generic failure. The compose importer produces
  // a remedy naming the lines to change.
  const blocked = proposal.blocked;

  // The entries the summary is summarizing.
  //
  // They travel in the envelope's details and nothing rendered them, so the
  // screen said "docker-compose.yml uses 1 construct(s) that cannot run inside
  // the boundary" and then "Each entry above says why" — above which there was
  // one line naming the construct and nothing saying why. The why is the whole
  // reason the importer writes one per rejection (R-105).
  const rejected = rejectedEntries(blocked?.details);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: '76ch' }}>
      <div>
        <h4 style={{ font: 'var(--type-h4)', margin: 0, color: 'var(--marker-deep)' }}>
          Pando can&rsquo;t run this app as it&rsquo;s written
        </h4>
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 'var(--space-2) 0 0' }}>
          {blocked?.message}
        </p>
      </div>

      {rejected.length > 0 && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          {rejected.map((entry) => (
            <div
              key={entry.service + entry.construct}
              style={{
                borderTop: 'var(--border-width) solid var(--rule)',
                paddingTop: 'var(--space-3)',
              }}
            >
              <div style={{ font: 'var(--type-code-sm)', color: 'var(--ink)' }}>
                {entry.service} &middot; {entry.construct}
              </div>
              <p
                style={{
                  font: 'var(--type-body-ui)',
                  color: 'var(--ink-secondary)',
                  margin: 'var(--space-2) 0 0',
                }}
              >
                {entry.reason}
              </p>
            </div>
          ))}
        </div>
      )}

      {blocked?.remedy && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          {blocked.remedy}
        </p>
      )}
      {proposal.trial_log && <CodeBlock title="What Pando saw" lines={proposal.trial_log} />}
    </div>
  );
}


export function DetectionFailed({
  error,
  onRetry,
  retrying,
}: {
  error?: { message?: string; remedy?: string };
  /** Absent for somebody who cannot ask Pando to look again. */
  onRetry?: () => void;
  retrying: boolean;
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: '68ch' }}>
      <div>
        <h4 style={{ font: 'var(--type-h4)', margin: 0, color: 'var(--marker-deep)' }}>
          Pando couldn&rsquo;t read this app
        </h4>
        {/* The server's sentence, as written. It is held to the R-105 standard
            and paraphrasing it here would undo that in the UI layer. */}
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 'var(--space-2) 0 0' }}>
          {error?.message ?? 'Something went wrong while reading the repository.'}
        </p>
      </div>
      {error?.remedy && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          {error.remedy}
        </p>
      )}
      {onRetry && (
        <div>
          <Button variant="secondary" disabled={retrying} onClick={onRetry}>
            {retrying ? 'Looking again' : 'Try again'}
          </Button>
        </div>
      )}
    </div>
  );
}

function Quiet({ children }: { children: React.ReactNode }) {
  return <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{children}</p>;
}

export function Failure({ error }: { error: unknown }) {
  // The envelope's message is written to the R-105 standard, so it is shown as
  // the server wrote it. The remedy goes below it, which is where the design
  // system's voice rules put "and what to do".
  const failed = error instanceof RequestFailed ? error : null;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--marker-deep)', margin: 0 }}>
        {failed?.message ?? 'Something went wrong. Reload the page to try again.'}
      </p>
      {failed?.remedy && (
        <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
          {failed.remedy}
        </p>
      )}
    </div>
  );
}
