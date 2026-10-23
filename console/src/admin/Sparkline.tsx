// A small line of the readings this page has taken (R-245).
//
// Drawn from what the page already asks for while it is open, and nothing
// else: Pando keeps no history (R-016), so the line starts when the page opens
// and is gone when it closes. A flat tint under the line, a dot at the latest
// reading, and a dashed line at the limit where there is one.

const WIDTH = 200;
const HEIGHT = 36;
// Room above the limit line, so it is not drawn on the edge.
const TOP = 1.5;

/** Where each reading sits, as SVG points, against `scale`. */
export function points(values: readonly number[], scale: number): Array<[number, number]> {
  if (values.length === 0 || scale <= 0) return [];
  // One reading is drawn across, so the first look at a page has a line.
  const series = values.length === 1 ? [values[0]!, values[0]!] : values;
  const step = WIDTH / (series.length - 1);
  return series.map((v, i) => {
    const share = Math.min(1, Math.max(0, v / scale));
    return [Number((i * step).toFixed(1)), Number((HEIGHT - share * (HEIGHT - TOP)).toFixed(1))];
  });
}

export function Sparkline({
  values,
  limit = 0,
  tone = 'normal',
}: Readonly<{
  values: readonly number[];
  /** The limit the reading runs under; 0 is none, and draws no limit line. */
  limit?: number;
  /** `warn` near the limit. */
  tone?: 'normal' | 'warn';
}>) {
  const color = tone === 'warn' ? 'var(--contour-text)' : 'var(--water)';
  // Against the limit where there is one; otherwise against the highest
  // reading, with room above it so a flat line is not drawn along the top.
  const scale = limit > 0 ? limit : Math.max(...values, 0) * 1.25;
  const at = points(values, scale);
  const line = at.map(([x, y]) => `${x},${y}`).join(' ');
  const last = at.at(-1);

  return (
    <div style={{ position: 'relative' }}>
      <svg
        width="100%"
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        preserveAspectRatio="none"
        aria-hidden="true"
        style={{ display: 'block', height: 'calc(var(--space-6) + var(--space-1))' }}
      >
        {limit > 0 && (
          <line
            x1={0}
            x2={WIDTH}
            y1={TOP}
            y2={TOP}
            stroke="var(--rule-strong)"
            strokeWidth={1}
            strokeDasharray="2 3"
            vectorEffect="non-scaling-stroke"
            data-limit=""
          />
        )}
        {at.length > 0 && (
          <>
            <polygon points={`0,${HEIGHT} ${line} ${WIDTH},${HEIGHT}`} fill={color} fillOpacity={0.22} />
            <polyline
              points={line}
              fill="none"
              stroke={color}
              strokeWidth={1.75}
              strokeLinejoin="round"
              strokeLinecap="round"
              vectorEffect="non-scaling-stroke"
            />
          </>
        )}
      </svg>
      {last && (
        <span
          aria-hidden="true"
          style={{
            position: 'absolute',
            right: 'calc(var(--space-1) * -0.75)',
            top: `calc(${((last[1] / HEIGHT) * 100).toFixed(1)}% - var(--space-1) * 0.75)`,
            width: 'calc(var(--space-1) * 1.5)',
            height: 'calc(var(--space-1) * 1.5)',
            borderRadius: 'var(--radius-pill)',
            background: color,
          }}
        />
      )}
    </div>
  );
}
