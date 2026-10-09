// The security score: a shield, colored, with the number beside it (R-310,
// R-320).
//
// The shield carries the color and the number carries the fact. R-320 rules out
// a grade or a color standing on its own — "F" tells a deployer nothing they can
// act on, and a red mark tells somebody who cannot see red nothing at all — so
// the two always travel together, which is the design system's own rule for
// status: a symbol plus a word, never a colored pill.
//
// The shape is drawn here rather than taken from the design system because its
// vendored icon set has no shield. When one is added upstream this becomes an
// `<Icon name="shield">` and nothing else changes; the stroke weight and the
// colors are the system's already.
//
// The color says what the number means *here*. An installation with a threshold
// colors against the threshold; one without colors against bands, because a
// score of 20 is worth noticing even where nothing is enforced.

const TONES = {
  bad: { ink: 'var(--marker)', fill: 'var(--status-failed-tint)' },
  warn: { ink: 'var(--contour)', fill: 'var(--status-building-tint)' },
  good: { ink: 'var(--vegetation-deep)', fill: 'var(--status-running-tint)' },
  none: { ink: 'var(--ink-muted)', fill: 'var(--paper-sunken)' },
} as const;

export type Verdict = 'ok' | 'insecure' | 'unscanned' | 'inert' | '';

function tone(score: number | null | undefined, verdict?: Verdict, threshold = 0) {
  if (score === null || score === undefined) return 'none' as const;
  if (verdict === 'insecure') return 'bad' as const;

  // Within ten of the floor: passing, and one new CVE from not passing.
  if (threshold > 0 && score < threshold + 10) return 'warn' as const;
  if (threshold > 0) return 'good' as const;

  // No threshold set. Bands, so the number still reads as something.
  if (score < 60) return 'bad' as const;
  if (score < 80) return 'warn' as const;
  return 'good' as const;
}

export function ScoreBadge({
  score,
  verdict,
  threshold = 0,
  full = false,
  size = 18,
}: {
  score?: number | null;
  verdict?: Verdict;
  /** The installation's minimum, when the reader is allowed to know it. */
  threshold?: number;
  /** Show "/ 100". On in a table, where a bare number is ambiguous. */
  full?: boolean;
  size?: number;
}) {
  const colors = TONES[tone(score, verdict, threshold)];
  const unscanned = score === null || score === undefined;

  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 'var(--space-2)',
        whiteSpace: 'nowrap',
      }}
      title={
        unscanned
          ? 'This app has not been scanned yet.'
          : verdict === 'insecure'
            ? `Below this installation's requirement of ${threshold}`
            : 'Security score, out of 100'
      }
    >
      <Shield color={colors.ink} fill={colors.fill} size={size} />
      <span style={{ font: 'var(--type-code-sm)', color: unscanned ? 'var(--ink-secondary)' : colors.ink }}>
        {unscanned ? 'Not scanned' : full ? `${score} / 100` : score}
      </span>
    </span>
  );
}

/** A shield at the brand's 1.5px stroke, filled with its own tint. */
function Shield({ color, fill, size }: { color: string; fill: string; size: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill={fill}
      stroke={color}
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      style={{ flex: '0 0 auto' }}
    >
      <path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z" />
    </svg>
  );
}
