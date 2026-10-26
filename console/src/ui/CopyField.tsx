// A value to be copied into another system, one to a field: a provider's
// redirect URI, a SCIM token. Copy takes exactly the value — never a label
// beside it, which a block of "Name: value" lines copied whole did.

import { useEffect, useRef, useState } from 'react';
import { Icon, IconButton } from '@design';
import { copyText } from './clipboard';

/** Copies text, or selects it where the browser will not copy at all. Says
 *  which happened. */
export async function copyOrSelect(text: string, node: HTMLElement | null): Promise<'copied' | 'selected'> {
  if (await copyText(text)) return 'copied';
  if (node) {
    const range = document.createRange();
    range.selectNodeContents(node);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
  }
  return 'selected';
}

function useCopy(value: string) {
  const node = useRef<HTMLElement>(null);
  const [done, setDone] = useState<'copied' | 'selected' | null>(null);
  useEffect(() => setDone(null), [value]);
  const copy = async () => {
    setDone(await copyOrSelect(value, node.current));
    window.setTimeout(() => setDone(null), 2_000);
  };
  return { node, done, copy };
}

export function CopyField({ label, value, helper }: Readonly<{ label: string; value: string; helper?: string }>) {
  const { node, done, copy } = useCopy(value);
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
      <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>{label}</span>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 'var(--space-2)',
          padding: 'var(--space-1) var(--space-1) var(--space-1) var(--space-3)',
          background: 'var(--paper-sunken)',
          border: 'var(--border-width) solid var(--rule)',
          borderRadius: 'var(--radius-sm)',
        }}
      >
        <code
          ref={node}
          style={{ flex: 1, minWidth: 0, font: 'var(--type-code-sm)', color: 'var(--ink)', overflowWrap: 'anywhere' }}
        >
          {value}
        </code>
        <IconButton label={done === 'copied' ? 'Copied' : `Copy ${label}`} onClick={() => void copy()}>
          <Icon name={done === 'copied' ? 'check' : 'copy'} size={16} />
        </IconButton>
      </div>
      {(done === 'selected' || helper) && (
        <output style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          {done === 'selected' ? 'Your browser blocked copying. It is selected; copy it from here.' : helper}
        </output>
      )}
    </div>
  );
}

const INLINE_LABEL = { idle: 'Copy', copied: 'Copied', selected: 'Selected; copy it from here' };

/** A value inside a sentence, such as a step's {callback_url}, with a copy
 *  button beside it. */
export function CopyInline({ value }: Readonly<{ value: string }>) {
  const { node, done, copy } = useCopy(value);
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)', maxWidth: '100%' }}>
      <code
        ref={node}
        style={{
          font: 'var(--type-code-sm)',
          background: 'var(--paper-sunken)',
          borderRadius: 'var(--radius-xs)',
          padding: '0 var(--space-1)',
          overflowWrap: 'anywhere',
        }}
      >
        {value}
      </code>
      <IconButton
        label={INLINE_LABEL[done ?? 'idle']}
        size={24}
        onClick={() => void copy()}
      >
        <Icon name={done === 'copied' ? 'check' : 'copy'} size={14} />
      </IconButton>
    </span>
  );
}
