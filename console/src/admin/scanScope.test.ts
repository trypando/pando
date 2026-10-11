import { describe, expect, it } from 'vitest';

import { scanScope } from './scanScope';

describe('a score says what was scanned (R-311)', () => {
  it('names detection’s scan of the source', () => {
    expect(scanScope({ commit: 'abc123' })).toBe('from the source, before this configuration');
  });

  it('does not let a scan of the source alone stand for the image', () => {
    expect(scanScope({ spec_id: 'spec_1', commit: 'abc123' })).toBe('of the source only, not an image built from it');
  });

  it('names a scan of the running image alone', () => {
    expect(scanScope({ spec_id: 'spec_1', image: 'sha256:ab' })).toBe('of the running image only, not its source');
  });

  it('names a deploy’s scan of both', () => {
    expect(scanScope({ spec_id: 'spec_1', commit: 'abc123', image: 'sha256:ab' })).toBe(
      'of the image and the source it was built from',
    );
  });

  it('says nothing without a scan', () => {
    expect(scanScope(undefined)).toBe('');
  });
});
