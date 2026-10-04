import { describe, expect, it, vi } from 'vitest';

vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import { settingArea } from './Installation';

// R-271: the startup configuration is grouped by area, so a setting is found
// where someone setting Pando up would look for it.
describe('startup settings by area (R-271)', () => {
  it('puts a listed key in its area', () => {
    expect(settingArea('server.base_domain').area).toBe('Public addresses');
    expect(settingArea('server.routing_mode').area).toBe('Routing');
    expect(settingArea('log.level').area).toBe('Logging');
  });

  it('puts an unlisted key under its prefix, or Other, so none goes missing', () => {
    expect(settingArea('database.max_conns').area).toBe('Database');
    expect(settingArea('telemetry.endpoint').area).toBe('Other');
  });

  it('orders areas as listed, and Other last', () => {
    const order = ['server.addr', 'server.external_url', 'server.routing_mode', 'database.connect_timeout', 'log.level', 'zzz.key'];
    const ranks = order.map((k) => settingArea(k).rank);
    expect([...ranks].sort((a, b) => a - b)).toEqual(ranks);
  });
});
