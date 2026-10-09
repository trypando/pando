import { describe, expect, it } from 'vitest';

import { draftOf, dryRunWords, gateWords, listEdits, modeWords, sameDraft, specOf } from './appEgress';
import type { DryRunResult } from './appEgress';

describe("an app's egress editor reads and writes the spec (R-182)", () => {
  it('opens an empty spec as following the installation', () => {
    expect(draftOf(null)).toEqual({ add: [], remove: [], ownMode: 'inherit', ownList: [], blockPrivate: 'inherit' });
    expect(specOf(draftOf(null))).toEqual({ mode: 'inherit' });
  });

  it('reads a spec from before issue #79 as the server now does', () => {
    // An old allowlist is the app's own list, which narrows.
    expect(draftOf({ mode: 'allowlist', allowlist: ['api.example.com'] })).toMatchObject({
      ownMode: 'allowlist',
      ownList: ['api.example.com'],
    });
    // The old block_private mode is the switch, turned on.
    expect(draftOf({ mode: 'block_private' })).toMatchObject({ ownMode: 'inherit', blockPrivate: 'on' });
    expect(draftOf({ mode: 'allow_all' }).ownMode).toBe('inherit');
  });

  it('writes additions, removals, its own list and the switch, leaving out what is empty', () => {
    expect(
      specOf({ add: ['a.example'], remove: [], ownMode: 'denylist', ownList: ['b.example'], blockPrivate: 'off' }),
    ).toEqual({ mode: 'denylist', list: ['b.example'], add: ['a.example'], block_private: false });
    // A list kept while the app follows the installation is not written.
    expect(specOf({ add: [], remove: [], ownMode: 'inherit', ownList: ['x'], blockPrivate: 'on' })).toEqual({
      mode: 'inherit',
      block_private: true,
    });
  });

  it('round-trips a spec it wrote', () => {
    const d = draftOf({ mode: 'allowlist', list: ['c.example'], remove: ['d.example'], block_private: true });
    expect(sameDraft(draftOf(specOf(d)), d)).toBe(true);
    expect(sameDraft(d, { ...d, add: ['e.example'] })).toBe(false);
  });
});

describe('the merged rules in words (R-188)', () => {
  it('names each mode', () => {
    expect(modeWords('allow_all')).toBe('Anywhere');
    expect(modeWords('')).toBe('Anywhere');
    expect(modeWords('denylist')).toBe('Anywhere except the destinations listed');
    expect(modeWords('allowlist')).toBe('Only the destinations listed');
  });

  it('says what a loosening needs, with forbidden as the one blocker', () => {
    expect(gateWords('forbidden').status).toBe('failed');
    expect(gateWords('approval').label).toBe('Needs a deploy approval');
    expect(gateWords('verb').label).toBe('Needs app.egress.loosen');
    expect(gateWords(undefined).status).toBe('info');
  });

  it('names the edits to the installation by what they do under its mode', () => {
    expect(listEdits('allowlist')?.add).toEqual({ label: 'Also allow', loosens: true });
    expect(listEdits('denylist')?.remove.loosens).toBe(true);
    expect(listEdits('denylist')?.add.loosens).toBe(false);
    expect(listEdits('allow_all')).toBeNull();
  });
});

describe("the server's decision on a draft, in words (R-188)", () => {
  const result = (over: Partial<DryRunResult>): DryRunResult => ({
    dry_run: true,
    egress: { mode: 'allowlist', block_private: false, restricted: true },
    egress_changed: true,
    new_loosenings: [],
    approval: { required: false, reasons: [] },
    ...over,
  });

  it('says a change stays within the rules', () => {
    expect(dryRunWords(result({}))).toEqual(['This stays within the installation’s rules.']);
  });

  it('says nothing about a draft that changes nothing', () => {
    expect(dryRunWords(result({ egress_changed: false }))).toEqual([]);
  });

  it("names each loosening in the server's words, and whether the deploy needs approval", () => {
    const words = dryRunWords(
      result({
        new_loosenings: [{ kind: 'allowlist_add', entry: 'api.stripe.com', message: 'Adds api.stripe.com to the installation’s allowlist, so this app can reach it.' }],
        approval: {
          required: true,
          reasons: [{ reason: 'egress_loosening', message: 'This deploy loosens the installation’s egress rules, which needs approval.' }],
        },
      }),
    );
    expect(words).toEqual([
      'Adds api.stripe.com to the installation’s allowlist, so this app can reach it.',
      'Deploying it needs approval. This deploy loosens the installation’s egress rules, which needs approval.',
    ]);
  });
});
