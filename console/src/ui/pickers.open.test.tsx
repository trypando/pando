// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import type { ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { RecipientField } from '../admin/RecipientField';
import { ActorField } from '../install/ActorField';
import { Menu, MenuItem } from './Menu';
import { TagField } from './TagField';
import { TipButton } from './TipButton';

// Rendered into a document, so the lists can be opened and driven: what a
// screen reader follows is the input's aria-activedescendant, which only
// exists once a list is open.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let root: Root;
let container: HTMLDivElement;
let queries: QueryClient;

beforeEach(() => {
  container = document.createElement('div');
  document.body.append(container);
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

function render(node: ReactNode) {
  act(() => root.render(<QueryClientProvider client={queries}>{node}</QueryClientProvider>));
}

const input = () => container.querySelector<HTMLInputElement>('input[role="combobox"]')!;
const options = () => Array.from(container.querySelectorAll<HTMLElement>('[role="option"]'));
const key = (el: Element, k: string) =>
  act(() => {
    el.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true }));
  });
const mouse = (el: Element, type: 'mousedown' | 'mouseover') =>
  act(() => {
    el.dispatchEvent(new MouseEvent(type, { bubbles: true, cancelable: true }));
  });
const focus = (el: HTMLElement) => act(() => el.focus());

/** The open list is wired the way the combobox pattern says. */
function expectWired(active: number) {
  const list = container.querySelector('[role="listbox"]')!;
  expect(list).not.toBeNull();
  expect(input().getAttribute('aria-controls')).toBe(list.id);
  expect(input().getAttribute('aria-activedescendant')).toBe(options()[active]!.id);
  options().forEach((o, i) => {
    expect(o.tabIndex).toBe(-1);
    expect(o.getAttribute('aria-selected')).toBe(String(i === active));
  });
}

describe('TagField, open', () => {
  const tags = [
    { id: 'grp_a', name: 'Operators', detail: 'Three people' },
    { id: 'grp_b', name: 'Readers' },
  ];

  it('names the highlighted option to a screen reader and follows the arrow keys', () => {
    render(<TagField label="Groups" value={[]} onChange={() => {}} nameOf={(id) => id} text="" onText={() => {}} options={tags} />);
    expect(input().getAttribute('aria-activedescendant')).toBeNull();

    focus(input());
    expectWired(0);
    expect(container.textContent).toContain('Three people');

    key(input(), 'ArrowDown');
    expectWired(1);
    mouse(options()[0]!, 'mouseover');
    expectWired(0);

    key(input(), 'Escape');
    expect(container.querySelector('[role="listbox"]')).toBeNull();
    expect(input().getAttribute('aria-activedescendant')).toBeNull();
  });

  it('adds what is chosen with the mouse or Enter', () => {
    const onChange = vi.fn();
    render(<TagField label="Groups" value={[]} onChange={onChange} nameOf={(id) => id} text="" onText={() => {}} options={tags} />);
    focus(input());
    mouse(options()[1]!, 'mousedown');
    expect(onChange).toHaveBeenLastCalledWith(['grp_b']);
    key(input(), 'Enter');
    expect(onChange).toHaveBeenLastCalledWith(['grp_a']);
  });

  it('sends a click on the empty part of the field to the text', () => {
    render(<TagField label="Groups" value={['grp_a']} onChange={() => {}} nameOf={() => 'Operators'} text="" onText={() => {}} options={[]} />);
    const area = container.querySelector<HTMLElement>('[aria-hidden="true"]')!;
    act(() => area.click());
    expect(document.activeElement).toBe(input());
  });
});

describe('ActorField, open', () => {
  it('offers Pando’s own actors and chooses one', () => {
    const onChange = vi.fn();
    render(<ActorField people={[]} value="" onChange={onChange} />);
    focus(input());
    expectWired(0);
    expect(options()[0]!.textContent).toContain('Any Pando process');

    key(input(), 'ArrowDown');
    expectWired(1);
    mouse(options()[0]!, 'mousedown');
    expect(onChange).toHaveBeenCalledWith('kind:system');
  });
});

describe('RecipientField, open', () => {
  it('lists the accounts the server found and chooses one', () => {
    queries.setQueryData(['apps', 'app_01NOTES', 'principals', ''], {
      users: [
        { id: 'usr_01ANA', username: 'ana', name: 'Ana Ruiz' },
        { id: 'usr_01BO', username: 'bo' },
      ],
      groups: null,
    });
    const onChange = vi.fn();
    render(<RecipientField appID="app_01NOTES" kind="user" value={null} onChange={onChange} />);
    focus(input());
    expectWired(0);
    expect(container.textContent).toContain('Ana Ruiz');

    key(input(), 'ArrowDown');
    expectWired(1);
    mouse(options()[0]!, 'mousedown');
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ label: 'ana' }));
  });
});

describe('Menu, open', () => {
  it('moves with the arrow keys on the menu and closes on Escape', () => {
    render(
      <Menu label="Options for Notes">
        {(close) => (
          <>
            <MenuItem onSelect={close}>Rename</MenuItem>
            <MenuItem onSelect={close}>Delete</MenuItem>
          </>
        )}
      </Menu>,
    );
    const button = container.querySelector<HTMLButtonElement>('button')!;
    act(() => button.click());
    const menu = container.querySelector<HTMLElement>('[role="menu"]')!;
    expect(menu.tabIndex).toBe(-1);
    const items = () => Array.from(container.querySelectorAll<HTMLElement>('[role="menuitem"]'));
    expect(document.activeElement).toBe(items()[0]);

    key(document.activeElement!, 'ArrowDown');
    expect(document.activeElement).toBe(items()[1]);
    key(document.activeElement!, 'ArrowUp');
    expect(document.activeElement).toBe(items()[0]);

    key(document.activeElement!, 'Escape');
    expect(container.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(button);
  });
});

describe('TipButton', () => {
  it('shows its tooltip on focus as well as hover, and says all of it in its label', () => {
    render(
      <TipButton tip="Set by PANDO_PORT." label="Environment: PANDO_PORT">
        Environment
      </TipButton>,
    );
    const button = container.querySelector<HTMLButtonElement>('button')!;
    expect(button.type).toBe('button');
    expect(button.getAttribute('aria-label')).toBe('Environment: PANDO_PORT');
    expect(container.querySelector('[role="tooltip"]')).toBeNull();

    focus(button);
    expect(container.querySelector('[role="tooltip"]')?.textContent).toBe('Set by PANDO_PORT.');
    act(() => button.blur());
    expect(container.querySelector('[role="tooltip"]')).toBeNull();
  });
});
