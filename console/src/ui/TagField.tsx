// Several things, chosen by typing: each chosen one sits in the field as a
// small tag with × to take it out, and the field grows a line at a time as
// more are added. Only what is chosen is drawn, so the field stays the same
// size whether there are ten things to choose from or twenty thousand.
//
// The caller owns the typed text and supplies the options for it, so the
// options can come from the page (PeopleField) or from a server search (the
// apps that always need deploy approval, issue #72).
//
// The same list under the field as ActorField and RecipientField — arrow keys,
// Enter, Escape — so every picker in the console behaves alike. Backspace in
// the empty field takes out the last tag, as it does in the address field of
// most mail clients.

import { useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Icon } from '@design';
import { stepIndex, useCloseOnOutside } from './combobox';

export interface TagOption {
  id: string;
  name: string;
  /** A second, quieter line of identification, such as an email. */
  detail?: string;
}

const SHOWN = 8;

export function TagField({
  label,
  value,
  onChange,
  nameOf,
  text,
  onText,
  options,
  placeholder,
  helper,
  disabled = false,
}: {
  label: string;
  /** The chosen IDs, in the order they were added. */
  value: string[];
  onChange: (ids: string[]) => void;
  /** What a chosen ID is called on its tag. */
  nameOf: (id: string) => string;
  text: string;
  onText: (text: string) => void;
  /** What could be chosen for the text typed so far. Chosen ones are left out here. */
  options: TagOption[];
  placeholder?: string;
  helper?: ReactNode;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [focus, setFocus] = useState(false);
  const [at, setAt] = useState(0);
  const root = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const id = 'f-' + label.replace(/\W+/g, '-').toLowerCase();

  useCloseOnOutside(root, open, () => setOpen(false));

  const found = options.filter((o) => !value.includes(o.id)).slice(0, SHOWN);

  const add = (o: TagOption) => {
    onChange([...value, o.id]);
    onText('');
    setAt(0);
    input.current?.focus();
  };
  const remove = (which: string) => onChange(value.filter((v) => v !== which));

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
          background: disabled ? 'var(--paper-sunken)' : 'var(--paper-raised)',
          border: 'var(--border-width) solid var(--field-border)',
          borderRadius: 'var(--radius-sm)',
          outline: focus ? 'var(--focus-outline-width) solid var(--ink)' : 'none',
          outlineOffset: 'var(--focus-outline-offset)',
          cursor: disabled ? 'not-allowed' : 'text',
        }}
      >
        {value.map((which) => {
          const name = nameOf(which);
          return (
            <span
              key={which}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 'var(--space-1)',
                padding: disabled ? '0 var(--space-2)' : '0 var(--space-1) 0 var(--space-2)',
                background: 'var(--paper-sunken)',
                border: 'var(--border-width) solid var(--rule)',
                borderRadius: 'var(--radius-xs)',
                font: 'var(--type-body-ui)',
                color: 'var(--ink)',
                maxWidth: '100%',
              }}
            >
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{name}</span>
              {!disabled && (
                <button
                  type="button"
                  aria-label={`Remove ${name}`}
                  onClick={(e) => {
                    e.stopPropagation();
                    remove(which);
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
              )}
            </span>
          );
        })}
        <input
          ref={input}
          id={id}
          role="combobox"
          aria-expanded={open && found.length > 0}
          aria-autocomplete="list"
          disabled={disabled}
          value={text}
          placeholder={value.length === 0 ? placeholder : ''}
          onChange={(e) => {
            onText(e.target.value);
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
              setAt((i) => stepIndex(e.key, i, found.length));
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
      {helper && (
        <div style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{helper}</div>
      )}
      {open && !disabled && found.length > 0 && (
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
              key={o.id}
              role="option"
              aria-selected={i === at}
              onMouseDown={(e) => {
                // Before the input blurs, so the choice lands.
                e.preventDefault();
                add(o);
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
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{o.name}</span>
              {o.detail && (
                <span style={{ color: 'var(--ink-secondary)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {o.detail}
                </span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
