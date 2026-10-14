import { describe, expect, it } from 'vitest';
import { trimTrailingSlashes } from './text';

describe('trimTrailingSlashes', () => {
  it('removes every trailing slash and nothing else', () => {
    expect(trimTrailingSlashes('https://example.com///')).toBe('https://example.com');
    expect(trimTrailingSlashes('a//b/')).toBe('a//b');
    expect(trimTrailingSlashes('a/b')).toBe('a/b');
    expect(trimTrailingSlashes('///')).toBe('');
    expect(trimTrailingSlashes('')).toBe('');
  });

  it('keeps a long run of slashes that does not reach the end', () => {
    const s = `${'/'.repeat(100_000)}x`;
    expect(trimTrailingSlashes(s)).toBe(s);
  });
});
