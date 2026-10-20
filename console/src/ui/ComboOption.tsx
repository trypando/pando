// One option in a combobox's list, shared by ActorField, RecipientField and
// TagField so that they look and behave alike.
//
// The input keeps focus while the arrow keys move the highlight; the option is
// named to assistive technology by the input's aria-activedescendant, which is
// why it has an id and is focusable by script (tabIndex -1) but not by Tab.

import type { ReactNode } from 'react';

export function ComboOption({
  id,
  active,
  onChoose,
  onHover,
  label,
  detail,
  mono = false,
}: Readonly<{
  /** The id the input's aria-activedescendant names. */
  id: string;
  /** Whether this is the highlighted option. */
  active: boolean;
  onChoose: () => void;
  onHover: () => void;
  label: ReactNode;
  /** Quieter text at the right, if any. */
  detail?: ReactNode;
  /** The label in the code face, for IDs and usernames. */
  mono?: boolean;
}>) {
  const clipped = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;
  return (
    <div
      role="option"
      id={id}
      tabIndex={-1}
      aria-selected={active}
      onMouseDown={(e) => {
        // Before the input blurs, so the choice lands.
        e.preventDefault();
        onChoose();
      }}
      onMouseEnter={onHover}
      style={{
        display: 'flex',
        justifyContent: 'space-between',
        gap: 'var(--space-3)',
        padding: 'var(--space-2) var(--space-3)',
        borderRadius: 'var(--radius-sm)',
        cursor: 'pointer',
        background: active ? 'var(--paper-sunken)' : 'transparent',
        font: 'var(--type-body-ui)',
        color: 'var(--ink)',
      }}
    >
      <span style={mono ? { ...clipped, font: 'var(--type-code-sm)' } : clipped}>{label}</span>
      {detail && <span style={{ ...clipped, color: 'var(--ink-secondary)' }}>{detail}</span>}
    </div>
  );
}

/** The list under a combobox's input, holding its ComboOptions. */
export function ComboList({
  id,
  label,
  none,
  children,
}: Readonly<{
  id: string;
  /** The list's accessible name, such as "Matching accounts". */
  label: string;
  /** Said in place of options when none match. */
  none?: string;
  children?: ReactNode;
}>) {
  return (
    <div
      role="listbox"
      id={id}
      aria-label={label}
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
      {none && (
        <p
          style={{
            font: 'var(--type-body-ui)',
            color: 'var(--ink-secondary)',
            margin: 0,
            padding: 'var(--space-2) var(--space-3)',
          }}
        >
          {none}
        </p>
      )}
      {children}
    </div>
  );
}
