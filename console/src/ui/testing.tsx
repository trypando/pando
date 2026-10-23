// Rendering a screen in a test, over a query cache seeded with its data.
//
// On the server, for what a screen shows as it opens; into a document, for
// what it shows after somebody presses, types or waits on a request. Used by
// tests only.

import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AppVerbs } from '../admin/verbs';

/** Query data by key, put in the cache before the screen renders. */
export type Seed = Array<[unknown[], unknown]>;

function wrapped(screen: ReactNode, data: Seed, verbs: string[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  for (const [key, value] of data) client.setQueryData(key, value);
  return (
    <QueryClientProvider client={client}>
      <AppVerbs.Provider value={verbs}>{screen}</AppVerbs.Provider>
    </QueryClientProvider>
  );
}

/** The screen's server output, without the empty comments React separates
 *  adjacent text with. `verbs` are the caller's on the open app. */
export function serverRender(screen: ReactNode, data: Seed = [], verbs: string[] = []): string {
  return renderToString(wrapped(screen, data, verbs)).replaceAll('<!-- -->', '');
}

/** A JSON response, as the API sends one. */
export function reply(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

/** A request that never answers, so a screen stays waiting on it. */
export function unanswered(): Promise<Response> {
  return new Promise<Response>(() => {});
}

/** A fetch that answers a request whose address holds one of `routes`' keys,
 *  the first that matches, with its value, and leaves any other unanswered. */
export function answering(routes: Record<string, unknown>) {
  return async (url: string | URL | Request) => {
    const path = url instanceof Request ? url.url : String(url);
    const match = Object.keys(routes).find((part) => path.includes(part));
    return match === undefined ? unanswered() : reply(routes[match]);
  };
}

async function settle(change: () => void) {
  await act(async () => {
    change();
    await new Promise((r) => setTimeout(r, 0));
  });
}

/** A screen rendered into the document, and what a person can do to it. */
export interface Mounted {
  /** Everything the screen says. */
  text: () => string;
  /** Presses the last button that reads exactly `label`: a dialog's, when
   *  the screen under it has one too. */
  press: (label: string) => Promise<void>;
  /** Clicks the innermost element that reads exactly `text`. */
  click: (text: string) => Promise<void>;
  /** Types into, or picks in, the field labeled `label`. */
  fill: (label: string, value: string) => Promise<void>;
  /** Submits the form the field labeled `label` is in. */
  submit: (label: string) => Promise<void>;
  unmount: () => void;
}

export function mount(screen: ReactNode, data: Seed = [], verbs: string[] = []): Mounted {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  act(() => root.render(wrapped(screen, data, verbs)));

  // The design system's fields are found by the ids it gives them: f- for an
  // input, s- for a select, then the label in lowercase words.
  const field = (label: string) => {
    const slug = label.replace(/\W+/g, '-').toLowerCase();
    return (document.getElementById('f-' + slug) ?? document.getElementById('s-' + slug)) as HTMLInputElement | HTMLSelectElement;
  };

  return {
    text: () => String(container.textContent),
    press: async (label) => {
      // Trimmed: an icon's markup leaves whitespace before the label.
      const buttons = [...container.querySelectorAll('button')].filter((b) => b.textContent?.trim() === label);
      await settle(() => buttons.at(-1)!.click());
    },
    click: async (text) => {
      const all = [...container.querySelectorAll<HTMLElement>('*')].filter((el) => el.textContent === text);
      await settle(() => all.at(-1)!.click());
    },
    fill: async (label, value) => {
      const el = field(label);
      const proto = el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
      await settle(() => {
        Object.getOwnPropertyDescriptor(proto, 'value')!.set!.call(el, value);
        el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? 'change' : 'input', { bubbles: true }));
      });
    },
    submit: async (label) => {
      await settle(() => field(label).closest('form')!.requestSubmit());
    },
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}
