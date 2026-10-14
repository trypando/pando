// A password Pando made up, for an administrator to hand to somebody.
//
// Adding an account and resetting one's password used to take a password field,
// which asked the administrator to invent a credential on the spot — and what
// people invent on the spot is short, reused, or both. The server generates it
// instead (POST /passwords/generate stores nothing), and this shows it once to
// be copied and passed on by some other channel.
//
// The field is read-only on purpose. The value is meant to be read and copied,
// not edited into something weaker; somebody who wants a different one asks for
// another.
//
// Masked until asked for. The dialog is open on an administrator's screen,
// which may be shared or looked over; Copy works without ever showing it.

import { useEffect, useRef, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Button, Checkbox, Icon, IconButton, Input } from '@design';

import { api } from '@api/client';
import { messageOf } from './Accounts';
import './GeneratedPassword.css';

export function GeneratedPassword({
  value,
  onChange,
  mustChange,
  onMustChange,
}: {
  /** The generated password, or '' until the first one arrives. */
  value: string;
  onChange: (password: string) => void;
  mustChange: boolean;
  onMustChange: (mustChange: boolean) => void;
}) {
  const generate = useMutation({
    mutationFn: () => api.post<{ password: string }>('/passwords/generate'),
    onSuccess: (r) => onChange(r.password),
  });

  // One on arrival, so the dialog opens with a password already in it. Guarded
  // so a remount in development does not ask twice.
  const asked = useRef(false);
  useEffect(() => {
    if (asked.current || value !== '') return;
    asked.current = true;
    generate.mutate();
  }, [generate, value]);

  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-4)',
      }}
    >
      <PasswordToCopy
        value={value}
        pending={generate.isPending && value === ''}
        error={generate.isError ? messageOf(generate.error) : undefined}
        extra={
          <Button variant="secondary" disabled={generate.isPending} onClick={() => generate.mutate()}>
            {generate.isPending ? 'Generating' : 'Regenerate'}
          </Button>
        }
      />
      <Checkbox
        label="Must change password at next sign-in"
        // R-046's flag, set by default. A password that has passed through an
        // administrator's hands is known to two people.
        description="Recommended."
        checked={mustChange}
        onChange={(e) => onMustChange(e.target.checked)}
      />
    </div>
  );
}

/** A password, masked read-only, with an eye to show it and a way to copy it. */
export function PasswordToCopy({
  value,
  pending = false,
  error,
  extra,
}: {
  value: string;
  pending?: boolean;
  error?: string;
  /** Another action beside Copy. */
  extra?: React.ReactNode;
}) {
  // The design system's Input takes no ref, so the field is found through its
  // container.
  const box = useRef<HTMLDivElement>(null);
  const [copied, setCopied] = useState<'yes' | 'select' | null>(null);
  // Kept across Regenerate: somebody who chose to see it wants to see the
  // next one too.
  const [shown, setShown] = useState(false);

  // A new password is not the one that was copied.
  useEffect(() => setCopied(null), [value]);

  async function copy() {
    try {
      // Absent outside a secure context, and an install reached over plain
      // HTTP on a private network is a normal way to run Pando.
      if (!navigator.clipboard) throw new Error('no clipboard');
      await navigator.clipboard.writeText(value);
      setCopied('yes');
      window.setTimeout(() => setCopied(null), 2_000);
    } catch {
      // Select it instead, so copying it is one keystroke away — shown first,
      // because browsers will not copy out of a masked field.
      setShown(true);
      window.setTimeout(() => {
        const field = box.current?.querySelector('input');
        field?.focus();
        field?.select();
      });
      setCopied('select');
    }
  }

  return (
    <div
      ref={box}
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-3)',
      }}
    >
      {/* The label is drawn here rather than by Input, so the box below holds
          only the field and the eye can be placed on it. */}
      <label
        htmlFor="generated-password"
        style={{
          font: 'var(--type-label)',
          color: 'var(--ink)',
          marginBottom: 'calc(-1 * var(--space-2))',
        }}
      >
        Password
      </label>
      <div style={{ position: 'relative' }}>
        <Input
          id="generated-password"
          className="pando-password-field"
          mono
          readOnly
          type={shown ? 'text' : 'password'}
          value={pending ? '' : value}
          placeholder={pending ? 'Generating' : undefined}
          autoComplete="off"
          spellCheck={false}
          error={error}
          helper={
            copied === 'select'
              ? 'Your browser blocked copying. The password is selected; copy it from here.'
              : undefined
          }
        />
        {/* The eye in the field, as sign-in forms everywhere put it: crossed
            out while the password is hidden, open while it shows. */}
        <div
          style={{
            position: 'absolute',
            top: 0,
            right: 'var(--space-1)',
            height: 'var(--control-input)',
            display: 'flex',
            alignItems: 'center',
          }}
        >
          <IconButton
            label={shown ? 'Hide password' : 'Show password'}
            aria-pressed={shown}
            disabled={value === ''}
            onClick={() => setShown((v) => !v)}
          >
            <Icon name={shown ? 'eye' : 'eye-off'} size={16} />
          </IconButton>
        </div>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-3)' }}>
        <Button variant="secondary" disabled={value === ''} onClick={() => void copy()}>
          {copied === 'yes' ? 'Copied' : 'Copy'}
        </Button>

        {extra}
      </div>
    </div>
  );
}
