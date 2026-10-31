// A password-type field with a show/hide toggle inside it, so someone who
// mistyped can see what they typed and fix it (#357).
//
// The design system's Input takes no adornment, so the label is drawn here and
// the toggle sits over the right end of the box. The box is 36px and the
// toggle 28px, so a 4px inset centers it. Toggling keeps the value and puts
// focus back in the field, with the caret or selection where it was.

import { useId, useLayoutEffect, useRef, useState } from 'react';
import { Icon, IconButton, Input } from '@design';
import type { InputProps } from '@design';

export function SecretInput({ label, id, ...rest }: Readonly<Omit<InputProps, 'type' | 'as'> & { label: string }>) {
  const [shown, setShown] = useState(false);
  const generated = useId();
  const inputId = id ?? generated;

  const caret = useRef<[number | null, number | null] | null>(null);

  // Input takes no ref and no style for its box, so its element is found by
  // the id given to it. Its right padding is widened to the toggle's width so
  // a long value never runs under it; Input's own style only sets the
  // shorthand, which React leaves alone while it is unchanged.
  useLayoutEffect(() => {
    const field = document.getElementById(inputId) as HTMLInputElement | null;
    if (!field) return;
    field.style.paddingRight = 'var(--control-input)';
    // Changing the type can collapse the selection, so it is put back.
    if (caret.current) {
      field.focus();
      field.setSelectionRange(caret.current[0], caret.current[1]);
      caret.current = null;
    }
  }, [shown, inputId]);

  const toggle = () => {
    const field = document.getElementById(inputId) as HTMLInputElement | null;
    caret.current = field ? [field.selectionStart, field.selectionEnd] : null;
    setShown(!shown);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <label htmlFor={inputId} style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>
        {label}
      </label>
      <div style={{ position: 'relative' }}>
        <Input {...rest} id={inputId} type={shown ? 'text' : 'password'} />
        <IconButton
          label={shown ? `Hide ${label.toLowerCase()}` : `Show ${label.toLowerCase()}`}
          aria-pressed={shown}
          onClick={toggle}
          style={{ position: 'absolute', top: 'var(--space-1)', right: 'var(--space-1)' }}
        >
          <Icon name={shown ? 'eye-off' : 'eye'} size={16} />
        </IconButton>
      </div>
    </div>
  );
}
