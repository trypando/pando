import React, { useState } from 'react';

export function Tooltip({ content, side = 'top', style, children, ...rest }) {
  const [show, setShow] = useState(false);
  const pos = {
    top: { bottom: '100%', left: '50%', transform: 'translate(-50%, -6px)' },
    bottom: { top: '100%', left: '50%', transform: 'translate(-50%, 6px)' },
    left: { right: '100%', top: '50%', transform: 'translate(-6px, -50%)' },
    right: { left: '100%', top: '50%', transform: 'translate(6px, -50%)' },
  }[side];
  return (
    <span
      style={{ position: 'relative', display: 'inline-flex', ...style }}
      onMouseEnter={() => setShow(true)} onMouseLeave={() => setShow(false)}
      onFocus={() => setShow(true)} onBlur={() => setShow(false)}
      {...rest}
    >
      {children}
      {show && (
        <span role="tooltip" style={{
          position: 'absolute', zIndex: 40, ...pos,
          padding: '4px 8px', whiteSpace: 'nowrap',
          background: 'var(--paper-raised)', color: 'var(--ink)',
          border: '1px solid var(--rule-strong)', borderRadius: 'var(--radius-sm)',
          boxShadow: 'var(--shadow-popover)',
          font: 'var(--type-caption)', pointerEvents: 'none',
        }}>{content}</span>
      )}
    </span>
  );
}
