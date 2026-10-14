import { describe, expect, it } from 'vitest';

import { expiry, isAwaiting, progress } from './approval';
import type { ApprovalDeployment } from './approval';

const base: ApprovalDeployment = {
  id: 'dep_01',
  app_id: 'app_01',
  spec_id: 'spec_01',
  trigger: 'manual',
  status: 'awaiting_approval',
  started_at: '2026-10-01T00:00:00Z',
  created_by: 'usr_01',
};

const yes = (id: string) => ({ principal_id: id, principal_name: '', decision: 'approve', decided_at: '2026-10-01T01:00:00Z' });

describe('a deploy request reads as how far it has got (R-156)', () => {
  it('reads the default of one approval plainly', () => {
    expect(progress(base)).toBe('Needs one approval.');
    expect(progress({ ...base, approvals_required: 0 })).toBe('Needs one approval.');
  });

  it('counts approvals against what is needed', () => {
    expect(progress({ ...base, approvals_required: 2 })).toBe('Needs 2 approvals. None yet.');
    expect(progress({ ...base, approvals_required: 2, approvals: [yes('usr_02')] })).toBe('1 of 2 approvals.');
  });

  it('knows which deployments are waiting', () => {
    expect(isAwaiting(base)).toBe(true);
    expect(isAwaiting({ ...base, status: 'pending' })).toBe(false);
    expect(isAwaiting(undefined)).toBe(false);
  });
});

describe('when a request stops waiting', () => {
  const now = new Date('2026-10-01T00:00:00Z').getTime();

  it('says a request with no expiry waits for an answer', () => {
    expect(expiry(undefined, now)).toBe('No expiry.');
  });

  it('says how long is left in the shortest honest unit', () => {
    expect(expiry('2026-10-01T00:30:00Z', now)).toBe('Expires in 30 minutes.');
    expect(expiry('2026-10-01T05:00:00Z', now)).toBe('Expires in 5 hours.');
    expect(expiry('2026-10-08T00:00:00Z', now)).toBe('Expires in 7 days.');
    expect(expiry('2026-09-30T00:00:00Z', now)).toBe('Expired.');
  });
});
