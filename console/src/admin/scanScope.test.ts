import { describe, expect, it } from 'vitest';

import { scanScope } from './scanScope';

const digest = 'sha256:' + 'ab'.repeat(32);

describe('a score says what was scanned (R-311)', () => {
  it('names detection’s scan of the source', () => {
    expect(scanScope({ commit: 'abc123' })).toBe('from the source, before this configuration');
  });

  it('does not let a scan of the source alone stand for the image', () => {
    expect(scanScope({ spec_id: 'spec_1', commit: 'abc123' })).toBe('of the source only');
  });

  it('names a scan of the running image alone', () => {
    expect(scanScope({ spec_id: 'spec_1', image: digest })).toBe('of the running image only');
  });

  it('names a deploy’s scan of a built image and its source', () => {
    expect(scanScope({ spec_id: 'spec_1', commit: 'abc123', image: digest })).toBe(
      'of the image it runs and its source',
    );
  });

  it('names a deploy’s scan of a published image and the repository', () => {
    expect(scanScope({ spec_id: 'spec_1', commit: 'abc123', image: `ghcr.io/bastienwirtz/homer@${digest}` })).toBe(
      'of the image it runs and its source',
    );
  });

  it('names a scan of an image app, which has no source but its image', () => {
    expect(scanScope({ spec_id: 'spec_1', commit: digest, image: `ghcr.io/acme/web@${digest}` })).toBe(
      'of the image it runs',
    );
  });

  it('names detection’s scan of the published image an app will run', () => {
    expect(scanScope({ commit: 'abc123', image: `ghcr.io/bastienwirtz/homer@${digest}` })).toBe(
      'of the image it will run and its source',
    );
    expect(scanScope({ commit: digest, image: `ghcr.io/acme/web@${digest}` })).toBe('of the image it will run');
  });

  it('says nothing without a scan', () => {
    expect(scanScope(undefined)).toBe('');
  });
});
