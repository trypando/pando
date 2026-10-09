// An SVG turned into a PNG in the browser, for an app's launcher image (R-340).
//
// The server refuses SVG and should: an SVG is a document that can carry
// script, and the image is served from Pando's own origin, so accepting one
// would be a stored cross-site script waiting for whoever opens the launcher.
// But SVG is what most logos come as. So the console draws it the one way a
// browser guarantees is inert — as an <img>, where scripts never run and
// nothing external loads — onto a canvas, and uploads the pixels. What reaches
// the server is an ordinary PNG, and the server's rule does not change.

/** How big the PNG is: a tile is a few hundred pixels across at most. */
const RASTER_SIZE = 512;

/** Whether a picked file is an SVG, by type or, failing that, by name. */
export function isSVG(file: File): boolean {
  return file.type === 'image/svg+xml' || /\.svg$/i.test(file.name);
}

/**
 * Where to draw a w×h picture inside a size×size square: as large as fits,
 * centered, aspect kept. Zero or missing dimensions draw it square.
 */
export function fit(w: number, h: number, size: number): { x: number; y: number; w: number; h: number } {
  if (!(w > 0) || !(h > 0)) return { x: 0, y: 0, w: size, h: size };
  const scale = Math.min(size / w, size / h);
  const dw = w * scale;
  const dh = h * scale;
  return { x: (size - dw) / 2, y: (size - dh) / 2, w: dw, h: dh };
}

/** The width and height an SVG's viewBox gives, for a file that states no
 *  width or height of its own. */
export function viewBoxSize(svg: string): { w: number; h: number } | null {
  const m = /viewBox\s*=\s*["']\s*[-\d.e]+[\s,]+[-\d.e]+[\s,]+([\d.e]+)[\s,]+([\d.e]+)\s*["']/i.exec(svg);
  if (!m) return null;
  const w = Number(m[1]);
  const h = Number(m[2]);
  return w > 0 && h > 0 ? { w, h } : null;
}

/** The SVG file as a PNG file of the same name. */
export async function svgToPng(file: File): Promise<File> {
  const text = await file.text();
  const url = URL.createObjectURL(new Blob([text], { type: 'image/svg+xml' }));
  try {
    const img = await load(url);
    // The viewBox first: for an SVG that states no width or height, browsers
    // report a default 300×150 as its size, whatever its real shape.
    const box = viewBoxSize(text);
    const w = box?.w || img.naturalWidth || 0;
    const h = box?.h || img.naturalHeight || 0;

    const canvas = document.createElement('canvas');
    canvas.width = RASTER_SIZE;
    canvas.height = RASTER_SIZE;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('Your browser could not convert this SVG. Export it as a PNG and upload that.');
    const at = fit(w, h, RASTER_SIZE);
    ctx.drawImage(img, at.x, at.y, at.w, at.h);

    const png = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
    if (!png) throw new Error('Your browser could not convert this SVG. Export it as a PNG and upload that.');
    return new File([png], file.name.replace(/\.svg$/i, '') + '.png', { type: 'image/png' });
  } finally {
    URL.revokeObjectURL(url);
  }
}

function load(url: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () =>
      reject(new Error('Pando could not read this SVG. Check that it opens in a browser, or export it as a PNG and upload that.'));
    img.src = url;
  });
}
