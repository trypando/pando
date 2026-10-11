// Asking AI to do something (R-343 … R-346).
//
// Out of the way until wanted: a screen shows one button with the AI mark, and
// everything else — the request, the preview, refining it, accepting it —
// happens in a dialog that button opens. The screen underneath stays the
// screen, and nothing AI proposes reaches it until a person accepts it.

import { useEffect, useRef, useState } from 'react';
import { Button, Dialog, Input } from '@design';

import { refusal } from '../install/Accounts';
import { AiStar } from './AiStar';
import { BesideField } from './BesideField';
import { WithCode, codeOf } from './ErrorCode';

/** The one way into an AI function on a screen: the AI mark and "Ask AI". */
export function AIButton({ onClick, label = 'Ask AI' }: { onClick: () => void; label?: string }) {
  return (
    <Button variant="secondary" icon={<AiStar size={16} />} onClick={onClick}>
      {label}
    </Button>
  );
}

/** A dialog for one AI function: the mark and what AI will do, then the
 *  caller's request field, preview and actions. */
export function AIDialog({
  title,
  intro,
  footer,
  onClose,
  children,
}: {
  title: string;
  /** What AI will do here, and that nothing applies until you accept it. */
  intro: string;
  footer: React.ReactNode;
  onClose: () => void;
  children: React.ReactNode;
}) {
  return (
    <Dialog open title={title} width={640} onClose={onClose} footer={footer}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <div style={{ display: 'flex', gap: 'var(--space-2)', alignItems: 'flex-start' }}>
          <AiStar size={18} />
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{intro}</span>
        </div>
        {children}
      </div>
    </Dialog>
  );
}

/** The request: one field and Ask AI beside it. Clears once asked, so a
 *  follow-up starts empty. */
export function AIPrompt({
  label,
  placeholder,
  pending,
  error,
  onAsk,
}: {
  label: string;
  placeholder?: string;
  pending: boolean;
  error?: unknown;
  onAsk: (text: string) => void;
}) {
  const [text, setText] = useState('');
  // The question stays in the field, locked, while AI works on it, so the
  // person can see what they asked; it clears once an answer arrives, and
  // stays when the asking failed so it can be sent again.
  //
  // Cleared on the request finishing, which is pending seen and then gone —
  // not on pending being false, which it still is in the render right after
  // sending, before the request has registered.
  const asking = useRef(false);
  useEffect(() => {
    if (pending) {
      asking.current = true;
      return;
    }
    if (!asking.current) return;
    asking.current = false;
    if (error == null) setText('');
  }, [pending, error]);
  const ask = () => {
    const t = text.trim();
    if (!t || pending) return;
    onAsk(t);
  };
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          ask();
        }}
        style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-end', flexWrap: 'wrap' }}
      >
        <Input
          label={label}
          placeholder={placeholder}
          value={text}
          autoFocus
          disabled={pending}
          onChange={(e) => setText(e.target.value)}
          style={{ flex: '1 1 20rem' }}
        />
        <BesideField>
          <Button type="submit" icon={<AiStar size={14} />} disabled={!text.trim() || pending}>
            {pending ? 'Asking' : 'Ask AI'}
          </Button>
        </BesideField>
      </form>
      {error != null && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          <WithCode code={codeOf(error)}>{refusal(error)}</WithCode>
        </p>
      )}
    </div>
  );
}

/** Which adapter and model wrote something, with the AI mark, so an answer is
 *  never mistaken for Pando's own. */
export function AnsweredBy({ adapter, model }: { adapter?: string; model?: string }) {
  if (!adapter) return null;
  return (
    <p
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 'var(--space-2)',
        font: 'var(--type-caption)',
        color: 'var(--ink-secondary)',
        margin: 0,
      }}
    >
      <AiStar size={12} />
      <span>
        Written by AI ({adapter}
        {model ? `, ${model}` : ''}). Check it before you use it.
      </span>
    </p>
  );
}

/** A heading inside an AI dialog. */
export function AIHeading({ children }: { children: React.ReactNode }) {
  return <h5 style={{ font: 'var(--type-label)', margin: 0 }}>{children}</h5>;
}
