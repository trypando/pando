// A line of quiet text that opens what is under it: for something most people
// never need to read, kept a click away rather than taking the page.

import { useState } from 'react';

export function Disclosure({
  show,
  hide,
  hidden = false,
  initiallyOpen = false,
  forceOpen = false,
  children,
}: {
  show: string;
  hide: string;
  hidden?: boolean;
  /** Starts open, when what is under it has something worth seeing. */
  initiallyOpen?: boolean;
  /** Held open, such as while what is under it has a problem to fix. */
  forceOpen?: boolean;
  children: React.ReactNode;
}) {
  const [opened, setOpen] = useState(initiallyOpen);
  const open = opened || forceOpen;
  if (hidden) return null;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <button
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        style={{
          alignSelf: 'flex-start',
          border: 'none',
          background: 'transparent',
          padding: 0,
          cursor: 'pointer',
          font: 'var(--type-body-ui)',
          color: 'var(--ink-secondary)',
          textAlign: 'left',
        }}
      >
        {open ? hide : show}
      </button>
      {open && children}
    </div>
  );
}
