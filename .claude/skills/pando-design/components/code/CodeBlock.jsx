import React, { useRef, useState } from 'react';
import { IconButton } from '../core/IconButton.jsx';
import { Icon } from '../core/Icon.jsx';

/** Copies the current selection; deprecated, and the only clipboard write a
 *  page has outside a secure context. */
function copySelection() {
  try {
    return typeof document.execCommand === 'function' && document.execCommand('copy');
  } catch {
    return false;
  }
}

/** Terminal surface: always ink background with terminal-text, in both themes. */
export function CodeBlock({ lines = [], prompt = false, title, copyable = true, dense = false, style, children, ...rest }) {
  const [copied, setCopied] = useState(null);
  const body = useRef(null);
  const rows = Array.isArray(lines) ? lines : String(lines).split('\n');
  const text = () => (children ? body.current?.textContent ?? '' : rows.map((l) => (typeof l === 'string' ? l : l.text)).join('\n'));
  const say = (what) => { setCopied(what); window.setTimeout(() => setCopied(null), 1400); };
  // The clipboard API is absent outside a secure context, and Pando reached
  // over plain HTTP on a private network is normal: the text is selected and
  // copied from the selection instead, which browsers still allow inside a
  // click. Where even that is refused, the selection stays, so copying it is
  // one keystroke away. The prompt and timestamps cannot be selected, so what
  // is copied from the selection is what Copy would copy.
  const copy = async () => {
    try {
      if (!navigator.clipboard) throw new Error('no clipboard');
      await navigator.clipboard.writeText(text());
      say('Copied');
    } catch {
      const range = document.createRange();
      range.selectNodeContents(body.current);
      window.getSelection()?.removeAllRanges();
      window.getSelection()?.addRange(range);
      say(copySelection() ? 'Copied' : 'Selected');
    }
  };
  return (
    <div
      style={{
        position: 'relative',
        background: 'var(--terminal)', color: 'var(--terminal-text)',
        border: '1px solid var(--rule-strong)',
        borderRadius: 'var(--radius-md)',
        padding: dense ? 'var(--space-3)' : 'var(--space-4)',
        // Room for the copy button, so it never sits on the text.
        paddingRight: copyable ? 'var(--space-6)' : undefined,
        font: 'var(--type-code)',
        overflow: 'hidden',
        ...style,
      }}
      {...rest}
    >
      {title && (
        <div style={{ font: 'var(--type-code-sm)', color: 'rgba(236,232,222,0.55)', marginBottom: 'var(--space-2)' }}>{title}</div>
      )}
      {copyable && (
        <div style={{ position: 'absolute', top: 'var(--space-2)', right: 'var(--space-2)', display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
          {copied && <output style={{ font: 'var(--type-code-sm)', color: 'rgba(236,232,222,0.55)' }}>{copied}</output>}
          <IconButton label="Copy" onTerminal onClick={() => void copy()}><Icon name={copied === 'Copied' ? 'check' : 'copy'} size={14} color="var(--terminal-text)" /></IconButton>
        </div>
      )}
      <div ref={body}>
      {children || rows.map((l, i) => {
        const line = typeof l === 'string' ? { text: l } : l;
        return (
          <div key={i} style={{ display: 'flex', gap: 'var(--space-2)', whiteSpace: 'pre-wrap', color: line.tone === 'muted' ? 'rgba(236,232,222,0.55)' : line.tone === 'ok' ? 'var(--vegetation-deep)' : line.tone === 'fail' ? 'var(--marker)' : 'var(--terminal-text)' }}>
            {prompt && <span style={{ color: 'var(--terminal-prompt)', flex: '0 0 auto', userSelect: 'none' }}>$</span>}
            {line.time && <span style={{ color: 'rgba(236,232,222,0.38)', flex: '0 0 auto', userSelect: 'none' }}>{line.time}</span>}
            <span>{line.text}</span>
          </div>
        );
      })}
      </div>
    </div>
  );
}
