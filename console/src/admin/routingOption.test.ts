import { describe, expect, it } from 'vitest';

import { endingIn } from './routingOption';

const option = { adapter_ref: 'rte_cf', name: 'Cloudflare', modes: ['subdomain'], default_mode: 'subdomain', is_default: true };

describe('endingIn', () => {
  it('names every domain the adapter serves', () => {
    expect(endingIn({ ...option, base_domain: 'bemeek.io', domains: ['bemeek.io', 'trypando.ai', 'example.org'] })).toBe(
      'A name ending in bemeek.io, trypando.ai or example.org.',
    );
  });

  it('falls back to the base domain', () => {
    expect(endingIn({ ...option, base_domain: 'bemeek.io' })).toBe('A name ending in bemeek.io.');
    expect(endingIn({ ...option, domains: ['bemeek.io'] })).toBe('A name ending in bemeek.io.');
  });

  it('says nothing when any hostname will do', () => {
    expect(endingIn(option)).toBeUndefined();
    expect(endingIn(undefined)).toBeUndefined();
  });
});
