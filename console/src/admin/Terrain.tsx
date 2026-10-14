// The elevation profile across the top of the onboarding page (design 08 §1.3).
//
// One of the brand's two orchestrated moments (readme §Motion): the ridge
// draws left to right as detection advances, and the summit mark lands when
// there is a plan. The design handoff approved it as an exception to where the
// contour system may appear; it is the one terrain element on the page.
//
// The profile is seeded by the app's source alone, known from the first frame,
// so the same app always gets the same hills: the ridge keeps its shape while
// detection runs, when the commit arrives, and when the page is reopened later.
// Undrawn terrain is a dashed ghost; the drawn part is revealed by
// a clip scaled on X, written straight to the DOM every animation frame so the
// motion is smooth without re-rendering React sixty times a second. Markers
// live in a second, unstretched SVG so circles stay circles at any width.

import { useEffect, useId, useMemo, useRef } from 'react';

import { elevation, peakOf } from './discovery';

const W = 1600;
const H = 150;
const SCALES = [1, 0.8, 0.6, 0.4, 0.2];
const SAMPLES = 400;

/** The fastest the ridge draws, in widths per second. */
const MAX_RATE = 0.32;

export interface TerrainStop {
  x: number;
  reached: boolean;
  ai: boolean;
  failed: boolean;
}

export function Terrain({
  seed,
  fraction,
  aiFrom,
  stops,
  summit,
  moving,
}: {
  seed: string;
  /** How much is drawn, 0 to 1, read every frame. */
  fraction: () => number;
  /** Where the AI step starts, 0 to 1; 1 when there is none. */
  aiFrom: number;
  stops: TerrainStop[];
  summit: boolean;
  /** Whether the leading edge is still moving: the tip is shown only then. */
  moving: boolean;
}) {
  const id = useId().replaceAll(':', '');
  const grow = useRef<SVGRectElement>(null);
  const tip = useRef<SVGSVGElement>(null);
  // Where the ridge is drawn to. Unset until the first frame: opened on a
  // detection that already finished, it starts where that detection ended —
  // there is nothing to watch.
  const shown = useRef<number | null>(null);
  // Read through a ref, so a re-render (the page polls) does not restart the
  // animation loop.
  const read = useRef(fraction);
  read.current = fraction;

  const { lines, yAt, peak } = useMemo(() => {
    const elev = elevation(seed);
    const y = (x: number, s: number) => H - 6 - elev(x) * s * (H - 30);
    const paths = SCALES.map((s) => {
      let d = '';
      for (let i = 0; i <= SAMPLES; i++) {
        const x = i / SAMPLES;
        d += `${i ? ' L ' : 'M '}${(x * W).toFixed(1)} ${y(x, s).toFixed(2)}`;
      }
      return d;
    });
    return { lines: paths, yAt: (x: number) => y(x, 1), peak: peakOf(elev) };
  }, [seed]);

  useEffect(() => {
    let frame = 0;
    let last = performance.now();
    const draw = (now: number) => {
      const dt = Math.min(0.1, Math.max(0, (now - last) / 1_000));
      last = now;
      // Never backwards: a step appearing mid-run (the trial run, the AI
      // step) re-divides the width, and a ridge that retracts reads as Pando
      // undoing work. And never faster than MAX_RATE: a step finishing moves
      // the target on by a whole step at once, and the ridge walks there
      // rather than jumping.
      const target = Math.min(1, Math.max(0, read.current()));
      const from = shown.current ?? (moving ? 0 : target);
      const x = target <= from ? from : Math.min(target, from + MAX_RATE * dt);
      shown.current = x;
      grow.current?.setAttribute('transform', `scale(${x.toFixed(5)} 1)`);
      const t = tip.current;
      if (t) {
        const visible = moving && x > 0 && x < 1;
        t.style.display = visible ? '' : 'none';
        if (visible) {
          t.setAttribute('x', `${(x * 100).toFixed(3)}%`);
          t.setAttribute('y', yAt(x).toFixed(2));
        }
      }
      frame = requestAnimationFrame(draw);
    };
    frame = requestAnimationFrame(draw);
    return () => cancelAnimationFrame(frame);
  }, [moving, yAt]);

  const tipColor = (shown.current ?? 0) > aiFrom ? 'var(--water)' : 'var(--ink-secondary)';

  return (
    <div className="pando-terrain" aria-hidden="true">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="pando-terrain-lines">
        <defs>
          <clipPath id={`${id}-grow`}>
            <rect ref={grow} x={0} y={-20} width={W} height={H + 40} transform="scale(0 1)" />
          </clipPath>
          <clipPath id={`${id}-ai`}>
            <rect x={aiFrom * W} y={-20} width={W} height={H + 40} />
          </clipPath>
        </defs>
        {lines.map((d, i) => (
          <path key={`ghost-${i}`} d={d} className="pando-terrain-ghost" />
        ))}
        <g clipPath={`url(#${id}-grow)`}>
          {lines.map((d, i) => (
            <path key={`line-${i}`} d={d} className={i === 0 ? 'pando-terrain-index' : 'pando-terrain-line'} />
          ))}
          {aiFrom < 1 && (
            <g clipPath={`url(#${id}-ai)`}>
              <path d={lines[0]} className="pando-terrain-ai" />
            </g>
          )}
          <path d={lines[0]} className="pando-terrain-trail" transform="translate(0,-5)" />
        </g>
        {stops.map((s, i) => (
          <line
            key={`stop-${i}`}
            x1={s.x * W}
            x2={s.x * W}
            y1={yAt(s.x)}
            y2={H - 6}
            className="pando-terrain-stop"
            opacity={s.reached ? 1 : 0.5}
          />
        ))}
      </svg>

      <svg className="pando-terrain-marks">
        {stops
          .filter((s) => s.reached)
          .map((s, i) => (
            <svg key={`way-${i}`} x={`${s.x * 100}%`} y={yAt(s.x)} overflow="visible">
              <circle
                r={4.5}
                className="pando-terrain-waypoint"
                stroke={s.failed ? 'var(--marker)' : s.ai ? 'var(--water)' : 'var(--ink)'}
              />
            </svg>
          ))}
        <svg ref={tip} overflow="visible" style={{ display: 'none' }}>
          <circle r={9} fill="none" stroke={tipColor} strokeWidth={1} opacity={0.4} />
          <circle r={3} fill={tipColor} />
        </svg>
        {summit && (
          <svg x={`${peak * 100}%`} y={yAt(peak) - 12} overflow="visible">
            {/* The summit: the survey benchmark triangle, the one red mark. */}
            <polygon points="0,-8.68 8.68,7 -8.68,7" className="pando-terrain-summit" />
          </svg>
        )}
      </svg>
    </div>
  );
}
