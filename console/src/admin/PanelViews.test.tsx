// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';

import type { App, Deployment } from '@api/types.gen';
import { PanelOutput } from './PanelOutput';
import { NOT_DEPLOYED, PanelDeployments, caption, startedBy, took } from './PanelDeployments';
import { EXEC_WARNING, Terminal } from './Terminal';
import { AppVerb } from './verbs';
import { mount, reply, serverRender, unanswered } from '../ui/testing';

const app = { id: 'app_1', name: 'crewmate', state: 'running', pinned_spec_id: 'spec_2' } as App;

/** A stand-in for EventSource that a test speaks through. */
class FakeStream {
  static last: FakeStream | undefined;
  readyState = 1;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  listeners = new Map<string, (ev: MessageEvent) => void>();
  constructor(readonly url: string) {
    FakeStream.last = this;
  }
  addEventListener(type: string, fn: (ev: MessageEvent) => void) {
    this.listeners.set(type, fn);
  }
  close() {}
  emit(type: string, data = '') {
    const ev = { data } as MessageEvent;
    if (type === 'message') this.onmessage?.(ev);
    else this.listeners.get(type)?.(ev);
  }
}

beforeEach(() => {
  FakeStream.last = undefined;
  vi.stubGlobal('EventSource', FakeStream);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  document.body.innerHTML = '';
});

const tick = () => act(async () => new Promise((r) => setTimeout(r, 60)));

describe('Output', () => {
  const status: [unknown[], unknown] = [
    ['apps', 'app_1', 'status'],
    {
      state: 'running',
      desired_state: 'running',
      workloads: [
        { name: 'web', primary: true, present: true, running: true, restarting: false, restart_count: 0, healthy: true },
        { name: 'worker', primary: false, present: true, running: true, restarting: false, restart_count: 0, healthy: null },
      ],
    },
  ];

  it('follows the chosen workload’s output, and copies it', async () => {
    vi.stubGlobal('fetch', unanswered);
    const writeText = vi.fn(async () => {});
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    const onWorkload = vi.fn();
    const screen = mount(<PanelOutput app={app} workload="worker" onWorkload={onWorkload} />, [status]);

    expect(FakeStream.last?.url).toBe('/api/v1/apps/app_1/logs/stream?workload=worker');
    await act(async () => {
      FakeStream.last?.emit('reset');
      FakeStream.last?.emit('message', 'job 8812 started');
      FakeStream.last?.emit('message', 'job 8812 done');
    });
    await tick();
    expect(screen.text()).toContain('job 8812 started');
    expect(screen.text()).toContain('Running');

    // A button with an icon reads " Copy": found by its words, not exactly.
    const copy = [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Copy')!;
    await act(async () => copy.click());
    expect(writeText).toHaveBeenCalledWith('job 8812 started\njob 8812 done');
    expect(screen.text()).toContain('Copied');

    await screen.fill('Workload', 'web');
    expect(onWorkload).toHaveBeenCalledWith('web');
    screen.unmount();
  });

  it('shows whether it follows new lines, and lets the reader stop it', async () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<PanelOutput app={app} onWorkload={() => {}} />, [status]);
    const follow = () => document.querySelector<HTMLInputElement>('input[role="switch"]')!;
    expect(follow().checked).toBe(true);
    await act(async () => follow().click());
    expect(follow().checked).toBe(false);
    screen.unmount();
  });

  it('says why when the log cannot be read, and tries again', async () => {
    vi.stubGlobal('fetch', async () => reply({ code: 'AUTHZ_DENIED', message: 'You can’t read this app’s logs.' }, 403));
    const screen = mount(<PanelOutput app={app} onWorkload={() => {}} />, [status]);
    const stream = FakeStream.last!;
    stream.readyState = 2;
    await act(async () => stream.onerror?.());
    await tick();
    expect(screen.text()).toContain('You can’t read this app’s logs.');
    await screen.press('Try again');
    expect(FakeStream.last).not.toBe(stream);
    screen.unmount();
  });

  it('has nothing to show for an app never deployed', () => {
    const html = serverRender(<PanelOutput app={{ ...app, pinned_spec_id: '' }} onWorkload={() => {}} />);
    expect(html).toContain('No output yet.');
  });
});

describe('Deployments', () => {
  const done: Deployment = {
    id: 'dep_2',
    app_id: 'app_1',
    spec_id: 'spec_2',
    spec_revision: 2,
    trigger: 'manual',
    status: 'succeeded',
    started_at: '2026-10-23T14:30:00Z',
    finished_at: '2026-10-23T14:31:42Z',
    created_by: 'usr_1',
  };
  const older: Deployment = { ...done, id: 'dep_1', spec_id: 'spec_1', spec_revision: 1, trigger: 'branch_updated' };
  const seed = (revisions = [{ id: 'spec_1', revision: 1 }, { id: 'spec_2', revision: 2 }]): Array<[unknown[], unknown]> => [
    [['apps', 'app_1', 'deployments'], { deployments: [done, older] }],
    [['apps', 'app_1', 'specs'], { revisions, pinned_spec_id: 'spec_2' }],
  ];

  it('describes each deploy in a line', () => {
    expect(startedBy('manual')).toBe('by hand');
    expect(startedBy('rollback')).toBe('rollback');
    expect(startedBy('branch_updated')).toBe('new commit');
    expect(startedBy('release_tagged')).toBe('new release');
    expect(startedBy('image_update')).toBe('image update');
    expect(took('2026-10-23T14:30:00Z', '2026-10-23T14:31:42Z')).toBe('1m 42s');
    expect(took('2026-10-23T14:30:00Z')).toBe('');
    expect(caption(done)).toMatch(/^Revision 2 · .+ · by hand · 1m 42s$/);
  });

  it('lists the history and streams the chosen one’s log', async () => {
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<PanelDeployments app={app} onDeployment={() => {}} />, seed(), [AppVerb.LogsRead]);
    expect(screen.text()).toContain('Deploy of revision 2');
    expect(FakeStream.last?.url).toBe('/api/v1/apps/app_1/deployments/dep_2/logs');
    await act(async () => FakeStream.last?.emit('message', 'Deployed a3f9c21.'));
    expect(screen.text()).toContain('Deployed a3f9c21.');
    // The one running is not offered as something to roll back to.
    expect(screen.text()).not.toContain('Roll back to this');
    screen.unmount();
  });

  it('rolls back to an earlier deploy for somebody who may deploy', async () => {
    const calls: Array<{ url: string; body?: string }> = [];
    vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
      calls.push({ url: String(url), body: init?.body as string | undefined });
      return reply({ id: 'dep_3', status: 'pending' }, 202);
    });
    const screen = mount(
      <PanelDeployments app={app} deployment="dep_1" onDeployment={() => {}} />,
      seed(),
      [AppVerb.LogsRead, AppVerb.Deploy],
    );
    await screen.press('Roll back to this');
    const rollback = calls.find((c) => c.url.endsWith('/apps/app_1/deployments/rollback'));
    expect(JSON.parse(rollback?.body ?? '{}')).toEqual({ to: 1 });
    screen.unmount();
  });

  it('says why a rollback was refused, in the panel', async () => {
    vi.stubGlobal('fetch', async () => reply({ code: 'STATE_INVALID', message: 'Pando is already deploying this app.' }, 409));
    const screen = mount(
      <PanelDeployments app={app} deployment="dep_1" onDeployment={() => {}} />,
      seed(),
      [AppVerb.LogsRead, AppVerb.Deploy],
    );
    await screen.press('Roll back to this');
    await tick();
    expect(screen.text()).toContain('Pando is already deploying this app.');
    await screen.press('Dismiss');
    expect(screen.text()).not.toContain('Pando is already deploying this app.');
    screen.unmount();
  });

  it('offers no rollback to somebody who may not deploy', () => {
    vi.stubGlobal('fetch', unanswered);
    const html = serverRender(<PanelDeployments app={app} deployment="dep_1" onDeployment={() => {}} />, seed(), [AppVerb.LogsRead]);
    expect(html).not.toContain('Roll back to this');
  });

  it('starts with a revision not deployed yet, with Deploy and Compare', async () => {
    vi.stubGlobal('fetch', unanswered);
    const onDeployment = vi.fn();
    const revisions = [
      { id: 'spec_1', revision: 1 },
      { id: 'spec_2', revision: 2 },
      { id: 'spec_3', revision: 3 },
    ];
    const screen = mount(<PanelDeployments app={app} onDeployment={onDeployment} />, seed(revisions), [
      AppVerb.LogsRead,
      AppVerb.Deploy,
    ]);
    expect(screen.text()).toContain('Not deployed yet');
    expect(screen.text()).toContain('Revision 3');
    expect([...document.querySelectorAll('button')].map((b) => b.textContent)).toContain('Deploy');
    await screen.press('Compare');
    expect(onDeployment).toHaveBeenCalledWith(NOT_DEPLOYED);
    screen.unmount();
  });

  it('compares the saved revision with the one running', () => {
    const revisions = [
      { id: 'spec_2', revision: 2 },
      { id: 'spec_3', revision: 3 },
    ];
    const html = serverRender(
      <PanelDeployments app={app} deployment={NOT_DEPLOYED} onDeployment={() => {}} />,
      [
        ...seed(revisions),
        [['apps', 'app_1', 'spec-diff', 2, 3], { changes: [{ path: 'egress', summary: 'Adds api.stripe.com to egress.' }] }],
      ],
      [AppVerb.LogsRead],
    );
    expect(html).toContain('Revision 3, not deployed yet');
    expect(html).toContain('Adds api.stripe.com to egress.');
    // Not offered to somebody who may not deploy.
    expect(html).not.toContain('>Deploy<');
  });

  it('says so when there are none', () => {
    const html = serverRender(<PanelDeployments app={app} onDeployment={() => {}} />, [
      [['apps', 'app_1', 'deployments'], { deployments: [] }],
    ]);
    expect(html).toContain('No deployments yet.');
  });

  it('says what happened to a deploy that never ran', () => {
    const rejected = { ...older, id: 'dep_9', status: 'rejected', finished_at: undefined };
    const html = serverRender(<PanelDeployments app={app} deployment="dep_9" onDeployment={() => {}} />, [
      [['apps', 'app_1', 'deployments'], { deployments: [done, rejected] }],
    ]);
    expect(html).toContain('Rejected. Who rejected it, and why, is in the audit log.');
  });

  it('says a finished deploy’s output is gone when the stream has none', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('fetch', unanswered);
    const screen = mount(<PanelDeployments app={app} onDeployment={() => {}} />, seed());
    await act(async () => vi.advanceTimersByTime(4_100));
    expect(screen.text()).toContain('Deploy logs are cleared when Pando restarts.');
    screen.unmount();
  });
});

describe('Terminal', () => {
  const specs: Array<[unknown[], unknown]> = [
    [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_2', revision: 2 }], pinned_spec_id: 'spec_2' }],
    [['apps', 'app_1', 'spec', 2], { body: { workloads: [{ name: 'web', primary: true }, { name: 'worker' }] } }],
  ];

  it('says what a terminal grants before one is opened (R-086), and only that', () => {
    const html = serverRender(<Terminal appID="app_1" onWorkload={() => {}} />, specs);
    expect(html).toContain(EXEC_WARNING);
    expect(html).toContain('Not connected');
    expect(html).toContain('Open terminal');
    expect(html).not.toContain('records');
  });

  it('opens in the workload the address names', () => {
    const html = serverRender(<Terminal appID="app_1" workload="worker" onWorkload={() => {}} />, specs);
    expect(html).toContain('start a shell in');
    expect(html).toMatch(/<code[^>]*>worker<\/code>/);
  });

  it('changes workload through the address', async () => {
    const onWorkload = vi.fn();
    const screen = mount(<Terminal appID="app_1" onWorkload={onWorkload} />, specs);
    await screen.fill('Workload', 'worker');
    expect(onWorkload).toHaveBeenCalledWith('worker');
    screen.unmount();
  });
});

describe('a terminal session', () => {
  const specs: Array<[unknown[], unknown]> = [
    [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_2', revision: 2 }], pinned_spec_id: 'spec_2' }],
    [['apps', 'app_1', 'spec', 2], { body: { workloads: [{ name: 'web', primary: true }] } }],
  ];

  class FakeSocket {
    static readonly OPEN = 1;
    static last: FakeSocket | undefined;
    readyState = 0;
    binaryType = '';
    sent: unknown[] = [];
    onopen: (() => void) | null = null;
    onmessage: ((ev: { data: unknown }) => void) | null = null;
    onclose: ((ev: { reason: string }) => void) | null = null;
    onerror: (() => void) | null = null;
    constructor(readonly url: string) {
      FakeSocket.last = this;
    }
    send(data: unknown) {
      this.sent.push(data);
    }
    close() {}
  }

  const open = () => {
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} });
    // xterm watches the display's pixel ratio; jsdom has no media queries.
    vi.stubGlobal('matchMedia', () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} }));
    return mount(<Terminal appID="app_1" onWorkload={() => {}} />, specs);
  };

  it('connects, says so, and says how the shell ended', async () => {
    const screen = open();
    await screen.press('Open terminal');
    expect(FakeSocket.last?.url).toContain('/api/v1/apps/app_1/exec?workload=web');
    // The warning is said before connecting, not over the shell.
    expect(screen.text()).not.toContain(EXEC_WARNING);
    expect(screen.text()).toContain('Connecting');

    const socket = FakeSocket.last!;
    await act(async () => {
      socket.readyState = 1;
      socket.onopen?.();
      socket.onmessage?.({ data: new TextEncoder().encode('/app $ ').buffer });
      socket.onmessage?.({ data: 'text' });
    });
    expect(screen.text()).toContain('Connected');

    await act(async () => socket.onclose?.({ reason: 'exited with status 1' }));
    expect(screen.text()).toContain('Session ended: exited with status 1');

    await screen.press('Disconnect');
    expect(screen.text()).toContain('Not connected');
    expect(screen.text()).toContain(EXEC_WARNING);
    screen.unmount();
  });

  it('says when a terminal could not be opened', async () => {
    const screen = open();
    await screen.press('Open terminal');
    await act(async () => FakeSocket.last?.onerror?.());
    expect(screen.text()).toContain('Pando couldn’t open a terminal in this app.');
    screen.unmount();
  });
});
