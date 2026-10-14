// A tooltip on something that is not otherwise a control: a word, a tag, an
// info icon. The trigger is a plain button, so the keyboard reaches it and the
// tooltip opens on focus as it does on hover; label says the whole of it to a
// screen reader, which does not read the tooltip.

import type { CSSProperties, ReactNode } from 'react';
import { Tooltip } from '@design';

export function TipButton({
  tip,
  label,
  side,
  style,
  children,
}: {
  tip: ReactNode;
  label: string;
  side?: 'top' | 'bottom' | 'left' | 'right';
  style?: CSSProperties;
  children: ReactNode;
}) {
  return (
    <Tooltip content={tip} side={side}>
      <button
        type="button"
        aria-label={label}
        style={{
          display: 'inline-flex',
          padding: 0,
          background: 'none',
          border: 'none',
          font: 'inherit',
          color: 'inherit',
          cursor: 'help',
          ...style,
        }}
      >
        {children}
      </button>
    </Tooltip>
  );
}
