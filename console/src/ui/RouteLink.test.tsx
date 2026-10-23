// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import type { ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { SidebarNav, Tabs } from '@design';

import { RouteLink } from './RouteLink';
import { Table } from './Table';

// The console's pages are links: a plain click goes there in place, and ⌘-,
// Ctrl- or middle-click is left to the browser, which opens a new tab.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let root: Root;
let container: HTMLDivElement;

beforeEach(() => {
  container = document.createElement('div');
  document.body.append(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.restoreAllMocks();
});

function render(node: ReactNode) {
  act(() => root.render(node));
}

/** Clicks, and says whether the page let the browser follow the link. The
 *  browser's part is then cancelled: jsdom cannot navigate. */
function click(el: Element, init: MouseEventInit = {}): boolean {
  let followed = true;
  const settle = (e: Event) => {
    followed = !e.defaultPrevented;
    e.preventDefault();
  };
  window.addEventListener('click', settle);
  act(() => {
    el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, ...init }));
  });
  window.removeEventListener('click', settle);
  return followed;
}

const link = (text: string) =>
  Array.from(container.querySelectorAll<HTMLAnchorElement>('a')).find((a) => a.textContent?.includes(text))!;

describe('RouteLink', () => {
  it('follows a plain click in place, and leaves a modified one to the browser', () => {
    const onFollow = vi.fn();
    render(
      <RouteLink href="/admin" onFollow={onFollow}>
        Admin
      </RouteLink>,
    );
    const a = link('Admin');
    expect(a.getAttribute('href')).toBe('/admin');
    expect(a.style.textDecoration).toBe('none');

    expect(click(a)).toBe(false);
    expect(onFollow).toHaveBeenCalledTimes(1);

    for (const init of [{ metaKey: true }, { ctrlKey: true }, { shiftKey: true }, { altKey: true }]) {
      expect(click(a, init)).toBe(true);
    }
    expect(onFollow).toHaveBeenCalledTimes(1);
  });
});

describe('SidebarNav', () => {
  it('renders an item with an href as a link', () => {
    const onChange = vi.fn();
    render(
      <SidebarNav
        value="apps"
        onChange={onChange}
        items={[
          { value: 'apps', label: 'Apps', href: '/admin' },
          { value: 'audit', label: 'Audit log', href: '/admin/audit' },
        ]}
      />,
    );
    const audit = link('Audit log');
    expect(audit.getAttribute('href')).toBe('/admin/audit');
    expect(link('Apps').getAttribute('aria-current')).toBe('page');

    expect(click(audit, { metaKey: true })).toBe(true);
    expect(onChange).not.toHaveBeenCalled();

    expect(click(audit)).toBe(false);
    expect(onChange).toHaveBeenCalledWith('audit');
  });

  it('lets the browser follow an item no onChange handles', () => {
    render(<SidebarNav value="a" items={[{ value: 'b', label: 'B', href: '/admin/b' }]} />);
    expect(click(link('B'))).toBe(true);
  });

  it('keeps an item without an href a button', () => {
    const onChange = vi.fn();
    render(<SidebarNav value="a" onChange={onChange} items={[{ value: 'a', label: 'A' }]} />);
    expect(container.querySelector('a')).toBeNull();
    click(container.querySelector('button')!);
    expect(onChange).toHaveBeenCalledWith('a');
  });
});

describe('Tabs', () => {
  it('renders a tab with an href as a link', () => {
    const onChange = vi.fn();
    render(
      <Tabs
        value="overview"
        onChange={onChange}
        items={[
          { value: 'overview', label: 'Overview', href: '/admin/apps/app_1/overview' },
          { value: 'logs', label: 'Logs', href: '/admin/apps/app_1/logs' },
        ]}
      />,
    );
    const logs = link('Logs');
    expect(logs.getAttribute('href')).toBe('/admin/apps/app_1/logs');
    expect(logs.getAttribute('role')).toBe('tab');

    expect(click(logs, { ctrlKey: true })).toBe(true);
    expect(onChange).not.toHaveBeenCalled();

    expect(click(logs)).toBe(false);
    expect(onChange).toHaveBeenCalledWith('logs');
  });

  it('lets the browser follow a controlled tab no onChange handles', () => {
    render(<Tabs value="a" items={[{ value: 'b', label: 'B', href: '/b' }]} />);
    expect(click(link('B'))).toBe(true);
  });
});

describe('Table rowHref', () => {
  const rows = [{ id: 'app_1', name: 'shop' }];
  const columns = [{ key: 'name', header: 'Name' }];

  function table(onRowClick?: (row: unknown) => void, cols: typeof columns = columns) {
    render(
      <Table
        columns={cols}
        rows={rows}
        onRowClick={onRowClick}
        rowHref={(r: { id: string }) => `/admin/apps/${r.id}`}
      />,
    );
    return Array.from(container.querySelectorAll('div')).find((d) => d.textContent === 'shop' && d.style.display === 'grid')!;
  }

  it('opens the row in a new tab on ⌘- or Ctrl-click, and in place on a plain click', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const onRowClick = vi.fn();
    const row = table(onRowClick);

    click(row, { metaKey: true });
    click(row, { ctrlKey: true });
    expect(open).toHaveBeenCalledTimes(2);
    expect(open).toHaveBeenCalledWith('/admin/apps/app_1', '_blank', 'noopener');
    expect(onRowClick).not.toHaveBeenCalled();

    click(row);
    expect(onRowClick).toHaveBeenCalledWith(rows[0]);
    expect(open).toHaveBeenCalledTimes(2);
  });

  it('opens the row in a new tab on a middle click', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const onRowClick = vi.fn();
    const row = table(onRowClick);

    let scrolled = true;
    act(() => {
      scrolled = row.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, button: 1 }));
      row.dispatchEvent(new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 }));
      row.dispatchEvent(new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 2 }));
    });
    expect(scrolled).toBe(false);
    expect(open).toHaveBeenCalledTimes(1);
    expect(onRowClick).not.toHaveBeenCalled();
  });

  it('opens the row on ⌘-click with no onRowClick', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const row = table();
    click(row, { metaKey: true });
    click(row);
    expect(open).toHaveBeenCalledTimes(1);
  });

  it('leaves a click on a control inside the row to the control', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const onRowClick = vi.fn();
    table(onRowClick, [{ key: 'name', header: 'Name', render: () => <button type="button">Tip</button> } as never]);
    const button = container.querySelector('button')!;

    click(button, { metaKey: true });
    act(() => {
      button.dispatchEvent(new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 }));
    });
    expect(open).not.toHaveBeenCalled();
  });
});
