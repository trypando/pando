// Several people, chosen by typing: each chosen person sits in the field as a
// small tag with × to take them out, and the field grows a line at a time as
// people are added.
//
// The same list under the field as ActorField and RecipientField — arrow keys,
// Enter, Escape — so every people picker in the console behaves alike.
// Backspace in the empty field takes out the last person, as it does in the
// address field of most mail clients.

import { useEffect, useRef, useState } from 'react';
import { Icon } from '@design';

import { matches } from './search';
import { unionPeople, usePeopleSearch } from './people';

export interface Person {
  id: string;
  display_name?: string;
  email?: string;
  external_id?: string;
}

const SHOWN = 8;

/** What a person is called: their name, else their email, else their ID. */
export function personName(p: Person | undefined, id: string): string {
  return p?.display_name || p?.email || p?.external_id || id;
}

export function PeopleField({
  label,
  people,
  value,
  onChange,
}: {
  label: string;
  /** The people already named — the chosen ones, at least. Anyone else is
   *  found by asking the server for what was typed (issue #72). */
  people: Person[];
  /** The chosen people's IDs, in the order they were added. */
  value: string[];
  onChange: (ids: string[]) => void;
}) {
  const [text, setText] = useState('');
  const [open, setOpen] = useState(false);
  const [focus, setFocus] = useState(false);
  const [at, setAt] = useState(0);
  const root = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const id = 'f-' + label.replace(/\W+/g, '-').toLowerCase();

  useEffect(() => {
    if (!open) return undefined;
    const outside = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', outside);
    return () => document.removeEventListener('mousedown', outside);
  }, [open]);

  const searched = usePeopleSearch(text);
  const found = unionPeople(people, searched)
    .filter((p) => !value.includes(p.id))
    .filter((p) => matches(text, p.display_name, p.email, p.external_id, p.id))
    .slice(0, SHOWN);

  const add = (p: Person) => {
    onChange([...value, p.id]);
    setText('');
    setAt(0);
    input.current?.focus();
  };
  const remove = (who: string) => onChange(value.filter((v) => v !== who));

  return (
    <div ref={root} style={{ position: 'relative', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <label htmlFor={id} style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>
        {label}
      </label>
      {/* Looks like an Input, and is one field to the person using it: a click
          anywhere in it goes to the text. */}
      <div
        onClick={() => input.current?.focus()}
        style={{
          display: 'flex',
          flexWrap: 'wrap',
          alignItems: 'center',
          gap: 'var(--space-1)',
          minHeight: 'var(--control-input)',
          padding: 'var(--space-1) var(--space-2)',
          background: 'var(--paper-raised)',
          border: 'var(--border-width) solid var(--field-border)',
          borderRadius: 'var(--radius-sm)',
          outline: focus ? 'var(--focus-outline-width) solid var(--ink)' : 'none',
          outlineOffset: 'var(--focus-outline-offset)',
          cursor: 'text',
        }}
      >
        {value.map((who) => {
          const name = personName(
            people.find((p) => p.id === who),
            who,
          );
          return (
            <span
              key={who}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 'var(--space-1)',
                padding: '0 var(--space-1) 0 var(--space-2)',
                background: 'var(--paper-sunken)',
                border: 'var(--border-width) solid var(--rule)',
                borderRadius: 'var(--radius-xs)',
                font: 'var(--type-body-ui)',
                color: 'var(--ink)',
                maxWidth: '100%',
              }}
            >
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{name}</span>
              <button
                type="button"
                aria-label={`Remove ${name}`}
                onClick={(e) => {
                  e.stopPropagation();
                  remove(who);
                }}
                style={{
                  display: 'inline-flex',
                  padding: 'var(--space-1)',
                  background: 'none',
                  border: 'none',
                  borderRadius: 'var(--radius-xs)',
                  cursor: 'pointer',
                }}
              >
                <Icon name="x" size={12} />
              </button>
            </span>
          );
        })}
        <input
          ref={input}
          id={id}
          role="combobox"
          aria-expanded={open && found.length > 0}
          aria-autocomplete="list"
          value={text}
          placeholder={value.length === 0 ? 'Type a name or email' : ''}
          onChange={(e) => {
            setText(e.target.value);
            setAt(0);
            setOpen(true);
          }}
          onFocus={() => {
            setFocus(true);
            setOpen(true);
          }}
          onBlur={() => setFocus(false)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
              e.preventDefault();
              if (found.length === 0) return;
              setOpen(true);
              setAt((i) => (e.key === 'ArrowDown' ? (i + 1) % found.length : (i - 1 + found.length) % found.length));
            } else if (e.key === 'Enter') {
              e.preventDefault();
              const chosen = found[at];
              if (open && chosen) add(chosen);
            } else if (e.key === 'Backspace' && text === '' && value.length > 0) {
              remove(value[value.length - 1]!);
            } else if (e.key === 'Escape') {
              setOpen(false);
            }
          }}
          style={{
            flex: '1 1 8rem',
            minWidth: '8rem',
            height: 'calc(var(--control-input) - 2 * var(--space-1) - 2 * var(--border-width))',
            padding: '0 var(--space-1)',
            background: 'transparent',
            border: 'none',
            outline: 'none',
            font: 'var(--type-body-ui)',
            color: 'var(--ink)',
          }}
        />
      </div>
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
          {found.map((p, i) => (
            <div
              key={p.id}
              role="option"
              aria-selected={i === at}
              onMouseDown={(e) => {
                // Before the input blurs, so the choice lands.
                e.preventDefault();
                add(p);
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
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {p.display_name || p.external_id || p.id}
              </span>
              <span style={{ color: 'var(--ink-secondary)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {p.email}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
