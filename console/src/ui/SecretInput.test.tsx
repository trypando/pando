// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, useState } from 'react';
import { createRoot, type Root } from 'react-dom/client';

import { SecretInput } from './SecretInput';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let root: Root;
let host: HTMLElement;

function Field() {
  const [value, setValue] = useState('hunter2');
  return <SecretInput label="Passcode" value={value} onChange={(e) => setValue(e.target.value)} />;
}

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
  act(() => root.render(<Field />));
});

afterEach(() => {
  act(() => root.unmount());
  host.remove();
});

const field = () => host.querySelector('input') as HTMLInputElement;
const toggle = () => host.querySelector('button') as HTMLButtonElement;

// #357: a visitor who mistyped a passcode can see it to fix it.
describe('SecretInput', () => {
  it('is hidden until shown, and the label names the field', () => {
    expect(field().type).toBe('password');
    expect(toggle().getAttribute('aria-label')).toBe('Show passcode');
    expect(toggle().getAttribute('aria-pressed')).toBe('false');
    expect(host.querySelector('label')?.htmlFor).toBe(field().id);
  });

  it('shows and hides the value without losing it, and keeps focus in the field', () => {
    act(() => toggle().click());
    expect(field().type).toBe('text');
    expect(field().value).toBe('hunter2');
    expect(document.activeElement).toBe(field());
    expect(toggle().getAttribute('aria-label')).toBe('Hide passcode');
    expect(toggle().getAttribute('aria-pressed')).toBe('true');

    act(() => toggle().click());
    expect(field().type).toBe('password');
    expect(field().value).toBe('hunter2');
  });

  it('keeps the caret where it was', () => {
    field().focus();
    field().setSelectionRange(3, 3);
    act(() => toggle().click());
    expect([field().selectionStart, field().selectionEnd]).toEqual([3, 3]);
  });

  it('leaves room for the toggle at the end of the field', () => {
    expect(field().style.paddingRight).toBe('var(--control-input)');
  });
});
