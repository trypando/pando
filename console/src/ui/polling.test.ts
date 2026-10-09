import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// React Query runs no interval where there is no window (see headPoll.test).
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { QueryClient, QueryObserver } from '@tanstack/react-query';

import {
  AWAITING_MS,
  DELIVERY_FIRST_MS,
  QUEUED_MS,
  DELIVERY_MAX_MS,
  SCAN_WATCH_MS,
  SCANNING_MS,
  STATUS_DEPLOYING_MS,
  STATUS_SETTLED_MS,
  STATUS_UNSETTLED_MS,
  deliveriesInterval,
  deploymentsInterval,
  recordIsBehind,
  securityInterval,
  statusInterval,
} from './polling';

const part = (over: Partial<{ present: boolean; running: boolean; restarting: boolean; healthy: boolean | null }> = {}) => ({
  present: true,
  running: true,
  restarting: false,
  healthy: true as boolean | null,
  ...over,
});

describe('statusInterval', () => {
  it('watches a deploy closely, by the record or by the status', () => {
    expect(statusInterval('deploying', undefined)).toBe(STATUS_DEPLOYING_MS);
    expect(statusInterval('running', { state: 'deploying', workloads: [] })).toBe(STATUS_DEPLOYING_MS);
  });

  it('watches a degraded app, or a running one with a part in trouble', () => {
    expect(statusInterval('degraded', undefined)).toBe(STATUS_UNSETTLED_MS);
    for (const trouble of [{ restarting: true }, { running: false }, { present: false }, { healthy: false }]) {
      expect(statusInterval('running', { state: 'running', workloads: [part(), part(trouble)] })).toBe(
        STATUS_UNSETTLED_MS,
      );
    }
  });

  it('asks slowly once everything runs as it should, and for a stopped app', () => {
    expect(statusInterval('running', { state: 'running', workloads: [part(), part({ healthy: null })] })).toBe(
      STATUS_SETTLED_MS,
    );
    expect(statusInterval('stopped', { state: 'stopped', workloads: [part({ running: false })] })).toBe(
      STATUS_SETTLED_MS,
    );
  });
});

describe('deploymentsInterval', () => {
  it('asks only while a deploy waits for approval', () => {
    expect(deploymentsInterval([{ status: 'running' }])).toBe(false);
    expect(deploymentsInterval(null)).toBe(false);
    expect(deploymentsInterval([{ status: 'succeeded' }, { status: 'awaiting_approval' }])).toBe(AWAITING_MS);
  });

  it('asks often while a deploy waits in the queue, whose place moves (issue #93)', () => {
    expect(deploymentsInterval([{ status: 'pending' }])).toBe(QUEUED_MS);
    expect(deploymentsInterval([{ status: 'awaiting_approval' }, { status: 'pending' }])).toBe(QUEUED_MS);
  });
});

describe('securityInterval', () => {
  it('asks while a scan runs or may start, and never off screen', () => {
    expect(securityInterval(true, false, true)).toBe(SCANNING_MS);
    expect(securityInterval(false, true, true)).toBe(SCAN_WATCH_MS);
    expect(securityInterval(false, false, true)).toBe(false);
    expect(securityInterval(true, true, false)).toBe(false);
  });
});

describe('recordIsBehind', () => {
  it('reads the record again when a newer status disagrees with it', () => {
    expect(recordIsBehind({ state: 'deploying', updatedAt: 1 }, { state: 'running', updatedAt: 2 })).toBe(true);
  });
  it('leaves it when they agree, when the status is older, or when either is missing', () => {
    expect(recordIsBehind({ state: 'running', updatedAt: 1 }, { state: 'running', updatedAt: 2 })).toBe(false);
    expect(recordIsBehind({ state: 'deploying', updatedAt: 3 }, { state: 'running', updatedAt: 2 })).toBe(false);
    expect(recordIsBehind(undefined, { state: 'running', updatedAt: 2 })).toBe(false);
    expect(recordIsBehind({ state: 'running', updatedAt: 1 }, { state: undefined, updatedAt: 2 })).toBe(false);
  });
});

describe('deliveriesInterval', () => {
  const now = Date.parse('2026-10-07T12:00:00Z');
  it('stops when nothing is pending', () => {
    expect(deliveriesInterval([{ status: 'succeeded', attempts: 1 }], now)).toBe(false);
    expect(deliveriesInterval(undefined, now)).toBe(false);
  });
  it('asks soon for a first attempt', () => {
    expect(deliveriesInterval([{ status: 'pending', attempts: 0 }], now)).toBe(DELIVERY_FIRST_MS);
  });
  it('asks when a retry is due, between the bounds', () => {
    const at = (ms: number) => new Date(now + ms).toISOString();
    expect(deliveriesInterval([{ status: 'pending', attempts: 2, next_attempt_at: at(20_000) }], now)).toBe(20_000);
    expect(deliveriesInterval([{ status: 'pending', attempts: 2, next_attempt_at: at(3_600_000) }], now)).toBe(
      DELIVERY_MAX_MS,
    );
    expect(deliveriesInterval([{ status: 'pending', attempts: 2, next_attempt_at: at(-5_000) }], now)).toBe(
      DELIVERY_FIRST_MS,
    );
    expect(deliveriesInterval([{ status: 'pending', attempts: 2 }], now)).toBe(DELIVERY_MAX_MS);
    expect(deliveriesInterval([{ status: 'pending', attempts: 2, next_attempt_at: 'soon' }], now)).toBe(
      DELIVERY_MAX_MS,
    );
  });
});

describe('a status poll driven by statusInterval', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('asks every three seconds through a deploy, then every thirty once it settles', async () => {
    const client = new QueryClient();
    let state = 'deploying';
    const queryFn = vi.fn(() => Promise.resolve({ state, workloads: [part()] }));
    const observer = new QueryObserver(client, {
      queryKey: ['apps', 'app_1', 'status'],
      queryFn,
      refetchInterval: (q) => statusInterval(undefined, q.state.data),
    });
    const stop = observer.subscribe(() => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(queryFn).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(STATUS_DEPLOYING_MS * 3);
    expect(queryFn).toHaveBeenCalledTimes(4);

    state = 'running';
    await vi.advanceTimersByTimeAsync(STATUS_DEPLOYING_MS);
    expect(queryFn).toHaveBeenCalledTimes(5);
    await vi.advanceTimersByTimeAsync(STATUS_SETTLED_MS - 1);
    expect(queryFn, 'settled: nothing until the slow interval').toHaveBeenCalledTimes(5);
    await vi.advanceTimersByTimeAsync(1);
    expect(queryFn).toHaveBeenCalledTimes(6);

    stop();
    client.clear();
  });
});
