// AI at work, the same wherever it is: the plan page and every AI dialog.
//
// A glyph turning through the asterisk-flower sequence in water blue, a
// present-tense phrase that changes every couple of seconds, and the time so
// far. Announced politely; under reduced motion the glyph holds still and the
// first phrase stays.

import { useEffect, useState } from 'react';

import './ai.css';

/**
 * The glyphs AI's working line turns through, out and back, one per frame:
 * the asterisk-flower that AI tools use for "thinking", so it reads as that
 * at a glance. An approved exception to the design system's no-unicode-icons
 * rule, for AI at work only (design 08 §1.3).
 */
const AI_GLYPHS = ['✢', '✳', '✶', '✻', '✽', '✻', '✶', '✳'] as const;

/** How long each glyph shows. */
const AI_GLYPH_MS = 120;

/** How often AI's working phrase changes. */
const THINKING_MS = 2_600;

export function useReducedMotion(): boolean {
  const [reduced] = useState(
    () => typeof window !== 'undefined' && Boolean(window.matchMedia?.('(prefers-reduced-motion: reduce)').matches),
  );
  return reduced;
}

/** What AI is doing, per function, in the voice of the plan page's phrases:
 *  present tense, plain, no trailing dots (the line animates its own). */
export const AI_PHRASES = {
  access: ['Reading what you asked for', 'Looking through Pando’s permissions', 'Matching people to accounts', 'Drafting the role and group'],
  policy: ['Reading what you asked for', 'Reading the current policy', 'Checking what the startup config fixes', 'Drafting the changes'],
  audit: ['Reading your question', 'Choosing the filters', 'Searching the audit log', 'Summarizing what it found'],
  reference: ['Reading your question', 'Looking through the reference', 'Finding the endpoints and commands', 'Writing the answer'],
} as const;

/**
 * AI at work: the glyph turning, a phrase that changes every couple of
 * seconds, and the time so far. Holds on the last phrase rather than cycling
 * back to the first, which would read as starting over.
 */
export function AiThinking({ phrases }: { phrases: readonly string[] }) {
  const reduced = useReducedMotion();
  const [tick, setTick] = useState(0);
  const [started] = useState(() => Date.now());
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const clock = window.setInterval(() => setNow(Date.now()), 1_000);
    if (reduced) return () => window.clearInterval(clock);
    const turn = window.setInterval(() => setTick((n) => n + 1), THINKING_MS);
    return () => {
      window.clearInterval(clock);
      window.clearInterval(turn);
    };
  }, [reduced]);
  const phrase = phrases[Math.min(reduced ? 0 : tick, phrases.length - 1)];
  return (
    <div role="status" aria-live="polite" style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
      <AiGlyph />
      <span key={phrase} className="pando-onboard-enter" style={{ font: 'var(--type-body-ui)', color: 'var(--water)' }}>
        {phrase}
        <span className="pando-dots" aria-hidden="true">
          <span>.</span>
          <span>.</span>
          <span>.</span>
        </span>
      </span>
      <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink-muted)' }}>
        {Math.max(0, Math.floor((now - started) / 1_000))}s
      </span>
    </div>
  );
}

/**
 * The glyph alone, in water blue. Hidden from assistive technology (the
 * phrase beside it is announced); under reduced motion it holds on the first.
 */
export function AiGlyph() {
  const reduced = useReducedMotion();
  const [frame, setFrame] = useState(0);
  useEffect(() => {
    if (reduced) return;
    const timer = window.setInterval(() => setFrame((n) => (n + 1) % AI_GLYPHS.length), AI_GLYPH_MS);
    return () => window.clearInterval(timer);
  }, [reduced]);
  return (
    <span
      aria-hidden="true"
      style={{
        display: 'inline-block',
        width: '1em',
        textAlign: 'center',
        font: 'var(--type-body-ui)',
        color: 'var(--water)',
      }}
    >
      {AI_GLYPHS[frame]}
    </span>
  );
}
