// The audit log's actor filter: type to find an actor, rather than scroll a
// list of every account.
//
// Offers accounts by username, name, email or ID, and Pando's own actors: the
// system as a whole, each system process, and anonymous — which has no ID and
// so can only be found by kind. Empty and focused, it offers those last few as
// quick picks, since they are not names anyone would think to type.
//
// Choosing an account filters by its ID. A typed usr_… or tok_… ID is used as
// is on Enter, which is how a token is found and all a reader without
// install.view can do. Clearing the field clears the filter.
//
// The value is a principal ID, or `kind:<kind>` for a whole kind of actor;
// auditQuery turns the second into principal_kind.
//
// The same field, relabeled and without Pando's own actors, is the log's
// "Actor or target" filter: those are accounts, and a kind is not an ID that
// can be on either side.

import { useEffect, useRef, useState } from 'react';
import { Input } from '@design';

import { matches } from '../ui/search';
import { unionPeople, usePeopleSearch, type Person } from '../ui/people';
import { stepIndex, useCloseOnOutside } from '../ui/combobox';

export type { Person };

interface Option {
  value: string;
  label: string;
  note?: string;
}

// Pando's own actors, as the server records them (principal kind "system" with
// these IDs, and "anonymous" with none).
const PANDO: Option[] = [
  { value: 'kind:system', label: 'system', note: 'Any Pando process' },
  { value: 'reconciler', label: 'reconciler', note: 'System' },
  { value: 'detection', label: 'detection', note: 'System' },
  { value: 'kind:anonymous', label: 'anonymous', note: 'Not signed in' },
];

const SHOWN = 8;

export function ActorField({
  people,
  value,
  onChange,
  label = 'Actor',
  pando = true,
}: {
  people: Person[];
  /** A principal ID, `kind:<kind>`, or empty. */
  value: string;
  onChange: (actor: string) => void;
  label?: string;
  /** Whether to offer Pando's own actors — the system, anonymous. */
  pando?: boolean;
}) {
  const own = pando ? PANDO : [];
  const [text, setText] = useState(value);
  // The accounts already named on the screen, and whoever the server finds
  // for what was typed (issue #72): not every account in the install.
  const searched = usePeopleSearch(text.startsWith('usr_') || text.startsWith('tok_') ? '' : text);
  const accounts = unionPeople(people, searched);
  const options: Option[] = [
    ...own,
    ...accounts.map((p) => ({ value: p.id, label: p.external_id || p.id, note: p.display_name || p.email })),
  ];
  const labelOf = (v: string) => options.find((o) => o.value === v)?.label ?? v;

  // A value that arrived with the page — a link from an account — shows as an
  // ID until the accounts load, and as the username once they have.
  const known = value ? labelOf(value) : '';
  useEffect(() => {
    if (value && text === value && known !== value) setText(known);
  }, [value, known, text]);
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState(0);
  const root = useRef<HTMLDivElement>(null);

  // Cleared from outside — "Clear filters" — clears the text too.
  useEffect(() => {
    if (!value) setText('');
  }, [value]);

  useCloseOnOutside(root, open, () => setOpen(false));

  const found = text.trim()
    ? options
        .filter((o) =>
          matches(text, o.label, o.note, o.value, accounts.find((p) => p.id === o.value)?.email),
        )
        .slice(0, SHOWN)
    : own;

  const choose = (o: Option) => {
    setText(o.label);
    setOpen(false);
    onChange(o.value);
  };

  return (
    <div ref={root} style={{ position: 'relative' }}>
      <Input
        label={label}
        mono
        role="combobox"
        aria-expanded={open && found.length > 0}
        aria-autocomplete="list"
        placeholder="Username, email or ID"
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setAt(0);
          setOpen(true);
          if (!e.target.value.trim()) onChange('');
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
            e.preventDefault();
            if (found.length === 0) return;
            setOpen(true);
            setAt((i) => stepIndex(e.key, i, found.length));
          } else if (e.key === 'Enter') {
            e.preventDefault();
            const typed = text.trim();
            const exact = options.find((o) => o.value === typed || o.label === typed);
            if (open && found[at]) choose(found[at]);
            else if (exact) choose(exact);
            else if (/^(usr|tok)_/.test(typed)) {
              setOpen(false);
              onChange(typed);
            }
          } else if (e.key === 'Escape') {
            setOpen(false);
          }
        }}
      />
      {open && found.length > 0 && (
        <div
          role="listbox"
          aria-label={`Matching ${label.toLowerCase()}`}
          style={{
            position: 'absolute',
            top: '100%',
            left: 0,
            right: 0,
            zIndex: 20,
            marginTop: 'var(--space-1)',
            padding: 'var(--space-1)',
            background: 'var(--paper-raised)',
            border: 'var(--border-width) solid var(--rule-strong)',
            borderRadius: 'var(--radius-md)',
            boxShadow: 'var(--shadow-popover)',
          }}
        >
          {found.map((o, i) => (
            <div
              key={o.value}
              role="option"
              aria-selected={i === at}
              onMouseDown={(e) => {
                // Before the input blurs, so the choice lands.
                e.preventDefault();
                choose(o);
              }}
              onMouseEnter={() => setAt(i)}
              style={{
                display: 'flex',
                justifyContent: 'space-between',
                gap: 'var(--space-3)',
                padding: 'var(--space-2) var(--space-3)',
                borderRadius: 'var(--radius-sm)',
                cursor: 'pointer',
                background: i === at ? 'var(--paper-sunken)' : 'transparent',
                font: 'var(--type-body-ui)',
                color: 'var(--ink)',
              }}
            >
              <span style={{ font: 'var(--type-code-sm)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {o.label}
              </span>
              <span style={{ color: 'var(--ink-secondary)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {o.note}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
