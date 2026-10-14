import { describe, expect, it } from 'vitest';

import { answering } from './testing';

describe('a fetch answering by address', () => {
  const fetch = answering({ '/groups': { groups: [] } });

  it.each([
    ['a string', 'https://pando.example.com/api/v1/groups'],
    ['a URL', new URL('https://pando.example.com/api/v1/groups')],
    ['a Request', new Request('https://pando.example.com/api/v1/groups')],
  ])('answers %s by its address', async (_, input) => {
    expect(await (await fetch(input)).json()).toEqual({ groups: [] });
  });
});
