import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

// Every Select's props, by label, so a test can choose an option the way the
// browser would. Rendered on the server there is no browser to do it.
const selects = vi.hoisted(() => new Map<string, { onChange: (e: { target: { value: string } }) => void }>());
vi.mock('@design', async (original) => {
  const design = await original<typeof import('@design')>();
  return {
    ...design,
    Select: (props: Parameters<typeof design.Select>[0]) => {
      selects.set(String(props.label), props as never);
      return design.Select(props);
    },
  };
});

import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AddApp } from './AddApp';
import { FilePicker } from './UploadSource';

function render(): string {
  const client = new QueryClient();
  return renderToString(
    <QueryClientProvider client={client}>
      <AddApp onAdded={() => {}} onClose={() => {}} />
    </QueryClientProvider>,
  );
}

describe('Add an app', () => {
  it('asks for the source first, then the repository, then the name', () => {
    const html = render();
    const source = html.indexOf('>Source<');
    const repository = html.indexOf('>Repository<');
    const name = html.indexOf('>Name<');
    expect(source).toBeGreaterThan(-1);
    expect(repository).toBeGreaterThan(source);
    expect(name).toBeGreaterThan(repository);
  });

  it('names each source plainly', () => {
    const html = render();
    expect(html).toContain('>Git repository<');
    expect(html).toContain('>Upload<');
    expect(html).toContain('>Container image<');
  });

  it('takes a choice of source without complaint', () => {
    render();
    expect(() => selects.get('Source')!.onChange({ target: { value: 'image' } })).not.toThrow();
  });

  it('says only what is in the dialog', () => {
    const html = render();
    expect(html).toContain('Choose a source and name the app.');
    expect(html).toContain('Private repositories use a source connection.');
    for (const gone of [
      'Nothing deploys until you accept',
      'Where it comes from',
      'Nothing is read from it at deploy time',
      'What you will call it here',
    ]) {
      expect(html).not.toContain(gone);
    }
  });
});

describe('Choosing files', () => {
  it('says where to drag them and how much fits', () => {
    const html = renderToString(<FilePicker files={[]} onPick={() => {}} />);
    expect(html).toContain('Drag a folder or files here.');
    expect(html).toContain('Choose a folder');
    expect(html).toContain('Choose files');
    expect(html).toContain('Up to 256 MB, compressed.');
    expect(html).not.toContain('index.html');
    expect(html).not.toContain('node_modules');
  });
});
