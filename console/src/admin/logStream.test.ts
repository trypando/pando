import { describe, expect, it } from 'vitest';
import { appendCapped, followLog, noticeLine, type EventSourceLike } from './logStream';

/** A stand-in EventSource the test drives by hand. */
class FakeSource implements EventSourceLike {
  readyState = 1;
  closed = false;
  onerror: EventSourceLike['onerror'] = null;
  onmessage: EventSourceLike['onmessage'] = null;
  private listeners = new Map<string, ((ev: MessageEvent) => void)[]>();

  addEventListener(type: string, listener: (ev: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  emit(type: string, data = '') {
    const ev = { data } as MessageEvent;
    if (type === 'message') this.onmessage?.call(this, ev);
    for (const l of this.listeners.get(type) ?? []) l(ev);
  }
  fail(readyState: number) {
    this.readyState = readyState;
    this.onerror?.call(this, {} as Event);
  }
}

function harness() {
  const source = new FakeSource();
  const ticks: (() => void)[] = [];
  const seen = { resets: 0, lines: [] as string[][], revoked: '', failed: 0, url: '' };
  const stop = followLog(
    '/api/v1/apps/app_1/logs/stream',
    {
      reset: () => seen.resets++,
      lines: (batch) => seen.lines.push(batch),
      revoked: (message) => (seen.revoked = message),
      failed: () => seen.failed++,
    },
    (url) => {
      seen.url = url;
      return source;
    },
    (fn) => ticks.push(fn),
  );
  const tick = () => ticks.splice(0).forEach((fn) => fn());
  return { source, seen, stop, tick };
}

describe('appendCapped', () => {
  it('keeps the newest lines', () => {
    expect(appendCapped(['a', 'b'], ['c', 'd'], 3)).toEqual(['b', 'c', 'd']);
    const same = ['a'];
    expect(appendCapped(same, [])).toBe(same);
  });
});

describe('followLog', () => {
  it('batches lines and notices between ticks', () => {
    const { source, seen, tick } = harness();
    expect(seen.url).toBe('/api/v1/apps/app_1/logs/stream');
    source.emit('reset');
    source.emit('message', 'one');
    source.emit('message', 'two');
    source.emit('notice', 'restarted');
    expect(seen.lines).toEqual([]);
    tick();
    expect(seen.resets).toBe(1);
    expect(seen.lines).toEqual([['one', 'two', noticeLine('restarted')]]);
  });

  it('drops pending lines on reset, since the backfill follows', () => {
    const { source, seen, tick } = harness();
    source.emit('message', 'stale');
    source.emit('reset');
    source.emit('message', 'fresh');
    tick();
    expect(seen.lines).toEqual([['fresh']]);
  });

  it('stops for good when access is revoked', () => {
    const { source, seen, tick } = harness();
    source.emit('message', 'last');
    source.emit('revoked', 'Your access ended.');
    expect(seen.lines).toEqual([['last']]);
    expect(seen.revoked).toBe('Your access ended.');
    expect(source.closed).toBe(true);
    source.fail(2);
    tick();
    expect(seen.failed).toBe(0);
  });

  it('shows why a lagged stream was cut off and leaves reconnecting to the browser', () => {
    const { source, seen, tick } = harness();
    source.emit('lagged', 'Fell behind.');
    tick();
    expect(seen.lines).toEqual([[noticeLine('Fell behind.')]]);
    expect(source.closed).toBe(false);
  });

  it('reports a stream that will not reopen, not one the browser is retrying', () => {
    const { source, seen } = harness();
    source.fail(0);
    expect(seen.failed).toBe(0);
    source.fail(2);
    expect(seen.failed).toBe(1);
  });

  it('closes the stream when stopped', () => {
    const { source, seen, stop, tick } = harness();
    source.emit('message', 'late');
    stop();
    tick();
    expect(source.closed).toBe(true);
    expect(seen.lines).toEqual([]);
  });
});
