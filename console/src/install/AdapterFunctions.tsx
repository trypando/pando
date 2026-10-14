// What an AI adapter handles, chosen on the adapter itself (R-259).
//
// Open the Anthropic adapter and tick what Anthropic does; open the OpenAI one
// and tick what it does. One function has one adapter at a time, so a function
// another adapter handles is shown with who handles it rather than offered:
// the server refuses a second adapter outright, and moving it is removing it
// there first. A function the config file assigns is shown with where (R-271).

import { useState } from 'react';
import { Button, Checkbox, Input } from '@design';

import { BesideField } from '../ui/BesideField';

import type { AIFunction } from './AIFunctions';

/** Each function this adapter should handle, with its model ('' for the
 *  adapter's own). Absent means not handled here. */
export type FunctionChoice = Record<string, string>;

/** What this adapter handles now, as a starting choice. */
export function currentChoice(functions: AIFunction[], adapterID: string): FunctionChoice {
  const out: FunctionChoice = {};
  for (const f of functions) if (f.adapter_id === adapterID) out[f.function] = f.model ?? '';
  return out;
}

/** The calls that turn `from` into `to` for this adapter. */
export function choiceChanges(adapterID: string, from: FunctionChoice, to: FunctionChoice) {
  const calls: { method: 'PUT' | 'DELETE'; fn: string; model?: string }[] = [];
  for (const fn of Object.keys(from)) if (!(fn in to)) calls.push({ method: 'DELETE', fn });
  for (const [fn, model] of Object.entries(to)) {
    if (!(fn in from) || from[fn] !== model) calls.push({ method: 'PUT', fn, model });
  }
  return calls.map((c) => ({ ...c, adapterID }));
}

export function AdapterFunctions({
  adapterID,
  functions,
  choice,
  onChange,
}: {
  adapterID: string;
  functions: AIFunction[];
  choice: FunctionChoice;
  onChange: (next: FunctionChoice) => void;
}) {
  // Functions whose model field was opened and is still empty.
  const [overriding, setOverriding] = useState<Set<string>>(new Set());
  return (
    <fieldset style={{ border: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <legend style={{ font: 'var(--type-label)', marginBottom: 'var(--space-2)' }}>Functions</legend>
      <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
        Each function is handled by one AI adapter. Changes take effect when you save.
      </p>
      {functions.map((f) => {
        const mine = f.function in choice;
        const declared = f.source?.kind === 'file';
        const elsewhere = f.adapter_id && f.adapter_id !== adapterID ? f.adapter_id : '';
        const why = declared
          ? `Set in ${f.source?.name ?? 'the config file'}, at ${f.source?.key ?? ''}.`
          : elsewhere
            ? `Handled by ${elsewhere}. Remove it there to handle it here.`
            : undefined;
        return (
          <div key={f.function} style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-start', flexWrap: 'wrap' }}>
            <div style={{ flex: '1 1 16rem' }}>
              <Checkbox
                label={f.title}
                description={why}
                checked={mine}
                disabled={declared || Boolean(elsewhere)}
                onChange={(e) => {
                  const next = { ...choice };
                  if (e.target.checked) next[f.function] = '';
                  else delete next[f.function];
                  onChange(next);
                }}
              />
            </div>
            {/* The model is the adapter's unless someone asks otherwise, so
                the field stays out of the way until then. One with an override
                already shows it. */}
            {mine && !declared && (overriding.has(f.function) || choice[f.function] !== '') ? (
              <div style={{ flex: '1 1 14rem', display: 'flex', gap: 'var(--space-2)', alignItems: 'flex-end' }}>
                <Input
                  label={`Model for ${f.title.toLowerCase()}`}
                  mono
                  autoFocus={overriding.has(f.function) && choice[f.function] === ''}
                  placeholder="claude-haiku-4-5"
                  value={choice[f.function]}
                  onChange={(e) => onChange({ ...choice, [f.function]: e.target.value })}
                  style={{ flex: 1 }}
                />
                <BesideField>
                  <Button
                    variant="ghost"
                    onClick={() => {
                      setOverriding((was) => new Set([...was].filter((x) => x !== f.function)));
                      onChange({ ...choice, [f.function]: '' });
                    }}
                  >
                    Use default
                  </Button>
                </BesideField>
              </div>
            ) : (
              mine &&
              !declared && (
                <Button variant="ghost" onClick={() => setOverriding((was) => new Set(was).add(f.function))}>
                  Override model
                </Button>
              )
            )}
          </div>
        );
      })}
    </fieldset>
  );
}
