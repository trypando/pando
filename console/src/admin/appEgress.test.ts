import { describe, expect, it } from 'vitest';

import {
  addActions,
  blocksPrivate,
  draftOf,
  egressLines,
  hostLine,
  installMode,
  looseningNote,
  ownLine,
  sameDraft,
  specOf,
  unrestricted,
  withEntry,
  withoutEntry,
} from './appEgress';
import type { EgressDraft } from './appEgress';

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

describe('the page’s one list of addresses (R-182, R-188)', () => {
  const draft = (over: Partial<EgressDraft> = {}): EgressDraft => ({ ...draftOf(null), ...over });

  it('lists the host’s addresses first, then what the app adds and keeps', () => {
    const lines = egressLines(
      { mode: 'allowlist', list: ['registry.npmjs.org', 'api.github.com'] },
      draft({ add: ['api.stripe.com'], remove: ['api.github.com'] }),
      [{ kind: 'allowlist_add', entry: 'api.stripe.com', message: 'Adds it.' }],
    );
    expect(lines).toEqual([
      { entry: 'registry.npmjs.org', from: 'host', loosens: false },
      { entry: 'api.github.com', from: 'excluded', loosens: false },
      { entry: 'api.stripe.com', from: 'added', loosens: true },
    ]);
  });

  it('lists no host addresses when the host allows everything', () => {
    const lines = egressLines({ mode: 'allow_all', list: ['ignored.example'] }, draft({ ownMode: 'denylist', ownList: ['x.example'] }), []);
    expect(lines).toEqual([{ entry: 'x.example', from: 'own', loosens: false }]);
  });

  it('is unrestricted only with no host list and nothing of the app’s own', () => {
    expect(unrestricted({ mode: 'allow_all' }, draft())).toBe(true);
    expect(unrestricted({ mode: 'denylist' }, draft())).toBe(false);
    expect(unrestricted({}, draft({ ownMode: 'allowlist', ownList: ['a'] }))).toBe(false);
    expect(unrestricted({}, draft({ add: ['a'] }))).toBe(false);
  });

  it('says where the host stands, and the app’s own list under it', () => {
    expect(hostLine('allowlist')).toBe('This host only allows the addresses below.');
    expect(hostLine('denylist')).toBe('This host blocks the addresses below.');
    expect(hostLine(installMode(undefined))).toBe('This host allows any address.');
    expect(ownLine(draft())).toBeNull();
    expect(ownLine(draft({ ownMode: 'allowlist' }))).toContain('only connects');
    expect(ownLine(draft({ ownMode: 'denylist' }))).toContain('never connects');
  });

  it('offers Allow or Block as the rules call for, and both with neither list', () => {
    expect(addActions('allowlist', draft())).toEqual(['allow']);
    expect(addActions('denylist', draft())).toEqual(['block']);
    expect(addActions('allow_all', draft())).toEqual(['allow', 'block']);
    expect(addActions('allowlist', draft({ ownMode: 'denylist' }))).toEqual(['block']);
    expect(addActions('denylist', draft({ ownMode: 'allowlist' }))).toEqual(['allow']);
  });

  it('adds to the app’s own list, to the host’s, or starts a list of its own', () => {
    expect(withEntry('allowlist', draft(), 'a.example', 'allow').add).toEqual(['a.example']);
    expect(withEntry('allowlist', draft({ ownMode: 'denylist', ownList: ['b'] }), 'a', 'block').ownList).toEqual(['b', 'a']);
    expect(withEntry('allow_all', draft(), 'a', 'block')).toMatchObject({ ownMode: 'denylist', ownList: ['a'] });
    expect(withEntry('allow_all', draft(), 'a', 'allow')).toMatchObject({ ownMode: 'allowlist', ownList: ['a'] });
  });

  it('removes an addition, undoes an exclusion, and drops an emptied list of its own', () => {
    const d = draft({ add: ['a'], remove: ['h'], ownMode: 'allowlist', ownList: ['o'] });
    expect(withoutEntry(d, { entry: 'a', from: 'added', loosens: false }).add).toEqual([]);
    expect(withoutEntry(d, { entry: 'h', from: 'excluded', loosens: false }).remove).toEqual([]);
    expect(withoutEntry(d, { entry: 'o', from: 'own', loosens: false })).toMatchObject({ ownMode: 'inherit', ownList: [] });
    expect(withoutEntry(d, { entry: 'h', from: 'host', loosens: false })).toBe(d);
    const two = draft({ ownMode: 'denylist', ownList: ['o', 'p'] });
    expect(withoutEntry(two, { entry: 'o', from: 'own', loosens: false })).toMatchObject({ ownMode: 'denylist', ownList: ['p'] });
  });

  it('says what a loosening needs, by the host’s gate (R-183)', () => {
    expect(looseningNote('approval')).toBe('Needs approval to deploy');
    expect(looseningNote('forbidden')).toBe('Not allowed on this host');
    expect(looseningNote('verb')).toBe('Loosens this host’s rules');
  });

  it('blocks private addresses as the app says, else as the host does', () => {
    expect(blocksPrivate({ block_private: true }, draft())).toBe(true);
    expect(blocksPrivate({ block_private: true }, draft({ blockPrivate: 'off' }))).toBe(false);
    expect(blocksPrivate({}, draft({ blockPrivate: 'on' }))).toBe(true);
    expect(blocksPrivate({}, draft())).toBe(false);
  });
});
