// Which AI adapter handles each AI function, and on which model (R-259), as
// GET /ai/functions reports it. Chosen on each AI adapter (AdapterFunctions);
// read here by the screens that offer a function, to offer it only when on.

import { useQuery } from '@tanstack/react-query';

import { api } from '@api/client';

export interface AIFunction {
  function: string;
  title: string;
  adapter_id?: string;
  model?: string;
  effective_model?: string;
  on: boolean;
  off?: string;
  source?: { kind: string; name?: string; key?: string };
  overridden?: { adapter_id: string; model?: string };
}

/**
 * Whether an AI function is on, for a screen deciding whether to offer it.
 * AI appears only where it can do something (design 08): a question field for
 * a function nobody assigned would only ever answer "not assigned". False
 * while loading, and when the caller cannot read the list.
 */
export function useAIFunctionOn(fn: string): boolean {
  return useAIFunctionState(fn) === 'on';
}

/** On, off, or unknown — the last when the caller cannot read the list, which
 *  needs install.view. A screen anyone may use offers the function unless it
 *  is known to be off, and the server's answer says the rest. */
export function useAIFunctionState(fn: string): 'on' | 'off' | 'unknown' {
  const functions = useQuery({
    queryKey: ['ai-functions'],
    queryFn: () => api.get<{ functions: AIFunction[] }>('/ai/functions'),
    retry: false,
  });
  const f = functions.data?.functions.find((x) => x.function === fn);
  if (!f) return functions.isPending ? 'off' : 'unknown';
  return f.on ? 'on' : 'off';
}
