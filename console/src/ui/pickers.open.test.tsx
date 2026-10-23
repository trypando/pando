// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, useState } from 'react';
import type { ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { RecipientField } from '../admin/RecipientField';
import { RepositoryField } from '../admin/RepositoryField';
import { api } from '@api/client';
import { ActorField } from '../install/ActorField';
import { GiveAccess } from '../install/AccountApps';
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

  it('lets a click on a tag fall through to the field, and keeps its remove button', () => {
    render(<TagField label="Groups" value={['grp_a']} onChange={() => {}} nameOf={() => 'Operators'} text="" onText={() => {}} options={[]} />);
    const remove = container.querySelector<HTMLButtonElement>('button[aria-label="Remove Operators"]')!;
    expect(remove.parentElement!.style.pointerEvents).toBe('none');
    expect(remove.style.pointerEvents).toBe('auto');
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

  it('names an account first and its username second, and an unnamed one by username', () => {
    const onChange = vi.fn();
    const people = [
      { id: 'usr_01ANA', external_id: 'ana', display_name: 'Ana Ruiz' },
      { id: 'usr_01BO', external_id: 'bo', email: 'bo@example.com' },
    ];
    render(<ActorField people={people} value="" onChange={onChange} />);
    act(() => {
      const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      set.call(input(), 'usr_01');
      input().dispatchEvent(new Event('input', { bubbles: true }));
    });
    const [ana, bo] = options().map((o) => Array.from(o.children, (c) => c.textContent));
    expect(ana).toEqual(['Ana Ruiz', 'ana']);
    expect(bo).toEqual(['bo', 'bo@example.com']);
    mouse(options()[0]!, 'mousedown');
    expect(onChange).toHaveBeenCalledWith('usr_01ANA');
    expect(input().value).toBe('Ana Ruiz');
  });
});

describe('RecipientField, open', () => {
  const seed = () =>
    queries.setQueryData(['apps', 'app_01NOTES', 'principals', ''], {
      users: [
        { id: 'usr_01ANA', username: 'ana', name: 'Ana Ruiz' },
        { id: 'usr_01BO', username: 'bo' },
      ],
      groups: null,
    });

  it('renders a chosen recipient by name, closed, on the server', () => {
    seed();
    const html = renderToString(
      <QueryClientProvider client={queries}>
        <RecipientField appID="app_01NOTES" kind="user" value={{ kind: 'user', id: 'usr_01ANA', label: 'ana' }} onChange={() => {}} />
      </QueryClientProvider>,
    );
    expect(html).toContain('value="ana"');
    expect(html).toContain('aria-expanded="false"');
    expect(html).not.toContain('aria-activedescendant');
  });

  it('lists the accounts the server found and chooses one', () => {
    seed();
    const onChange = vi.fn();
    render(<RecipientField appID="app_01NOTES" kind="user" value={null} onChange={onChange} />);
    focus(input());
    expectWired(0);
    expect(container.textContent).toContain('Ana Ruiz');

    key(input(), 'ArrowDown');
    expectWired(1);
    mouse(options()[0]!, 'mousedown');
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ label: 'Ana Ruiz' }));
  });

  it('names a person first and their username second, and an unnamed one by username', () => {
    seed();
    render(<RecipientField appID="app_01NOTES" kind="user" value={null} onChange={() => {}} />);
    focus(input());
    const [ana, bo] = options().map((o) => Array.from(o.children, (c) => c.textContent));
    expect(ana).toEqual(['Ana Ruiz', 'ana']);
    expect(bo).toEqual(['bo']);
  });
});

describe('RecipientField, people and groups in one list', () => {
  it('lists both, says which is a group, and chooses either', () => {
    queries.setQueryData(['apps', 'app_01NOTES', 'principals', ''], {
      users: [{ id: 'usr_01ANA', username: 'ana', name: 'Ana Ruiz' }],
      groups: [{ id: 'grp_01DESIGN', name: 'Design' }],
    });
    const onChange = vi.fn();
    render(<RecipientField appID="app_01NOTES" kind="any" value={null} onChange={onChange} />);
    expect(container.textContent).toContain('Person or group');
    focus(input());
    const [ana, design] = options().map((o) => Array.from(o.children, (c) => c.textContent));
    expect(ana).toEqual(['Ana Ruiz', 'ana']);
    expect(design).toEqual(['Design', 'Group']);

    mouse(options()[1]!, 'mousedown');
    expect(onChange).toHaveBeenCalledWith({ kind: 'group', id: 'grp_01DESIGN', label: 'Design' });
  });
});

describe('RepositoryField, open', () => {
  const seed = () =>
    queries.setQueryData(['source-repositories', 'src_01GH', ''], [
      { id: 'https://github.com/acme/api', label: 'acme/api' },
      { id: 'https://github.com/acme/web', label: 'acme/web' },
    ]);

  it('is one field that lists repositories and chooses one', () => {
    seed();
    const onChange = vi.fn();
    render(<RepositoryField source="src_01GH" onChange={onChange} />);
    expect(container.querySelectorAll('input')).toHaveLength(1);
    expect(container.querySelector('select')).toBeNull();

    focus(input());
    expectWired(0);
    key(input(), 'ArrowDown');
    expectWired(1);
    key(input(), 'Enter');
    expect(onChange).toHaveBeenLastCalledWith('https://github.com/acme/web');
    expect(input().value).toBe('acme/web');
    expect(container.querySelector('[role="listbox"]')).toBeNull();
  });

  it('drops the choice once the text changes', () => {
    seed();
    const onChange = vi.fn();
    render(<RepositoryField source="src_01GH" onChange={onChange} />);
    focus(input());
    mouse(options()[0]!, 'mousedown');
    expect(onChange).toHaveBeenLastCalledWith('https://github.com/acme/api');

    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    act(() => {
      set.call(input(), 'acme/ap');
      input().dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(onChange).toHaveBeenLastCalledWith('');
  });

  it('says when nothing matches', () => {
    queries.setQueryData(['source-repositories', 'src_01GH', ''], []);
    render(<RepositoryField source="src_01GH" onChange={() => {}} />);
    focus(input());
    expect(container.textContent).toContain('No repositories match.');
  });

  it('asks the connection for what is typed', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue({ repositories: [{ url: 'https://github.com/acme/web', full_name: 'acme/web' }] });
    render(<RepositoryField source="src_01GH" onChange={() => {}} />);
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    act(() => {
      set.call(input(), 'web');
      input().dispatchEvent(new Event('input', { bubbles: true }));
    });
    await vi.waitFor(() => expect(get).toHaveBeenCalledWith('/sources/src_01GH/repositories?q=web'));
    await vi.waitFor(() => expect(options().map((o) => o.textContent)).toEqual(['acme/web']));
    get.mockRestore();
  });

  it('says a failed search as an error, not as no matches', async () => {
    const get = vi.spyOn(api, 'get').mockRejectedValue(new TypeError('Failed to fetch'));
    render(<RepositoryField source="src_01GH" onChange={() => {}} />);
    focus(input());
    await vi.waitFor(() => expect(container.textContent).toContain('Pando could not reach the server.'));
    expect(container.textContent).not.toContain('No repositories match.');
    get.mockRestore();
  });
});

describe('Adding an app to an account', () => {
  const dana = { kind: 'user' as const, id: 'usr_1', name: 'dana' };

  it('picks the app from one field, leaving out apps the account has', async () => {
    const get = vi.spyOn(api, 'get').mockImplementation(async (path: string) =>
      path.includes('q=led')
        ? { apps: [{ id: 'app_2', name: 'ledger' }] }
        : { apps: [{ id: 'app_1', name: 'inventory' }, { id: 'app_2', name: 'ledger' }] },
    );
    render(<GiveAccess principal={dana} has={new Set(['app_1'])} roles={[]} onClose={() => {}} />);
    await vi.waitFor(() => expect(input()).not.toBeNull());
    expect(container.querySelectorAll('input[role="combobox"]')).toHaveLength(1);
    expect(container.textContent).not.toContain('Search apps');

    focus(input());
    await vi.waitFor(() => expect(options().map((o) => o.textContent)).toEqual(['ledger']));
    key(input(), 'Enter');
    expect(input().value).toBe('ledger');
    get.mockRestore();
  });

  it('says when every app is already given', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue({ apps: [{ id: 'app_1', name: 'inventory' }] });
    render(<GiveAccess principal={dana} has={new Set(['app_1'])} roles={[]} onClose={() => {}} />);
    await vi.waitFor(() => expect(container.textContent).toContain('This account already has access to every app.'));
    expect(container.querySelector('input[role="combobox"]')).toBeNull();
    get.mockRestore();
  });

  it('reads past a first page of apps the account already has', async () => {
    const get = vi.spyOn(api, 'get').mockImplementation(async (path: string) =>
      path.includes('cursor=c2')
        ? { apps: [{ id: 'app_3', name: 'payroll' }] }
        : { apps: [{ id: 'app_1', name: 'inventory' }], next_cursor: 'c2' },
    );
    render(<GiveAccess principal={dana} has={new Set(['app_1'])} roles={[]} onClose={() => {}} />);
    await vi.waitFor(() => expect(input()).not.toBeNull());
    focus(input());
    await vi.waitFor(() => expect(options().map((o) => o.textContent)).toEqual(['payroll']));
    expect(container.textContent).not.toContain('already has access to every app');
    get.mockRestore();
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
    // Portaled to the body, so no header that hides its overflow can cut it off.
    const menu = document.querySelector<HTMLElement>('[role="menu"]')!;
    expect(menu.parentElement).toBe(document.body);
    expect(menu.tabIndex).toBe(-1);
    const items = () => Array.from(document.querySelectorAll<HTMLElement>('[role="menuitem"]'));
    expect(document.activeElement).toBe(items()[0]);

    key(document.activeElement!, 'ArrowDown');
    expect(document.activeElement).toBe(items()[1]);
    key(document.activeElement!, 'ArrowUp');
    expect(document.activeElement).toBe(items()[0]);

    key(document.activeElement!, 'Escape');
    expect(document.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(button);
  });
  // jsdom lays nothing out, so the button is stubbed at `at` (32 wide, 20
  // down) and the menu at 200 by 100. jsdom's window is 1024 wide. Tests move
  // `at` to stand for a page that moved under an open menu.
  let at = 0;
  const stubLayout = () => {
    const rect = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(() => new DOMRect(at, 20, 32, 32));
    const width = vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(200);
    const height = vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(100);
    return () => [rect, width, height].forEach((m) => m.mockRestore());
  };
  const left = () => Number.parseFloat(document.querySelector<HTMLElement>('[role="menu"]')!.style.left);
  const openMenu = () => act(() => container.querySelector<HTMLButtonElement>('button')!.click());

  const openAt = (x: number, align?: 'start' | 'end') => {
    at = x;
    const restore = stubLayout();
    try {
      render(
        <Menu label="Options for Notes" align={align}>
          {(close) => <MenuItem onSelect={close}>Rename</MenuItem>}
        </Menu>,
      );
      openMenu();
      return left();
    } finally {
      restore();
    }
  };

  it('lines up with the right of its button when it fits', () => {
    expect(openAt(500)).toBe(500 + 32 - 200);
  });

  it('moves back inside the window when it would open off the left', () => {
    // A leftward menu on a button near the left of the window: the admin
    // sidebar's, or a tile's on a narrow window.
    expect(openAt(20)).toBe(16);
  });

  it('moves left when it would run past the right of the window', () => {
    expect(openAt(900, 'start')).toBe(1024 - 200 - 16);
  });

  it('follows its button when the window is resized or the page scrolls', () => {
    at = 500;
    const restore = stubLayout();
    try {
      render(
        <Menu label="Options for Notes">
          {(close) => <MenuItem onSelect={close}>Rename</MenuItem>}
        </Menu>,
      );
      openMenu();
      expect(left()).toBe(332);
      at = 20;
      act(() => {
        window.dispatchEvent(new Event('resize'));
      });
      expect(left()).toBe(16);
      at = 400;
      act(() => {
        document.dispatchEvent(new Event('scroll'));
      });
      expect(left()).toBe(232);
    } finally {
      restore();
    }
  });

  it('is placed again when it shows another page of items', () => {
    function Paged() {
      const [page, setPage] = useState('main');
      return (
        <Menu label="Options for Notes" view={page}>
          {() => (
            <MenuItem
              onSelect={() => {
                at = 20;
                setPage('sections');
              }}
            >
              Move to section…
            </MenuItem>
          )}
        </Menu>
      );
    }
    at = 500;
    const restore = stubLayout();
    try {
      render(<Paged />);
      openMenu();
      expect(left()).toBe(332);
      act(() => document.querySelector<HTMLElement>('[role="menuitem"]')!.click());
      expect(left()).toBe(16);
    } finally {
      restore();
    }
  });
});

describe('TipButton', () => {
  // The bubble is rendered into the page's body, where no cell clips it.
  it('keeps its click from the row around it when told to', () => {
    const row = vi.fn();
    render(
      <div onClick={row}>
        <TipButton tip="Anyone on the internet can open it." label="Public" onClick={(e) => e.stopPropagation()}>
          Public
        </TipButton>
      </div>,
    );
    act(() => container.querySelector('button')!.click());
    expect(row).not.toHaveBeenCalled();
  });

  it('shows its tooltip on focus as well as hover, and says all of it in its label', () => {
    render(
      <TipButton tip="Set by PANDO_PORT." label="Environment: PANDO_PORT">
        Environment
      </TipButton>,
    );
    const button = container.querySelector<HTMLButtonElement>('button')!;
    expect(button.type).toBe('button');
    expect(button.getAttribute('aria-label')).toBe('Environment: PANDO_PORT');
    expect(document.querySelector('[role="tooltip"]')).toBeNull();

    focus(button);
    expect(document.querySelector('[role="tooltip"]')?.textContent).toBe('Set by PANDO_PORT.');
    // Outside the trigger, so a table cell's hidden overflow cannot clip it.
    expect(document.querySelector('[role="tooltip"]')?.parentElement).toBe(document.body);
    act(() => button.blur());
    expect(document.querySelector('[role="tooltip"]')).toBeNull();
  });

  it('stays open while either the pointer is over it or its trigger has focus', () => {
    render(
      <TipButton tip="Set by PANDO_PORT." label="Environment: PANDO_PORT">
        Environment
      </TipButton>,
    );
    const button = container.querySelector<HTMLButtonElement>('button')!;
    const tip = () => document.querySelector('[role="tooltip"]');
    const pointer = (type: 'mouseover' | 'mouseout') =>
      act(() => {
        button.dispatchEvent(new MouseEvent(type, { bubbles: true, relatedTarget: type === 'mouseout' ? document.body : null }));
      });

    focus(button);
    pointer('mouseover');
    pointer('mouseout');
    expect(tip()).not.toBeNull();

    pointer('mouseover');
    act(() => button.blur());
    expect(tip()).not.toBeNull();
    pointer('mouseout');
    expect(tip()).toBeNull();
  });
});
