// The logic behind the onboarding page's discovery view (AppOnboarding.tsx):
// which steps Pando has taken and what each found, how far along the terrain
// has been drawn, and how the review groups its questions. Plain functions, so
// the mapping is tested rather than eyeballed.
//
// Every step and every finding comes from the detection the server returned.
// The design handoff's prototype ran on canned data; this is the same view
// driven by real stages (design 08 §1.3). Several steps finish on the one
// event that ends the auction, and the page reveals their findings on a short
// stagger — the findings are real, only their pacing is the page's.

import type { AppSpec, Candidate, Proposal, Question, Report, Source } from '@api/types.gen';
import { describeAmendment } from './screeningText';

/** Where a running detection is. `stage` is set only while it runs. */
type Stage = 'fetching' | 'detecting' | 'trying' | 'scanning' | 'screening';
const ORDER: Stage[] = ['fetching', 'detecting', 'trying', 'scanning', 'screening'];

type StepState = 'done' | 'current' | 'pending';

export interface Finding {
  key: string;
  value: string;
  /** Prose rather than machine text: shown in the UI face, not mono. */
  text?: boolean;
}

export interface DiscoveryStep {
  id: string;
  name: string;
  /** The headline while this step runs: what Pando is doing, as it does it. */
  active: string;
  state: StepState;
  /** One line once the step is done. */
  result?: string;
  failed?: boolean;
  ai?: boolean;
  findings: Finding[];
}

/** What the steps are read from: the detection response, loosely. */
export interface DiscoveryInput {
  status: string;
  stage?: string;
  proposal: Partial<Proposal>;
  source?: Source;
  commit?: string;
  /** The app's security report, once the scan has run (GET /security). */
  security?: Report;
}

const STRATEGY: Record<string, string> = {
  dockerfile: 'Dockerfile',
  compose: 'Compose file',
  buildpack: 'Buildpack',
  static: 'Static site',
  prebuilt: 'Published image',
};

export function strategyLabel(strategy: string | undefined): string {
  if (!strategy || strategy === 'unknown') return 'Unknown';
  return STRATEGY[strategy] ?? strategy;
}

const SLOT_LABEL: Record<string, string> = {
  postgres: 'PostgreSQL',
  mysql: 'MySQL',
  redis: 'Redis',
  s3: 'S3 storage',
  smtp: 'Mail server',
};

export function slotLabel(type: string): string {
  return SLOT_LABEL[type] ?? 'A service';
}

type SpecSlot = NonNullable<AppSpec['slots']>[number];
type SpecWorkload = NonNullable<AppSpec['workloads']>[number];

/**
 * Whether a slot is a service the app talks to — a database, a cache — rather
 * than a value somebody has to supply. The compose importer declares a
 * variable named with no value in `.env.example` as a slot of type `unknown`:
 * it blocks the deploy until it is set (R-132), but it is not a service, and
 * listing it beside PostgreSQL made a key read like a database.
 */
export function isService(slot: Pick<SpecSlot, 'type'>): boolean {
  return Boolean(slot.type) && slot.type !== 'unknown';
}

/**
 * The image a service runs, when detection read one: the compose importer's
 * evidence names it ("compose service "db" runs docker.io/library/postgres:16-alpine").
 */
export function serviceImage(slot: SpecSlot): string | undefined {
  for (const line of slot.evidence ?? []) {
    const match = /\bruns (\S+)/.exec(line);
    if (match) return match[1]!.replace(/^docker\.io\/library\//, '').replace(/^docker\.io\//, '');
  }
  return undefined;
}

/** How a service will be provided, in the words the settings screen uses. */
export function serviceProvision(slot: SpecSlot): { status: 'info' | 'building' | 'stopped'; label: string } {
  switch (slot.resolution?.mode) {
    case 'provisioned':
      return { status: 'info', label: 'Pando creates it' };
    case 'bound':
      return { status: 'info', label: 'Connects to one you have' };
    case 'literal':
      return { status: 'info', label: 'You set it' };
    default:
      return slot.required
        ? { status: 'building', label: 'Choose after accepting' }
        : { status: 'stopped', label: 'Optional' };
  }
}

function cleanPath(path: string): string {
  return path
    .split('/')
    .filter((part) => part !== '' && part !== '.')
    .join('/');
}

/** A Dockerfile's CMD as detection recorded it, exec form unwrapped. */
function commandText(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed.startsWith('[')) {
    try {
      const parts = JSON.parse(trimmed) as unknown;
      if (Array.isArray(parts) && parts.every((p) => typeof p === 'string')) return parts.join(' ');
    } catch {
      // Not JSON after all: shown as written.
    }
  }
  return trimmed;
}

/**
 * What a workload runs when it starts.
 *
 * A workload built from a Dockerfile usually has no command in the spec: the
 * Dockerfile's own CMD runs, and "the image's own command" said nothing about
 * what that is. The Dockerfile detector reads the CMD and records it in its
 * evidence, naming the Dockerfile it read, so the command is shown when the
 * workload builds from that same file. `text` marks a description rather than
 * a command.
 */
export function startCommand(
  proposal: Partial<Proposal>,
  spec: AppSpec | undefined,
  workload: SpecWorkload,
): { value: string; text?: boolean } {
  const own = (workload.command ?? []).join(' ');
  if (own) return { value: own };
  if (workload.image) return { value: 'The image’s default command', text: true };

  const built = workload.build
    ? cleanPath(`${workload.build.context ?? ''}/${workload.build.dockerfile || 'Dockerfile'}`)
    : cleanPath(spec?.build?.dockerfile ?? '');
  if (built) {
    const readings: Array<{ candidate: Candidate | undefined; dockerfile?: string }> = [
      { candidate: proposal.winning_bid, dockerfile: proposal.draft_spec?.build?.dockerfile },
      ...(proposal.runners_up ?? []).map((c) => ({ candidate: c, dockerfile: c.spec?.build?.dockerfile })),
    ];
    for (const { candidate, dockerfile } of readings) {
      if (candidate?.strategy !== 'dockerfile' || cleanPath(dockerfile ?? '') !== built) continue;
      const cmd = (candidate.evidence ?? []).find((line) => line.startsWith('CMD '));
      if (cmd) return { value: commandText(cmd.slice(4)) };
    }
  }
  return { value: 'From its Dockerfile', text: true };
}

/** How far a detection has got: 0 fetching … 3 screening, 4 finished. */
function stageIndex(status: string, stage: string | undefined): number {
  if (status !== 'running') return ORDER.length;
  // A stage this console does not know is read as the first: under-claiming
  // progress is harmless, claiming a step finished that is not is not.
  return Math.max(0, ORDER.indexOf((stage ?? 'fetching') as Stage));
}

/**
 * Which AI function detection needs, mirroring core/detection.needed (R-336):
 * a repair when the trial run crashed or no detector could read the
 * repository, answers when a question is open, nothing otherwise. Used only to
 * name the AI step while it runs, before the outcome says which it was.
 */
export function neededFunction(proposal: Partial<Proposal>): 'repair_plan' | 'answer_questions' | undefined {
  if (proposal.trial?.crashed || proposal.winning_bid?.strategy === 'unknown') return 'repair_plan';
  if ((proposal.questions ?? []).some((q) => !q.deferred)) return 'answer_questions';
  return undefined;
}

function short(commit: string | undefined): string | undefined {
  return commit ? commit.slice(0, 7) : undefined;
}

function list(names: string[]): string {
  if (names.length <= 1) return names.join('');
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? `${n} ${one}` : `${n} ${many}`;
}

/** Every variable the plan declares, in workload order, once each. */
function envNames(spec: AppSpec | undefined): { own: string[]; filled: string[] } {
  const own: string[] = [];
  const filled: string[] = [];
  for (const w of spec?.workloads ?? []) {
    for (const e of w.env ?? []) {
      const into = e.slot_ref ? filled : own;
      if (!into.includes(e.key)) into.push(e.key);
    }
  }
  return { own, filled };
}

function readStep(input: DiscoveryInput, at: number): DiscoveryStep {
  const source = input.source;
  const commit = short(input.commit || input.proposal.commit);
  const findings: Finding[] = [];
  let name = 'Read the repo';
  let active = 'Reading the repo';
  if (source?.type === 'image') {
    name = 'Read the image';
    active = 'Reading the image';
    if (source.image) findings.push({ key: 'Image', value: source.image });
  } else if (source?.type === 'upload') {
    findings.push({ key: 'Source', value: 'An uploaded archive', text: true });
  } else if (source) {
    if (source.url) findings.push({ key: 'Repository', value: source.url });
    if (source.ref) findings.push({ key: 'Branch', value: source.ref });
    if (source.subdir) findings.push({ key: 'Folder', value: source.subdir });
  }
  if (commit) findings.push({ key: 'Commit', value: commit });
  return {
    id: 'read',
    name,
    active,
    state: at > 0 ? 'done' : 'current',
    result: at > 0 ? (commit ? `At ${commit}` : 'Done') : undefined,
    findings: at > 0 ? findings : findings.filter((f) => f.key !== 'Commit'),
  };
}

function stackStep(bid: Candidate | undefined, at: number): DiscoveryStep {
  const done = at > 1 && Boolean(bid);
  const unknown = !bid?.strategy || bid.strategy === 'unknown';
  const findings: Finding[] = [];
  if (done && bid) {
    findings.push({ key: 'Approach', value: strategyLabel(bid.strategy), text: true });
    (bid.evidence ?? []).forEach((line, i) => findings.push({ key: i === 0 ? 'Because' : '', value: line, text: true }));
  }
  return {
    id: 'stack',
    name: 'Work out how it’s built',
    active: 'Reading the code',
    state: done ? 'done' : at === 1 ? 'current' : 'pending',
    result: done ? (unknown ? 'No detector could read it' : strategyLabel(bid?.strategy)) : undefined,
    failed: done && unknown,
    findings,
  };
}

function runsStep(proposal: Partial<Proposal>, spec: AppSpec | undefined, at: number): DiscoveryStep {
  const done = at > 1;
  const workloads = spec?.workloads ?? [];
  const slots = (spec?.slots ?? []).filter(isService);
  const findings: Finding[] = [];
  for (const w of workloads) {
    const start = startCommand(proposal, spec, w);
    const port = (w.ports ?? [])[0]?.number;
    findings.push({
      key: w.name,
      value: [start.value, port ? `on :${port}` : ''].filter(Boolean).join(' '),
      text: start.text,
    });
  }
  for (const s of slots) {
    const image = serviceImage(s);
    findings.push({ key: slotLabel(s.type), value: image ?? `Reached at ${s.key}`, text: !image });
  }
  for (const w of workloads) {
    for (const m of w.mounts ?? []) findings.push({ key: 'Keeps data in', value: m.path });
  }
  const names = [...workloads.map((w) => w.name), ...slots.map((s) => slotLabel(s.type))];
  return {
    id: 'runs',
    name: 'Find processes and services',
    active: 'Looking for what it runs on',
    state: done ? 'done' : 'pending',
    result: done ? (names.length ? list(names) : 'Nothing found') : undefined,
    findings: done ? findings : [],
  };
}

function varsStep(spec: AppSpec | undefined, at: number): DiscoveryStep {
  const done = at > 1;
  const { own, filled: slotted } = envNames(spec);
  // A slot-backed variable is filled by Pando only when the slot is a
  // service; one of type `unknown` is a value somebody has to set.
  const services = new Set((spec?.slots ?? []).filter(isService).map((s) => s.key));
  const filled = slotted.filter((key) => services.has(key));
  const needed = slotted.filter((key) => !services.has(key));
  const findings: Finding[] = [];
  if (own.length) findings.push({ key: 'Reads', value: own.join(' ') });
  if (filled.length) findings.push({ key: 'Pando fills', value: filled.join(' ') });
  if (needed.length) findings.push({ key: 'You set', value: needed.join(' ') });
  const total = own.length + slotted.length;
  return {
    id: 'vars',
    name: 'Collect variables',
    active: 'Collecting variables',
    state: done ? 'done' : 'pending',
    result: done ? (total ? `${total} found${filled.length ? `, Pando fills ${filled.length}` : ''}` : 'None found') : undefined,
    findings: done ? findings : [],
  };
}

function trialStep(proposal: Partial<Proposal>, at: number): DiscoveryStep | undefined {
  const trial = proposal.trial;
  if (at !== 2 && !trial?.ran) return undefined;
  const done = at > 2;
  const findings: Finding[] = [];
  let result: string | undefined;
  if (done && trial?.ran) {
    const ports = trial.observed_ports ?? [];
    if (trial.crashed) {
      findings.push({ key: 'Result', value: 'The app exited with an error', text: true });
      result = 'Exited with an error';
    } else if (!trial.started) {
      findings.push({ key: 'Result', value: 'The app did not start', text: true });
      result = 'Didn’t start';
    } else {
      findings.push({ key: 'Result', value: 'The app started', text: true });
      result = ports.length ? `Started on :${ports.join(', :')}` : 'Started';
    }
    if (ports.length) findings.push({ key: 'Opened', value: ports.map((p) => `:${p}`).join(' ') });
    for (const path of trial.observed_writes ?? []) findings.push({ key: 'Wrote to', value: path });
  }
  return {
    id: 'trial',
    name: 'Try running it',
    active: 'Starting it to watch',
    state: done ? 'done' : 'current',
    result,
    failed: done && Boolean(trial?.crashed),
    findings,
  };
}

/**
 * The security scan (R-310), as a step of the plan: shown while the scan runs,
 * and once there is a scan to report. An install with no scanner has neither,
 * and no step.
 */
function scanStep(security: Report | undefined, at: number): DiscoveryStep | undefined {
  const scan = security?.scan;
  if (at !== 3 && !(at > 3 && scan)) return undefined;
  const step: DiscoveryStep = {
    id: 'scan',
    name: 'Scan for known vulnerabilities',
    active: 'Scanning for vulnerabilities',
    state: at > 3 ? 'done' : 'current',
    findings: [],
  };
  if (at <= 3 || !scan || !security) return step;

  if (scan.error) {
    step.result = 'Couldn’t scan';
    step.findings.push({ key: 'Why', value: scan.error, text: true });
    return step;
  }
  const counts = security.counts;
  const total = counts.critical + counts.high + counts.medium + counts.low + counts.unknown;
  if (scan.score !== undefined && scan.score !== null) step.findings.push({ key: 'Score', value: `${scan.score} / 100` });
  const severities: Array<[string, number]> = [
    ['Critical', counts.critical],
    ['High', counts.high],
    ['Medium', counts.medium],
    ['Low', counts.low],
  ];
  for (const [label, n] of severities) if (n > 0) step.findings.push({ key: label, value: String(n) });
  const worst = (security.worst ?? []).slice(0, 3).map((f) => f.id);
  if (worst.length) step.findings.push({ key: 'Worst', value: worst.join(' ') });
  step.result = total === 0 ? 'Nothing known' : plural(total, 'vulnerability', 'vulnerabilities');
  step.failed = security.standing.verdict === 'insecure';
  return step;
}

/** Skip codes that say nothing: no adapter, nothing needed, blocked (R-335, R-336). */
const QUIET = new Set(['not_configured', 'not_needed', 'blocked']);

function aiStep(proposal: Partial<Proposal>, stage: string | undefined, at: number): DiscoveryStep | undefined {
  const outcome = proposal.screening;
  const running = at === 4 && stage === 'screening';
  const visible = outcome && (outcome.ran || (outcome.skip_code && !QUIET.has(outcome.skip_code)));
  if (!running && !visible) return undefined;

  const fn = outcome?.function || neededFunction(proposal);
  const repair = fn === 'repair_plan';
  const step: DiscoveryStep = {
    id: 'ai',
    ai: true,
    name: repair ? 'Review the failed plan' : 'Answer what it can',
    active: repair ? 'AI is looking at the failed plan' : 'AI is answering what it can',
    state: running ? 'current' : 'done',
    findings: [],
  };
  if (running || !outcome) return step;

  if (!outcome.ran) {
    step.result = 'Didn’t run';
    step.findings.push({ key: 'Why', value: outcome.skipped ?? 'The AI adapter didn’t finish.', text: true });
    return step;
  }

  const answers = Object.entries(outcome.answers ?? {});
  const changes = (outcome.applied ?? []).filter((a) => a.amendment.kind !== 'answer_question');
  for (const change of changes) {
    step.findings.push({ key: 'Changed', value: describeAmendment(change.amendment), text: true });
  }
  for (const [key, value] of answers) step.findings.push({ key: questionName(key), value });
  const files = outcome.files_read ?? [];
  if (files.length) step.findings.push({ key: 'Read', value: files.join(' ') });

  if (repair) {
    step.result = changes.length ? `Changed ${plural(changes.length, 'thing', 'things')}` : 'Found nothing to change';
  } else {
    const asked = (proposal.questions ?? []).filter((q) => !q.deferred).length;
    step.result = `Answered ${answers.length} of ${asked}`;
  }
  return step;
}

/** The steps, in the order they run. Only steps that apply are listed. */
export function discoverySteps(input: DiscoveryInput): DiscoveryStep[] {
  const at = stageIndex(input.status, input.stage);
  const proposal = input.proposal;
  const spec = proposal.draft_spec as AppSpec | undefined;
  const steps: DiscoveryStep[] = [
    readStep(input, at),
    stackStep(proposal.winning_bid, at),
    runsStep(proposal, spec, at),
    varsStep(spec, at),
  ];
  const trial = trialStep(proposal, at);
  if (trial) steps.push(trial);
  const scan = scanStep(input.security, at);
  if (scan) steps.push(scan);
  const ai = aiStep(proposal, input.stage, at);
  if (ai) steps.push(ai);
  return steps;
}

/**
 * The steps as shown, when the page paces them.
 *
 * A small repository is read in a second or two, and the auction's three steps
 * finish on one server event, so the list, the tallies and the terrain all
 * jumped to the end at once. The page reveals steps one at a time instead:
 * `revealed` steps are shown done, the next is shown under way with its
 * findings rising in, and the rest are not shown yet. Nothing is invented — a
 * step is never shown done before the server says it is, only later.
 */
export function paced(steps: DiscoveryStep[], revealed: number): DiscoveryStep[] {
  return steps.map((step, i) => {
    if (i < revealed) return step;
    if (i === revealed) return { ...step, state: 'current', result: undefined, failed: false };
    return { ...step, state: 'pending', result: undefined, failed: false, findings: [] };
  });
}

/** How many steps the server has finished, in order from the first. */
export function finishedCount(steps: DiscoveryStep[]): number {
  const i = steps.findIndex((s) => s.state !== 'done');
  return i < 0 ? steps.length : i;
}

/**
 * The least time a step stays under way on screen. Fast, but long enough to
 * see: the point is that each step visibly happens, not that anybody waits.
 */
export const MIN_STEP_MS = 700;
const PER_FINDING_MS = 90;
const MAX_STEP_MS = 1_400;

/** How long a finished step is held as under way while its findings rise in. */
export function dwellFor(step: DiscoveryStep): number {
  return Math.min(MAX_STEP_MS, MIN_STEP_MS + step.findings.length * PER_FINDING_MS);
}

/** The step under way, or undefined when every step is done. */
export function currentStep(steps: DiscoveryStep[]): DiscoveryStep | undefined {
  return steps.find((s) => s.state === 'current');
}

/**
 * How long a stage usually takes, for easing the terrain forward between the
 * server's reports. A guess only moves the drawing within the current step and
 * never past it: the step boundary is drawn only when the server says so.
 */
const EXPECTED_MS: Record<string, number> = {
  read: 4_000,
  scan: 8_000,
  stack: 3_000,
  trial: 30_000,
  ai: 40_000,
};

/** The largest share of a step the terrain covers before the step is done. */
const CAP = 0.9;

/**
 * How much of the terrain is drawn, 0 to 1: the finished steps, plus an eased
 * share of the current one by how long it has been running.
 */
export function progress(steps: DiscoveryStep[], currentForMs: number, expectedMs?: number): number {
  if (steps.length === 0) return 0;
  let done = 0;
  for (const step of steps) {
    if (step.state === 'done') done += 1;
    else if (step.state === 'current') {
      const expected = expectedMs ?? EXPECTED_MS[step.id] ?? 5_000;
      done += CAP * (1 - Math.exp(-Math.max(0, currentForMs) / expected));
    }
  }
  return Math.min(1, done / steps.length);
}

/** Where each step ends along the terrain, 0 to 1, for its waypoint. */
export function stops(steps: DiscoveryStep[]): Array<{ x: number; reached: boolean; ai: boolean; failed: boolean }> {
  return steps.slice(0, -1).map((step, i) => ({
    x: (i + 1) / steps.length,
    reached: step.state === 'done',
    ai: Boolean(step.ai),
    failed: Boolean(step.failed),
  }));
}

/** Where along the terrain the AI step starts, or 1 when there is none. */
export function aiFrom(steps: DiscoveryStep[]): number {
  const i = steps.findIndex((s) => s.ai);
  return i < 0 ? 1 : i / steps.length;
}

// --- Questions ---------------------------------------------------------------

const QUESTION_NAMES: Record<string, string> = {
  primary_port: 'port',
  primary_service: 'main service',
  start_command: 'start command',
  dockerfile_path: 'Dockerfile',
  deployable_project: 'project folder',
  static_source: 'static files',
  build_strategy: 'build method',
  build_method: 'build method',
};

/** A question's short name, for "Still needed: port and start command." */
export function questionName(key: string): string {
  if (key.startsWith('build_arg.')) return key.slice('build_arg.'.length);
  return QUESTION_NAMES[key] ?? key;
}

/** The questions a person is asked: not the ones deferred to the trial run. */
export function askedQuestions(proposal: Partial<Proposal>): Question[] {
  return (proposal.questions ?? []).filter((q) => !q.deferred);
}

/**
 * The answer in effect for a question: what the person typed or saved, else
 * what an AI adapter suggested (R-338).
 */
export function answerFor(q: Question, saved: Record<string, string> | null | undefined, draft?: string): string {
  if (draft !== undefined) return draft;
  const own = (saved ?? {})[q.key];
  if (own) return own;
  return q.suggested?.value ?? '';
}

export type QuestionGroup = 'needs' | 'ai';

/**
 * Which group a question is shown in. Fixed by the server's data, not by what
 * the person has typed, so a card does not jump between groups mid-edit.
 */
export function groupOf(q: Question): QuestionGroup {
  return q.suggested ? 'ai' : 'needs';
}

export type AnswerState = 'needed' | 'ai' | 'answered';

export function answerState(q: Question, value: string): AnswerState {
  if (!value.trim()) return 'needed';
  if (q.suggested && value === q.suggested.value) return 'ai';
  return 'answered';
}

/** The questions still without an answer, given what is on the page. */
export function stillNeeded(
  questions: Question[],
  saved: Record<string, string> | null | undefined,
  drafts: Record<string, string>,
): Question[] {
  return questions.filter((q) => !answerFor(q, saved, drafts[q.key]).trim());
}

export function listNames(names: string[]): string {
  return list(names);
}

// --- The plan ------------------------------------------------------------------

/**
 * The spec the review shows as the plan: the reading the build-method answer
 * picks, when it picks another one, else the draft. The tie-break adopts a
 * runner-up's whole reading (detect.Proposal.chosen), so showing the winner's
 * plan beside an answer naming the other would describe an app nobody chose.
 */
export function planSpec(proposal: Partial<Proposal>, answers: Record<string, string>): AppSpec | undefined {
  const want = answers.build_strategy || answers.build_method;
  if (want && want !== proposal.winning_bid?.strategy) {
    const other = (proposal.runners_up ?? []).find((c) => c.strategy === want && c.spec);
    if (other?.spec) return other.spec;
  }
  return proposal.draft_spec as AppSpec | undefined;
}

// --- Compose notes ----------------------------------------------------------------

/** The warning code the compose importer writes for a construct it rewrote. */
export const COMPOSE_REWRITTEN = 'WARN_COMPOSE_CONSTRUCT_REWRITTEN';

export interface ComposeNote {
  service?: string;
  construct?: string;
  /** The first sentence of the explanation: what Pando did instead. */
  gist: string;
  /** The whole message, as the importer wrote it. */
  full: string;
}

/**
 * A compose rewrite, split for a compact list: the service, the construct, and
 * the first sentence of why. A compose file with a proxy and a database
 * produced eight paragraphs nobody read; the paragraph stays one hover away.
 * A message not in the importer's shape is kept whole.
 */
export function composeNote(message: string): ComposeNote {
  const match = /^In the compose service "([^"]+)", `([^`]+)` was not carried over as written\.\s*(.*)$/s.exec(
    message.trim(),
  );
  if (!match) return { gist: message, full: message };
  const rest = match[3] ?? '';
  const sentence = /^(.+?[.!?])(\s|$)/s.exec(rest);
  return { service: match[1], construct: match[2], gist: (sentence?.[1] ?? rest).trim(), full: message };
}

// --- Terrain --------------------------------------------------------------------

/** FNV-1a: the terrain is the same every time for the same app and commit. */
function hashString(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

/** mulberry32. */
function random(seed: number): () => number {
  let a = seed || 1;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * An elevation profile, 0 to 1 at its highest: one main hill, two to four
 * minor ones, and three low waves (design handoff, Terrain).
 */
export function elevation(seed: string): (x: number) => number {
  const r = random(hashString(seed));
  const hills = [{ c: 0.35 + r() * 0.4, w: 0.1 + r() * 0.08, a: 0.75 + r() * 0.15 }];
  const n = 2 + Math.floor(r() * 3);
  for (let i = 0; i < n; i++) hills.push({ c: 0.05 + r() * 0.9, w: 0.04 + r() * 0.1, a: 0.12 + r() * 0.35 });
  const waves = [0, 1, 2].map(() => ({ f: 20 + r() * 90, p: r() * 6.28, a: 0.01 + r() * 0.02 }));
  const raw = (x: number) => {
    let e = 0.05;
    for (const h of hills) e += h.a * Math.exp(-(((x - h.c) / h.w) ** 2));
    for (const w of waves) e += w.a * Math.sin(x * w.f + w.p);
    return Math.max(0.03, e);
  };
  let max = 0;
  for (let i = 0; i <= 400; i++) max = Math.max(max, raw(i / 400));
  return (x: number) => raw(x) / max;
}

/** Where the profile peaks, 0 to 1: where the summit mark stands. */
export function peakOf(elev: (x: number) => number): number {
  let peak = 0;
  let high = -1;
  for (let i = 0; i <= 400; i++) {
    const e = elev(i / 400);
    if (e > high) {
      high = e;
      peak = i / 400;
    }
  }
  return peak;
}
