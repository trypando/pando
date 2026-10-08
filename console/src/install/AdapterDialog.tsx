// Adding or changing an adapter — the console's form for POST /adapters.
//
// The API could configure an adapter and the console could not, which is the
// gap R-261 exists to close: every surface is a client of the same API, and
// none may lack a capability it has. The form is generated from
// GET /adapters/kinds, so a kind added to Pando appears here without this file
// learning about it.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, Dialog, Input, Radio, Select } from '@design';

import { api } from '@api/client';
import { Quiet, refusal } from './Accounts';
import { Term } from '../ui/Term';
import { FieldSkeleton, Loading } from '../ui/Loading';
import { Disclosure } from '../ui/Disclosure';
import {
  adapterRequest,
  advancedChanged,
  blankForm,
  boolValue,
  categoryLabel,
  categoryNote,
  fieldPlaceholder,
  formProblems,
  isShown,
  kindKey,
  orderCategories,
  sortKinds,
} from './adapters';
import type { AdapterForm, AdapterKind, KindField } from './adapters';
import { AdapterFunctions, choiceChanges, currentChoice } from './AdapterFunctions';
import type { FunctionChoice } from './AdapterFunctions';
import type { AIFunction } from './AIFunctions';

/** A configured adapter, as GET /adapters returns it. */
export interface ConfiguredAdapter {
  id: string;
  category: string;
  kind: string;
  name?: string;
  is_default?: boolean;
  enabled?: boolean;
  healthy?: boolean;
  /** Which credentials are stored — names only; no value is ever returned. */
  credentials_set?: string[];
  /** The stored settings (never credentials), returned to whoever may change
   *  adapters, so a change starts from what is there. */
  config?: Record<string, unknown>;
}

/** An adapter's stored settings as the form's values: text for strings and
 *  numbers, as the inputs hold them, and booleans as they are. */
function storedValues(config: Record<string, unknown> | undefined): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (const [key, value] of Object.entries(config ?? {})) {
    if (typeof value === 'boolean') out[key] = value;
    else if (typeof value === 'string' || typeof value === 'number') out[key] = String(value);
  }
  return out;
}

export function AdapterDialog({
  existing,
  category: startCategory,
  adapters,
  onClose,
  onSaved,
}: {
  /** The adapter being changed, or none to add one. */
  existing?: ConfiguredAdapter;
  /** The category to start in, when adding from a category's section. */
  category?: string;
  /** Every configured adapter, to know whether a category has one yet. */
  adapters: ConfiguredAdapter[];
  onClose: () => void;
  /** Told the saved adapter's name, for the restart notice. */
  onSaved: (name: string, category: string) => void;
}) {
  const kinds = useQuery({
    queryKey: ['adapter-kinds'],
    queryFn: () => api.get<{ kinds: AdapterKind[] | null }>('/adapters/kinds'),
  });
  const catalog = sortKinds(kinds.data?.kinds ?? []);

  const [category, setCategory] = useState(existing?.category ?? startCategory ?? '');
  const [picked, setPicked] = useState(existing ? kindKey(existing) : '');
  // A source connection is used as soon as it is saved (R-091), and so is an
  // image registry (issue #153); every other category is loaded at startup.
  // The dialog says which.
  const isSource = category === 'source';
  const isRegistry = category === 'image_registry';
  // An image registry can be turned off here, keeping its settings: what
  // removing it did before it was an adapter.
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const categories = orderCategories(catalog.map((k) => k.category));
  const inCategory = catalog.filter((k) => k.category === category);
  // A category with one kind has nothing to choose between.
  const only = inCategory.length === 1 ? inCategory[0] : undefined;
  const chosen = picked || (only ? kindKey(only) : '');
  const [draft, setDraft] = useState<AdapterForm | null>(null);
  const [tried, setTried] = useState(false);

  const kind = catalog.find((k) => kindKey(k) === chosen);
  const stored = existing?.credentials_set ?? [];

  // The form starts from the kind until someone types into it.
  const initial: AdapterForm | null = !kind
    ? null
    : existing
      ? {
          id: existing.id,
          name: existing.name || kind.name,
          isDefault: existing.is_default ?? false,
          values: storedValues(existing.config),
        }
      : blankForm(kind, !adapters.some((a) => a.category === kind.category));
  const form = draft ?? initial;
  const edit = (patch: Partial<AdapterForm>) => form && setDraft({ ...form, ...patch });
  const setValue = (key: string, value: string | boolean) =>
    form && setDraft({ ...form, values: { ...form.values, [key]: value } });

  const problems = kind && form ? formProblems(kind, form, stored) : {};
  const shown = (key: string) => (tried ? problems[key] : undefined);

  // An AI adapter's functions are chosen here, on the adapter (R-259). Only a
  // running one can be given functions: the server checks what it advertises.
  const queries = useQueryClient();
  const isAI = Boolean(existing) && (existing?.category ?? category) === 'ai';
  const functions = useQuery({
    queryKey: ['ai-functions'],
    queryFn: () => api.get<{ functions: AIFunction[] }>('/ai/functions'),
    enabled: isAI,
  });
  const had = currentChoice(functions.data?.functions ?? [], existing?.id ?? '');
  const [choice, setChoice] = useState<FunctionChoice | null>(null);
  const chosenFunctions = choice ?? had;

  const save = useMutation({
    mutationFn: async () => {
      // The adapter's own settings are saved only when they changed: that is
      // what needs a restart, and choosing functions does not.
      const settingsChanged = draft !== null || !existing;
      if (settingsChanged) {
        await api.post<{ id: string; note?: string }>('/adapters', adapterRequest(kind!, form!, isRegistry ? enabled : existing?.enabled));
      }
      if (existing && choice) {
        for (const c of choiceChanges(existing.id, had, choice)) {
          if (c.method === 'DELETE') await api.del(`/ai/functions/${c.fn}`);
          else await api.put(`/ai/functions/${c.fn}`, { adapter_id: c.adapterID, model: c.model ?? '' });
        }
      }
      return settingsChanged;
    },
    onSuccess: (settingsChanged) => {
      if (settingsChanged) onSaved(form!.name.trim() || kind!.name, kind!.category);
      else onClose();
    },
    onSettled: () => void queries.invalidateQueries({ queryKey: ['ai-functions'] }),
  });

  const submit = () => {
    setTried(true);
    if (!kind || !form) return;
    // Settings left as they were are not re-checked: only functions changed.
    if ((draft !== null || !existing) && Object.keys(problems).length > 0) return;
    save.mutate();
  };

  // Saving replaces the stored settings whole. GET /adapters returns them to
  // whoever may change adapters, and the form starts from them; if they did
  // not come back, a change starts from empty fields, and that is said before
  // anything is typed rather than discovered after a restart.
  const settings = (kind?.fields ?? []).filter((f) => !f.credential);
  const missingKind = existing && kinds.isSuccess && !kind;

  const visible = kind && form ? (kind.fields ?? []).filter((f) => isShown(kind, f, form.values)) : [];
  const basic = visible.filter((f) => !f.advanced);
  const advanced = visible.filter((f) => f.advanced);
  const fieldInput = (f: KindField) => (
    <FieldInput
      key={f.key}
      field={f}
      value={form?.values[f.key]}
      stored={existing ? stored.includes(f.key) : undefined}
      error={shown(f.key)}
      onChange={(v) => setValue(f.key, v)}
    />
  );

  return (
    <Dialog
      open
      title={existing ? `Edit ${existing.name || existing.id}` : 'Add adapter'}
      description={
        isSource
          ? 'Pando reads the private repositories this connection covers with it, from the moment it is saved.'
          : isRegistry
            ? 'Pando pushes built images here when a runtime pulls them, from the moment it is saved.'
            : 'Pando reads adapters when it starts, so a saved change takes effect after a restart.'
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!kind || !form || save.isPending} onClick={submit}>
            {existing ? (save.isPending ? 'Saving' : 'Save adapter') : save.isPending ? 'Adding' : 'Add adapter'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {kinds.isPending ? (
          <Loading gap="var(--space-4)">
            <FieldSkeleton />
            <FieldSkeleton />
          </Loading>
        ) : kinds.isError ? (
          <Banner tone="failed">{refusal(kinds.error)}</Banner>
        ) : missingKind ? (
          <Quiet>
            This build of Pando has no {existing.category} adapter of kind {existing.kind}, so it can&rsquo;t be
            changed here.
          </Quiet>
        ) : (
          <>
            {/* Changing an adapter, its category, kind and ID are fixed: a
                different kind or ID would be a different adapter, and is added
                as one. So they are stated, not offered as fields. */}
            {existing ? (
              <dl style={{ display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: 'var(--space-2) var(--space-4)', margin: 0 }}>
                <Term label="Category">{categoryLabel(existing.category)}</Term>
                <Term label="Adapter">{kind?.name ?? existing.kind}</Term>
                <Term label="ID">
                  <code style={{ font: 'var(--type-code)' }}>{existing.id}</code>
                </Term>
              </dl>
            ) : (
            <>
            {/* Category first, then the adapter within it: the question someone
                arrives with is "I need a builder", not a list of every kind. */}
            <div>
              <Select
                label="Category"
                value={category}
                options={[
                  ...(category ? [] : [{ value: '', label: 'Choose a category' }]),
                  ...categories.map((c) => ({ value: c, label: categoryLabel(c) })),
                ]}
                onChange={(e) => {
                  setCategory(e.target.value);
                  setPicked('');
                  setDraft(null);
                  setTried(false);
                }}
              />
              {category && categoryNote(category) && (
                <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 'var(--space-1) 0 0' }}>
                  {categoryNote(category)}
                </p>
              )}
            </div>
            {category && (
              <div>
                <Select
                  label="Adapter"
                  value={chosen}
                  options={[
                    ...(chosen ? [] : [{ value: '', label: 'Choose an adapter' }]),
                    ...inCategory.map((k) => ({ value: kindKey(k), label: k.name })),
                  ]}
                  onChange={(e) => {
                    setPicked(e.target.value);
                    setDraft(null);
                    setTried(false);
                  }}
                />
                {kind && (
                  <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 'var(--space-1) 0 0' }}>
                    {kind.description}
                  </p>
                )}
              </div>
            )}
            </>
            )}

            {kind && form && (
              <>
                {existing && !existing.config && settings.length > 0 && (
                  <Banner tone="info">
                    Pando doesn&rsquo;t show an adapter&rsquo;s current settings. Enter each one you want to keep:
                    saving clears a setting left empty. A stored credential is kept unless you enter a new one.
                  </Banner>
                )}

                {!existing && (
                  <Input
                    label="ID"
                    mono
                    value={form.id}
                    helper="How specs and the CLI refer to this adapter. It can't be changed later."
                    error={shown('id')}
                    onChange={(e) => edit({ id: e.target.value })}
                  />
                )}
                <Input label="Name" value={form.name} onChange={(e) => edit({ name: e.target.value })} />

                {basic.map(fieldInput)}

                {/* Less common settings, each with a default, a click away
                    rather than beside the ones that matter. Open from the
                    start when one is set to something else, so nothing
                    configured is hidden. */}
                <Disclosure
                  key={chosen}
                  show="Show advanced settings"
                  hide="Hide advanced settings"
                  hidden={advanced.length === 0}
                  initiallyOpen={advancedChanged(kind, form.values)}
                  forceOpen={advanced.some((f) => shown(f.key))}
                >
                  {advanced.map(fieldInput)}
                </Disclosure>

                {/* An AI adapter has no default: it handles the functions
                    chosen on it (R-259). A new one is not running until Pando
                    restarts, so its functions are chosen after that. */}
                {kind.category === 'ai' ? (
                  existing ? (
                    functions.isSuccess && (
                      <AdapterFunctions
                        adapterID={existing.id}
                        functions={functions.data.functions}
                        choice={chosenFunctions}
                        onChange={setChoice}
                      />
                    )
                  ) : (
                    <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
                      After Pando restarts, open this adapter again to choose what it handles.
                    </p>
                  )
                ) : isSource ? null : (
                  <Checkbox
                    label={`Use as the default ${kind.category} adapter`}
                    description="Used by anything that needs this kind of adapter and doesn't name one."
                    checked={form.isDefault}
                    onChange={(e) => edit({ isDefault: e.target.checked })}
                  />
                )}

                {isRegistry && existing && (
                  <Checkbox
                    label="Push builds to this registry"
                    description="Turned off, Pando does not use it and keeps its settings."
                    checked={enabled}
                    onChange={(e) => {
                      setEnabled(e.target.checked);
                      if (!draft) edit({});
                    }}
                  />
                )}
              </>
            )}

            {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
          </>
        )}
      </div>
    </Dialog>
  );
}

/** A caption for a credential field. */
const CREDENTIAL = 'Stored encrypted. Pando never shows it again.';

/** One setting, as its kind describes it. */
export function FieldInput({
  field,
  value,
  stored,
  error,
  onChange,
}: {
  field: KindField;
  value: string | boolean | undefined;
  /** Changing an adapter: whether this credential is already stored. */
  stored?: boolean;
  error?: string;
  onChange: (v: string | boolean) => void;
}) {
  const label = field.required ? `${field.label} (required)` : field.label;

  if (field.type === 'bool') {
    return (
      <Checkbox
        label={label}
        description={field.help}
        checked={boolValue(field, value)}
        onChange={(e) => onChange(e.target.checked)}
      />
    );
  }

  if (field.type === 'select') {
    return <ChoiceInput field={field} label={label} value={value} error={error} onChange={onChange} />;
  }

  // A credential's helper says what happens to what is typed. The kind's own
  // help comes first when it has one, since it may say more — such as where
  // the key is read from when none is stored.
  let helper = field.help;
  if (field.credential) {
    if (stored === true) helper = 'One is stored. Leave empty to keep the current one.';
    else if (stored === false) helper = `None is stored. ${field.help ?? CREDENTIAL}`;
    else helper = field.help ?? CREDENTIAL;
  }

  if (field.multiline) {
    // NAME=value lines. Mono, because they are names a program reads; not
    // masked, because a text area cannot be, and the value is never shown
    // again once saved.
    return (
      <Input
        as="textarea"
        rows={4}
        mono
        spellCheck={false}
        autoComplete="off"
        label={label}
        value={typeof value === 'string' ? value : ''}
        placeholder={fieldPlaceholder(field)}
        helper={helper}
        error={error}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  return (
    <Input
      label={label}
      type={field.credential ? 'password' : field.type === 'int' ? 'number' : 'text'}
      autoComplete={field.credential ? 'new-password' : 'off'}
      value={typeof value === 'string' ? value : ''}
      placeholder={fieldPlaceholder(field)}
      helper={helper}
      error={error}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

/** The value a select offers for typing in one it does not list. */
const OTHER = '__other__';

/**
 * A `select` setting. Two or three choices are radio buttons, so the tradeoff
 * each one's description states is visible at once; more is a select. One that
 * takes a value it does not list offers "Other" and a field to type it in —
 * a DNS provider Pando does not name, say.
 */
function ChoiceInput({
  field,
  label,
  value,
  error,
  onChange,
}: {
  field: KindField;
  label: string;
  value: string | boolean | undefined;
  error?: string;
  onChange: (v: string) => void;
}) {
  const options = field.options ?? [];
  const text = typeof value === 'string' ? value : '';
  const current = text || field.default || '';
  const listed = options.some((o) => o.value === current);
  // "Other" is chosen when the value is one the list lacks, or when someone
  // picked it and has not typed anything yet.
  const [otherPicked, setOtherPicked] = useState(false);
  const typing = Boolean(field.other) && (otherPicked || (current !== '' && !listed));

  if (options.length <= 3 && !field.other) {
    return (
      <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>{label}</legend>
        {field.help && (
          <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>{field.help}</p>
        )}
        {options.map((o) => (
          <Radio
            key={o.value}
            name={field.key}
            value={o.value}
            label={o.label}
            description={o.description}
            checked={current === o.value}
            onChange={() => onChange(o.value)}
          />
        ))}
        {error && <p style={{ font: 'var(--type-caption)', color: 'var(--marker-deep)', margin: 0 }}>{error}</p>}
      </fieldset>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <Select
        label={label}
        helper={field.help}
        value={typing ? OTHER : current}
        options={[
          ...(current || typing ? [] : [{ value: '', label: `Choose ${field.label.toLowerCase()}` }]),
          ...options.map((o) => ({ value: o.value, label: o.label })),
          ...(field.other ? [{ value: OTHER, label: 'Other' }] : []),
        ]}
        onChange={(e) => {
          if (e.target.value === OTHER) {
            setOtherPicked(true);
            onChange('');
          } else {
            setOtherPicked(false);
            onChange(e.target.value);
          }
        }}
      />
      {typing && (
        <Input
          label={`${field.label} code`}
          mono
          autoComplete="off"
          spellCheck={false}
          value={listed ? '' : text}
          error={error}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {!typing && error && <p style={{ font: 'var(--type-caption)', color: 'var(--marker-deep)', margin: 0 }}>{error}</p>}
    </div>
  );
}
