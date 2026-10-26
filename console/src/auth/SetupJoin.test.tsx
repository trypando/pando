// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, unanswered, type Mounted } from '../ui/testing';
import { Login } from './Login';

// A new installation joins a cluster at setup, instead of creating its first
// administrator (R-415): a join token and a host name, posted to
// POST /setup/join.

let screen: Mounted | undefined;
let posted: Array<{ path: string; body: unknown }>;

function answer(join: () => Response) {
  posted = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      posted.push({ path: String(url), body: JSON.parse(String(init.body)) });
      return join();
    }
    return unanswered();
  });
}

beforeEach(() => {
  vi.stubGlobal('matchMedia', () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
});

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

async function openJoin() {
  screen = mount(<Login />, [[['setup'], { needed: true }]]);
  expect(screen.text()).toContain('Set up Pando');
  await screen.press('Join a cluster');
  expect(screen.text()).toContain('Join token');
  expect(screen.text()).not.toContain('Setup token');
}

describe('joining a cluster at setup (R-415)', () => {
  it('posts the token and host name, then says the host joined and is restarting', async () => {
    answer(() => reply({ roles: ['control', 'app'], status: 'joined' }, 202));
    await openJoin();
    await screen!.fill('Join token', ' pando-join.10.0.0.1:7443.abc.s3cret ');
    await screen!.fill('Host name', 'edge-2');
    await screen!.submit('Host name');
    expect(posted).toEqual([
      { path: '/api/v1/setup/join', body: { token: 'pando-join.10.0.0.1:7443.abc.s3cret', name: 'edge-2' } },
    ]);
    expect(screen!.text()).toContain('Joined the cluster');
    expect(screen!.text()).toContain('control and app');
    expect(screen!.text()).toContain('restarting');
  });

  it('shows the refusal’s message and remedy', async () => {
    answer(() =>
      reply(
        {
          code: 'AUTH_INVALID',
          message: 'This join token has expired.',
          remedy: 'Create a new join token on a host of the cluster.',
        },
        401,
      ),
    );
    await openJoin();
    await screen!.fill('Join token', 'pando-join.x');
    await screen!.fill('Host name', 'edge-2');
    await screen!.press('Join');
    expect(screen!.text()).toContain('This join token has expired. Create a new join token on a host of the cluster.');
    // Editing clears it.
    await screen!.fill('Join token', 'pando-join.y');
    expect(screen!.text()).not.toContain('has expired');
  });

  it('goes back to creating the administrator', async () => {
    answer(() => reply({}));
    await openJoin();
    await screen!.press('Create administrator');
    expect(screen!.text()).toContain('Setup token');
  });
});
