import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AppOnboarding, repoPage } from './AppOnboarding';
import type { DetectionResponse } from './DetectionReview';
import type { AppWithVerbs } from './verbs';

const app = {
  id: 'app_x',
  name: 'crewmate',
  source: { type: 'git', url: 'https://github.com/acme/crewmate.git', ref: 'main' },
  verbs: ['app.view', 'app.spec.edit', 'app.deploy', 'app.delete', 'app.secrets.write'],
} as unknown as AppWithVerbs;

const detection = {
  commit: '3f9a2c1d0000',
  winning_bid: { detector: 'dockerfile', strategy: 'dockerfile', confidence: 0.9, evidence: ['Dockerfile at the root'] },
  runners_up: [{ detector: 'compose', strategy: 'compose', confidence: 0.8 }],
  questions: [
    {
      key: 'build_strategy',
      prompt: 'Pando found two plausible ways to build this app.',
      why: 'Pando needs to know which one describes how this app is meant to run.',
      kind: 'choice',
      options: ['dockerfile', 'compose'],
      suggested: { value: 'compose', reason: 'The README runs it with docker compose.', evidence: ['README.md'] },
    },
    { key: 'primary_port', prompt: 'Which port does it serve HTTP on?', why: '', kind: 'port' },
  ],
  draft_spec: {
    build: { strategy: 'dockerfile', dockerfile: 'Dockerfile' },
    workloads: [
      {
        name: 'web',
        primary: true,
        exposed: true,
        command: ['npm', 'start'],
        env: [
          { key: 'STRIPE_SECRET_KEY' },
          { key: 'LOG_LEVEL', value: 'info' },
          { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' },
        ],
      },
    ],
    slots: [{ key: 'DATABASE_URL', type: 'postgres', required: true }],
  },
  trial: {},
  screening: {
    ran: true,
    function: 'answer_questions',
    answers: { build_strategy: 'compose' },
    files_read: ['README.md'],
    notes: ['The README says the app expects a mounted uploads folder.'],
    applied: [
      {
        summary: 'answered build_strategy: compose',
        amendment: { kind: 'answer_question', key: 'build_strategy', value: 'compose', reason: 'r', evidence: ['README.md'] },
      },
    ],
  },
};

function render(response: Partial<DetectionResponse>, security?: unknown, subject: AppWithVerbs = app): string {
  const client = new QueryClient();
  client.setQueryData(['apps', 'app_x', 'detection'], response);
  if (security) client.setQueryData(['apps', 'app_x', 'security'], security);
  return renderToString(
    <QueryClientProvider client={client}>
      <AppOnboarding app={subject} onBack={() => {}} onDeployRefused={() => {}} />
    </QueryClientProvider>,
  );
}

describe('AppOnboarding', () => {
  it('renders the review once detection has finished', () => {
    const html = render({ status: 'needs_answers', answers: {}, commit: '3f9a2c1d0000', detection } as never);
    expect(html).toContain('Plan ready');
    expect(html).toContain('AI answers');
    expect(html).toContain('Needs your answer');
    expect(html).toContain('Why AI picked this.');
    expect(html).toContain('STRIPE_SECRET_KEY');
    expect(html).toContain('The plan');
    expect(html).toContain('Accept and deploy');
    expect(html).toContain('1 answer needed');

    // AI's notes sit after the variables and before the plan.
    const variables = html.indexOf('>Variables<');
    const notes = html.indexOf('Notes from AI');
    const plan = html.indexOf('The plan');
    expect(variables).toBeGreaterThan(-1);
    expect(notes).toBeGreaterThan(variables);
    expect(plan).toBeGreaterThan(notes);
  });

  it('shows the compose reading AI picked: its real command, its database, and values as variables', () => {
    const compose = {
      build: { strategy: 'compose', compose_file: 'docker-compose.yml' },
      workloads: [
        {
          name: 'app',
          build: { context: '.' },
          env: [
            { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' },
            { key: 'CREW_TOKEN_ENC_KEY', slot_ref: 'CREW_TOKEN_ENC_KEY' },
            { key: 'APP_DOMAIN', slot_ref: 'APP_DOMAIN' },
            { key: 'VAPID_SUBJECT', slot_ref: 'VAPID_SUBJECT' },
          ],
        },
      ],
      routing: { adapter_ref: 'rte_loopback', mode: 'port', mode_source: 'adapter_default', port: 9003 },
      slots: [
        {
          key: 'DATABASE_URL',
          type: 'postgres',
          required: true,
          evidence: ['compose service "db" runs docker.io/library/postgres:16-alpine'],
          resolution: { mode: 'provisioned' },
        },
        { key: 'CREW_TOKEN_ENC_KEY', type: 'unknown', required: true },
        { key: 'APP_DOMAIN', type: 'unknown', required: true },
        // Filled by AI during detection: a readable value on the slot.
        {
          key: 'VAPID_SUBJECT',
          type: 'unknown',
          required: true,
          resolution: { mode: 'bound', target: 'mailto:ops@acme.dev' },
        },
      ],
    };
    const crewmate = {
      ...detection,
      winning_bid: { ...detection.winning_bid, evidence: ['Dockerfile at repository root', 'CMD ["/crewmate"]'] },
      runners_up: [{ detector: 'compose', strategy: 'compose', confidence: 0.8, spec: compose }],
      questions: [detection.questions[0]],
      screening: {
        ...detection.screening,
        applied: [
          ...detection.screening.applied,
          {
            summary: 'VAPID_SUBJECT set to mailto:ops@acme.dev',
            amendment: { kind: 'set_env', key: 'VAPID_SUBJECT', value: 'mailto:ops@acme.dev', reason: 'README', evidence: ['README.md'] },
          },
        ],
      },
    };
    const html = render({
      status: 'ready',
      answers: {},
      commit: '3f9a2c1d0000',
      address: '//localhost:9003/',
      detection: crewmate,
    } as never);

    // Where it will be, from the plan's routing.
    expect(html).toContain('>Address<');
    expect(html).toContain('http://localhost:9003');
    expect(html).toContain('rte_loopback');

    const plan = html.slice(html.indexOf('The plan'), html.indexOf('pando-actionbar'));
    expect(plan).toContain('/crewmate');
    expect(plan).not.toContain('The image’s own command');
    expect(plan).toContain('PostgreSQL');
    expect(plan).toContain('postgres:16-alpine');
    expect(plan).toContain('Provisioned');
    expect(plan).not.toContain('CREW_TOKEN_ENC_KEY');

    // The database is a service the app needs, listed as one ahead of the
    // variables, with the choice of who provides it (R-131).
    const servicesAt = html.indexOf('Pando creates each one at the first deploy');
    const variablesAt = html.indexOf('Used on every deploy');
    expect(servicesAt).toBeGreaterThan(-1);
    expect(variablesAt).toBeGreaterThan(servicesAt);
    const services = html.slice(servicesAt, variablesAt);
    expect(services).toContain('PostgreSQL');
    expect(services).toContain('Via DATABASE_URL');
    expect(services).toContain('Provisioned');
    expect(services).toContain('Use your own PostgreSQL');
    expect(services).not.toContain('CREW_TOKEN_ENC_KEY');

    // Every slot-filled variable is a value somebody can set here.
    const variables = html.slice(variablesAt, html.indexOf('The plan'));
    expect(variables).toContain('CREW_TOKEN_ENC_KEY');
    expect(variables).toContain('Needs a value');
    expect(variables).toContain('Required');
    // What Pando fills is said plainly, as what it holds, with no field to
    // overwrite by accident. Who provides the database is chosen above.
    expect(variables).toContain('Address of the PostgreSQL Pando creates');
    expect(variables).toContain('Filled in by Pando at deploy');
    expect(variables).not.toContain('Use your own');
    expect(variables).not.toContain('which Pando creates');
    expect(variables).not.toContain('Provisioned');
    // Secret is a default on every value, never a lock.
    expect(variables).toContain('>Secret<');
    expect(variables).not.toMatch(/Secret<\/span><\/label>[^]*?disabled=""/);
    // A required value still empty is marked in red, with a way to say the
    // app runs without it.
    expect(variables).toContain('var(--marker-deep)');
    expect(variables).toContain('Required. Mark not required.');
    // The app's own domain comes from its address; a value AI filled says so;
    // any value can be generated.
    expect(variables).toContain('From the app’s address');
    expect(variables).toContain('value="localhost"');
    expect(variables).toContain('Filled by AI');
    expect(variables).toContain('mailto:ops@acme.dev');
    expect(variables).toContain('Generate');

    // The repository opens in a new tab.
    expect(html).toContain('href="https://github.com/acme/crewmate"');
    expect(html).toContain('target="_blank"');

    // Deploying waits for the required value; accepting does not.
    const bar = html.slice(html.indexOf('pando-actionbar'));
    expect(bar).toContain('1 value needed to deploy');
  });

  it('offers a conversation with AI when an adapter is there, showing what it changed (R-336)', () => {
    const talked = {
      ...detection,
      conversation: [
        { from: 'person', text: 'It serves on 8080.', at: '2026-09-26T12:00:00Z' },
        {
          from: 'ai',
          text: 'server.js calls listen(8080), so I set the port.',
          at: '2026-09-26T12:00:05Z',
          changes: ['port 8080'],
          refused: ['set HOST: HOST is a declared slot'],
        },
      ],
    };
    const html = render({ status: 'ready', answers: {}, commit: '3f9a2c1d0000', detection: talked } as never);
    expect(html).toContain('Ask AI about this plan');
    expect(html).toContain('It serves on 8080.');
    expect(html).toContain('Changed: port 8080');
    expect(html).toContain('Pando didn’t apply: set HOST');
    expect(html).toContain('Ask AI');
  });

  it('says what AI changed after the plan failed (R-334)', () => {
    const repaired = {
      ...detection,
      screening: {
        ran: true,
        function: 'repair_plan',
        applied: [{ summary: 'set port', amendment: { kind: 'set_port', port: 8080, workload: 'web' } }],
      },
    };
    const html = render({ status: 'ready', answers: {}, commit: '3f9a2c1d0000', detection: repaired } as never);
    expect(html).toContain('AI changed the plan after it failed.');
    expect(html).toContain('Serve for web on port 8080.');
  });

  it('has no conversation at all when no AI adapter is configured', () => {
    const plain = { ...detection, screening: { ran: false, skip_code: 'not_configured' } };
    const html = render({ status: 'ready', answers: {}, commit: '3f9a2c1d0000', detection: plain } as never);
    expect(html).not.toContain('Ask AI');
  });

  it('links a repository to its page, and nothing else', () => {
    expect(repoPage('https://github.com/acme/crewmate.git')).toBe('https://github.com/acme/crewmate');
    expect(repoPage('git@github.com:acme/crewmate.git')).toBe('https://github.com/acme/crewmate');
    expect(repoPage('/srv/repos/crewmate')).toBeUndefined();
    expect(repoPage('javascript:alert(1)')).toBeUndefined();
  });

  it('renders the discovery view while detection runs, with no review', () => {
    const html = render({
      status: 'running',
      answers: null,
      commit: '',
      detection: { stage: 'detecting' } as DetectionResponse['detection'],
    });
    expect(html).toContain('Reading the code');
    expect(html).toContain('Read the repo');
    expect(html).not.toContain('Accept plan');
  });

  it('keeps the same ridge before and after detection learns the commit', () => {
    const ridge = (html: string) => html.match(/<path d="([^"]+)" class="pando-terrain-index"/)?.[1];
    const image = { ...app, source: { type: 'image', image: 'ghcr.io/acme/crewmate:1.4' } } as unknown as AppWithVerbs;
    const sourceless = { ...app, source: undefined } as unknown as AppWithVerbs;
    const ridges = [app, image, sourceless].map((subject) => {
      const running = ridge(
        render(
          {
            status: 'running',
            answers: null,
            commit: '',
            detection: { stage: 'detecting' } as DetectionResponse['detection'],
          },
          undefined,
          subject,
        ),
      );
      const finished = ridge(
        render({ status: 'needs_answers', answers: {}, commit: '3f9a2c1d0000', detection } as never, undefined, subject),
      );
      expect(running).toBeTruthy();
      expect(finished).toBe(running);
      return running;
    });
    // Each falls back to a different seed: the repository, the image, the app ID.
    expect(new Set(ridges).size).toBe(3);
  });

  it('renders a failed detection with its reason', () => {
    const html = render({
      status: 'failed',
      answers: null,
      commit: '',
      detection: { error: { message: 'Pando couldn’t clone the repository.' } } as DetectionResponse['detection'],
    });
    expect(html).toContain('Pando couldn’t clone the repository.');
    expect(html).toContain('Reject plan');
  });

  it('shows the build log of a build that failed while Pando read the app (R-107)', () => {
    const html = render({
      status: 'failed',
      answers: null,
      commit: '',
      detection: {
        error: {
          message: 'Pando builds an app while reading it, to score the image it would run, and the build of this app failed: the RUN step exited 1.',
          remedy: 'The build log shows the step that failed.',
          details: { build_log: '#7 ERROR: process "/bin/sh -c npm ci" did not complete successfully' },
        },
      } as DetectionResponse['detection'],
    });
    expect(html).toContain('Build log');
    expect(html).toContain('did not complete successfully');
  });

  it('shows the image being built while the source is scanned', () => {
    const html = render({
      status: 'running',
      answers: null,
      commit: '',
      detection: { ...detection, stage: 'building', running: ['building', 'scanning'] } as never,
    });
    expect(html).toContain('Building the image');
  });

  it('says a ready plan is ready, and nothing about what happens on accepting it', () => {
    const ready = {
      ...detection,
      questions: [],
      screening: { ran: false },
      draft_spec: {
        build: { strategy: 'dockerfile', dockerfile: 'Dockerfile' },
        workloads: [{ name: 'web', primary: true, exposed: true, command: ['npm', 'start'], env: [] }],
        slots: [],
      },
    };
    const html = render({ status: 'ready', answers: {}, commit: '3f9a2c1d0000', detection: ready } as never);
    expect(html).toContain('Everything’s answered.');
    expect(html).toContain('Ready to deploy');
    expect(html).not.toContain('Nothing runs until you accept');
  });

  it('tags an upload, lists a static directory, and marks the primary of several parts', () => {
    const uploaded = { ...app, source: { type: 'upload' } } as unknown as AppWithVerbs;
    const plan = {
      ...detection,
      questions: [],
      screening: { ran: false },
      draft_spec: {
        build: { strategy: 'static', static_dir: 'dist' },
        workloads: [
          { name: 'web', primary: true, exposed: true, command: ['npm', 'start'], env: [] },
          { name: 'worker', command: ['npm', 'run', 'work'], env: [] },
        ],
        slots: [],
      },
    };
    const html = render({ status: 'ready', answers: {}, detection: plan } as never, undefined, uploaded).replaceAll('<!-- -->', '');
    expect(html).toContain('>Upload<');
    expect(html).toContain('Static directory');
    expect(html).toContain('>Primary<');
    expect(html).toContain('>Runs<');
  });

  describe('the plan’s security score (R-311)', () => {
    const ready = {
      ...detection,
      questions: [],
      screening: { ran: false },
      draft_spec: {
        build: { strategy: 'dockerfile', dockerfile: 'Dockerfile' },
        workloads: [{ name: 'web', primary: true, exposed: true, command: ['npm', 'start'], env: [] }],
        slots: [],
      },
    };
    const counts = { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 };

    it('does not score an image not yet built: the source’s findings, and when it is scored', () => {
      const html = render({ status: 'ready', answers: {}, commit: '3f9a2c1d0000', detection: ready } as never, {
        standing: { verdict: 'ok', score: null, threshold: 0 },
        scan: { id: 'scn_1', app_id: 'app_x', commit: '3f9a2c1d0000', scanner_ref: 'scn', score: 100, findings: [], ran_at: '' },
        counts,
        scope: 'source',
        awaiting_image: true,
        ignoring_unfixable: false,
      });
      expect(html).toContain('Scored once the image is built.');
      expect(html).toContain('No known vulnerabilities in the source');
      expect(html).not.toContain('100 / 100');
    });

    it('scores a published image the plan scanned', () => {
      const html = render({ status: 'ready', answers: {}, commit: '3f9a2c1d0000', detection: ready } as never, {
        standing: { verdict: 'ok', score: 72, threshold: 0 },
        scan: {
          id: 'scn_1',
          app_id: 'app_x',
          commit: '3f9a2c1d0000',
          image: 'ghcr.io/bastienwirtz/homer@sha256:' + 'ab'.repeat(32),
          scanner_ref: 'scn',
          score: 72,
          findings: [],
          ran_at: '',
        },
        counts: { ...counts, high: 2, medium: 2, low: 2 },
        scope: 'image_and_source',
        ignoring_unfixable: false,
      });
      expect(html).toContain('>72<');
      expect(html).not.toContain('72 / 100');
      expect(html).not.toContain('Scored once the image is built');
    });
  });
});
