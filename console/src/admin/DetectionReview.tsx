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
// The pieces are shared: the onboarding page is the review before an app is
// accepted, and the Build card of Settings (SettingsGeneral.tsx) shows how
// Pando decided once it has been. Reading all of it is app.view. Answering,
// accepting and looking again are app.spec.edit, so without that verb the
// questions and any answers already given are shown, and nothing offers to
// change them.

import { useRef, useState } from 'react';
import { Button, Card, CodeBlock, Icon, IconButton, Input, Select, Tag } from '@design';

import { api, RequestFailed } from '@api/client';
import type { Candidate, Proposal, Question } from '@api/types.gen';
import { rejectedEntries } from './rejections';
import { copyOrSelect } from '../ui/CopyField';

/** Why a detection failed: the server's envelope. */
export interface DetectionError {
  message?: string;
  remedy?: string;
  /** A build that failed while Pando read the app carries its log (R-107). */
  details?: { build_log?: string };
}

export interface DetectionResponse {
  status: string;
  detection: Proposal & {
    // Set only when detection failed outright — a repository that could not be
    // cloned, a trial run that could not start. Distinct from `blocked`, which
    // is a proposal that succeeded and found an app Pando cannot run: one is
    // "Pando never got to look", the other is "Pando looked and the answer is
    // no". They read differently and lead to different next steps.
    error?: DetectionError;
    // Where a running detection has got to: fetching, detecting, building,
    // trying, scanning, screening. Set only while status is 'running', and the
    // proposal fields are present only once that stage has produced them — so
    // while running, any of them may be absent (see AppOnboarding.tsx).
    stage?: string;
    // Every step under way while it runs. The scan runs beside the build and
    // beside the trial run, so there can be two.
    running?: string[];
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
  const prompt = useRef<HTMLParagraphElement>(null);

  async function copy() {
    // The prompt exactly, and nothing else. Anything prepended would arrive in
    // the assistant as part of the question.
    // Selected instead where the browser will not copy.
    if ((await copyOrSelect(question.prompt, prompt.current)) !== 'copied') return;
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2_000);
  }

  return (
    <Card padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
          {/* Verbatim. Not shortened, not re-worded, not split up. */}
          <p ref={prompt} style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0, flex: 1 }}>
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

export function RunnersUp({ candidates }: { candidates: Candidate[] }) {
  const [open, setOpen] = useState(false);
  if (candidates.length === 0) return null;

  return (
    <section>
      <button
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        style={{
          border: 'none',
          background: 'transparent',
          padding: 0,
          cursor: 'pointer',
          font: 'var(--type-body-ui)',
          color: 'var(--ink-secondary)',
        }}
      >
        Other candidates ({candidates.length})
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
      {proposal.trial_log && <CodeBlock title="Trial run output" lines={proposal.trial_log} />}
    </div>
  );
}


export function DetectionFailed({
  error,
  onRetry,
  retrying,
}: {
  error?: DetectionError;
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
      {/* The build's own output is the answer when a build failed (R-107). */}
      {error?.details?.build_log && <CodeBlock title="Build log" lines={error.details.build_log} />}
      {onRetry && (
        <div>
          <Button variant="secondary" disabled={retrying} onClick={onRetry}>
            {retrying ? 'Trying again' : 'Try again'}
          </Button>
        </div>
      )}
    </div>
  );
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
