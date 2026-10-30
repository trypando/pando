// A password-type field with a show/hide toggle inside it, so someone who
// mistyped can see what they typed and fix it (#357).
//
// The design system's Input takes no adornment, so the label is drawn here and
// the toggle sits over the right end of the box. The box is 36px and the
// toggle 28px, so a 4px inset centers it. Toggling keeps the value and puts
// focus back in the field, where the caret was.

import { useId, useState } from 'react';
import { Icon, IconButton, Input } from '@design';
import type { InputProps } from '@design';

export function SecretInput({ label, id, ...rest }: Readonly<Omit<InputProps, 'type' | 'as'> & { label: string }>) {
  const [shown, setShown] = useState(false);
  const generated = useId();
  const inputId = id ?? generated;

  const toggle = () => {
    setShown(!shown);
    // Input takes no ref; its element is found by the id given to it.
    document.getElementById(inputId)?.focus();
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
