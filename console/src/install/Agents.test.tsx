// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import { mount, reply, type Mounted } from '../ui/testing';
import { Agents, nextDraft, type Instructions } from './Agents';

// R-424: the company's instructions for agents, edited by whoever manages
// policy, with Pando's own guidance beside them.

const doc: Instructions = {
  text: '# Building apps on Pando\n\nUse Go.',
  builtin: '# Building apps on Pando',
  company: 'Use Go.',
  updated_at: '2026-11-04T09:00:00Z',
  updated_by: 'usr_admin',
};

interface Call {
  method: string;
  body?: unknown;
}

function recording(answer: (call: Call) => unknown) {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (_url: string, init?: RequestInit) => {
    const call: Call = { method: init?.method ?? 'GET' };
    if (init?.body) call.body = JSON.parse(String(init.body));
    calls.push(call);
    const body = answer(call);
    return body instanceof Response ? body : reply(body);
  });
  return calls;
}

let screen: Mounted | undefined;
afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

const field = () => document.querySelector<HTMLTextAreaElement>('textarea')!;
const saveButton = () => [...document.querySelectorAll('button')].find((b) => b.textContent === 'Save');

async function type(value: string) {
  await screen!.settle(() => {
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(field(), value);
    field().dispatchEvent(new Event('input', { bubbles: true }));
  });
}

describe('the Agents page', () => {
  it('saves the company’s instructions once they change', async () => {
    const calls = recording((c) => (c.method === 'PUT' ? { ...doc, company: 'Use Rust.' } : doc));
    screen = mount(<Agents canEdit />, [[['instructions'], doc]]);

    expect(field().value).toBe('Use Go.');
    expect(saveButton()!.disabled).toBe(true);

    await type('Use Rust.');
    expect(saveButton()!.disabled).toBe(false);
    await screen.press('Save');
    expect(calls.filter((c) => c.method === 'PUT')).toEqual([{ method: 'PUT', body: { company: 'Use Rust.' } }]);
    await screen.settle(() => undefined);
    expect(field().value).toBe('Use Rust.');
    expect(saveButton()!.disabled).toBe(true);
  });

  it('shows a refusal, and clears it on the next edit', async () => {
    recording((c) =>
      c.method === 'PUT'
        ? reply({ code: 'VALID_INVALID', message: 'These instructions are 40 KB, and the limit is 32 KB.', remedy: 'Shorten them.' }, 400)
        : doc,
    );
    screen = mount(<Agents canEdit />, [[['instructions'], doc]]);
    await type('x'.repeat(10));
    await screen.press('Save');
    expect(screen.text()).toContain('the limit is 32 KB. Shorten them.');
    await type('short');
    expect(screen.text()).not.toContain('the limit is 32 KB');
  });

  it('is read-only without install.policy.manage', () => {
    recording(() => doc);
    screen = mount(<Agents canEdit={false} />, [[['instructions'], doc]]);
    expect(field().disabled).toBe(true);
    expect(saveButton()).toBeUndefined();
  });

  it('shows Pando’s guidance on request', async () => {
    recording(() => doc);
    screen = mount(<Agents canEdit />, [[['instructions'], doc]]);
    expect(screen.text()).not.toContain('# Building apps on Pando');
    await screen.press('Show');
    expect(screen.text()).toContain('# Building apps on Pando');
    await screen.press('Hide');
    expect(screen.text()).not.toContain('# Building apps on Pando');
  });

  it('says when the instructions cannot be read', async () => {
    recording(() => reply({ code: 'INTERNAL', message: 'Could not read the instructions for agents.' }, 500));
    screen = mount(<Agents canEdit />);
    await screen.settle(() => undefined);
    await screen.settle(() => undefined);
    expect(screen.text()).toContain('Could not read the instructions for agents.');
  });
});

describe('nextDraft', () => {
  it('takes the server’s value into an untouched field, and keeps edits', () => {
    expect(nextDraft('Use Go.', 'Use Go.', 'Use Rust.')).toBe('Use Rust.');
    expect(nextDraft('Use Zig.', 'Use Go.', 'Use Rust.')).toBe('Use Zig.');
  });
});
