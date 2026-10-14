import { describe, expect, it } from 'vitest';

import { describeAmendment, filesRead, screeningVisible } from './screeningText';

describe('describeAmendment', () => {
  it('says what each kind of change proposed', () => {
    expect(describeAmendment({ kind: 'set_env', key: 'HOST', value: '0.0.0.0', reason: '', evidence: [] }))
      .toBe('Set HOST to 0.0.0.0');
    expect(describeAmendment({ kind: 'set_port', port: 8080, reason: '', evidence: [] }))
      .toBe('Serve on port 8080');
    expect(describeAmendment({ kind: 'set_command', command: ['npm', 'start'], reason: '', evidence: [] }))
      .toBe('Start with npm start');
    expect(describeAmendment({ kind: 'set_command', value: 'node server.js', reason: '', evidence: [] }))
      .toBe('Start with node server.js');
    expect(describeAmendment({ kind: 'add_slot', key: 'DATABASE_URL', slot_type: 'postgres', reason: '', evidence: [] }))
      .toBe('Add DATABASE_URL as a postgres dependency');
  });

  it('names a kind it does not know rather than hiding it', () => {
    expect(describeAmendment({ kind: 'set_isolation_floor', reason: '', evidence: [] }))
      .toContain('set_isolation_floor');
  });
});

describe('screeningVisible', () => {
  it('stays silent when no AI adapter is configured', () => {
    expect(screeningVisible(undefined)).toBe(false);
    expect(screeningVisible({ ran: false, skip_code: 'not_configured', skipped: 'No AI adapter is configured.' }))
      .toBe(false);
  });

  it('stays silent when detection did not need an AI adapter', () => {
    expect(screeningVisible({ ran: false, skip_code: 'not_needed', skipped: 'Detection produced a plan.' }))
      .toBe(false);
  });

  it('shows a screening that ran, and one that was skipped for another reason', () => {
    expect(screeningVisible({ ran: true })).toBe(true);
    expect(screeningVisible({
      ran: false,
      skip_code: 'policy',
      skipped: 'An administrator has turned off AI screening on this installation.',
    })).toBe(true);
  });
});

describe('filesRead', () => {
  it('counts plainly', () => {
    expect(filesRead(0)).toBe('Files read (0)');
    expect(filesRead(1)).toBe('Files read (1)');
    expect(filesRead(7)).toBe('Files read (7)');
  });
});
