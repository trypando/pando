import { describe, expect, it } from 'vitest';

import { deployLabel, deployLabelFor, deployStatus } from './deploys';

describe('how a deploy reads in a list', () => {
  it('says when the app it started never came up', () => {
    // The deploy worked: built, applied, routed. The app did not report
    // healthy, and the history said "Deployed" three times for an app that had
    // never served a request.
    expect(deployLabel('succeeded', 'degraded')).toBe('Deployed, not healthy');
    expect(deployStatus('succeeded', 'degraded')).toBe('building');
  });

  it('says plainly when it did', () => {
    expect(deployLabel('succeeded', 'running')).toBe('Deployed');
    expect(deployStatus('succeeded', 'running')).toBe('running');
  });

  // Deploys recorded before the result was kept carry nothing. Reading that as
  // "not healthy" would repaint an app's whole history on an upgrade.
  it('reads a deploy with no recorded result as it always did', () => {
    expect(deployLabel('succeeded')).toBe('Deployed');
    expect(deployStatus('succeeded')).toBe('running');
  });

  // R-154: a deploy waiting for approval has not started. Reading it as
  // "Deploying" would have its requester watch for something not happening.
  it('says a deploy is waiting for approval, never that it is deploying', () => {
    expect(deployLabel('awaiting_approval')).toBe('Waiting for approval');
    expect(deployStatus('awaiting_approval')).toBe('info');
  });

  // A decision, not a failure: no marker red.
  it('reads a rejected, expired or replaced request as not going ahead', () => {
    expect(deployLabel('rejected')).toBe('Rejected');
    expect(deployLabel('expired')).toBe('Expired, not approved');
    expect(deployLabel('superseded')).toBe('Replaced by a newer deploy');
    for (const s of ['rejected', 'expired', 'superseded']) expect(deployStatus(s)).toBe('stopped');
  });

  it('leaves the failures alone', () => {
    expect(deployLabel('failed')).toBe('Failed');
    expect(deployStatus('failed')).toBe('failed');
    expect(deployLabel('rolled_back')).toBe('Rolled back');
    expect(deployLabel('running')).toBe('Deploying');
  });
});

describe('a queued deploy (issue #93)', () => {
  it('says where it is in the queue', () => {
    expect(deployLabelFor({ status: 'pending', queue_position: 0 })).toBe('Queued, next');
    expect(deployLabelFor({ status: 'pending', queue_position: 3 })).toBe('Queued, 3 ahead');
    expect(deployLabelFor({ status: 'pending' })).toBe('Queued');
    expect(deployLabelFor({ status: 'succeeded', result_state: 'degraded', queue_position: 2 })).toBe(
      deployLabel('succeeded', 'degraded'),
    );
  });
});
