import { describe, expect, it } from 'vitest';

import type { Amendment, AppSpec, Outcome } from '@api/types.gen';
import {
  envKey,
  generateFor,
  mergeRows,
  neededValues,
  primaryWorkload,
  rowProblems,
  suggestions,
  valuesPayload,
  variableRows,
  type VariableRow,
} from './onboarding';

function amend(a: Partial<Amendment> & { kind: string }): Amendment {
  return { reason: '', evidence: [], ...a };
}

function spec(workloads: Array<Partial<NonNullable<AppSpec['workloads']>[number]>>): AppSpec {
  return { workloads } as unknown as AppSpec;
}

const twoWorkloads = spec([
  {
    name: 'web',
    primary: true,
    env: [
      { key: 'API_KEY' },
      { key: 'NODE_ENV', value: 'production' },
      { key: 'DATABASE_URL', slot_ref: 'DATABASE_URL' },
      { key: 'STORED', secret_ref: 'STORED' },
    ],
  },
  { name: 'worker', primary: false, env: [{ key: 'QUEUE' }] },
]);

describe('primaryWorkload', () => {
  it('is the workload marked primary, or the only one', () => {
    expect(primaryWorkload(twoWorkloads)).toBe('web');
    expect(primaryWorkload(spec([{ name: 'solo', primary: false }]))).toBe('solo');
    expect(primaryWorkload(spec([{ name: 'a' }, { name: 'b' }]))).toBe('');
    expect(primaryWorkload(undefined)).toBe('');
  });
});

describe('suggestions', () => {
  it('finds nothing when the screening did not run', () => {
    const found = suggestions({ ran: false, skip_code: 'not_configured' } as Outcome, twoWorkloads);
    expect(found.count).toBe(0);
    expect(found.env).toEqual({});
  });

  it('puts each applied amendment where it landed on the page', () => {
    const outcome: Outcome = {
      ran: true,
      applied: [
        { summary: '', amendment: amend({ kind: 'set_env', key: 'HOST', value: '0.0.0.0' }) },
        { summary: '', amendment: amend({ kind: 'set_env', workload: 'worker', key: 'QUEUE', value: 'jobs' }) },
        { summary: '', amendment: amend({ kind: 'set_command', command: ['npm', 'start'] }) },
        { summary: '', amendment: amend({ kind: 'set_port', port: 3000 }) },
        { summary: '', amendment: amend({ kind: 'set_health', path: '/health' }) },
        { summary: '', amendment: amend({ kind: 'set_dockerfile', path: 'Dockerfile.prod' }) },
        { summary: '', amendment: amend({ kind: 'add_slot', key: 'REDIS_URL', slot_type: 'redis' }) },
        { summary: '', amendment: amend({ kind: 'add_volume', path: '/data/' }) },
        { summary: '', amendment: amend({ kind: 'add_warning', value: ' Uploads go to disk. ' }) },
        { summary: '', amendment: amend({ kind: 'add_warning', reason: 'No value, so the reason.' }) },
        { summary: '', amendment: amend({ kind: 'answer_question', key: 'primary_port', value: '3000' }) },
      ],
      answers: { primary_port: '3000' },
    };
    const found = suggestions(outcome, twoWorkloads);

    // No workload means the primary one, as the server applies it.
    expect(Object.keys(found.env).sort()).toEqual([envKey('web', 'HOST'), envKey('worker', 'QUEUE')]);
    expect(found.command.web?.command).toEqual(['npm', 'start']);
    expect(found.port.web?.port).toBe(3000);
    expect(found.health?.path).toBe('/health');
    expect(found.build.dockerfile?.path).toBe('Dockerfile.prod');
    expect(found.slots.REDIS_URL?.slot_type).toBe('redis');
    // Mount paths compared without a trailing slash.
    expect(found.volumes['/data']).toBeDefined();
    expect(Object.keys(found.warnings).sort()).toEqual(['No value, so the reason.', 'Uploads go to disk.']);
    expect(found.answers).toEqual([
      { key: 'primary_port', value: '3000', amendment: outcome.applied![10]!.amendment },
    ]);
    expect(found.count).toBe(11);
  });
});

describe('variableRows', () => {
  it('lists every variable a person could set, including ones a slot fills, not ones a secret fills', () => {
    const rows = variableRows(twoWorkloads);
    expect(rows.map((r) => r.id)).toEqual(['web/API_KEY', 'web/NODE_ENV', 'web/DATABASE_URL', 'worker/QUEUE']);
  });

  it('starts a credential-looking name with no value as a secret', () => {
    const [apiKey, nodeEnv, , queue] = variableRows(twoWorkloads);
    expect(apiKey).toMatchObject({ secret: true, value: '', original: '' });
    expect(nodeEnv).toMatchObject({ secret: false, value: 'production', original: 'production' });
    expect(queue).toMatchObject({ secret: false });
  });

  it('marks a slot-filled variable with its slot, always as a secret', () => {
    const withSlots = {
      ...twoWorkloads,
      slots: [
        { key: 'DATABASE_URL', type: 'postgres', required: true, resolution: { mode: 'provisioned' } },
      ],
    } as AppSpec;
    const database = variableRows(withSlots).find((r) => r.key === 'DATABASE_URL');
    expect(database).toMatchObject({
      secret: true,
      slot: { key: 'DATABASE_URL', type: 'postgres', required: true, service: true, provisioned: true },
    });
  });
});

describe('generateFor', () => {
  it('makes a value in the shape the app checks (R-130)', () => {
    const laravel = generateFor('laravel_key');
    expect(laravel.startsWith('base64:')).toBe(true);
    expect(atob(laravel.slice('base64:'.length))).toHaveLength(32);
    expect(generateFor('random_long').length).toBeGreaterThanOrEqual(64);
    expect(generateFor('django_key').length).toBeGreaterThanOrEqual(50);
    expect(generateFor('hex_64')).toMatch(/^[0-9a-f]{64}$/);
    expect(generateFor()).toHaveLength(43);
  });

  it('carries the slot format onto the row', () => {
    const rows = variableRows({
      workloads: [{ name: 'web', primary: true, env: [{ key: 'APP_KEY', slot_ref: 'APP_KEY' }] }],
      slots: [{ key: 'APP_KEY', type: 'unknown', required: true, format: 'laravel_key' }],
    } as AppSpec);
    expect(rows[0]?.slot?.format).toBe('laravel_key');
  });
});

describe('neededValues', () => {
  it('lists required slot values Pando will not create and nobody has set (R-132)', () => {
    const slotted = (key: string, slot: Partial<NonNullable<VariableRow['slot']>>, value = ''): VariableRow => ({
      id: key,
      workload: 'app',
      key,
      value,
      secret: true,
      original: '',
      slot: { key, type: 'unknown', required: true, service: false, provisioned: false, ...slot },
    });
    const rows = [
      slotted('CREW_TOKEN_ENC_KEY', {}),
      slotted('VAPID_SUBJECT', { required: false }),
      slotted('DATABASE_URL', { type: 'postgres', service: true, provisioned: true }),
      slotted('ANTHROPIC_API_KEY', {}, 'sk-ant-set'),
    ];
    expect(neededValues(rows).map((r) => r.key)).toEqual(['CREW_TOKEN_ENC_KEY']);
    expect(neededValues(rows, new Set(['CREW_TOKEN_ENC_KEY']))).toEqual([]); // marked not required by the person
  });

  it('counts the parts of one database once (R-131)', () => {
    const rows = variableRows({
      workloads: [
        {
          name: 'web',
          primary: true,
          env: [
            { key: 'PGHOST', slot_ref: 'PGHOST', slot_part: 'host' },
            { key: 'PGPASSWORD', slot_ref: 'PGHOST', slot_part: 'password' },
          ],
        },
      ],
      slots: [{ key: 'PGHOST', type: 'postgres', required: true }],
    } as AppSpec);
    expect(rows[1]?.slot).toMatchObject({ key: 'PGHOST', part: 'password' });
    expect(neededValues(rows).map((r) => r.key)).toEqual(['PGHOST']);
  });
});

describe('slot values are secret by default, never by force', () => {
  it('starts a plain-named value readable and a service address or key-like name secret', () => {
    const domain = 'APP_DOMAIN';
    const token = 'CREW_TOKEN_ENC_KEY';
    const rows = variableRows({
      workloads: [
        {
          name: 'app',
          primary: true,
          exposed: true,
          env: [
            { key: domain, slot_ref: domain },
            { key: token, slot_ref: token },
          ],
        },
      ],
      slots: [
        { key: domain, type: 'unknown', required: true },
        { key: token, type: 'unknown', required: true },
      ],
    } as unknown as AppSpec);
    expect(rows.find((r) => r.key === domain)?.secret).toBe(false);
    expect(rows.find((r) => r.key === token)?.secret).toBe(true);
  });
});

describe('mergeRows', () => {
  it('keeps what was typed when more of the proposal arrives', () => {
    const edits = { 'web/API_KEY': { value: 'sk-1' } };
    const before = mergeRows(variableRows(spec([{ name: 'web', primary: true, env: [{ key: 'API_KEY' }] }])), edits, []);
    expect(before[0]!.value).toBe('sk-1');

    // The screening then sets a value for it and adds another variable.
    const later = spec([
      { name: 'web', primary: true, env: [{ key: 'API_KEY', value: 'from-ai' }, { key: 'HOST', value: '0.0.0.0' }] },
    ]);
    const after = mergeRows(variableRows(later), edits, []);
    expect(after.map((r) => [r.key, r.value])).toEqual([
      ['API_KEY', 'sk-1'],
      ['HOST', '0.0.0.0'],
    ]);
  });

  it('puts added rows last', () => {
    const added: VariableRow = { id: 'added-1', workload: '', key: 'EXTRA', value: '1', secret: false };
    expect(mergeRows(variableRows(twoWorkloads), {}, [added]).at(-1)).toBe(added);
  });
});

describe('valuesPayload', () => {
  const base = variableRows(twoWorkloads);

  it('sends nothing when nothing was changed', () => {
    expect(valuesPayload(base)).toEqual([]);
  });

  it('sends changed values with their workload, and new rows without one', () => {
    const rows = mergeRows(
      base,
      { 'web/API_KEY': { value: 'sk-1' }, 'web/NODE_ENV': { value: 'development' } },
      [
        { id: 'added-1', workload: '', key: ' EXTRA ', value: 'x', secret: false },
        { id: 'added-2', workload: '', key: 'EMPTY', value: '', secret: false },
        { id: 'added-3', workload: '', key: '', value: '', secret: false },
      ],
    );
    expect(valuesPayload(rows)).toEqual([
      { key: 'API_KEY', value: 'sk-1', secret: true, workload: 'web' },
      { key: 'NODE_ENV', value: 'development', workload: 'web' },
      { key: 'EXTRA', value: 'x' },
    ]);
  });

  it('sends a detected value turned into a secret, and never an empty secret', () => {
    const rows = mergeRows(base, { 'web/NODE_ENV': { secret: true }, 'worker/QUEUE': { secret: true } }, []);
    expect(valuesPayload(rows)).toEqual([{ key: 'NODE_ENV', value: 'production', secret: true, workload: 'web' }]);
  });

  it('sends a detected value somebody cleared', () => {
    const rows = mergeRows(base, { 'web/NODE_ENV': { value: '' } }, []);
    expect(valuesPayload(rows)).toEqual([{ key: 'NODE_ENV', value: '', workload: 'web' }]);
  });
});

describe('rowProblems', () => {
  it('checks only added rows, for a usable and unrepeated name', () => {
    const rows = [
      ...variableRows(twoWorkloads),
      { id: 'a', workload: '', key: '1BAD', value: '', secret: false },
      { id: 'b', workload: '', key: 'NODE_ENV', value: 'x', secret: false },
      { id: 'c', workload: '', key: 'NEW', value: 'x', secret: false },
      { id: 'd', workload: '', key: 'NEW', value: 'y', secret: false },
      { id: 'e', workload: '', key: '', value: 'orphan', secret: false },
      { id: 'f', workload: '', key: '', value: '', secret: false },
    ];
    const problems = rowProblems(rows);
    expect(Object.keys(problems).sort()).toEqual(['a', 'b', 'd', 'e']);
    expect(problems.b).toContain('already in the list');
  });
});

