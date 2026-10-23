import { describe, expect, it } from 'vitest';
import { renderToString } from 'react-dom/server';

import { ScoreBadge } from './ScoreBadge';
import { TopoBackground, TopoMap } from './TopoBackground';

describe('the security score badge (R-320)', () => {
  it('is a shield and a number, never "/ 100"', () => {
    const html = renderToString(<ScoreBadge score={72} />);
    expect(html).toContain('<svg');
    expect(html).toContain('>72<');
    expect(html).not.toContain('/ 100');
  });

  it('says an app has not been scanned rather than showing a number', () => {
    expect(renderToString(<ScoreBadge score={null} />)).toContain('Not scanned');
  });
});

describe('the contour background', () => {
  it('draws behind pages in the background line color, at full strength', () => {
    const html = renderToString(<TopoBackground seed="apps/" />);
    expect(html).toContain('var(--contour-background)');
    expect(html).not.toContain('opacity');
    expect(html).not.toContain('var(--contour-line)');
  });

  it('draws the sign-in map in the same color', () => {
    const html = renderToString(<TopoMap seed="sign-in" recede="down" />);
    expect(html).toContain('var(--contour-background)');
    expect(html).not.toContain('var(--contour-line)');
    expect(html).not.toContain('stroke="var(--contour)"');
  });
});
