import { describe, expect, it } from 'vitest';

import type { Proposal, Question, Report } from '@api/types.gen';
import {
  MIN_STEP_MS,
  aiFrom,
  answerFor,
  answerState,
  composeNote,
  discoverySteps,
  dwellFor,
  elevation,
  finishedCount,
  groupOf,
  isService,
  neededFunction,
  paced,
  peakOf,
  planSpec,
  progress,
  questionName,
  serviceImage,
  serviceProvision,
  startCommand,
  stillNeeded,
  stops,
} from './discovery';

const source = { type: 'git', url: 'https://github.com/acme/inventory.git', ref: 'main' };

const found: Partial<Proposal> = {
  commit: '3f9a2c1d',
  winning_bid: { detector: 'dockerfile', strategy: 'dockerfile', confidence: 0.9, evidence: ['Dockerfile at the root'] },
  draft_spec: {
    workloads: [
      {
        name: 'web',
        command: ['npm', 'start'],
        ports: [{ number: 3000 }],
        env: [{ key: 'PORT', value: '3000' }, { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' }],
        exposed: true,
        primary: true,
      },
    ],
    slots: [{ key: 'DATABASE_URL', type: 'postgres', required: true }],
  } as unknown as Proposal['draft_spec'],
};

describe('discoverySteps', () => {
  it('shows only reading the repo while fetching', () => {
    const steps = discoverySteps({ status: 'running', stage: 'fetching', proposal: {}, source });
    expect(steps.filter((s) => s.state !== 'pending').map((s) => s.id)).toEqual(['read']);
    expect(steps[0]!.state).toBe('current');
  });

  it('finishes the auction steps together, each with what it found', () => {
    const steps = discoverySteps({ status: 'ready', proposal: found, source, commit: '3f9a2c1d' });
    expect(steps.map((s) => s.id)).toEqual(['read', 'stack', 'runs', 'vars']);
    expect(steps.every((s) => s.state === 'done')).toBe(true);
    expect(steps[0]!.result).toBe('At 3f9a2c1');
    expect(steps[1]!.result).toBe('Dockerfile');
    expect(steps[2]!.findings).toContainEqual({ key: 'web', value: 'npm start on :3000', text: undefined });
    expect(steps[3]!.result).toBe('2 found, Pando fills 1');
  });

  it('adds a trial step only when a trial runs, and marks a crash as failed', () => {
    const crashed = { ...found, trial: { ran: true, crashed: true } };
    const steps = discoverySteps({ status: 'ready', proposal: crashed, source });
    const trial = steps.find((s) => s.id === 'trial');
    expect(trial?.failed).toBe(true);
    expect(discoverySteps({ status: 'ready', proposal: found, source }).some((s) => s.id === 'trial')).toBe(false);
  });

  it('adds an AI step only when AI was called (R-336)', () => {
    const quiet = { ...found, screening: { ran: false, skip_code: 'not_needed' } };
    expect(discoverySteps({ status: 'ready', proposal: quiet, source }).some((s) => s.ai)).toBe(false);

    const answered = {
      ...found,
      questions: [{ key: 'primary_port', prompt: 'Which port?', why: '', kind: 'port' }],
      screening: { ran: true, function: 'answer_questions', answers: { primary_port: '3000' }, files_read: ['server.js'] },
    };
    const ai = discoverySteps({ status: 'ready', proposal: answered, source }).find((s) => s.ai);
    expect(ai?.name).toBe('Answer what it can');
    expect(ai?.result).toBe('Answered 1 of 1');
    expect(ai?.findings).toContainEqual({ key: 'port', value: '3000' });
  });

  it('names a running AI step by what detection needs', () => {
    const crashed = { ...found, trial: { ran: true, crashed: true } };
    const steps = discoverySteps({ status: 'running', stage: 'screening', proposal: crashed, source });
    expect(steps.at(-1)?.name).toBe('Review the failed plan');
    expect(steps.at(-1)?.state).toBe('current');
  });
});

// Crewmate's shape: a Dockerfile winner whose CMD is only in its evidence, and
// a compose runner-up building that same Dockerfile beside a Postgres service,
// with keys from .env.example declared as slots of type unknown.
const crewmate: Partial<Proposal> = {
  winning_bid: {
    detector: 'dockerfile',
    strategy: 'dockerfile',
    confidence: 0.9,
    evidence: ['Dockerfile at repository root', 'EXPOSE 8080', 'CMD ["/crewmate"]'],
  },
  draft_spec: {
    build: { strategy: 'dockerfile', dockerfile: 'Dockerfile' },
    workloads: [{ name: 'web', primary: true, exposed: true }],
  } as unknown as Proposal['draft_spec'],
  runners_up: [
    {
      detector: 'compose',
      strategy: 'compose',
      confidence: 0.8,
      spec: {
        build: { strategy: 'compose', compose_file: 'docker-compose.yml' },
        workloads: [
          {
            name: 'app',
            build: { context: '.' },
            env: [
              { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' },
              { key: 'CREW_TOKEN_ENC_KEY', slot_ref: 'CREW_TOKEN_ENC_KEY' },
            ],
          },
          { name: 'proxy', image: 'docker.io/library/caddy:2-alpine' },
        ],
        slots: [
          {
            key: 'DATABASE_URL',
            type: 'postgres',
            required: true,
            evidence: ['compose service "db" runs docker.io/library/postgres:16-alpine'],
            resolution: { mode: 'provisioned' },
          },
          { key: 'CREW_TOKEN_ENC_KEY', type: 'unknown', required: true },
        ],
      } as unknown as Proposal['draft_spec'],
    },
  ],
};

describe('services and commands', () => {
  const compose = crewmate.runners_up![0]!.spec!;

  it('treats a slot of type unknown as a value, not a service', () => {
    expect(isService({ type: 'postgres' })).toBe(true);
    expect(isService({ type: 'unknown' })).toBe(false);
  });

  it('names the image a service runs, and how Pando provides it', () => {
    const [postgres] = compose.slots!;
    expect(serviceImage(postgres!)).toBe('postgres:16-alpine');
    expect(serviceProvision(postgres!)).toEqual({ status: 'info', label: 'Pando creates it' });
  });

  it('shows the Dockerfile’s CMD for a workload that builds from it', () => {
    const [app, proxy] = compose.workloads!;
    expect(startCommand(crewmate, compose, app!)).toEqual({ value: '/crewmate' });
    expect(startCommand(crewmate, crewmate.draft_spec, crewmate.draft_spec!.workloads![0]!)).toEqual({
      value: '/crewmate',
    });
    expect(startCommand(crewmate, compose, proxy!).text).toBe(true);
  });

  it('says so plainly when the command is not known', () => {
    const other = { ...compose.workloads![0]!, build: { context: 'worker' } };
    expect(startCommand(crewmate, compose, other)).toEqual({ value: 'From its Dockerfile', text: true });
  });

  it('keeps required values out of the services a step reports', () => {
    const steps = discoverySteps({ status: 'ready', proposal: { ...crewmate, draft_spec: compose }, source });
    const runs = steps.find((s) => s.id === 'runs')!;
    expect(runs.findings).toContainEqual({ key: 'PostgreSQL', value: 'postgres:16-alpine', text: false });
    expect(runs.findings.some((f) => f.key === 'CREW_TOKEN_ENC_KEY')).toBe(false);
    const vars = steps.find((s) => s.id === 'vars')!;
    expect(vars.findings).toContainEqual({ key: 'Pando fills', value: 'DATABASE_URL' });
    expect(vars.findings).toContainEqual({ key: 'You set', value: 'CREW_TOKEN_ENC_KEY' });
  });
});

describe('pacing', () => {
  const steps = discoverySteps({ status: 'ready', proposal: found, source });

  it('shows steps done one at a time, never ahead of the server', () => {
    const shown = paced(steps, 1);
    expect(shown.map((s) => s.state)).toEqual(['done', 'current', 'pending', 'pending']);
    expect(shown[1]!.result).toBeUndefined();
    expect(shown[2]!.findings).toEqual([]);
    expect(finishedCount(steps)).toBe(4);
    expect(finishedCount(discoverySteps({ status: 'running', stage: 'detecting', proposal: {}, source }))).toBe(1);
  });

  it('holds a step longer the more it found, within bounds', () => {
    expect(dwellFor({ ...steps[0]!, findings: [] })).toBe(MIN_STEP_MS);
    expect(dwellFor(steps[2]!)).toBeGreaterThan(MIN_STEP_MS);
    expect(dwellFor({ ...steps[0]!, findings: Array(50).fill({ key: '', value: '' }) })).toBeLessThanOrEqual(3_600);
  });
});

describe('the scan step (R-310)', () => {
  const report = {
    standing: { verdict: 'ok', score: 82, threshold: 60, scanned: '', stop_at: '' },
    scan: { id: 's', app_id: 'a', scanner_ref: 'scn', score: 82, findings: [] },
    counts: { critical: 0, high: 2, medium: 1, low: 0, unknown: 0 },
    worst: [{ id: 'CVE-2026-0001', severity: 'high', title: '', target: '' }],
    ignoring_unfixable: false,
  } as unknown as Report;

  it('is under way while the source is scanned', () => {
    const steps = discoverySteps({ status: 'running', stage: 'scanning', proposal: found, source });
    expect(steps.at(-1)).toMatchObject({ id: 'scan', state: 'current' });
  });

  it('reports the score and what it found once there is a scan', () => {
    const scan = discoverySteps({ status: 'ready', proposal: found, source, security: report }).find((s) => s.id === 'scan');
    expect(scan?.result).toBe('3 vulnerabilities');
    expect(scan?.findings).toContainEqual({ key: 'Score', value: '82 / 100' });
    expect(scan?.findings).toContainEqual({ key: 'High', value: '2' });
    expect(scan?.findings).toContainEqual({ key: 'Worst', value: 'CVE-2026-0001' });
  });

  it('does not score an image not yet built, and still shows what the source scan found (R-311)', () => {
    const sourceOnly = {
      ...report,
      standing: { ...report.standing, score: undefined },
      scan: { ...report.scan, score: 100, commit: 'abc123' },
      scope: 'source',
      awaiting_image: true,
    } as unknown as Report;
    const scan = discoverySteps({ status: 'ready', proposal: found, source, security: sourceOnly }).find(
      (s) => s.id === 'scan',
    );
    expect(scan?.findings).toContainEqual({ key: 'Score', value: 'Scored once the image is built', text: true });
    expect(scan?.findings.some((f) => f.value.includes('/ 100'))).toBe(false);
    expect(scan?.findings).toContainEqual({ key: 'High', value: '2' });
    expect(scan?.result).toBe('3 vulnerabilities');
  });

  it('scores the image detection built, with the source (R-311)', () => {
    const built = { ...found, built: { inputs: 'i', source: '3f9a2c1d', image: 'sha256:0123456789abcdef0123' } };
    const both = {
      ...report,
      scan: { ...report.scan, score: 61, commit: '3f9a2c1d', image: 'sha256:0123456789abcdef0123' },
      scope: 'image_and_source',
    } as unknown as Report;
    const steps = discoverySteps({ status: 'ready', proposal: built, source, security: both });
    expect(steps.map((s) => s.id)).toEqual(['read', 'stack', 'runs', 'vars', 'build', 'scan']);
    expect(steps.find((s) => s.id === 'build')).toMatchObject({
      state: 'done',
      result: 'Built',
      findings: [{ key: 'Image', value: '0123456789ab' }],
    });
    const scan = steps.find((s) => s.id === 'scan');
    expect(scan?.findings).toContainEqual({ key: 'Score', value: '61 / 100' });
    expect(scan?.findings.some((f) => f.value === 'Scored once the image is built')).toBe(false);
  });

  it('is absent on an install with no scanner', () => {
    expect(discoverySteps({ status: 'ready', proposal: found, source }).some((s) => s.id === 'scan')).toBe(false);
  });
});

describe('the build step (R-311)', () => {
  it('is under way beside the source scan', () => {
    const steps = discoverySteps({
      status: 'running',
      stage: 'building',
      running: ['building', 'scanning'],
      proposal: found,
      source,
    });
    expect(steps.filter((s) => s.state === 'current').map((s) => s.id)).toEqual(['build', 'scan']);
    expect(steps.find((s) => s.id === 'build')?.active).toBe('Building the image');
  });

  it('shows the trial run and the image scan in flight together once the image is built', () => {
    const built = { ...found, built: { inputs: 'i', source: 's', images: { web: 'reg/app@sha256:fedcba9876543210ff' } } };
    const steps = discoverySteps({
      status: 'running',
      stage: 'trying',
      running: ['trying', 'scanning'],
      proposal: built,
      source,
    });
    expect(steps.find((s) => s.id === 'build')).toMatchObject({
      state: 'done',
      findings: [{ key: 'web', value: 'fedcba987654' }],
    });
    expect(steps.filter((s) => s.state === 'current').map((s) => s.id)).toEqual(['trial', 'scan']);
  });

  it('reads the stage alone from a server that sends no list of steps', () => {
    const steps = discoverySteps({ status: 'running', stage: 'building', proposal: found, source });
    expect(steps.filter((s) => s.state === 'current').map((s) => s.id)).toEqual(['build']);
  });

  it('is absent for an app with nothing built', () => {
    expect(discoverySteps({ status: 'ready', proposal: found, source }).some((s) => s.id === 'build')).toBe(false);
  });

  it('paces two steps in flight: in turn until caught up, then both under way', () => {
    const steps = discoverySteps({
      status: 'running',
      stage: 'building',
      running: ['building', 'scanning'],
      proposal: found,
      source,
      commit: '3f9a2c1d',
    });
    const early = paced(steps, 1);
    expect(early.filter((s) => s.state === 'current').map((s) => s.id)).toEqual(['stack']);
    expect(early.find((s) => s.id === 'scan')?.state).toBe('pending');

    const caughtUp = paced(steps, finishedCount(steps));
    expect(caughtUp.filter((s) => s.state === 'current').map((s) => s.id)).toEqual(['build', 'scan']);
    expect(caughtUp.find((s) => s.id === 'scan')?.findings).toEqual([]);
  });
});

describe('composeNote', () => {
  it('splits the importer’s sentence into service, construct and the first sentence of why', () => {
    const note = composeNote(
      'In the compose service "proxy", `ports: 80:80` was not carried over as written. Pando gives the app its own address and routes to it, so the app does not need a port published on the host. The container’s port is kept.',
    );
    expect(note).toMatchObject({
      service: 'proxy',
      construct: 'ports: 80:80',
      gist: 'Pando gives the app its own address and routes to it, so the app does not need a port published on the host.',
    });
  });

  it('keeps a message in another shape whole', () => {
    expect(composeNote('Something else.')).toEqual({ gist: 'Something else.', full: 'Something else.' });
  });
});

describe('neededFunction', () => {
  it('mirrors core/detection.needed', () => {
    expect(neededFunction({ trial: { ran: true, crashed: true } })).toBe('repair_plan');
    expect(neededFunction({ winning_bid: { detector: '', strategy: 'unknown', confidence: 0 } })).toBe('repair_plan');
    expect(neededFunction({ questions: [{ key: 'k', prompt: '', why: '', kind: 'text' }] })).toBe('answer_questions');
    expect(neededFunction({ questions: [{ key: 'k', prompt: '', why: '', kind: 'text', deferred: true }] })).toBeUndefined();
  });
});

describe('progress', () => {
  it('counts finished steps and eases into the current one without reaching its end', () => {
    const steps = discoverySteps({ status: 'running', stage: 'detecting', proposal: {}, source });
    const early = progress(steps, 0);
    const late = progress(steps, 60_000);
    expect(early).toBeCloseTo(1 / steps.length);
    expect(late).toBeGreaterThan(early);
    expect(late).toBeLessThan(2 / steps.length);
  });

  it('places waypoints between steps and marks where AI starts', () => {
    const answered = { ...found, screening: { ran: true, function: 'answer_questions' } };
    const steps = discoverySteps({ status: 'ready', proposal: answered, source });
    expect(stops(steps)).toHaveLength(steps.length - 1);
    expect(aiFrom(steps)).toBeCloseTo(4 / 5);
    expect(aiFrom(discoverySteps({ status: 'ready', proposal: found, source }))).toBe(1);
  });
});

describe('questions', () => {
  const plain: Question = { key: 'primary_port', prompt: 'Which port?', why: '', kind: 'port' };
  const suggested: Question = { ...plain, key: 'start_command', suggested: { value: 'npm start', reason: 'package.json' } };

  it('groups by whether AI answered, not by what is typed', () => {
    expect(groupOf(plain)).toBe('needs');
    expect(groupOf(suggested)).toBe('ai');
  });

  it('takes a draft, then a saved answer, then the suggestion (R-338)', () => {
    expect(answerFor(suggested, null)).toBe('npm start');
    expect(answerFor(suggested, { start_command: 'node server.js' })).toBe('node server.js');
    expect(answerFor(suggested, { start_command: 'node server.js' }, 'yarn start')).toBe('yarn start');
  });

  it('says whether an answer is AI’s, the person’s, or missing', () => {
    expect(answerState(suggested, 'npm start')).toBe('ai');
    expect(answerState(suggested, 'yarn start')).toBe('answered');
    expect(answerState(plain, '')).toBe('needed');
  });

  it('lists what is still needed, counting a suggestion as an answer', () => {
    expect(stillNeeded([plain, suggested], null, {}).map((q) => q.key)).toEqual(['primary_port']);
    expect(stillNeeded([plain, suggested], null, { primary_port: '8080' })).toEqual([]);
  });

  it('names questions briefly', () => {
    expect(questionName('primary_port')).toBe('port');
    expect(questionName('build_arg.NODE_VERSION')).toBe('NODE_VERSION');
    expect(questionName('mystery')).toBe('mystery');
  });
});

describe('planSpec', () => {
  it('shows the reading the build-method answer picks', () => {
    const compose = { workloads: [{ name: 'app' }] } as unknown as Proposal['draft_spec'];
    const proposal: Partial<Proposal> = {
      ...found,
      runners_up: [{ detector: 'compose', strategy: 'compose', confidence: 0.8, spec: compose }],
    };
    expect(planSpec(proposal, { build_strategy: 'compose' })).toBe(compose);
    expect(planSpec(proposal, { build_strategy: 'dockerfile' })).toBe(found.draft_spec);
    expect(planSpec(proposal, {})).toBe(found.draft_spec);
  });
});

describe('terrain', () => {
  it('is the same for the same seed, peaks at 1, and differs by seed', () => {
    const a = elevation('acme/inventory@3f9a2c1');
    const b = elevation('acme/inventory@3f9a2c1');
    const c = elevation('acme/other@3f9a2c1');
    expect(a(0.42)).toBe(b(0.42));
    expect(a(peakOf(a))).toBeCloseTo(1, 5);
    expect(a(0.42)).not.toBe(c(0.42));
  });
});
