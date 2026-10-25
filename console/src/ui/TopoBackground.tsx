// Topographic terrain behind the console.
//
// Generated, not drawn: a height field made of irregular hills, with contour
// lines traced through it by marching squares and smoothed into curves. Lines
// at different heights can never cross, because they are level sets of one
// surface — which is what makes it read as a real survey map rather than a
// pattern.
//
// It is part of the page, not pinned to the window: it scrolls with the
// content. To cover a page of any length it is a tile, and the terrain is
// periodic — hills wrap around the tile's edges and the warp uses whole
// periods — so the lines meet across every seam and there is no edge to see.
//
// Behind working screens it is extremely quiet — one colour, one line width,
// no index contours — because a table has to be read over it. The colour is
// `--contour-background`, one step darker than the paper and drawn at full
// strength, rather than the brown contour line faded down: the brown is the
// illustration's, and kept for the places the map is meant to stand out.
// TopoMap, below, is the same terrain drawn as a picture in colour, for the
// sign-in screen. Each page gets its own terrain from a seed (the section, and
// the app when there is one); the same seed always gives the same map.

import { useMemo } from 'react';

// One tile, in CSS pixels. Large, so a repeat is rarely in view at once.
const COLS = 160;
const ROWS = 120;
const CELL = 10;
const TAU = Math.PI * 2;

const BACKGROUND_LEVELS = 12;

type Pt = [number, number];

/** FNV-1a, so a route string becomes a stable 32-bit seed. */
function hash(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.codePointAt(i) ?? 0;
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/** mulberry32: small, fast, and the same sequence for the same seed. */
function rng(seed: number): () => number {
  let a = seed;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0; // kept to 32 bits, not truncated
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Distance on a unit circle, so a hill near one edge continues past it. */
function wrap(d: number): number {
  const a = Math.abs(d) % 1;
  return Math.min(a, 1 - a);
}

type Height = (x: number, y: number) => number;

/** The seeded surface. Periodic over the tile, so it can repeat seamlessly. */
function surface(seed: string): Height {
  const r = rng(hash(seed));
  const between = (lo: number, hi: number) => lo + r() * (hi - lo);

  const hills = Array.from({ length: 7 + Math.floor(r() * 4) }, () => ({
    cx: r(),
    cy: r(),
    sx: between(0.07, 0.18),
    sy: between(0.08, 0.2),
    a: between(0.3, 1),
  }));
  const p = Array.from({ length: 6 }, () => between(0, TAU));

  // Every term is periodic over the tile, so opposite edges have equal
  // heights and the contours line up across the seam.
  return (x: number, y: number) => {
    const wx = x + 0.03 * Math.sin(TAU * 2 * y + p[0]!) + 0.015 * Math.sin(TAU * 5 * y + p[1]!);
    const wy = y + 0.03 * Math.sin(TAU * 2 * x + p[2]!) + 0.015 * Math.sin(TAU * 4 * x + p[3]!);
    let h = 0;
    for (const k of hills) {
      const dx = wrap(wx - k.cx) / k.sx;
      const dy = wrap(wy - k.cy) / k.sy;
      h += k.a * Math.exp(-(dx * dx + dy * dy));
    }
    return h + 0.05 * Math.sin(TAU * x + p[4]!) * Math.cos(TAU * y + p[5]!);
  };
}

/** Every contour of the tile, one path per level, lowest first. */
function trace(height: Height, levels: number, floor = 0.06, cols = COLS, rows = ROWS): string[] {
  const w = cols + 1;
  const v = new Float64Array(w * (rows + 1));
  let min = Infinity;
  let max = -Infinity;
  for (let j = 0; j <= rows; j++) {
    for (let i = 0; i <= cols; i++) {
      const h = height(i / cols, j / rows);
      v[j * w + i] = h;
      if (h < min) min = h;
      if (h > max) max = h;
    }
  }
  const at = (i: number, j: number): number => v[j * w + i] ?? 0;

  const out: string[] = [];
  const lo = min + (max - min) * floor;
  const hi = max - (max - min) * 0.04;

  for (let l = 0; l < levels; l++) {
    const t = lo + ((hi - lo) * l) / (levels - 1);
    const pts = new Map<string, Pt>();
    const adj = new Map<string, string[]>();

    const edge = (key: string, x1: number, y1: number, v1: number, x2: number, y2: number, v2: number) => {
      if (!pts.has(key)) {
        const f = (t - v1) / (v2 - v1);
        pts.set(key, [(x1 + (x2 - x1) * f) * CELL, (y1 + (y2 - y1) * f) * CELL]);
      }
      return key;
    };
    const link = (a: string, b: string) => {
      (adj.get(a) ?? adj.set(a, []).get(a)!).push(b);
      (adj.get(b) ?? adj.set(b, []).get(b)!).push(a);
    };

    for (let j = 0; j < rows; j++) {
      for (let i = 0; i < cols; i++) {
        const a = at(i, j);
        const b = at(i + 1, j);
        const c = at(i + 1, j + 1);
        const d = at(i, j + 1);
        const idx = (a >= t ? 8 : 0) | (b >= t ? 4 : 0) | (c >= t ? 2 : 0) | (d >= t ? 1 : 0);
        if (idx === 0 || idx === 15) continue;

        const top = () => edge(`h${i},${j}`, i, j, a, i + 1, j, b);
        const right = () => edge(`v${i + 1},${j}`, i + 1, j, b, i + 1, j + 1, c);
        const bottom = () => edge(`h${i},${j + 1}`, i, j + 1, d, i + 1, j + 1, c);
        const left = () => edge(`v${i},${j}`, i, j, a, i, j + 1, d);
        const centre = (a + b + c + d) / 4 >= t;

        switch (idx) {
          case 1: case 14: link(left(), bottom()); break;
          case 2: case 13: link(bottom(), right()); break;
          case 3: case 12: link(left(), right()); break;
          case 4: case 11: link(top(), right()); break;
          case 6: case 9: link(top(), bottom()); break;
          case 7: case 8: link(left(), top()); break;
          case 5:
            if (centre) { link(left(), top()); link(bottom(), right()); }
            else { link(top(), right()); link(left(), bottom()); }
            break;
          case 10:
            if (centre) { link(top(), right()); link(left(), bottom()); }
            else { link(left(), top()); link(bottom(), right()); }
            break;
        }
      }
    }

    // Join segments into lines: open ones first (they end at the tile edge,
    // where the neighbouring tile continues them), then closed loops.
    const used = new Set<string>();
    const walk = (start: string): Pt[] => {
      const chain = [start];
      used.add(start);
      let cur = start;
      for (;;) {
        const next = (adj.get(cur) ?? []).find((k) => !used.has(k));
        if (!next) break;
        chain.push(next);
        used.add(next);
        cur = next;
      }
      if (chain.length > 2 && (adj.get(cur) ?? []).includes(start)) chain.push(start);
      return chain.map((k) => pts.get(k) ?? [0, 0]);
    };

    let d = '';
    const ends = [...adj.keys()].filter((k) => adj.get(k)?.length === 1);
    for (const k of [...ends, ...adj.keys()]) {
      if (used.has(k)) continue;
      const line = walk(k);
      if (line.length >= 2) d += smooth(line);
    }
    out.push(d);
  }
  return out;
}

/** A curve through the midpoints of a polyline, so traced steps read as terrain.
 *  Endpoints are kept exact, so lines still meet their neighbours at a seam. */
function smooth(p: Pt[]): string {
  const f = (n: number) => n.toFixed(1);
  const pt = (i: number): Pt => p[i] ?? [0, 0];
  let d = `M${f(pt(0)[0])} ${f(pt(0)[1])}`;
  for (let i = 1; i < p.length - 1; i++) {
    const [x, y] = pt(i);
    const [nx, ny] = pt(i + 1);
    d += `Q${f(x)} ${f(y)} ${f((x + nx) / 2)} ${f((y + ny) / 2)}`;
  }
  const [lx, ly] = pt(p.length - 1);
  return d + `L${f(lx)} ${f(ly)}`;
}

const cache = new Map<string, string>();

/**
 * Place as the first child of a page root that has `position: relative` and
 * `isolation: isolate`. It fills that root — its full scrolling height — and
 * sits above the root's paper and below everything else.
 */
export function TopoBackground({
  seed,
  fade,
}: {
  seed: string;
  /** Where the lines fade out, as a CSS mask — for a map confined to a band,
   *  such as the launcher's header, that should not end at a hard edge. */
  fade?: string;
}) {
  const d = useMemo(() => {
    let t = cache.get(seed);
    if (t === undefined) {
      t = trace(surface(seed), BACKGROUND_LEVELS).join('');
      cache.set(seed, t);
    }
    return t;
  }, [seed]);

  const id = `topo-${hash(seed).toString(36)}`;

  return (
    <svg
      aria-hidden="true"
      style={{
        position: 'absolute',
        inset: 0,
        width: '100%',
        height: '100%',
        zIndex: -1,
        pointerEvents: 'none',
        maskImage: fade,
        WebkitMaskImage: fade,
      }}
    >
      <defs>
        <pattern id={id} width={COLS * CELL} height={ROWS * CELL} patternUnits="userSpaceOnUse">
          <path
            d={d}
            fill="none"
            stroke="var(--contour-background)"
            strokeWidth="var(--contour-line-width)"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </pattern>
      </defs>
      <rect width="100%" height="100%" fill={`url(#${id})`} />
    </svg>
  );
}

interface Feature {
  levels: string[];
  summit: Pt;
}

const features = new Map<string, Feature>();

/** Which way the land falls away: toward the left edge, or toward the bottom. */
type Recede = 'left' | 'down';

const smoothstep = (a: number, b: number, x: number) => {
  const t = Math.min(1, Math.max(0, (x - a) / (b - a)));
  return t * t * (3 - 2 * t);
};

// How far the land reaches, how many contours it carries, and where to look
// for its summit, per direction. The falloff is long and its start wanders, so
// the lowest contours spread out and follow the hills instead of stacking into
// a straight wall along the edge.
//
// `floor` is how far up the land's height range the first contour sits: high
// enough that the map ends on the flanks of hills, not on a plain. A desktop
// window has room beside the form for more of the map, so the land there
// reaches further toward it and carries more lines.
const RECEDE = {
  left: {
    envelope: (x: number, y: number) =>
      smoothstep(0.2 + 0.06 * Math.sin(TAU * 1.3 * y + 1.1) + 0.03 * Math.sin(TAU * 3.1 * y + 0.4), 0.72, x),
    levels: 26,
    floor: 0.1,
    search: { x: [0.6, 0.88], y: [0.3, 0.7] },
    align: 'xMaxYMid slice',
  },
  down: {
    envelope: (x: number, y: number) =>
      smoothstep(0.5 + 0.06 * Math.sin(TAU * 1.2 * x + 0.7) + 0.03 * Math.sin(TAU * 2.7 * x + 2.2), 0.08, y),
    levels: 20,
    floor: 0.16,
    search: { x: [0.3, 0.7], y: [0.04, 0.22] },
    align: 'xMidYMin slice',
  },
} as const;

/**
 * The map as a picture rather than a background: the sign-in screen. Here it
 * is the thing to look at, so it is drawn in colour — every fifth line an
 * index contour in `--contour`, the rest in `--contour-line` — with the
 * marker-red summit triangle on the top of a hill, as on the brand's own hero
 * figure.
 *
 * It has no edge. The land falls away toward one side (`recede`), and below the
 * lowest contour there is simply no line to draw, so the map ends where its
 * outermost contour does — an irregular line made by the terrain, not a crop
 * and not a fade. Fills its positioned parent; lines keep their width at any
 * size.
 */
export function TopoMap({ seed, recede }: { seed: string; recede: Recede }) {
  const key = `${seed}|${recede}`;
  const { envelope, levels, floor, search, align } = RECEDE[recede];

  const f = useMemo(() => {
    let t = features.get(key);
    if (!t) {
      const ground = surface(seed);
      const height: Height = (x, y) => ground(x, y) * envelope(x, y);
      // The summit: start at the highest point in the search area — ground a
      // cropped view always shows — then climb to the top of that hill, so
      // the mark sits inside its innermost ring rather than on a slope.
      let bi = 0;
      let bj = 0;
      let top = -Infinity;
      for (let j = Math.round(ROWS * search.y[0]); j <= Math.round(ROWS * search.y[1]); j++) {
        for (let i = Math.round(COLS * search.x[0]); i <= Math.round(COLS * search.x[1]); i++) {
          const h = height(i / COLS, j / ROWS);
          if (h > top) {
            top = h;
            bi = i;
            bj = j;
          }
        }
      }
      for (let moved = true; moved; ) {
        moved = false;
        for (const [di, dj] of [[1, 0], [-1, 0], [0, 1], [0, -1], [1, 1], [-1, -1], [1, -1], [-1, 1]] as const) {
          const i = bi + di;
          const j = bj + dj;
          if (i < 0 || j < 0 || i > COLS || j > ROWS) continue;
          const h = height(i / COLS, j / ROWS);
          if (h > top) {
            top = h;
            bi = i;
            bj = j;
            moved = true;
          }
        }
      }
      t = { levels: trace(height, levels, floor), summit: [bi * CELL, bj * CELL] };
      features.set(key, t);
    }
    return t;
  }, [key, seed, envelope, levels, floor, search]);

  const [sx, sy] = f.summit;
  const s = 7;

  return (
    <svg
      aria-hidden="true"
      viewBox={`0 0 ${COLS * CELL} ${ROWS * CELL}`}
      preserveAspectRatio={align}
      style={{
        position: 'absolute',
        inset: 0,
        width: '100%',
        height: '100%',
        zIndex: -1,
        pointerEvents: 'none',
      }}
    >
      {f.levels.map((d, i) => {
        const index = i % 5 === 4;
        return (
          <path
            key={i}
            d={d}
            fill="none"
            stroke={index ? 'var(--contour)' : 'var(--contour-line)'}
            strokeWidth={index ? 'var(--contour-index-width)' : 'var(--contour-line-width)'}
            vectorEffect="non-scaling-stroke"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        );
      })}
      <polygon
        points={`${sx},${sy - s} ${sx + s},${sy + s * 0.8} ${sx - s},${sy + s * 0.8}`}
        fill="var(--marker)"
      />
    </svg>
  );
}

// The survey sheets a tile can be printed on: ground and line, from the
// palette's own terrain colors. Red is not one of them — the marker is kept
// rare, and a launcher of red squares would spend all of it at once. Nor is
// grey: on the launcher a grey tile means an app that will not open. Nor is
// paper: the picture sits on a paper card, and on the same paper it has no
// edge.
const SHEETS = [
  { ground: 'var(--vegetation)', line: 'var(--vegetation-deep)' },
  { ground: 'var(--vegetation-deep)', line: 'var(--vegetation)' },
  { ground: 'var(--status-info-tint)', line: 'var(--water)' },
  { ground: 'var(--water)', line: 'var(--status-info-tint)' },
  { ground: 'var(--status-building-tint)', line: 'var(--contour)' },
] as const;

// A small, square grid: a tile is drawn a few hundred pixels across at most,
// and there are as many of them as there are apps.
const TILE = 40;

// How far in a tile looks. The surface is a whole landscape; a tile shows a
// window a third of its width, so one or two hills fill it and their lines are
// few and large — a mark, not a map.
const ZOOM = 3;

const tiles = new Map<string, string[]>();

/** The contours and the sheet for one seed. Pure, and the same every time. */
export function tileOf(seed: string): { levels: string[]; sheet: (typeof SHEETS)[number] } {
  let levels = tiles.get(seed);
  if (!levels) {
    const ground = surface(seed);
    // Where the window sits is part of the seed too, so two apps whose
    // landscapes happen to share a hill do not share a picture of it.
    const r = rng(hash(`${seed}|window`));
    const ox = r();
    const oy = r();
    const height: Height = (x, y) => ground(ox + x / ZOOM, oy + y / ZOOM);
    // Four to six lines: enough to read as terrain, few enough to stay quiet.
    const count = 4 + pick(`${seed}|levels`, 3);
    levels = trace(height, count, 0.12, TILE, TILE);
    tiles.set(seed, levels);
  }
  return { levels, sheet: SHEETS[pick(`${seed}|sheet`, SHEETS.length)]! };
}

/**
 * A choice among n, from a string. Through the generator rather than `hash % n`:
 * FNV's low bits are poorly mixed — its lowest is the parity of the input's
 * characters — so IDs that differ in a pair of characters landed on the same
 * half of the sheets every time.
 */
function pick(s: string, n: number): number {
  return Math.floor(rng(hash(s))() * n);
}

/**
 * An app's generated picture (R-340): a patch of terrain of its own.
 *
 * The seed is the app's ID, so the same app always gets the same land and no
 * two apps get the same land — the contours are what make it unique, and the
 * sheet it is printed on varies it further. The middle line is an index
 * contour, heavier, as on the sign-in map.
 *
 * Fills its parent, which should be square.
 */
export function TopoTile({ seed }: { seed: string }) {
  const { levels, sheet } = useMemo(() => tileOf(seed), [seed]);
  const middle = Math.floor(levels.length / 2);

  return (
    <svg
      aria-hidden="true"
      viewBox={`0 0 ${TILE * CELL} ${TILE * CELL}`}
      preserveAspectRatio="xMidYMid slice"
      style={{ display: 'block', width: '100%', height: '100%', background: sheet.ground }}
    >
      {levels.map((d, i) => {
        const index = i === middle;
        return (
          <path
            key={i}
            d={d}
            fill="none"
            stroke={sheet.line}
            strokeOpacity={index ? 0.9 : 0.6}
            strokeWidth={index ? 'var(--contour-index-width)' : 'var(--contour-line-width)'}
            vectorEffect="non-scaling-stroke"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        );
      })}
    </svg>
  );
}
