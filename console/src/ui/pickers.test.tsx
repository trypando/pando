import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import type { ReactNode } from 'react';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { RecipientField } from '../admin/RecipientField';
import { ActorField } from '../install/ActorField';
import { Menu, MenuItem } from './Menu';
import { TagField } from './TagField';

const render = (node: ReactNode) =>
  renderToString(<QueryClientProvider client={new QueryClient()}>{node}</QueryClientProvider>);

// Every list that shares ui/combobox renders closed, as a field or a button,
// before anyone has typed or clicked.
describe('pickers render closed', () => {
  it('ActorField', () => {
    const html = render(<ActorField people={[]} value="" onChange={() => {}} label="Actor" />);
    expect(html).toContain('Username, email or ID');
    expect(html).toContain('aria-expanded="false"');
  });

  it('RecipientField', () => {
    const html = render(<RecipientField appID="app_01NOTES" kind="user" value={null} onChange={() => {}} />);
    expect(html).toContain('aria-expanded="false"');
  });

  it('TagField', () => {
    const html = render(
      <TagField label="Apps" value={['app_1']} onChange={() => {}} nameOf={() => 'Notes'} text="" onText={() => {}} options={[]} />,
    );
    expect(html).toContain('Notes');
  });

  it('Menu', () => {
    const html = render(
      <Menu label="Options for Notes">
        {(close) => <MenuItem onSelect={close}>Rename</MenuItem>}
      </Menu>,
    );
    expect(html).toContain('Options for Notes');
    expect(html).not.toContain('Rename');
  });
});
