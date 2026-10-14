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
