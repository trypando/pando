// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { Host, JoinToken } from '@api/types.gen';
import { mount, reply, unanswered, type Mounted, type Seed } from '../ui/testing';
import { Hosts, rolesLabel, usesLabel } from './Hosts';
import { systemPages } from './System';

// The cluster's hosts and join tokens (R-414, R-415): both listed, a token
// shown once when it is created, and Revoke and Remove each calling their own
// endpoint.

const host: Host = {
  id: 'host_01',
  name: 'edge-2',
  roles: ['control', 'app'],
  address: '10.0.0.2:7443',
  cert_sha256: 'ab',
  version: '0.17.0',
  joined_at: '2026-10-20T10:00:00Z',
  last_seen_at: '2026-10-26T09:00:00Z',
};

const token = (id: string, more: Partial<JoinToken> = {}): JoinToken => ({
  id,
  roles: ['app'],
  max_uses: 3,
  uses: 1,
  created_by: 'usr_01',
  created_at: '2026-10-26T08:00:00Z',
  expires_at: '2026-10-27T08:00:00Z',
  ...more,
});

const seed = (hosts: Host[], tokens: JoinToken[]): Seed => [
  [['cluster-hosts'], { hosts }],
  [['cluster-join-tokens'], { tokens }],
];

interface Call {
  method: string;
  path: string;
  body?: unknown;
}

/** A fetch that records each request and answers it from `answer`. */
function recording(answer: (c: Call) => Response | Promise<Response>) {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    const call: Call = {
      method: init?.method ?? 'GET',
      path: String(url),
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    };
    calls.push(call);
    return answer(call);
  });
  return calls;
}

let screen: Mounted | undefined;

afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('the cluster’s hosts (R-414)', () => {
  it('lists each host with its roles, address, version and when it was last seen', () => {
    recording(() => unanswered());
    screen = mount(<Hosts />, seed([host, { ...host, id: 'host_02', name: 'apps-1', roles: ['app'], version: '', last_seen_at: undefined }], []));
    const text = screen.text();
    expect(text).toContain('edge-2');
    expect(text).toContain('Control and app');
    expect(text).toContain('10.0.0.2:7443');
    expect(text).toContain('0.17.0');
    expect(text).toContain('apps-1');
    expect(text).toContain('App');
  });

  it('is a page of System only for a holder of install.cluster.manage', () => {
    expect(systemPages({ view: true, managePolicy: false, manageBackups: false, manageCluster: true })).toContain('hosts');
    expect(systemPages({ view: true, managePolicy: false, manageBackups: false })).not.toContain('hosts');
  });

  it('asks before removing a host, then deletes it', async () => {
    const calls = recording((c) => (c.method === 'DELETE' ? new Response(null, { status: 204 }) : unanswered()));
    screen = mount(<Hosts />, seed([host], []));
    await screen.press('Remove');
    expect(calls.filter((c) => c.method === 'DELETE')).toEqual([]);
    expect(screen.text()).toContain('Remove edge-2');
    await screen.press('Remove host');
    expect(calls.find((c) => c.method === 'DELETE')?.path).toContain('/api/v1/cluster/hosts/host_01');
  });

  it('shows why a removal was refused', async () => {
    recording((c) =>
      c.method === 'DELETE' ? reply({ code: 'NOT_FOUND', message: 'No host host_01 is in this cluster.' }, 404) : unanswered(),
    );
    screen = mount(<Hosts />, seed([host], []));
    await screen.press('Remove');
    await screen.press('Remove host');
    expect(screen.text()).toContain('No host host_01 is in this cluster.');
  });
});

describe('join tokens (R-415)', () => {
  it('lists each token’s roles, uses, expiry and revocation, and offers Revoke on a live one', () => {
    recording(() => unanswered());
    screen = mount(<Hosts />, seed([], [token('jt_live'), token('jt_gone', { revoked_at: '2026-10-26T09:30:00Z', roles: ['control'] })]));
    const text = screen.text();
    expect(text).toContain('1 of 3');
    expect(text).toContain('Control');
    const revokes = [...document.querySelectorAll('button')].filter((b) => b.textContent === 'Revoke');
    expect(revokes).toHaveLength(1);
  });

  it('revokes a token through its own endpoint', async () => {
    const calls = recording((c) => (c.method === 'DELETE' ? new Response(null, { status: 204 }) : unanswered()));
    screen = mount(<Hosts />, seed([], [token('jt_live')]));
    await screen.press('Revoke');
    expect(calls.find((c) => c.method === 'DELETE')?.path).toContain('/api/v1/cluster/join-tokens/jt_live');
  });

  it('creates a token and shows it once, with a way to copy it', async () => {
    const calls = recording((c) =>
      c.method === 'POST'
        ? reply({ token: token('jt_new'), join: 'pando-join.10.0.0.1:7443.abc.s3cret', note: 'Shown once.' }, 201)
        : unanswered(),
    );
    screen = mount(<Hosts />, seed([], []));
    await screen.press('Create join token');
    await screen.fill('Roles', 'control,app');
    await screen.fill('Uses', '2');
    await screen.fill('Expires in hours', '48');
    await screen.press('Create');
    const post = calls.find((c) => c.method === 'POST');
    expect(post?.path).toContain('/api/v1/cluster/join-tokens');
    expect(post?.body).toEqual({ roles: ['control', 'app'], max_uses: 2, expires_hours: 48 });
    expect(screen.text()).toContain('pando-join.10.0.0.1:7443.abc.s3cret');
    expect(screen.text()).toContain('Shown once.');
    expect(document.querySelector('[aria-label="Copy Token"]')).toBeTruthy();

    await screen.press('Done');
    expect(screen.text()).not.toContain('s3cret');
  });

  it('leaves the roles to the API when none are chosen', async () => {
    const calls = recording((c) => (c.method === 'POST' ? reply({ token: token('jt_new'), join: 'pando-join.x' }, 201) : unanswered()));
    screen = mount(<Hosts />, seed([], []));
    await screen.press('Create join token');
    await screen.press('Create');
    expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ roles: [], max_uses: 1, expires_hours: 24 });
  });

  it('shows the refusal and its remedy, and does not create from a bad number', async () => {
    const calls = recording((c) =>
      c.method === 'POST'
        ? reply(
            {
              code: 'AUTHZ_DENIED',
              message: 'You cannot create a join token for the control role.',
              remedy: 'Ask an administrator who holds every install verb.',
            },
            403,
          )
        : unanswered(),
    );
    screen = mount(<Hosts />, seed([], []));
    await screen.press('Create join token');
    await screen.fill('Uses', '0');
    await screen.press('Create');
    expect(calls.filter((c) => c.method === 'POST')).toEqual([]);
    await screen.fill('Uses', '1');
    await screen.press('Create');
    expect(screen.text()).toContain('You cannot create a join token for the control role.');
    expect(screen.text()).toContain('Ask an administrator who holds every install verb.');
    await screen.press('Cancel');
    expect(document.querySelector('[role="dialog"]')).toBeNull();
  });
});

describe('labels', () => {
  it('reads roles and uses as words', () => {
    expect(rolesLabel(['control', 'app'])).toBe('Control and app');
    expect(rolesLabel(['app'])).toBe('App');
    expect(rolesLabel(null)).toBe('—');
    expect(usesLabel({ uses: 0, max_uses: 1 })).toBe('0 of 1');
  });
});
