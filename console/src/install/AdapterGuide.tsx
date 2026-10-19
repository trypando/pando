// Adding an adapter one step at a time.
//
// Every kind is set up in the same order: choose it, answer its own steps,
// name it, review. The first and the last two are this file's and read the
// same for every kind; the steps between are the kind's, from GET
// /adapters/kinds (KindInfo.Steps), so a kind added to Pando brings its own
// walk-through without this file learning about it (R-261).

import { useState } from 'react';
import { Banner, Button, Checkbox, Dialog, Icon, Input, Select } from '@design';

import { refusal } from './Accounts';
import { Disclosure } from '../ui/Disclosure';
import { useNarrow } from '../ui/narrow';
import {
  advancedCount,
  categoryLabel,
  fillRefs,
  fillText,
  formProblems,
  isShown,
  kindKey,
  kindSteps,
  PRESET_OTHER,
  withPreset,
  reviewText,
  runs,
  stepFields,
  unasked,
} from './adapters';
import type { AdapterForm, AdapterKind, FieldValue, KindField, KindPreset, KindStep } from './adapters';

/** One screen of the walk-through. */
type Screen =
  | { kind: 'choose' }
  | { kind: 'provider' }
  | { kind: 'step'; step: KindStep }
  | { kind: 'name' }
  | { kind: 'review' };

const screenID = (s: Screen) => (s.kind === 'step' ? `step:${s.step.id}` : s.kind);

const caption = { font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 } as const;

export function AdapterGuide({
  categories,
  category,
  askCategory,
  onCategory,
  inCategory,
  chosen,
  onPick,
  skipChoose,
  kind,
  form,
  edit,
  stored,
  renderField,
  showDefault,
  isAI,
  saving,
  saveError,
  onSave,
  onShowAll,
  onClose,
  pickLabel = 'Adapter',
  pickTitle,
  noun = '',
  addLabel = 'Add adapter',
  supplied = {},
  checkFields,
  nameStep,
  onProvider,
  reviewNote,
}: Readonly<{
  categories: string[];
  category: string;
  /** Whether the category is asked, or was given by where the dialog opened. */
  askCategory: boolean;
  onCategory: (c: string) => void;
  inCategory: AdapterKind[];
  chosen: string;
  onPick: (key: string) => void;
  /** A category with one kind, opened from that category, has nothing to choose. */
  skipChoose: boolean;
  kind?: AdapterKind;
  form: AdapterForm | null;
  edit: (patch: Partial<AdapterForm>) => void;
  stored: string[];
  renderField: (f: KindField, error?: string, instructed?: boolean) => React.ReactNode;
  showDefault: boolean;
  isAI: boolean;
  saving: boolean;
  saveError: unknown;
  onSave: () => void;
  onShowAll: () => void;
  onClose: () => void;
  /** What the first screen picks, for its tiles and the step list. */
  pickLabel?: string;
  /** The first screen's title, when not "Add <category> adapter". */
  pickTitle?: string;
  /** Said after the kind's name in the title: "Add OpenID Connect provider". */
  noun?: string;
  /** The last screen's button. */
  addLabel?: string;
  /** Values the form supplies for step text and links, such as an identity
   *  provider's {callback_url}. */
  supplied?: Record<string, string>;
  /** What is wrong with the settings, by key, when not the adapter rules. */
  checkFields?: (values: Record<string, FieldValue>) => Record<string, string>;
  /** Pando's Name screen, when not an adapter's name, ID and default. */
  nameStep?: {
    content: React.ReactNode;
    problems: () => Record<string, string>;
    rows: { label: string; value: string }[];
  };
  /** Told the provider chosen, for a kind with presets. */
  onProvider?: (preset: KindPreset | undefined) => void;
  /** One line on the review, above the button that saves. */
  reviewNote?: string;
}>) {
  const narrow = useNarrow();
  const [at, setAt] = useState(0);
  const [errors, setErrors] = useState<Record<string, string>>({});
  // The provider chosen, for the kind it was chosen for: choosing another
  // kind starts the choice again.
  const [provider, setProvider] = useState<{ kind: string; id: string } | null>(null);

  // A kind with presets asks which provider first, and its steps are the
  // chosen provider's; until one is chosen there are none to show.
  const presets = kind?.presets ?? [];
  const preset = provider?.kind === chosen ? provider.id : undefined;
  const ready = Boolean(kind) && (presets.length === 0 || preset !== undefined);
  const steps = kind && form && ready ? kindSteps(kind, form.values, preset) : [];
  const screens = buildScreens(skipChoose, Boolean(kind) && presets.length > 0, steps, ready);
  const index = Math.min(at, screens.length - 1);
  // Never empty: there is a choice to make, or a kind and its screens.
  const screen: Screen = screens[index] ?? { kind: 'choose' };
  const last = index === screens.length - 1 && screen.kind === 'review';

  // The settings' problems, whole or for one step.
  const fieldProblems = (): Record<string, string> => {
    if (!kind || !form) return {};
    const all = checkFields ? checkFields(form.values) : formProblems(kind, form, stored);
    const { id: _id, ...rest } = all;
    return rest;
  };
  const problemsOn = (step: KindStep): Record<string, string> => {
    if (!kind || !form) return {};
    const all = fieldProblems();
    const out: Record<string, string> = {};
    for (const f of stepFields(kind, step, form.values)) if (all[f.key]) out[f.key] = all[f.key]!;
    return out;
  };

  const go = (i: number) => {
    setErrors({});
    setAt(i);
  };
  const goTo = (id: string) => go(Math.max(0, screens.findIndex((s) => screenID(s) === id)));

  const nameProblems = (): Record<string, string> => {
    if (nameStep) return nameStep.problems();
    const id = kind && form ? formProblems(kind, form, stored).id : undefined;
    return id ? { id } : {};
  };
  // What keeps Continue from moving on, if anything.
  const blocking = (): Record<string, string> | null => {
    switch (screen.kind) {
      case 'choose':
        return kind ? null : { choose: chooseProblem(category) };
      case 'provider':
        return preset === undefined ? { choose: 'Choose a provider to continue.' } : null;
      case 'step':
        return nonEmpty(problemsOn(screen.step));
      case 'name':
        return nonEmpty(nameProblems());
      default:
        return null;
    }
  };
  // Each step was checked on the way; one changed since is sent back to.
  const save = () => {
    if (!kind || !form) return;
    const problems = fieldProblems();
    const back = screens.findIndex(
      (s) => s.kind === 'step' && stepFields(kind, s.step, form.values).some((f) => problems[f.key]),
    );
    const target = screens[back];
    if (target?.kind === 'step') {
      setAt(back);
      setErrors(problemsOn(target.step));
      return;
    }
    // A setting no step asks is on Review itself, and shown there.
    const left = nonEmpty(problems);
    if (left) {
      setErrors(left);
      return;
    }
    onSave();
  };
  const next = () => {
    if (screen.kind === 'review') {
      save();
      return;
    }
    const stop = blocking();
    if (stop) setErrors(stop);
    else go(index + 1);
  };

  const choosing = screen.kind === 'choose';
  // Picking the adapter or its provider has the dialog to itself: until then
  // there are no steps of its own to list.
  const picking = choosing || screen.kind === 'provider';
  const pickProvider = (id: string) => {
    if (!kind || !form) return;
    const was = presets.find((p) => p.id === preset);
    edit({ values: withPreset(form.values, was, presets.find((p) => p.id === id)) });
    setProvider({ kind: chosen, id });
    onProvider?.(presets.find((p) => p.id === id));
    setErrors({});
  };
  const categoryWord = wordFor(category);
  const title = guideTitle({ pickTitle, kindName: choosing ? undefined : kind?.name, noun, categoryWord: category ? categoryWord : '' });

  return (
    <Dialog
      open
      width={760}
      title={title}
      onClose={onClose}
      footer={
        <>
          {index > 0 && (
            <span style={{ marginRight: 'auto' }}>
              <Button variant="ghost" onClick={() => go(index - 1)}>
                Back
              </Button>
            </span>
          )}
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={saving} onClick={next}>
            {primaryLabel(last, saving, addLabel)}
          </Button>
        </>
      }
    >
      {/* Choosing the adapter has the dialog to itself: until one is chosen
          there are no steps to list. From its first step on, every step is
          named beside the one being answered — or, at phone width, counted
          above it, since a rail beside it leaves the step half the screen. */}
      <div
        style={{
          display: 'grid',
          gridTemplateColumns:
            picking || narrow ? 'minmax(0, 1fr)' : 'calc(var(--space-9) + var(--space-7)) minmax(0, 1fr)',
          gap: narrow ? 'var(--space-4)' : 'var(--space-6)',
          alignItems: 'start',
        }}
      >
        {!picking && narrow && <StepCount index={index} total={screens.length} onShowAll={onShowAll} />}
        {!picking && !narrow && <StepRail screens={screens} index={index} pickLabel={pickLabel} onGo={go} onShowAll={onShowAll} />}

        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', minWidth: 0 }}>
          {screen.kind === 'choose' && (
            <ChooseScreen
              askCategory={askCategory}
              categories={categories}
              category={category}
              inCategory={inCategory}
              chosen={chosen}
              pickLabel={pickLabel}
              error={errors.choose}
              onCategory={(c) => {
                setErrors({});
                onCategory(c);
              }}
              onPick={(key) => {
                setErrors({});
                onPick(key);
              }}
            />
          )}

          {screen.kind === 'provider' && kind && (
            <ProviderScreen kind={kind} preset={preset} error={errors.choose} onPick={pickProvider} />
          )}

          {screen.kind === 'step' && kind && form && (
            <StepBody
              step={screen.step}
              fill={(text) => fillText(kind, form.values, text, supplied)}
              links={(screen.step.links ?? []).flatMap((l) => {
                const url = fillRefs(kind, form.values, l.url, supplied);
                return url ? [{ label: l.label, url }] : [];
              })}
            >
              {stepFields(kind, screen.step, form.values).map((f) => (
                <div key={f.key}>{renderField(f, errors[f.key], (screen.step.do?.length ?? 0) > 0)}</div>
              ))}
            </StepBody>
          )}

          {screen.kind === 'name' && kind && form && (
            <>
              <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Name</h4>
              {nameStep ? (
                nameStep.content
              ) : (
                <AdapterName form={form} edit={edit} error={errors.id} defaultLabel={showDefault ? `Default ${categoryWord} adapter` : undefined} isAI={isAI} />
              )}
            </>
          )}

          {screen.kind === 'review' && kind && form && (
            <Review
              kind={kind}
              form={form}
              steps={steps}
              preset={preset}
              provider={providerName(kind, preset)}
              showDefault={showDefault}
              nameRows={nameStep?.rows}
              renderField={renderField}
              errors={errors}
              onChange={goTo}
            >
              {reviewNote && <p style={caption}>{reviewNote}</p>}
              {saveError ? <Banner tone="failed">{refusal(saveError)}</Banner> : null}
            </Review>
          )}
        </div>
      </div>
    </Dialog>
  );
}

/** The screens of a walk-through, in order. */
function buildScreens(skipChoose: boolean, hasPresets: boolean, steps: KindStep[], ready: boolean): Screen[] {
  const screens: Screen[] = [];
  if (!skipChoose) screens.push({ kind: 'choose' });
  if (hasPresets) screens.push({ kind: 'provider' });
  for (const step of steps) screens.push({ kind: 'step', step });
  if (ready) screens.push({ kind: 'name' }, { kind: 'review' });
  return screens;
}

/** Problems, or none at all. */
function nonEmpty(problems: Record<string, string>): Record<string, string> | null {
  return Object.keys(problems).length > 0 ? problems : null;
}

function chooseProblem(category: string): string {
  return category ? 'Choose an adapter to continue.' : 'Choose a category to continue.';
}

/** A category as it reads mid-sentence: "routing", "AI". */
function wordFor(category: string): string {
  return category === 'ai' ? 'AI' : categoryLabel(category).toLowerCase();
}

/** The dialog's title. The kind is named once it is past choosing, since on
 *  the first screen a pick can still change. */
function guideTitle(o: { pickTitle?: string; kindName?: string; noun: string; categoryWord: string }): string {
  if (o.kindName) return o.noun ? `Add ${o.kindName} ${o.noun}` : `Add ${o.kindName}`;
  if (o.pickTitle) return o.pickTitle;
  return o.categoryWord ? `Add ${o.categoryWord} adapter` : 'Add adapter';
}

function primaryLabel(last: boolean, saving: boolean, addLabel: string): string {
  if (!last) return 'Continue';
  return saving ? 'Adding' : addLabel;
}

/** The provider chosen, by name. */
function providerName(kind: AdapterKind, preset: string | undefined): string | undefined {
  if (preset === PRESET_OTHER) return kind.other_preset;
  return kind.presets?.find((p) => p.id === preset)?.label;
}

/** Choosing the kind: the category, when it was not given, then its kinds. */
function ChooseScreen({
  askCategory,
  categories,
  category,
  inCategory,
  chosen,
  pickLabel,
  error,
  onCategory,
  onPick,
}: Readonly<{
  askCategory: boolean;
  categories: string[];
  category: string;
  inCategory: AdapterKind[];
  chosen: string;
  pickLabel: string;
  error?: string;
  onCategory: (c: string) => void;
  onPick: (key: string) => void;
}>) {
  return (
    <>
      {askCategory && (
        <div>
          <Select
            label="Category"
            value={category}
            options={[
              ...(category ? [] : [{ value: '', label: 'Choose a category' }]),
              ...categories.map((c) => ({ value: c, label: categoryLabel(c) })),
            ]}
            onChange={(e) => onCategory(e.target.value)}
          />
        </div>
      )}
      {category && (
        <TilePicker
          label={pickLabel}
          items={inCategory.map((k) => ({ key: kindKey(k), name: k.name, description: k.description }))}
          chosen={chosen}
          onPick={onPick}
        />
      )}
      {error && <p style={{ ...caption, color: 'var(--marker-deep)' }}>{error}</p>}
    </>
  );
}

/** Choosing the provider, for a kind with presets. */
function ProviderScreen({
  kind,
  preset,
  error,
  onPick,
}: Readonly<{ kind: AdapterKind; preset?: string; error?: string; onPick: (id: string) => void }>) {
  const items = (kind.presets ?? []).map((p) => ({ key: p.id, name: p.label }));
  if (kind.other_preset) items.push({ key: PRESET_OTHER, name: kind.other_preset });
  return (
    <>
      <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Choose a provider</h4>
      <TilePicker label="Provider" items={items} chosen={preset ?? ''} onPick={onPick} />
      {error && <p style={{ ...caption, color: 'var(--marker-deep)' }}>{error}</p>}
    </>
  );
}

/** An adapter's name, ID and whether it is its category's default. */
function AdapterName({
  form,
  edit,
  error,
  defaultLabel,
  isAI,
}: Readonly<{
  form: AdapterForm;
  edit: (p: Partial<AdapterForm>) => void;
  error?: string;
  /** Offered as the default of its category, under this label; absent is not offered. */
  defaultLabel?: string;
  isAI: boolean;
}>) {
  return (
    <>
      <NameFields form={form} edit={edit} error={error} />
      {defaultLabel && (
        <Checkbox label={defaultLabel} checked={form.isDefault} onChange={(e) => edit({ isDefault: e.target.checked })} />
      )}
      {isAI && <p style={caption}>Choose its functions after Pando restarts.</p>}
    </>
  );
}

/** A screen's name in the list of steps. */
function railState(i: number, index: number): 'done' | 'current' | 'next' {
  if (i < index) return 'done';
  return i === index ? 'current' : 'next';
}

function screenLabel(s: Screen, pickLabel: string): string {
  if (s.kind === 'choose') return pickLabel;
  if (s.kind === 'provider') return 'Provider';
  if (s.kind === 'step') return s.step.label;
  if (s.kind === 'name') return 'Name';
  return 'Review';
}

const quietLink = {
  ...caption,
  border: 0,
  background: 'transparent',
  padding: 0,
  cursor: 'pointer',
  textAlign: 'left',
  textDecoration: 'underline',
  textDecorationColor: 'var(--rule-strong)',
  textUnderlineOffset: 'var(--space-1)',
} as const;

/** The step rail at phone width: where the walk-through is, in one line. */
function StepCount({ index, total, onShowAll }: Readonly<{ index: number; total: number; onShowAll: () => void }>) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 'var(--space-3)' }}>
      <span style={caption}>
        Step {index + 1} of {total}
      </span>
      <button type="button" onClick={onShowAll} style={quietLink}>
        Show all settings
      </button>
    </div>
  );
}

/**
 * Every step by name, so what is still to come is in view: finished ones
 * marked and a click from going back to, the current one set off.
 */
function StepRail({
  screens,
  index,
  pickLabel,
  onGo,
  onShowAll,
}: Readonly<{
  screens: Screen[];
  index: number;
  pickLabel: string;
  onGo: (i: number) => void;
  onShowAll: () => void;
}>) {
  const items: { label: string; state: 'done' | 'current' | 'next'; at: number }[] = screens.map((s, i) => ({
    label: screenLabel(s, pickLabel),
    state: railState(i, index),
    at: i,
  }));

  return (
    <nav aria-label="Steps" style={{ position: 'sticky', top: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <ol style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        {items.map((it, i) => {
          const row = {
            display: 'flex',
            alignItems: 'center',
            gap: 'var(--space-2)',
            width: '100%',
            padding: 'var(--space-1) var(--space-2)',
            borderRadius: 'var(--radius-sm)',
            font: it.state === 'current' ? 'var(--type-label)' : 'var(--type-body-ui)',
            color: it.state === 'next' ? 'var(--ink-muted)' : 'var(--ink)',
            background: it.state === 'current' ? 'var(--paper-sunken)' : 'transparent',
            border: 0,
            textAlign: 'left',
          } as const;
          const marker = (
            <span
              aria-hidden="true"
              style={{
                flex: 'none',
                width: 'var(--space-4)',
                display: 'inline-flex',
                justifyContent: 'center',
                font: 'var(--type-code-sm)',
                color: 'var(--ink-secondary)',
              }}
            >
              {it.state === 'done' ? <Icon name="check" size={14} /> : it.at + 1}
            </span>
          );
          return (
            <li key={`${it.label}-${i}`} aria-current={it.state === 'current' ? 'step' : undefined}>
              {it.state === 'done' ? (
                <button type="button" onClick={() => onGo(it.at)} style={{ ...row, cursor: 'pointer' }}>
                  {marker}
                  {it.label}
                </button>
              ) : (
                <span style={row}>
                  {marker}
                  {it.label}
                </span>
              )}
            </li>
          );
        })}
      </ol>
      <button type="button" onClick={onShowAll} style={{ ...quietLink, paddingLeft: 'var(--space-2)' }}>
        Show all settings
      </button>
    </nav>
  );
}

/** More than this many kinds in a category, and the picker can be searched. */
const SEARCH_FROM = 6;

/**
 * Choices as tiles, two to a row, so a long list stays a short page; past a
 * handful, a search narrows them. A radio group: one is chosen, marked by a
 * check as well as its border. Adapters of a category, and a kind's providers.
 */
function TilePicker({
  label,
  items,
  chosen,
  onPick,
}: Readonly<{
  label: string;
  items: { key: string; name: string; description?: string }[];
  chosen: string;
  onPick: (key: string) => void;
}>) {
  const narrow = useNarrow();
  const [query, setQuery] = useState('');
  const q = query.trim().toLowerCase();
  const shown = q
    ? items.filter((k) => k.name.toLowerCase().includes(q) || (k.description ?? '').toLowerCase().includes(q))
    : items;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      {items.length > SEARCH_FROM && (
        <Input label={`Search ${label.toLowerCase()}s`} value={query} onChange={(e) => setQuery(e.target.value)} />
      )}
      <div
        role="radiogroup"
        aria-label={label}
        style={{ display: 'grid', gridTemplateColumns: narrow ? 'minmax(0, 1fr)' : 'repeat(2, minmax(0, 1fr))', gap: 'var(--space-3)' }}
      >
        {shown.map((k) => {
          const key = k.key;
          const on = chosen === key;
          return (
            <button
              key={key}
              type="button"
              role="radio"
              aria-checked={on}
              title={k.description}
              onClick={() => onPick(key)}
              style={{
                display: 'flex',
                flexDirection: 'column',
                gap: 'var(--space-1)',
                padding: 'var(--space-3) var(--space-4)',
                textAlign: 'left',
                cursor: 'pointer',
                background: 'var(--paper-raised)',
                border: `var(--border-width) solid ${on ? 'var(--ink)' : 'var(--rule)'}`,
                borderRadius: 'var(--radius-md)',
                color: 'var(--ink)',
              }}
            >
              <span style={{ display: 'flex', justifyContent: 'space-between', gap: 'var(--space-2)', font: 'var(--type-label)' }}>
                {k.name}
                {on && <Icon name="check" size={16} />}
              </span>
              {k.description && (
                <span
                  style={{
                    ...caption,
                    display: '-webkit-box',
                    WebkitLineClamp: 2,
                    WebkitBoxOrient: 'vertical',
                    overflow: 'hidden',
                  }}
                >
                  {k.description}
                </span>
              )}
            </button>
          );
        })}
      </div>
      {shown.length === 0 && <p style={caption}>Nothing matches “{query.trim()}”.</p>}
    </div>
  );
}

/** Step text, with its two marks: bold for a name on the provider's screen,
 *  code for something typed exactly. */
function Marked({ text }: Readonly<{ text: string }>) {
  return (
    <>
      {keyed(runs(text), (r) => `${r.kind}:${r.text}`).map(([key, r]) => {
        if (r.kind === 'bold') return <strong key={key} style={{ fontWeight: 600 }}>{r.text}</strong>;
        if (r.kind === 'code')
          return (
            <code
              key={key}
              style={{
                font: 'var(--type-code-sm)',
                background: 'var(--paper-sunken)',
                borderRadius: 'var(--radius-xs)',
                padding: '0 var(--space-1)',
                overflowWrap: 'anywhere',
              }}
            >
              {r.text}
            </code>
          );
        return <span key={key}>{r.text}</span>;
      })}
    </>
  );
}

/** Items with a key each made from what they say, numbered only where two
 *  say the same, so a key follows its item rather than its place. */
function keyed<T>(items: readonly T[], name: (item: T) => string): [string, T][] {
  const seen = new Map<string, number>();
  return items.map((item) => {
    const base = name(item);
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    return [n === 0 ? base : `${base}#${n}`, item];
  });
}

/** A kind's step: its title, what to do, its links with any {key} filled in, and its settings. */
function StepBody({
  step,
  fill,
  links,
  children,
}: Readonly<{
  step: KindStep;
  /** Step text with its {key}s filled in. */
  fill: (text: string) => string;
  links: { label: string; url: string }[];
  children: React.ReactNode;
}>) {
  return (
    <>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>{step.title}</h4>
        {step.lede && (
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            <Marked text={fill(step.lede)} />
          </p>
        )}
      </div>
      {(step.do?.length ?? 0) > 0 && (
        <ol
          style={{
            margin: 0,
            paddingLeft: 'var(--space-5)',
            font: 'var(--type-body-ui)',
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-2)',
          }}
        >
          {keyed(step.do ?? [], (a) => a.text).map(([key, a]) => (
            <li key={key}>
              <Marked text={fill(a.text)} />
              {a.table && <StepTable table={a.table} />}
            </li>
          ))}
        </ol>
      )}
      {links.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
          {links.map((l) => (
            <a
              key={l.url}
              href={l.url}
              target="_blank"
              rel="noreferrer"
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 'var(--space-2)',
                minHeight: 'var(--control-console)',
                padding: 'var(--space-1) var(--button-padding-console)',
                font: 'var(--type-label)',
                color: 'var(--ink)',
                textDecoration: 'none',
                background: 'var(--paper-raised)',
                border: 'var(--border-width) solid var(--rule-strong)',
                borderRadius: 'var(--radius-sm)',
              }}
            >
              <Icon name="external-link" size={16} />
              {l.label}
            </a>
          ))}
        </div>
      )}
      {children}
    </>
  );
}

function StepTable({ table }: Readonly<{ table: NonNullable<NonNullable<KindStep['do']>[number]['table']> }>) {
  // Scrolls sideways inside itself at phone width rather than widening the
  // dialog, which pushed every step's text off the screen with it.
  return (
    <div
      style={{
        marginTop: 'var(--space-2)',
        overflowX: 'auto',
        background: 'var(--paper-sunken)',
        borderRadius: 'var(--radius-md)',
      }}
    >
      <table
        style={{
          width: '100%',
          font: 'var(--type-code-sm)',
          textAlign: 'left',
        }}
      >
        <thead>
          <tr>
            {table.columns.map((c) => (
              <th key={c} style={{ ...caption, fontWeight: 400, padding: 'var(--space-2) var(--space-3) var(--space-1)' }}>
                {c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {keyed(table.rows, (row) => row.join('|')).map(([key, row]) => (
            <tr key={key}>
              {table.columns.map((column, j) => (
                <td key={column} style={{ padding: 'var(--space-1) var(--space-3)' }}>
                  {row[j]}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function NameFields({ form, edit, error }: Readonly<{ form: AdapterForm; edit: (p: Partial<AdapterForm>) => void; error?: string }>) {
  return (
    <>
      <Input label="Name" value={form.name} onChange={(e) => edit({ name: e.target.value })} />
      <Input
        label="ID"
        mono
        value={form.id}
        helper="Can’t be changed later."
        error={error}
        onChange={(e) => edit({ id: e.target.value })}
      />
    </>
  );
}

/** Every answer, by the step that asked it, each a click from changing. */
function Review({
  kind,
  form,
  steps,
  preset,
  provider,
  showDefault,
  nameRows,
  renderField,
  errors,
  onChange,
  children,
}: Readonly<{
  kind: AdapterKind;
  form: AdapterForm;
  steps: KindStep[];
  /** The provider chosen, by preset ID and by name, for a kind with presets. */
  preset?: string;
  provider?: string;
  /** The Name screen's answers, when it is not an adapter's. */
  nameRows?: { label: string; value: string }[];
  showDefault: boolean;
  renderField: (f: KindField, error?: string, instructed?: boolean) => React.ReactNode;
  /** Problems with the settings shown here, found as it was saved. */
  errors: Record<string, string>;
  onChange: (screenID: string) => void;
  children: React.ReactNode;
}>) {
  const rows: { label: string; value: string; mono: boolean; to: string }[] = [];
  if (provider) rows.push({ label: 'Provider', value: provider, mono: false, to: 'provider' });
  for (const st of steps) {
    for (const f of stepFields(kind, st, form.values)) {
      const value = reviewText(f, form.values[f.key]);
      const typed = typeof form.values[f.key] === 'string' && String(form.values[f.key]).trim() !== '';
      rows.push({ label: f.label, value, mono: typed && !f.credential && f.type === 'string', to: `step:${st.id}` });
    }
  }
  if (nameRows) {
    for (const r of nameRows) rows.push({ ...r, mono: false, to: 'name' });
  } else {
    rows.push(
      { label: 'Name', value: form.name.trim() || kind.name, mono: false, to: 'name' },
      { label: 'ID', value: form.id.trim(), mono: true, to: 'name' },
    );
    if (showDefault) rows.push({ label: 'Default', value: form.isDefault ? 'Yes' : 'No', mono: false, to: 'name' });
  }

  // The advanced settings, and those this way through asks on no step.
  const left = unasked(kind, steps, form.values, preset);
  const advanced = [...left, ...(kind.fields ?? []).filter((f) => f.advanced)];
  const total = advancedCount(kind, form.values).total + left.length;
  const shownAdvanced = advanced.filter((f) => isShown(kind, f, form.values));

  return (
    <>
      <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Review</h4>
      <dl
        style={{
          margin: 0,
          display: 'grid',
          gridTemplateColumns: 'fit-content(40%) minmax(0, 1fr) max-content',
          gap: 'var(--space-2) var(--space-4)',
          font: 'var(--type-body-ui)',
        }}
      >
        {rows.map((r) => (
          <div key={r.label} style={{ display: 'contents' }}>
            <dt style={{ color: 'var(--ink-secondary)' }}>{r.label}</dt>
            <dd style={{ margin: 0, overflowWrap: 'anywhere', font: r.mono ? 'var(--type-code)' : undefined }}>{r.value}</dd>
            <button
              type="button"
              onClick={() => onChange(r.to)}
              aria-label={`Change ${r.label}`}
              style={{
                ...caption,
                border: 0,
                background: 'transparent',
                padding: 0,
                cursor: 'pointer',
                textDecoration: 'underline',
                textDecorationColor: 'var(--rule-strong)',
                textUnderlineOffset: 'var(--space-1)',
              }}
            >
              Change
            </button>
          </div>
        ))}
      </dl>
      <Disclosure
        show="Advanced settings"
        hide="Advanced settings"
        hidden={total === 0}
        initiallyOpen={false}
        forceOpen={shownAdvanced.some((f) => errors[f.key])}
      >
        {shownAdvanced.map((f) => (
          <div key={f.key}>{renderField(f, errors[f.key])}</div>
        ))}
      </Disclosure>
      {children}
    </>
  );
}
