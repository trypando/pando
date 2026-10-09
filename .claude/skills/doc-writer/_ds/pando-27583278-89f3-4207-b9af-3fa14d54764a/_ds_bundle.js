/* @ds-bundle: {"format":4,"namespace":"Pando_275832","components":[{"name":"ContourMap","sourcePath":"components/brand/ContourMap.jsx"},{"name":"Logo","sourcePath":"components/brand/Logo.jsx"},{"name":"CodeBlock","sourcePath":"components/code/CodeBlock.jsx"},{"name":"InlineCode","sourcePath":"components/code/InlineCode.jsx"},{"name":"Badge","sourcePath":"components/core/Badge.jsx"},{"name":"Button","sourcePath":"components/core/Button.jsx"},{"name":"Card","sourcePath":"components/core/Card.jsx"},{"name":"Icon","sourcePath":"components/core/Icon.jsx"},{"name":"IconButton","sourcePath":"components/core/IconButton.jsx"},{"name":"Tag","sourcePath":"components/core/Tag.jsx"},{"name":"StatusSymbol","sourcePath":"components/data/StatusIndicator.jsx"},{"name":"StatusIndicator","sourcePath":"components/data/StatusIndicator.jsx"},{"name":"Table","sourcePath":"components/data/Table.jsx"},{"name":"Banner","sourcePath":"components/feedback/Banner.jsx"},{"name":"Dialog","sourcePath":"components/feedback/Dialog.jsx"},{"name":"EmptyState","sourcePath":"components/feedback/EmptyState.jsx"},{"name":"Toast","sourcePath":"components/feedback/Toast.jsx"},{"name":"Tooltip","sourcePath":"components/feedback/Tooltip.jsx"},{"name":"Checkbox","sourcePath":"components/forms/Checkbox.jsx"},{"name":"Input","sourcePath":"components/forms/Input.jsx"},{"name":"Radio","sourcePath":"components/forms/Radio.jsx"},{"name":"Select","sourcePath":"components/forms/Select.jsx"},{"name":"Switch","sourcePath":"components/forms/Switch.jsx"},{"name":"SidebarNav","sourcePath":"components/navigation/SidebarNav.jsx"},{"name":"Tabs","sourcePath":"components/navigation/Tabs.jsx"}],"sourceHashes":{"components/brand/ContourMap.jsx":"5a5076ab5d91","components/brand/Logo.jsx":"87f450cda690","components/code/CodeBlock.jsx":"7f511165cb74","components/code/InlineCode.jsx":"e4c9c1dc6fdd","components/core/Badge.jsx":"f75d2f6706fb","components/core/Button.jsx":"35d899c2a5f9","components/core/Card.jsx":"5241c29d0246","components/core/Icon.jsx":"a01edafd277a","components/core/IconButton.jsx":"fff54cfd8661","components/core/Tag.jsx":"bb061af43ac3","components/data/StatusIndicator.jsx":"cc3bffe2375a","components/data/Table.jsx":"f36f08c41e3c","components/feedback/Banner.jsx":"27fe6608d9fc","components/feedback/Dialog.jsx":"6f23e7a8f053","components/feedback/EmptyState.jsx":"52fc64120863","components/feedback/Toast.jsx":"4eb10b24b50c","components/feedback/Tooltip.jsx":"4df3bde73969","components/forms/Checkbox.jsx":"b2f82eb47a9f","components/forms/Input.jsx":"df5938ab3629","components/forms/Radio.jsx":"faf970381c41","components/forms/Select.jsx":"d6d7bd708ebc","components/forms/Switch.jsx":"5f74eba5c56a","components/navigation/SidebarNav.jsx":"c01e2a3973b6","components/navigation/Tabs.jsx":"bc97da268886","ui_kits/console/AddApp.jsx":"9b8315fd3f97","ui_kits/console/AppDetail.jsx":"99e34991377d","ui_kits/console/Apps.jsx":"a6439416906e","ui_kits/console/ConsoleApp.jsx":"b29255eeca38","ui_kits/console/Shell.jsx":"74e26c4174c4","ui_kits/console/data.js":"d8d94746b446","ui_kits/site/Docs.jsx":"48c06da02359","ui_kits/site/Home.jsx":"4f7ea5a776c0","ui_kits/site/Nav.jsx":"d2e9bdd2ef72","ui_kits/site/SiteApp.jsx":"834e7cc311c0"},"inlinedExternals":[],"unexposedExports":[]} */

(() => {

const __ds_ns = (window.Pando_275832 = window.Pando_275832 || {});

const __ds_scope = {};

(__ds_ns.__errors = __ds_ns.__errors || []);

// components/brand/ContourMap.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/* Deterministic terrain: one shape function scaled per ring, so rings are
   irregular, roughly concentric, and can never cross. */
function shape(t) {
  return 1 + 0.10 * Math.sin(3 * t + 0.7) + 0.07 * Math.sin(5 * t + 2.1) + 0.045 * Math.sin(7 * t + 4.4) + 0.03 * Math.sin(2 * t + 1.3);
}
function ringPath(cx, cy, r, kx, ky, samples) {
  const pts = [];
  for (let i = 0; i < samples; i++) {
    const t = i / samples * Math.PI * 2;
    const rr = r * shape(t);
    pts.push([cx + Math.cos(t) * rr * kx, cy + Math.sin(t) * rr * ky]);
  }
  const mid = (a, b) => [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
  let d = 'M ' + mid(pts[pts.length - 1], pts[0]).map(n => n.toFixed(2)).join(' ');
  for (let i = 0; i < pts.length; i++) {
    const p = pts[i];
    const m = mid(p, pts[(i + 1) % pts.length]);
    d += ` Q ${p[0].toFixed(2)} ${p[1].toFixed(2)} ${m[0].toFixed(2)} ${m[1].toFixed(2)}`;
  }
  return d + ' Z';
}
function ContourMap({
  size = 520,
  rings,
  collar = false,
  summit = true,
  animate = false,
  coordinates = ['38°31′30″N', '111°45′00″W'],
  elevation = '9,140 ft',
  scaleLabels = ['0', '½', '1 mi'],
  style,
  ...rest
}) {
  const hero = size >= 320;
  const count = rings || (hero ? 8 : 4);
  const w = size;
  const h = Math.round(size * 0.72);
  const pad = collar ? 26 : 4;
  const cx = w * 0.52;
  const cy = h * 0.48;
  const spacing = hero ? (h - pad * 2) / (count * 2.6) : Math.max(6, (h - pad * 2) / (count * 2.6));
  const inner = spacing * 0.9;
  const paths = [];
  for (let i = count - 1; i >= 0; i--) {
    const r = inner + i * spacing * (1 + 0.06 * Math.sin(i * 1.7));
    const index = i % 5 === 0;
    paths.push({
      d: ringPath(cx, cy, r, 1.22, 0.86, hero ? 72 : 48),
      index,
      i
    });
  }
  const summitSize = hero ? 8 : 6;
  const dur = 900;
  return /*#__PURE__*/React.createElement("figure", _extends({
    style: {
      margin: 0,
      width: w,
      maxWidth: '100%',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("div", {
    style: {
      position: 'relative',
      border: collar ? '1px solid var(--rule-strong)' : 'none',
      padding: collar ? 'var(--space-3)' : 0
    }
  }, collar && /*#__PURE__*/React.createElement(Ticks, null), collar && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      justifyContent: 'space-between',
      font: 'var(--type-code-sm)',
      color: 'var(--ink-secondary)',
      padding: '0 var(--space-1) var(--space-2)'
    }
  }, /*#__PURE__*/React.createElement("span", null, coordinates[0]), /*#__PURE__*/React.createElement("span", null, coordinates[1])), /*#__PURE__*/React.createElement("svg", {
    width: "100%",
    viewBox: `0 0 ${w} ${h}`,
    style: {
      display: 'block',
      overflow: 'visible'
    },
    "aria-hidden": "true"
  }, paths.map(({
    d,
    index,
    i
  }) => /*#__PURE__*/React.createElement("path", {
    key: i,
    d: d,
    fill: "none",
    stroke: index ? 'var(--contour)' : 'var(--contour-line)',
    strokeWidth: index ? 'var(--contour-index-width)' : 'var(--contour-line-width)',
    pathLength: animate ? 1 : undefined,
    style: animate ? {
      '--dash': 1,
      strokeDasharray: 1,
      animation: `pando-draw ${dur}ms var(--ease) ${(count - 1 - i) * (dur / count / 2)}ms both`
    } : undefined
  })), summit && (hero ? /*#__PURE__*/React.createElement("polygon", {
    points: `${cx},${cy - summitSize * 0.62} ${cx + summitSize * 0.62},${cy + summitSize * 0.5} ${cx - summitSize * 0.62},${cy + summitSize * 0.5}`,
    fill: "var(--marker)",
    style: animate ? {
      animation: `pando-fade var(--dur-base) var(--ease) ${dur}ms both`
    } : undefined
  }) : /*#__PURE__*/React.createElement("circle", {
    cx: cx,
    cy: cy,
    r: summitSize / 2,
    fill: "var(--marker)"
  })), collar && elevation && /*#__PURE__*/React.createElement("g", null, /*#__PURE__*/React.createElement("rect", {
    x: cx + spacing * 2.2,
    y: cy - 7,
    width: elevation.length * 7.4,
    height: 14,
    fill: "var(--paper)"
  }), /*#__PURE__*/React.createElement("text", {
    x: cx + spacing * 2.4,
    y: cy + 4,
    style: {
      font: 'var(--type-code-sm)',
      fill: 'var(--contour-text)'
    }
  }, elevation))), collar && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-2)',
      paddingTop: 'var(--space-3)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex'
    }
  }, [0, 1, 2, 3].map(i => /*#__PURE__*/React.createElement("span", {
    key: i,
    style: {
      width: 12,
      height: 4,
      background: i % 2 === 0 ? 'var(--ink)' : 'var(--paper-raised)',
      border: '1px solid var(--ink)',
      borderLeft: i === 0 ? '1px solid var(--ink)' : 'none'
    }
  }))), /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, scaleLabels.join('    ')))));
}
function Ticks() {
  const t = {
    position: 'absolute',
    width: 8,
    height: 8,
    borderColor: 'var(--rule-strong)',
    borderStyle: 'solid',
    borderWidth: 0
  };
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("span", {
    style: {
      ...t,
      top: -1,
      left: -1,
      borderTopWidth: 1,
      borderLeftWidth: 1,
      transform: 'translate(-3px,-3px)'
    }
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      ...t,
      top: -1,
      right: -1,
      borderTopWidth: 1,
      borderRightWidth: 1,
      transform: 'translate(3px,-3px)'
    }
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      ...t,
      bottom: -1,
      left: -1,
      borderBottomWidth: 1,
      borderLeftWidth: 1,
      transform: 'translate(-3px,3px)'
    }
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      ...t,
      bottom: -1,
      right: -1,
      borderBottomWidth: 1,
      borderRightWidth: 1,
      transform: 'translate(3px,3px)'
    }
  }));
}
Object.assign(__ds_scope, { ContourMap });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/brand/ContourMap.jsx", error: String((e && e.message) || e) }); }

// components/brand/Logo.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/** Nested contour rings with a red summit dot, plus the lowercase wordmark. */
function Logo({
  size = 24,
  wordmark = true,
  style,
  ...rest
}) {
  const small = size < 24;
  const rings = small ? 2 : 3;
  const sw = small ? 1.25 : 1.5;
  const box = size;
  const c = box / 2;
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: size * 0.34,
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("svg", {
    width: box,
    height: box,
    viewBox: `0 0 ${box} ${box}`,
    "aria-hidden": "true",
    style: {
      display: 'block',
      flex: '0 0 auto'
    }
  }, Array.from({
    length: rings
  }).map((_, i) => {
    const r = (box / 2 - sw) * (1 - i / (rings + 0.4));
    return /*#__PURE__*/React.createElement("ellipse", {
      key: i,
      cx: c,
      cy: c,
      rx: r * 1.04,
      ry: r * 0.9,
      fill: "none",
      stroke: i === 0 ? 'var(--contour)' : 'var(--contour-line)',
      strokeWidth: i === 0 ? sw : Math.max(1, sw - 0.25)
    });
  }), /*#__PURE__*/React.createElement("circle", {
    cx: c,
    cy: c,
    r: Math.max(1.4, box * 0.075),
    fill: "var(--marker)"
  })), wordmark && /*#__PURE__*/React.createElement("span", {
    style: {
      font: `500 ${Math.round(size * 1.15)}px/1 var(--font-display)`,
      fontOpticalSizing: 'auto',
      letterSpacing: 'var(--tracking-wordmark)',
      color: 'var(--ink)'
    }
  }, "pando"));
}
Object.assign(__ds_scope, { Logo });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/brand/Logo.jsx", error: String((e && e.message) || e) }); }

// components/code/InlineCode.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function InlineCode({
  style,
  children,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("code", _extends({
    style: {
      background: 'var(--vegetation)',
      color: 'var(--ink)',
      borderRadius: 'var(--radius-xs)',
      padding: '1px 4px',
      font: 'var(--type-code)',
      ...style
    }
  }, rest), children);
}
Object.assign(__ds_scope, { InlineCode });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/code/InlineCode.jsx", error: String((e && e.message) || e) }); }

// components/core/Badge.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Badge({
  count,
  style,
  children,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      minWidth: 18,
      height: 18,
      padding: '0 5px',
      borderRadius: 'var(--radius-xs)',
      background: 'var(--paper-sunken)',
      color: 'var(--ink-secondary)',
      font: 'var(--type-code-sm)',
      ...style
    }
  }, rest), count !== undefined ? count : children);
}
Object.assign(__ds_scope, { Badge });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Badge.jsx", error: String((e && e.message) || e) }); }

// components/core/Button.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
const VARIANTS = {
  primary: {
    background: 'var(--ink)',
    color: 'var(--paper)',
    border: '1px solid var(--ink)',
    hover: {
      background: 'var(--primary-hover)',
      borderColor: 'var(--primary-hover)'
    }
  },
  secondary: {
    background: 'var(--paper-raised)',
    color: 'var(--ink)',
    border: '1px solid var(--rule-strong)',
    hover: {
      borderColor: 'var(--ink-secondary)'
    }
  },
  ghost: {
    background: 'transparent',
    color: 'var(--ink)',
    border: '1px solid transparent',
    hover: {
      background: 'var(--paper-sunken)'
    }
  },
  destructive: {
    background: 'var(--paper-raised)',
    color: 'var(--marker-deep)',
    border: '1px solid var(--marker)',
    hover: {
      background: 'var(--destructive-hover)'
    }
  }
};
function Button({
  variant = 'secondary',
  size = 'console',
  icon = null,
  disabled = false,
  fullWidth = false,
  type = 'button',
  onClick,
  style,
  children,
  ...rest
}) {
  const [hover, setHover] = useState(false);
  const [press, setPress] = useState(false);
  const v = VARIANTS[variant] || VARIANTS.secondary;
  const marketing = size === 'marketing';
  return /*#__PURE__*/React.createElement("button", _extends({
    type: type,
    disabled: disabled,
    onClick: disabled ? undefined : onClick,
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => {
      setHover(false);
      setPress(false);
    },
    onMouseDown: () => setPress(true),
    onMouseUp: () => setPress(false),
    style: {
      display: fullWidth ? 'flex' : 'inline-flex',
      width: fullWidth ? '100%' : undefined,
      alignItems: 'center',
      justifyContent: 'center',
      gap: 'var(--space-2)',
      height: marketing ? 'var(--control-marketing)' : 'var(--control-console)',
      padding: marketing ? '0 var(--button-padding-marketing)' : '0 var(--button-padding-console)',
      font: 'var(--type-label)',
      borderRadius: 'var(--radius-sm)',
      cursor: disabled ? 'not-allowed' : 'pointer',
      opacity: disabled ? 0.5 : 1,
      whiteSpace: 'nowrap',
      transform: press && !disabled ? 'scale(var(--press-scale))' : 'none',
      transition: 'background-color var(--dur-fast) var(--ease), border-color var(--dur-fast) var(--ease), color var(--dur-fast) var(--ease), transform var(--press-duration) var(--ease)',
      ...v,
      hover: undefined,
      ...(hover && !disabled ? v.hover : null),
      ...style
    }
  }, rest), icon, children);
}
Object.assign(__ds_scope, { Button });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Button.jsx", error: String((e && e.message) || e) }); }

// components/core/Card.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
const PADS = {
  none: 0,
  sm: 'var(--space-3)',
  md: 'var(--space-4)',
  lg: 'var(--space-5)'
};
function Card({
  tone = 'paper',
  padding = 'md',
  interactive = false,
  as = 'div',
  style,
  children,
  ...rest
}) {
  const [hover, setHover] = useState(false);
  const Tag = as;
  const tones = {
    paper: {
      background: 'var(--paper-raised)',
      borderColor: 'var(--rule)'
    },
    sunken: {
      background: 'var(--paper-sunken)',
      borderColor: 'var(--rule)'
    },
    plain: {
      background: 'transparent',
      borderColor: 'var(--rule)'
    }
  };
  return /*#__PURE__*/React.createElement(Tag, _extends({
    onMouseEnter: interactive ? () => setHover(true) : undefined,
    onMouseLeave: interactive ? () => setHover(false) : undefined,
    style: {
      border: '1px solid',
      borderStyle: 'solid',
      borderRadius: 'var(--radius-md)',
      padding: PADS[padding] !== undefined ? PADS[padding] : PADS.md,
      boxShadow: 'none',
      cursor: interactive ? 'pointer' : undefined,
      transition: 'border-color var(--dur-fast) var(--ease), background-color var(--dur-fast) var(--ease)',
      ...(tones[tone] || tones.paper),
      ...(interactive && hover ? {
        borderColor: 'var(--ink-secondary)'
      } : null),
      ...style
    }
  }, rest), children);
}
Object.assign(__ds_scope, { Card });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Card.jsx", error: String((e && e.message) || e) }); }

// components/core/Icon.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const CDN = 'https://unpkg.com/lucide-static@0.469.0/icons/';
const cache = {};

/** Lucide outline icon, normalized to the brand's 1.5px stroke. */
function Icon({
  name,
  size = 16,
  stroke = 1.5,
  color = 'var(--ink-secondary)',
  style,
  ...rest
}) {
  const [svg, setSvg] = React.useState(cache[name] || null);
  React.useEffect(() => {
    if (cache[name]) {
      setSvg(cache[name]);
      return;
    }
    let live = true;
    fetch(CDN + name + '.svg').then(r => r.ok ? r.text() : Promise.reject(r.status)).then(t => {
      cache[name] = t;
      if (live) setSvg(t);
    }).catch(() => {});
    return () => {
      live = false;
    };
  }, [name]);
  const markup = svg ? svg.replace(/width="24"/, 'width="100%"').replace(/height="24"/, 'height="100%"').replace(/stroke-width="[\d.]+"/, 'stroke-width="' + stroke + '"') : '';
  return /*#__PURE__*/React.createElement("span", _extends({
    "aria-hidden": "true",
    "data-icon": name,
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      flex: '0 0 auto',
      width: size,
      height: size,
      color,
      ...style
    },
    dangerouslySetInnerHTML: {
      __html: markup
    }
  }, rest));
}
Object.assign(__ds_scope, { Icon });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Icon.jsx", error: String((e && e.message) || e) }); }

// components/core/IconButton.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
function IconButton({
  label,
  size = 28,
  variant = 'ghost',
  onTerminal = false,
  disabled = false,
  onClick,
  style,
  children,
  ...rest
}) {
  const [hover, setHover] = useState(false);
  const [press, setPress] = useState(false);
  return /*#__PURE__*/React.createElement("button", _extends({
    type: "button",
    "aria-label": label,
    title: label,
    disabled: disabled,
    onClick: disabled ? undefined : onClick,
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => {
      setHover(false);
      setPress(false);
    },
    onMouseDown: () => setPress(true),
    onMouseUp: () => setPress(false),
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      width: size,
      height: size,
      padding: 0,
      borderRadius: 'var(--radius-sm)',
      border: variant === 'secondary' ? '1px solid var(--rule-strong)' : '1px solid transparent',
      background: hover && !disabled ? onTerminal ? 'rgba(236,232,222,0.10)' : 'var(--paper-sunken)' : variant === 'secondary' ? 'var(--paper-raised)' : 'transparent',
      color: onTerminal ? 'var(--terminal-text)' : 'var(--ink-secondary)',
      cursor: disabled ? 'not-allowed' : 'pointer',
      opacity: disabled ? 0.5 : 1,
      transform: press && !disabled ? 'scale(var(--press-scale))' : 'none',
      transition: 'background-color var(--dur-fast) var(--ease), transform var(--press-duration) var(--ease)',
      ...style
    }
  }, rest), children);
}
Object.assign(__ds_scope, { IconButton });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/IconButton.jsx", error: String((e && e.message) || e) }); }

// components/code/CodeBlock.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
/** Terminal surface: always ink background with terminal-text, in both themes. */
function CodeBlock({
  lines = [],
  prompt = false,
  title,
  copyable = true,
  dense = false,
  style,
  children,
  ...rest
}) {
  const [copied, setCopied] = useState(false);
  const rows = Array.isArray(lines) ? lines : String(lines).split('\n');
  const copy = () => {
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1400);
  };
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      position: 'relative',
      background: 'var(--terminal)',
      color: 'var(--terminal-text)',
      border: '1px solid var(--rule-strong)',
      borderRadius: 'var(--radius-md)',
      padding: dense ? 'var(--space-3)' : 'var(--space-4)',
      font: 'var(--type-code)',
      overflow: 'hidden',
      ...style
    }
  }, rest), title && /*#__PURE__*/React.createElement("div", {
    style: {
      font: 'var(--type-code-sm)',
      color: 'rgba(236,232,222,0.55)',
      marginBottom: 'var(--space-2)'
    }
  }, title), copyable && /*#__PURE__*/React.createElement("div", {
    style: {
      position: 'absolute',
      top: 'var(--space-2)',
      right: 'var(--space-2)',
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-2)'
    }
  }, copied && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-code-sm)',
      color: 'rgba(236,232,222,0.55)'
    }
  }, "Copied"), /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    label: "Copy",
    onTerminal: true,
    onClick: copy
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "copy",
    size: 14,
    color: "var(--terminal-text)"
  }))), children || rows.map((l, i) => {
    const line = typeof l === 'string' ? {
      text: l
    } : l;
    return /*#__PURE__*/React.createElement("div", {
      key: i,
      style: {
        display: 'flex',
        gap: 'var(--space-2)',
        whiteSpace: 'pre-wrap',
        color: line.tone === 'muted' ? 'rgba(236,232,222,0.55)' : line.tone === 'ok' ? 'var(--vegetation-deep)' : line.tone === 'fail' ? 'var(--marker)' : 'var(--terminal-text)'
      }
    }, prompt && /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--terminal-prompt)',
        flex: '0 0 auto'
      }
    }, "$"), line.time && /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'rgba(236,232,222,0.38)',
        flex: '0 0 auto'
      }
    }, line.time), /*#__PURE__*/React.createElement("span", null, line.text));
  }));
}
Object.assign(__ds_scope, { CodeBlock });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/code/CodeBlock.jsx", error: String((e && e.message) || e) }); }

// components/core/Tag.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Tag({
  mono = false,
  icon = null,
  tone = 'default',
  style,
  children,
  ...rest
}) {
  const tones = {
    default: {
      background: 'var(--paper-sunken)',
      color: 'var(--ink-secondary)',
      borderColor: 'var(--rule)'
    },
    contour: {
      background: 'transparent',
      color: 'var(--contour-text)',
      borderColor: 'var(--contour-line)'
    },
    vegetation: {
      background: 'var(--vegetation)',
      color: 'var(--ink)',
      borderColor: 'transparent'
    }
  };
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 'var(--space-1)',
      padding: '1px 6px',
      borderRadius: 'var(--radius-xs)',
      border: '1px solid',
      borderStyle: 'solid',
      font: mono ? 'var(--type-code-sm)' : 'var(--type-caption)',
      ...(tones[tone] || tones.default),
      ...style
    }
  }, rest), icon, children);
}
Object.assign(__ds_scope, { Tag });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/core/Tag.jsx", error: String((e && e.message) || e) }); }

// components/data/StatusIndicator.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const STATUS = {
  running: {
    label: 'Running',
    color: 'var(--status-running)',
    shape: 'circle'
  },
  building: {
    label: 'Building',
    color: 'var(--status-building)',
    shape: 'ring'
  },
  failed: {
    label: 'Failed',
    color: 'var(--status-failed)',
    shape: 'triangle'
  },
  stopped: {
    label: 'Stopped',
    color: 'var(--status-stopped)',
    shape: 'dash'
  },
  info: {
    label: 'Info',
    color: 'var(--status-info)',
    shape: 'circle'
  }
};
function StatusSymbol({
  status = 'running',
  size = 8,
  style
}) {
  const s = STATUS[status] || STATUS.info;
  const box = size + 2;
  return /*#__PURE__*/React.createElement("svg", {
    width: box,
    height: box,
    viewBox: `0 0 ${box} ${box}`,
    "aria-hidden": "true",
    style: {
      display: 'block',
      flex: '0 0 auto',
      ...style
    }
  }, s.shape === 'circle' && /*#__PURE__*/React.createElement("circle", {
    cx: box / 2,
    cy: box / 2,
    r: size / 2,
    fill: s.color
  }), s.shape === 'ring' && /*#__PURE__*/React.createElement("circle", {
    cx: box / 2,
    cy: box / 2,
    r: size / 2 - 0.6,
    fill: "none",
    stroke: s.color,
    strokeWidth: "1.5"
  }), s.shape === 'triangle' && /*#__PURE__*/React.createElement("polygon", {
    points: `${box / 2},${box / 2 - size / 2} ${box / 2 + size / 2},${box / 2 + size / 2.6} ${box / 2 - size / 2},${box / 2 + size / 2.6}`,
    fill: s.color
  }), s.shape === 'dash' && /*#__PURE__*/React.createElement("rect", {
    x: box / 2 - size / 2,
    y: box / 2 - 0.75,
    width: size,
    height: "1.5",
    fill: s.color
  }));
}
function StatusIndicator({
  status = 'running',
  label,
  size = 8,
  style,
  ...rest
}) {
  const s = STATUS[status] || STATUS.info;
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 'var(--space-2)',
      font: 'var(--type-body-ui)',
      color: 'var(--ink)',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement(StatusSymbol, {
    status: status,
    size: size
  }), label || s.label);
}
Object.assign(__ds_scope, { StatusSymbol, StatusIndicator });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/StatusIndicator.jsx", error: String((e && e.message) || e) }); }

// components/data/Table.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
function Table({
  columns = [],
  rows = [],
  dense = false,
  onRowClick,
  empty = null,
  style,
  ...rest
}) {
  const grid = columns.map(c => c.width || '1fr').join(' ');
  const h = dense ? 'var(--row-height-dense)' : 'var(--row-height)';
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      width: '100%',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: grid,
      alignItems: 'center',
      gap: 'var(--space-4)',
      minHeight: dense ? 28 : 32,
      padding: '0 var(--space-3)',
      background: 'var(--paper-sunken)',
      borderTop: '1px solid var(--rule)',
      borderBottom: '1px solid var(--rule)'
    }
  }, columns.map(c => /*#__PURE__*/React.createElement("span", {
    key: c.key,
    style: {
      font: 'var(--type-label)',
      color: 'var(--ink-secondary)',
      textAlign: c.align || 'left'
    }
  }, c.header))), rows.length === 0 && empty, rows.map((r, i) => /*#__PURE__*/React.createElement(Row, {
    key: r.id || i,
    row: r,
    columns: columns,
    grid: grid,
    height: h,
    onRowClick: onRowClick
  })));
}
function Row({
  row,
  columns,
  grid,
  height,
  onRowClick
}) {
  const [hover, setHover] = useState(false);
  return /*#__PURE__*/React.createElement("div", {
    onClick: onRowClick ? () => onRowClick(row) : undefined,
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    style: {
      display: 'grid',
      gridTemplateColumns: grid,
      alignItems: 'center',
      gap: 'var(--space-4)',
      minHeight: height,
      padding: '0 var(--space-3)',
      borderBottom: '1px solid var(--rule)',
      background: hover && onRowClick ? 'var(--paper-raised)' : 'transparent',
      cursor: onRowClick ? 'pointer' : undefined,
      transition: 'background-color var(--dur-fast) var(--ease)'
    }
  }, columns.map(c => /*#__PURE__*/React.createElement("div", {
    key: c.key,
    style: {
      font: c.mono ? 'var(--type-code-sm)' : 'var(--type-body-ui)',
      color: c.muted ? 'var(--ink-secondary)' : 'var(--ink)',
      textAlign: c.align || 'left',
      justifySelf: c.align === 'right' ? 'end' : undefined,
      minWidth: 0,
      overflow: 'hidden',
      textOverflow: 'ellipsis',
      whiteSpace: 'nowrap'
    }
  }, c.render ? c.render(row) : row[c.key])));
}
Object.assign(__ds_scope, { Table });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/Table.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Banner.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const TONES = {
  info: {
    bg: 'var(--status-info-tint)',
    border: 'var(--status-info)',
    status: 'info',
    color: 'var(--ink)'
  },
  running: {
    bg: 'var(--status-running-tint)',
    border: 'var(--status-running)',
    status: 'running',
    color: 'var(--ink)'
  },
  building: {
    bg: 'var(--status-building-tint)',
    border: 'var(--status-building)',
    status: 'building',
    color: 'var(--ink)'
  },
  failed: {
    bg: 'var(--status-failed-tint)',
    border: 'var(--status-failed)',
    status: 'failed',
    color: 'var(--marker-deep)'
  }
};
function Banner({
  tone = 'info',
  action,
  style,
  children,
  ...rest
}) {
  const t = TONES[tone] || TONES.info;
  return /*#__PURE__*/React.createElement("div", _extends({
    role: tone === 'failed' ? 'alert' : 'status',
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-3)',
      width: '100%',
      padding: 'var(--space-3) var(--space-4)',
      background: t.bg,
      border: '1px solid ' + t.border,
      borderRadius: 'var(--radius-sm)',
      font: 'var(--type-body-ui)',
      color: t.color,
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement(__ds_scope.StatusSymbol, {
    status: t.status,
    size: 8,
    style: {
      marginTop: 1
    }
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1,
      maxWidth: 'none'
    }
  }, children), action);
}
Object.assign(__ds_scope, { Banner });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Banner.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Dialog.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Dialog({
  open = false,
  title,
  description,
  footer,
  width = 460,
  onClose,
  style,
  children,
  ...rest
}) {
  if (!open) return null;
  return /*#__PURE__*/React.createElement("div", {
    onClick: onClose,
    style: {
      position: 'absolute',
      inset: 0,
      zIndex: 60,
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      padding: 'var(--space-6)',
      background: 'var(--scrim)'
    }
  }, /*#__PURE__*/React.createElement("div", _extends({
    role: "dialog",
    "aria-modal": "true",
    onClick: e => e.stopPropagation(),
    style: {
      width,
      maxWidth: '100%',
      background: 'var(--paper-raised)',
      border: '1px solid var(--rule)',
      borderRadius: 'var(--radius-lg)',
      boxShadow: 'var(--shadow-popover)',
      ...style
    }
  }, rest), (title || description) && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-2)',
      padding: 'var(--space-5) var(--space-5) var(--space-3)'
    }
  }, title && /*#__PURE__*/React.createElement("h3", {
    style: {
      font: 'var(--type-h3)',
      color: 'var(--ink)'
    }
  }, title), description && /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, description)), children && /*#__PURE__*/React.createElement("div", {
    style: {
      padding: '0 var(--space-5) var(--space-4)'
    }
  }, children), footer && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      justifyContent: 'flex-end',
      gap: 'var(--space-2)',
      padding: 'var(--space-4) var(--space-5)',
      borderTop: '1px solid var(--rule)'
    }
  }, footer)));
}
Object.assign(__ds_scope, { Dialog });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Dialog.jsx", error: String((e && e.message) || e) }); }

// components/feedback/EmptyState.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function EmptyState({
  heading,
  children,
  action,
  size = 120,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      textAlign: 'center',
      gap: 'var(--space-3)',
      padding: 'var(--space-7) var(--space-5)',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement(__ds_scope.ContourMap, {
    size: size,
    rings: 4,
    style: {
      marginBottom: 'var(--space-2)'
    }
  }), /*#__PURE__*/React.createElement("h3", {
    style: {
      font: 'var(--type-h3)',
      color: 'var(--ink)'
    }
  }, heading), children && /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)',
      maxWidth: '46ch'
    }
  }, children), action && /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 'var(--space-2)'
    }
  }, action));
}
Object.assign(__ds_scope, { EmptyState });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/EmptyState.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Toast.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Toast({
  status = 'info',
  action,
  onDismiss,
  style,
  children,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("div", _extends({
    role: "status",
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-3)',
      maxWidth: 420,
      padding: 'var(--space-3) var(--space-3) var(--space-3) var(--space-4)',
      background: 'var(--paper-raised)',
      border: '1px solid var(--rule-strong)',
      borderRadius: 'var(--radius-sm)',
      boxShadow: 'var(--shadow-popover)',
      font: 'var(--type-body-ui)',
      color: 'var(--ink)',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement(__ds_scope.StatusSymbol, {
    status: status,
    size: 8
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1
    }
  }, children), action, onDismiss && /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    label: "Dismiss",
    size: 24,
    onClick: onDismiss
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "x",
    size: 14
  })));
}
Object.assign(__ds_scope, { Toast });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Toast.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Tooltip.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
function Tooltip({
  content,
  side = 'top',
  style,
  children,
  ...rest
}) {
  const [show, setShow] = useState(false);
  const pos = {
    top: {
      bottom: '100%',
      left: '50%',
      transform: 'translate(-50%, -6px)'
    },
    bottom: {
      top: '100%',
      left: '50%',
      transform: 'translate(-50%, 6px)'
    },
    left: {
      right: '100%',
      top: '50%',
      transform: 'translate(-6px, -50%)'
    },
    right: {
      left: '100%',
      top: '50%',
      transform: 'translate(6px, -50%)'
    }
  }[side];
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      position: 'relative',
      display: 'inline-flex',
      ...style
    },
    onMouseEnter: () => setShow(true),
    onMouseLeave: () => setShow(false)
  }, rest), children, show && /*#__PURE__*/React.createElement("span", {
    role: "tooltip",
    style: {
      position: 'absolute',
      zIndex: 40,
      ...pos,
      padding: '4px 8px',
      whiteSpace: 'nowrap',
      background: 'var(--paper-raised)',
      color: 'var(--ink)',
      border: '1px solid var(--rule-strong)',
      borderRadius: 'var(--radius-sm)',
      boxShadow: 'var(--shadow-popover)',
      font: 'var(--type-caption)',
      pointerEvents: 'none'
    }
  }, content));
}
Object.assign(__ds_scope, { Tooltip });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Tooltip.jsx", error: String((e && e.message) || e) }); }

// components/forms/Checkbox.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Checkbox({
  label,
  description,
  checked = false,
  indeterminate = false,
  disabled = false,
  onChange,
  style,
  ...rest
}) {
  const on = checked || indeterminate;
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: 'inline-flex',
      alignItems: 'flex-start',
      gap: 'var(--space-2)',
      cursor: disabled ? 'not-allowed' : 'pointer',
      opacity: disabled ? 0.55 : 1,
      ...style
    }
  }, /*#__PURE__*/React.createElement("input", _extends({
    type: "checkbox",
    checked: checked,
    disabled: disabled,
    onChange: onChange,
    style: {
      position: 'absolute',
      opacity: 0,
      width: 0,
      height: 0
    }
  }, rest)), /*#__PURE__*/React.createElement("span", {
    "aria-hidden": "true",
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      width: 16,
      height: 16,
      marginTop: 2,
      flex: '0 0 auto',
      borderRadius: 'var(--radius-xs)',
      border: '1px solid ' + (on ? 'var(--ink)' : 'var(--field-border)'),
      background: on ? 'var(--ink)' : 'var(--paper-raised)'
    }
  }, on && /*#__PURE__*/React.createElement("svg", {
    width: "10",
    height: "10",
    viewBox: "0 0 10 10"
  }, indeterminate ? /*#__PURE__*/React.createElement("path", {
    d: "M2 5h6",
    stroke: "var(--paper)",
    strokeWidth: "1.5",
    fill: "none"
  }) : /*#__PURE__*/React.createElement("path", {
    d: "M1.5 5.2l2.3 2.3L8.5 2.8",
    stroke: "var(--paper)",
    strokeWidth: "1.5",
    fill: "none"
  }))), (label || description) && /*#__PURE__*/React.createElement("span", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 2
    }
  }, label && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink)'
    }
  }, label), description && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, description)));
}
Object.assign(__ds_scope, { Checkbox });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Checkbox.jsx", error: String((e && e.message) || e) }); }

// components/forms/Input.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
function Input({
  label,
  helper,
  error,
  mono = false,
  as = 'input',
  rows = 4,
  disabled = false,
  id,
  style,
  ...rest
}) {
  const [focus, setFocus] = useState(false);
  const inputId = id || (label ? 'f-' + label.replace(/\W+/g, '-').toLowerCase() : undefined);
  const Tag = as === 'textarea' ? 'textarea' : 'input';
  const shared = {
    width: '100%',
    height: as === 'textarea' ? undefined : 'var(--control-input)',
    padding: as === 'textarea' ? 'var(--space-2) 10px' : '0 10px',
    background: 'var(--paper-raised)',
    border: '1px solid ' + (error ? 'var(--marker)' : 'var(--field-border)'),
    borderRadius: 'var(--radius-sm)',
    font: mono ? 'var(--type-code)' : 'var(--type-body-ui)',
    color: disabled ? 'var(--ink-muted)' : 'var(--ink)',
    outline: focus ? '2px solid var(--ink)' : 'none',
    outlineOffset: focus ? 2 : 0,
    resize: as === 'textarea' ? 'vertical' : undefined
  };
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-1)',
      ...style
    }
  }, label && /*#__PURE__*/React.createElement("label", {
    htmlFor: inputId,
    style: {
      font: 'var(--type-label)',
      color: 'var(--ink)'
    }
  }, label), /*#__PURE__*/React.createElement(Tag, _extends({
    id: inputId,
    rows: as === 'textarea' ? rows : undefined,
    disabled: disabled,
    onFocus: () => setFocus(true),
    onBlur: () => setFocus(false),
    style: shared
  }, rest)), (helper || error) && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: error ? 'var(--marker-deep)' : 'var(--ink-secondary)'
    }
  }, error || helper));
}
Object.assign(__ds_scope, { Input });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Input.jsx", error: String((e && e.message) || e) }); }

// components/forms/Radio.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Radio({
  label,
  description,
  checked = false,
  disabled = false,
  name,
  value,
  onChange,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: 'inline-flex',
      alignItems: 'flex-start',
      gap: 'var(--space-2)',
      cursor: disabled ? 'not-allowed' : 'pointer',
      opacity: disabled ? 0.55 : 1,
      ...style
    }
  }, /*#__PURE__*/React.createElement("input", _extends({
    type: "radio",
    name: name,
    value: value,
    checked: checked,
    disabled: disabled,
    onChange: onChange,
    style: {
      position: 'absolute',
      opacity: 0,
      width: 0,
      height: 0
    }
  }, rest)), /*#__PURE__*/React.createElement("span", {
    "aria-hidden": "true",
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      width: 16,
      height: 16,
      marginTop: 2,
      flex: '0 0 auto',
      borderRadius: 'var(--radius-pill)',
      border: '1px solid ' + (checked ? 'var(--ink)' : 'var(--field-border)'),
      background: 'var(--paper-raised)'
    }
  }, checked && /*#__PURE__*/React.createElement("span", {
    style: {
      width: 8,
      height: 8,
      borderRadius: 'var(--radius-pill)',
      background: 'var(--ink)'
    }
  })), (label || description) && /*#__PURE__*/React.createElement("span", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 2
    }
  }, label && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink)'
    }
  }, label), description && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, description)));
}
Object.assign(__ds_scope, { Radio });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Radio.jsx", error: String((e && e.message) || e) }); }

// components/forms/Select.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
function Select({
  label,
  helper,
  options = [],
  mono = false,
  disabled = false,
  id,
  style,
  ...rest
}) {
  const [focus, setFocus] = useState(false);
  const selectId = id || (label ? 's-' + label.replace(/\W+/g, '-').toLowerCase() : undefined);
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-1)',
      ...style
    }
  }, label && /*#__PURE__*/React.createElement("label", {
    htmlFor: selectId,
    style: {
      font: 'var(--type-label)',
      color: 'var(--ink)'
    }
  }, label), /*#__PURE__*/React.createElement("div", {
    style: {
      position: 'relative',
      display: 'flex'
    }
  }, /*#__PURE__*/React.createElement("select", _extends({
    id: selectId,
    disabled: disabled,
    onFocus: () => setFocus(true),
    onBlur: () => setFocus(false),
    style: {
      appearance: 'none',
      WebkitAppearance: 'none',
      flex: 1,
      height: 'var(--control-input)',
      padding: '0 28px 0 10px',
      background: 'var(--paper-raised)',
      border: '1px solid var(--field-border)',
      borderRadius: 'var(--radius-sm)',
      font: mono ? 'var(--type-code)' : 'var(--type-body-ui)',
      color: disabled ? 'var(--ink-muted)' : 'var(--ink)',
      outline: focus ? '2px solid var(--ink)' : 'none',
      outlineOffset: focus ? 2 : 0,
      cursor: disabled ? 'not-allowed' : 'pointer'
    }
  }, rest), options.map(o => {
    const opt = typeof o === 'string' ? {
      value: o,
      label: o
    } : o;
    return /*#__PURE__*/React.createElement("option", {
      key: opt.value,
      value: opt.value
    }, opt.label);
  })), /*#__PURE__*/React.createElement("svg", {
    "aria-hidden": "true",
    width: "10",
    height: "6",
    viewBox: "0 0 10 6",
    style: {
      position: 'absolute',
      right: 10,
      top: '50%',
      marginTop: -3,
      pointerEvents: 'none'
    }
  }, /*#__PURE__*/React.createElement("path", {
    d: "M1 1l4 4 4-4",
    fill: "none",
    stroke: "var(--ink-secondary)",
    strokeWidth: "1.5"
  }))), helper && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, helper));
}
Object.assign(__ds_scope, { Select });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Select.jsx", error: String((e && e.message) || e) }); }

// components/forms/Switch.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Switch({
  checked = false,
  disabled = false,
  label,
  description,
  onChange,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("label", {
    style: {
      display: 'inline-flex',
      alignItems: 'flex-start',
      gap: 'var(--space-3)',
      cursor: disabled ? 'not-allowed' : 'pointer',
      opacity: disabled ? 0.55 : 1,
      ...style
    }
  }, /*#__PURE__*/React.createElement("input", _extends({
    type: "checkbox",
    role: "switch",
    checked: checked,
    disabled: disabled,
    onChange: onChange,
    style: {
      position: 'absolute',
      opacity: 0,
      width: 0,
      height: 0
    }
  }, rest)), /*#__PURE__*/React.createElement("span", {
    "aria-hidden": "true",
    style: {
      position: 'relative',
      flex: '0 0 auto',
      width: 34,
      height: 20,
      marginTop: 1,
      borderRadius: 'var(--radius-pill)',
      background: checked ? 'var(--ink)' : 'var(--paper-sunken)',
      border: '1px solid ' + (checked ? 'var(--ink)' : 'var(--field-border)'),
      transition: 'background-color var(--dur-fast) var(--ease), border-color var(--dur-fast) var(--ease)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      position: 'absolute',
      top: 2,
      left: checked ? 16 : 2,
      width: 14,
      height: 14,
      borderRadius: 'var(--radius-pill)',
      background: 'var(--paper-raised)',
      transition: 'left var(--dur-fast) var(--ease)'
    }
  })), (label || description) && /*#__PURE__*/React.createElement("span", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 2
    }
  }, label && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink)'
    }
  }, label), description && /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, description)));
}
Object.assign(__ds_scope, { Switch });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Switch.jsx", error: String((e && e.message) || e) }); }

// components/navigation/SidebarNav.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function SidebarNav({
  items = [],
  value,
  onChange,
  header,
  footer,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("nav", _extends({
    style: {
      width: 'var(--console-sidebar)',
      flex: '0 0 auto',
      display: 'flex',
      flexDirection: 'column',
      background: 'var(--paper)',
      borderRight: '1px solid var(--rule)',
      ...style
    }
  }, rest), header && /*#__PURE__*/React.createElement("div", {
    style: {
      padding: 'var(--space-4)'
    }
  }, header), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      padding: '0 var(--space-2)',
      gap: 2
    }
  }, items.map(it => {
    const on = it.value === value;
    return /*#__PURE__*/React.createElement("button", {
      key: it.value,
      onClick: () => onChange && onChange(it.value),
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 'var(--space-2)',
        height: 'var(--control-console)',
        padding: '0 var(--space-2)',
        border: '1px solid transparent',
        borderRadius: 'var(--radius-sm)',
        background: on ? 'var(--paper-sunken)' : 'transparent',
        color: on ? 'var(--ink)' : 'var(--ink-secondary)',
        font: on ? 'var(--type-label)' : 'var(--type-body-ui)',
        textAlign: 'left',
        cursor: 'pointer',
        transition: 'background-color var(--dur-fast) var(--ease), color var(--dur-fast) var(--ease)'
      }
    }, it.icon, /*#__PURE__*/React.createElement("span", {
      style: {
        flex: 1
      }
    }, it.label), it.trailing);
  })), footer && /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 'auto',
      padding: 'var(--space-4)',
      borderTop: '1px solid var(--rule)'
    }
  }, footer));
}
Object.assign(__ds_scope, { SidebarNav });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/navigation/SidebarNav.jsx", error: String((e && e.message) || e) }); }

// components/navigation/Tabs.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const {
  useState
} = React;
function Tabs({
  items = [],
  value,
  defaultValue,
  onChange,
  style,
  ...rest
}) {
  const [internal, setInternal] = useState(defaultValue || items[0] && items[0].value);
  const active = value !== undefined ? value : internal;
  const pick = v => {
    if (value === undefined) setInternal(v);
    if (onChange) onChange(v);
  };
  return /*#__PURE__*/React.createElement("div", _extends({
    role: "tablist",
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-5)',
      borderBottom: '1px solid var(--rule)',
      ...style
    }
  }, rest), items.map(it => {
    const on = it.value === active;
    return /*#__PURE__*/React.createElement("button", {
      key: it.value,
      role: "tab",
      "aria-selected": on,
      onClick: () => pick(it.value),
      style: {
        display: 'inline-flex',
        alignItems: 'center',
        gap: 'var(--space-2)',
        height: 36,
        padding: 0,
        marginBottom: -1,
        border: 'none',
        borderBottom: '1px solid ' + (on ? 'var(--ink)' : 'transparent'),
        background: 'transparent',
        color: on ? 'var(--ink)' : 'var(--ink-secondary)',
        font: on ? 'var(--type-label)' : 'var(--type-body-ui)',
        cursor: 'pointer',
        transition: 'color var(--dur-fast) var(--ease), border-color var(--dur-fast) var(--ease)'
      }
    }, it.label, it.trailing);
  }));
}
Object.assign(__ds_scope, { Tabs });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/navigation/Tabs.jsx", error: String((e && e.message) || e) }); }

// ui_kits/console/AddApp.jsx
try { (() => {
const {
  Dialog,
  Button,
  Input,
  Radio,
  Card,
  Tag,
  Icon,
  StatusSymbol
} = window.Pando_275832;
function RepoRow({
  repo,
  selected,
  onSelect
}) {
  const [hover, setHover] = React.useState(false);
  return /*#__PURE__*/React.createElement("div", {
    onClick: () => onSelect(repo.name),
    onMouseEnter: () => setHover(true),
    onMouseLeave: () => setHover(false),
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-3)',
      minHeight: 'var(--row-height)',
      padding: '0 var(--space-3)',
      borderBottom: '1px solid var(--rule)',
      background: selected ? 'var(--paper-sunken)' : hover ? 'var(--paper)' : 'transparent',
      cursor: 'pointer',
      transition: 'background-color var(--dur-fast) var(--ease)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      width: 10
    }
  }, selected && /*#__PURE__*/React.createElement(StatusSymbol, {
    status: "info",
    size: 8
  })), /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-code)'
    }
  }, repo.name), /*#__PURE__*/React.createElement(Tag, null, repo.language), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 'auto',
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, repo.updated));
}
function AddApp({
  open,
  repos,
  onClose,
  onDeploy
}) {
  const [repo, setRepo] = React.useState(null);
  const [when, setWhen] = React.useState('push');
  const name = repo ? repo.split('/')[1] : '';
  return /*#__PURE__*/React.createElement(Dialog, {
    open: open,
    onClose: onClose,
    width: 560,
    title: "Add app",
    description: "Pick a repo. Pando reads it, works out how to build it, and gives you a URL.",
    footer: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Button, {
      variant: "ghost",
      onClick: onClose
    }, "Cancel"), /*#__PURE__*/React.createElement(Button, {
      variant: "primary",
      disabled: !repo,
      onClick: () => onDeploy(repo, when)
    }, "Deploy"))
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)'
    }
  }, /*#__PURE__*/React.createElement(Card, {
    padding: "none",
    tone: "plain",
    style: {
      overflow: 'hidden'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-2)',
      minHeight: 32,
      padding: '0 var(--space-3)',
      background: 'var(--paper-sunken)',
      borderBottom: '1px solid var(--rule)'
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: "search",
    size: 16
  }), /*#__PURE__*/React.createElement("input", {
    placeholder: "Find a repo",
    style: {
      flex: 1,
      border: 'none',
      background: 'transparent',
      outline: 'none',
      font: 'var(--type-body-ui)',
      color: 'var(--ink)'
    }
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, "acme on GitHub")), repos.map(r => /*#__PURE__*/React.createElement(RepoRow, {
    key: r.name,
    repo: r,
    selected: repo === r.name,
    onSelect: setRepo
  }))), /*#__PURE__*/React.createElement(Input, {
    label: "App name",
    value: name,
    onChange: () => {},
    placeholder: "inventory",
    helper: name ? 'Your app will be at ' + name + '.pando.app' : 'Pando suggests a name from the repo.'
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-2)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-label)'
    }
  }, "When should Pando deploy?"), /*#__PURE__*/React.createElement(Radio, {
    name: "when",
    checked: when === 'push',
    onChange: () => setWhen('push'),
    label: "When I push",
    description: "Pando deploys every commit on main."
  }), /*#__PURE__*/React.createElement(Radio, {
    name: "when",
    checked: when === 'manual',
    onChange: () => setWhen('manual'),
    label: "Only when I ask"
  }))));
}
Object.assign(window, {
  AddApp
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/console/AddApp.jsx", error: String((e && e.message) || e) }); }

// ui_kits/console/AppDetail.jsx
try { (() => {
const {
  Tabs,
  Table,
  StatusIndicator,
  Button,
  Card,
  CodeBlock,
  InlineCode,
  Banner,
  Input,
  Select,
  Checkbox,
  Switch,
  Tag,
  Tooltip,
  Icon
} = window.Pando_275832;
function SummaryRow({
  label,
  children
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-4)',
      padding: 'var(--space-2) 0',
      borderBottom: '1px solid var(--rule)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      width: 110,
      flex: '0 0 auto',
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, label), /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink)'
    }
  }, children));
}
function Overview({
  app,
  data
}) {
  const log = app.status === 'failed' ? data.failedLog : data.log;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)'
    }
  }, app.status === 'failed' && /*#__PURE__*/React.createElement(Banner, {
    tone: "failed",
    action: /*#__PURE__*/React.createElement(Button, {
      variant: "ghost"
    }, "Open settings")
  }, "Pando couldn't find a start command. Add one in app settings."), app.status === 'building' && /*#__PURE__*/React.createElement(Banner, {
    tone: "building"
  }, "Building ", app.commit, ". The live URL keeps serving the last good deploy."), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) minmax(0,1.25fr)',
      gap: 'var(--space-6)',
      alignItems: 'start'
    }
  }, /*#__PURE__*/React.createElement(Card, {
    padding: "md"
  }, /*#__PURE__*/React.createElement(SummaryRow, {
    label: "Status"
  }, /*#__PURE__*/React.createElement(StatusIndicator, {
    status: app.status
  })), /*#__PURE__*/React.createElement(SummaryRow, {
    label: "URL"
  }, /*#__PURE__*/React.createElement("a", {
    href: "#"
  }, app.url)), /*#__PURE__*/React.createElement(SummaryRow, {
    label: "Repo"
  }, app.repo, " \xB7 ", /*#__PURE__*/React.createElement(Tag, {
    mono: true
  }, app.branch)), /*#__PURE__*/React.createElement(SummaryRow, {
    label: "Host"
  }, app.host), /*#__PURE__*/React.createElement(SummaryRow, {
    label: "Runtime"
  }, app.runtime), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-4)',
      padding: 'var(--space-2) 0'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      width: 110,
      flex: '0 0 auto',
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, "Last deploy"), /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-body-ui)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-code-sm)'
    }
  }, app.commit), " \xB7 ", app.by, " \xB7 ", /*#__PURE__*/React.createElement(Tooltip, {
    content: app.exact
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--ink-secondary)'
    }
  }, app.updated))))), /*#__PURE__*/React.createElement(CodeBlock, {
    title: 'Build log · ' + app.name + ' · ' + app.updated,
    lines: log
  })), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("h4", {
    style: {
      font: 'var(--type-h4)',
      marginBottom: 'var(--space-3)'
    }
  }, "Share it"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)',
      marginBottom: 'var(--space-3)'
    }
  }, "Anyone with the link can open the app. Run ", /*#__PURE__*/React.createElement(InlineCode, null, "pando share ", app.name), " to invite someone by email."), /*#__PURE__*/React.createElement(CodeBlock, {
    prompt: true,
    lines: ['pando share ' + app.name + ' --email teammate@acme.com']
  })));
}
function Deploys({
  data
}) {
  return /*#__PURE__*/React.createElement(Table, {
    columns: [{
      key: 'commit',
      header: 'Commit',
      width: '110px',
      mono: true
    }, {
      key: 'message',
      header: 'Message',
      width: 'minmax(0,2fr)'
    }, {
      key: 'status',
      header: 'Status',
      width: '130px',
      render: r => /*#__PURE__*/React.createElement(StatusIndicator, {
        status: r.status,
        label: r.status === 'stopped' ? 'Replaced' : undefined
      })
    }, {
      key: 'by',
      header: 'By',
      width: '90px',
      muted: true
    }, {
      key: 'when',
      header: 'When',
      width: '110px',
      muted: true
    }, {
      key: 'took',
      header: 'Took',
      width: '70px',
      align: 'right',
      muted: true,
      mono: true
    }],
    rows: data.deploys
  });
}
function Variables({
  data
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)'
    }
  }, /*#__PURE__*/React.createElement(Table, {
    columns: [{
      key: 'key',
      header: 'Name',
      width: 'minmax(0,1fr)',
      mono: true
    }, {
      key: 'value',
      header: 'Value',
      width: 'minmax(0,2fr)',
      mono: true,
      muted: true
    }, {
      key: 'where',
      header: 'Where it applies',
      width: '160px',
      muted: true
    }],
    rows: data.variables
  }), /*#__PURE__*/React.createElement(Card, {
    padding: "md",
    style: {
      maxWidth: 620
    }
  }, /*#__PURE__*/React.createElement("h4", {
    style: {
      font: 'var(--type-h4)',
      marginBottom: 'var(--space-3)'
    }
  }, "Add a variable"), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-3)',
      alignItems: 'flex-end'
    }
  }, /*#__PURE__*/React.createElement(Input, {
    label: "Name",
    mono: true,
    placeholder: "DATABASE_URL",
    style: {
      flex: 1
    }
  }), /*#__PURE__*/React.createElement(Input, {
    label: "Value",
    mono: true,
    placeholder: "postgres://\u2026",
    style: {
      flex: 1.4
    }
  }), /*#__PURE__*/React.createElement(Select, {
    label: "Where",
    options: ['Live and previews', 'Live only', 'Previews only'],
    style: {
      width: 170
    }
  }), /*#__PURE__*/React.createElement(Button, {
    variant: "primary"
  }, "Add variable"))));
}
function AppSettings({
  app,
  onDelete
}) {
  const [sleep, setSleep] = React.useState(true);
  const [push, setPush] = React.useState(true);
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-6)',
      maxWidth: 620
    }
  }, /*#__PURE__*/React.createElement("section", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-3)'
    }
  }, /*#__PURE__*/React.createElement("h4", {
    style: {
      font: 'var(--type-h4)'
    }
  }, "How Pando runs this app"), /*#__PURE__*/React.createElement(Input, {
    label: "Start command",
    mono: true,
    defaultValue: app.runtime === 'Static' ? '' : 'node server.js',
    placeholder: "node server.js",
    helper: "Pando guesses this from your repo. Change it if the guess is wrong."
  }), /*#__PURE__*/React.createElement(Input, {
    label: "Port",
    mono: true,
    defaultValue: "3000",
    placeholder: "3000"
  }), /*#__PURE__*/React.createElement(Select, {
    label: "Host",
    options: ['survey-01 · Frankfurt', 'survey-02 · Oregon']
  })), /*#__PURE__*/React.createElement("section", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-3)',
      paddingTop: 'var(--space-5)',
      borderTop: '1px solid var(--rule)'
    }
  }, /*#__PURE__*/React.createElement("h4", {
    style: {
      font: 'var(--type-h4)'
    }
  }, "When to deploy"), /*#__PURE__*/React.createElement(Checkbox, {
    checked: push,
    onChange: e => setPush(e.target.checked),
    label: "Deploy when I push to main",
    description: "Other branches get their own URL."
  }), /*#__PURE__*/React.createElement(Switch, {
    checked: sleep,
    onChange: e => setSleep(e.target.checked),
    label: "Sleep when nobody's using it",
    description: "Wakes on the next request, in about a second."
  })), /*#__PURE__*/React.createElement("section", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-3)',
      paddingTop: 'var(--space-5)',
      borderTop: '1px solid var(--rule)'
    }
  }, /*#__PURE__*/React.createElement("h4", {
    style: {
      font: 'var(--type-h4)'
    }
  }, "Delete this app"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, "The app, its URL and its history go away. This can't be undone."), /*#__PURE__*/React.createElement(Button, {
    variant: "destructive",
    onClick: onDelete,
    style: {
      alignSelf: 'flex-start'
    }
  }, "Delete app")));
}
function AppDetail({
  app,
  data,
  tab,
  onTab,
  onBack,
  onDeploy,
  onDelete
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      maxWidth: 'var(--console-max)'
    }
  }, /*#__PURE__*/React.createElement(PageHeader, {
    title: app.name,
    back: {
      label: 'Apps',
      onClick: onBack
    },
    actions: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Button, {
      onClick: () => window.open('#', '_self')
    }, "Share app"), /*#__PURE__*/React.createElement(Button, {
      variant: "primary",
      onClick: onDeploy
    }, "Deploy"))
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-3)',
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, /*#__PURE__*/React.createElement(StatusIndicator, {
    status: app.status
  }), /*#__PURE__*/React.createElement("span", null, "\xB7"), /*#__PURE__*/React.createElement("a", {
    href: "#"
  }, app.url))), /*#__PURE__*/React.createElement("div", {
    style: {
      padding: '0 var(--console-padding)'
    }
  }, /*#__PURE__*/React.createElement(Tabs, {
    value: tab,
    onChange: onTab,
    items: [{
      value: 'overview',
      label: 'Overview'
    }, {
      value: 'deploys',
      label: 'Deploys'
    }, {
      value: 'variables',
      label: 'Variables'
    }, {
      value: 'settings',
      label: 'Settings'
    }]
  })), /*#__PURE__*/React.createElement("div", {
    style: {
      padding: 'var(--space-5) var(--console-padding) var(--space-7)'
    }
  }, tab === 'overview' && /*#__PURE__*/React.createElement(Overview, {
    app: app,
    data: data
  }), tab === 'deploys' && /*#__PURE__*/React.createElement(Deploys, {
    data: data
  }), tab === 'variables' && /*#__PURE__*/React.createElement(Variables, {
    data: data
  }), tab === 'settings' && /*#__PURE__*/React.createElement(AppSettings, {
    app: app,
    onDelete: onDelete
  })));
}
Object.assign(window, {
  AppDetail
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/console/AppDetail.jsx", error: String((e && e.message) || e) }); }

// ui_kits/console/Apps.jsx
try { (() => {
const {
  Table,
  StatusIndicator,
  Tooltip,
  Button,
  EmptyState,
  Banner,
  Tag
} = window.Pando_275832;
function Apps({
  apps,
  onOpen,
  onAdd,
  showEmpty
}) {
  const failed = apps.find(a => a.status === 'failed');
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      maxWidth: 'var(--console-max)'
    }
  }, /*#__PURE__*/React.createElement(PageHeader, {
    title: "Apps",
    description: "Every app your team has pointed Pando at.",
    actions: /*#__PURE__*/React.createElement(Button, {
      variant: "primary",
      onClick: onAdd
    }, "Add app")
  }), failed && !showEmpty && /*#__PURE__*/React.createElement("div", {
    style: {
      padding: '0 var(--console-padding) var(--space-4)'
    }
  }, /*#__PURE__*/React.createElement(Banner, {
    tone: "failed",
    action: /*#__PURE__*/React.createElement(Button, {
      variant: "ghost",
      onClick: () => onOpen(failed)
    }, "View log")
  }, "Pando couldn't find a start command for ", failed.name, ". Add one in app settings.")), /*#__PURE__*/React.createElement("div", {
    style: {
      padding: '0 var(--console-padding) var(--space-7)'
    }
  }, showEmpty ? /*#__PURE__*/React.createElement(EmptyState, {
    heading: "Deploy your first app",
    action: /*#__PURE__*/React.createElement(Button, {
      variant: "primary",
      onClick: onAdd
    }, "Add app")
  }, "Point Pando at a repo and it builds, runs, and shares the app.") : /*#__PURE__*/React.createElement(Table, {
    onRowClick: onOpen,
    columns: [{
      key: 'name',
      header: 'Name',
      width: 'minmax(0,1.4fr)'
    }, {
      key: 'status',
      header: 'Status',
      width: '130px',
      render: r => /*#__PURE__*/React.createElement(StatusIndicator, {
        status: r.status
      })
    }, {
      key: 'message',
      header: 'Last deploy',
      width: 'minmax(0,2fr)',
      muted: true
    }, {
      key: 'updated',
      header: 'Updated',
      width: '120px',
      muted: true,
      render: r => /*#__PURE__*/React.createElement(Tooltip, {
        content: r.exact
      }, /*#__PURE__*/React.createElement("span", null, r.updated))
    }, {
      key: 'commit',
      header: 'Commit',
      width: '100px',
      mono: true
    }],
    rows: apps
  })));
}
Object.assign(window, {
  Apps
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/console/Apps.jsx", error: String((e && e.message) || e) }); }

// ui_kits/console/ConsoleApp.jsx
try { (() => {
const {
  Toast,
  Button
} = window.Pando_275832;
function Console() {
  const data = window.PandoData;
  const [apps, setApps] = React.useState(data.apps);
  const [nav, setNav] = React.useState('apps');
  const [open, setOpen] = React.useState(null);
  const [tab, setTab] = React.useState('overview');
  const [dialog, setDialog] = React.useState(false);
  const [dark, setDark] = React.useState(false);
  const [toast, setToast] = React.useState(null);
  const [empty, setEmpty] = React.useState(false);
  React.useEffect(() => {
    const root = document.documentElement;
    if (dark) root.setAttribute('data-theme', 'dark');else root.removeAttribute('data-theme');
  }, [dark]);
  const fire = (node, status) => {
    setToast({
      node,
      status
    });
    window.clearTimeout(window.__pandoToast);
    window.__pandoToast = window.setTimeout(() => setToast(null), 4000);
  };
  const deploy = repoName => {
    const name = repoName.split('/')[1];
    const app = {
      id: name,
      name,
      status: 'building',
      updated: 'Just now',
      exact: 'Just now',
      commit: 'f0c19de',
      message: 'First deploy',
      repo: repoName,
      branch: 'main',
      url: name + '.pando.app',
      host: 'survey-01 · Frankfurt',
      runtime: 'Node 20',
      by: 'Dana'
    };
    setApps(list => [app, ...list]);
    setEmpty(false);
    setDialog(false);
    fire('Building ' + name + '.', 'building');
    window.setTimeout(() => {
      setApps(list => list.map(a => a.id === app.id ? {
        ...a,
        status: 'running',
        updated: '1 min ago',
        message: 'First deploy'
      } : a));
      fire('Deployed ' + name + '.', 'running');
    }, 2600);
  };
  const redeploy = () => fire('Deploying ' + open.name + '.', 'building');
  const remove = () => {
    setApps(list => list.filter(a => a.id !== open.id));
    fire('Deleted ' + open.name + '.', 'info');
    setOpen(null);
  };
  const current = open && apps.find(a => a.id === open.id) ? apps.find(a => a.id === open.id) : open;
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      height: '100%',
      minHeight: 0,
      position: 'relative',
      background: 'var(--paper)'
    }
  }, /*#__PURE__*/React.createElement(ConsoleSidebar, {
    nav: nav,
    onNav: n => {
      setNav(n);
      setOpen(null);
    },
    appCount: apps.length,
    dark: dark,
    onDark: setDark
  }), /*#__PURE__*/React.createElement("main", {
    style: {
      flex: 1,
      minWidth: 0,
      overflow: 'auto',
      display: 'flex',
      flexDirection: 'column'
    }
  }, nav !== 'apps' ? /*#__PURE__*/React.createElement(NotInKit, {
    name: nav === 'audit' ? 'Audit log' : nav.charAt(0).toUpperCase() + nav.slice(1),
    onBack: () => setNav('apps')
  }) : current ? /*#__PURE__*/React.createElement(AppDetail, {
    app: current,
    data: data,
    tab: tab,
    onTab: setTab,
    onBack: () => setOpen(null),
    onDeploy: redeploy,
    onDelete: remove
  }) : /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Apps, {
    apps: apps,
    showEmpty: empty,
    onOpen: a => {
      setOpen(a);
      setTab('overview');
    },
    onAdd: () => setDialog(true)
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 'auto',
      padding: 'var(--space-4) var(--console-padding)',
      borderTop: '1px solid var(--rule)',
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-3)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, "Kit demo:"), /*#__PURE__*/React.createElement(Button, {
    variant: "ghost",
    onClick: () => setEmpty(!empty)
  }, empty ? 'Show the app list' : 'Show the empty state')))), /*#__PURE__*/React.createElement(AddApp, {
    open: dialog,
    repos: data.repos,
    onClose: () => setDialog(false),
    onDeploy: deploy
  }), toast && /*#__PURE__*/React.createElement("div", {
    style: {
      position: 'absolute',
      right: 'var(--space-5)',
      bottom: 'var(--space-5)',
      zIndex: 80
    }
  }, /*#__PURE__*/React.createElement(Toast, {
    status: toast.status,
    onDismiss: () => setToast(null)
  }, toast.node)));
}
ReactDOM.createRoot(document.getElementById('root')).render(/*#__PURE__*/React.createElement(Console, null));
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/console/ConsoleApp.jsx", error: String((e && e.message) || e) }); }

// ui_kits/console/Shell.jsx
try { (() => {
const {
  SidebarNav,
  Logo,
  Badge,
  Switch,
  Icon,
  Button
} = window.Pando_275832;
function ConsoleSidebar({
  nav,
  onNav,
  appCount,
  dark,
  onDark
}) {
  return /*#__PURE__*/React.createElement(SidebarNav, {
    value: nav,
    onChange: onNav,
    header: /*#__PURE__*/React.createElement(Logo, {
      size: 20
    }),
    items: [{
      value: 'apps',
      label: 'Apps',
      trailing: /*#__PURE__*/React.createElement(Badge, {
        count: appCount
      })
    }, {
      value: 'hosts',
      label: 'Hosts'
    }, {
      value: 'people',
      label: 'People'
    }, {
      value: 'audit',
      label: 'Audit log'
    }, {
      value: 'settings',
      label: 'Settings'
    }],
    footer: /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--space-3)'
      }
    }, /*#__PURE__*/React.createElement(Switch, {
      checked: dark,
      onChange: e => onDark(e.target.checked),
      label: "Night survey"
    }), /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 'var(--space-2)',
        font: 'var(--type-body-ui)',
        color: 'var(--ink-secondary)'
      }
    }, /*#__PURE__*/React.createElement(Icon, {
      name: "user",
      size: 16
    }), "Dana \xB7 acme"))
  });
}
function PageHeader({
  title,
  description,
  back,
  actions,
  children
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-3)',
      padding: 'var(--space-6) var(--console-padding) var(--space-4)'
    }
  }, back && /*#__PURE__*/React.createElement("button", {
    onClick: back.onClick,
    style: {
      alignSelf: 'flex-start',
      display: 'inline-flex',
      alignItems: 'center',
      gap: 'var(--space-1)',
      border: 'none',
      background: 'transparent',
      padding: 0,
      cursor: 'pointer',
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, /*#__PURE__*/React.createElement(Icon, {
    name: "arrow-left",
    size: 16
  }), back.label), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'flex-start',
      justifyContent: 'space-between',
      gap: 'var(--space-4)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-1)'
    }
  }, /*#__PURE__*/React.createElement("h3", {
    style: {
      font: 'var(--type-h3)',
      color: 'var(--ink)'
    }
  }, title), description && /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, description), children), actions && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-2)',
      flex: '0 0 auto'
    }
  }, actions)));
}
function NotInKit({
  name,
  onBack
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      justifyContent: 'center',
      gap: 'var(--space-3)',
      flex: 1,
      padding: 'var(--space-9)'
    }
  }, /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)',
      textAlign: 'center'
    }
  }, name, " isn't part of this kit. The brand spec doesn't describe it, so nothing has been invented here."), /*#__PURE__*/React.createElement(Button, {
    onClick: onBack
  }, "Back to apps"));
}
Object.assign(window, {
  ConsoleSidebar,
  PageHeader,
  NotInKit
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/console/Shell.jsx", error: String((e && e.message) || e) }); }

// ui_kits/console/data.js
try { (() => {
window.PandoData = {
  apps: [{
    id: 'inventory',
    name: 'inventory',
    status: 'running',
    updated: '2 min ago',
    exact: '12 Sep 2026, 14:02 UTC',
    commit: 'a41f9c2',
    message: 'Use a token bucket for rate limits',
    repo: 'acme/inventory',
    branch: 'main',
    url: 'inventory.pando.app',
    host: 'survey-01 · Frankfurt',
    runtime: 'Node 20',
    by: 'Dana'
  }, {
    id: 'time-off',
    name: 'time-off',
    status: 'building',
    updated: 'Just now',
    exact: '12 Sep 2026, 14:06 UTC',
    commit: '7be0d13',
    message: 'Add carry-over rules',
    repo: 'acme/time-off',
    branch: 'main',
    url: 'time-off.pando.app',
    host: 'survey-01 · Frankfurt',
    runtime: 'Node 20',
    by: 'Luis'
  }, {
    id: 'onboarding',
    name: 'onboarding',
    status: 'failed',
    updated: '1 hr ago',
    exact: '12 Sep 2026, 13:04 UTC',
    commit: 'c09e7a4',
    message: 'Move checklists to the new schema',
    repo: 'acme/onboarding',
    branch: 'main',
    url: 'onboarding.pando.app',
    host: 'survey-02 · Oregon',
    runtime: 'Python 3.12',
    by: 'Priya'
  }, {
    id: 'expenses',
    name: 'expenses',
    status: 'running',
    updated: '3 hrs ago',
    exact: '12 Sep 2026, 11:10 UTC',
    commit: '19da3f8',
    message: 'Round receipts to the cent',
    repo: 'acme/expenses',
    branch: 'main',
    url: 'expenses.pando.app',
    host: 'survey-01 · Frankfurt',
    runtime: 'Node 20',
    by: 'Dana'
  }, {
    id: 'archive',
    name: 'archive',
    status: 'stopped',
    updated: '6 days ago',
    exact: '6 Sep 2026, 09:22 UTC',
    commit: '4c1e77b',
    message: 'Freeze the 2025 records',
    repo: 'acme/archive',
    branch: 'main',
    url: 'archive.pando.app',
    host: 'survey-02 · Oregon',
    runtime: 'Static',
    by: 'Sam'
  }],
  deploys: [{
    id: 1,
    commit: 'a41f9c2',
    message: 'Use a token bucket for rate limits',
    status: 'running',
    when: '2 min ago',
    took: '18s',
    by: 'Dana'
  }, {
    id: 2,
    commit: 'd1a83b6',
    message: 'Bump the HTTP client',
    status: 'stopped',
    when: '2 hrs ago',
    took: '17s',
    by: 'Dana'
  }, {
    id: 3,
    commit: '5c7e910',
    message: 'Tune the connection pool',
    status: 'stopped',
    when: '6 hrs ago',
    took: '21s',
    by: 'Priya'
  }, {
    id: 4,
    commit: '0ab42df',
    message: 'Drop the legacy routes',
    status: 'failed',
    when: 'Yesterday',
    took: '—',
    by: 'Sam'
  }],
  log: [{
    time: '14:02:04',
    text: 'Cloning acme/inventory at a41f9c2',
    tone: 'muted'
  }, {
    time: '14:02:08',
    text: 'Detected Node 20. No config found.',
    tone: 'muted'
  }, {
    time: '14:02:09',
    text: 'Installing dependencies',
    tone: 'muted'
  }, {
    time: '14:02:18',
    text: 'Added 412 packages',
    tone: 'muted'
  }, {
    time: '14:02:19',
    text: 'Running the build',
    tone: 'muted'
  }, {
    time: '14:02:27',
    text: 'Build finished in 9.1s'
  }, {
    time: '14:02:29',
    text: 'Starting on survey-01',
    tone: 'muted'
  }, {
    time: '14:02:31',
    text: 'Listening on port 3000'
  }, {
    time: '14:02:31',
    text: 'Running at inventory.pando.app',
    tone: 'ok'
  }],
  failedLog: [{
    time: '13:04:11',
    text: 'Detected Python 3.12',
    tone: 'muted'
  }, {
    time: '13:04:26',
    text: 'Installing dependencies',
    tone: 'muted'
  }, {
    time: '13:04:48',
    text: 'No start command found',
    tone: 'fail'
  }, {
    time: '13:04:48',
    text: 'Pando couldn\'t start the app. Add a start command in app settings.',
    tone: 'fail'
  }],
  variables: [{
    key: 'DATABASE_URL',
    value: 'postgres://••••••••@db.acme.internal:5432/inventory',
    where: 'Live and previews'
  }, {
    key: 'REDIS_URL',
    value: 'redis://••••••••@cache.acme.internal:6379',
    where: 'Live and previews'
  }, {
    key: 'STRIPE_SECRET',
    value: 'sk_live_••••••••••••',
    where: 'Live only'
  }, {
    key: 'LOG_LEVEL',
    value: 'info',
    where: 'Live and previews'
  }],
  repos: [{
    name: 'acme/payments-api',
    language: 'Go',
    updated: 'Updated 2 hrs ago'
  }, {
    name: 'acme/dashboard',
    language: 'TypeScript',
    updated: 'Updated yesterday'
  }, {
    name: 'acme/ml-inference',
    language: 'Python',
    updated: 'Updated 3 days ago'
  }, {
    name: 'acme/internal-tools',
    language: 'TypeScript',
    updated: 'Updated last week'
  }]
};
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/console/data.js", error: String((e && e.message) || e) }); }

// ui_kits/site/Docs.jsx
try { (() => {
const {
  ContourMap,
  CodeBlock,
  InlineCode,
  Banner,
  Button,
  Tag,
  SidebarNav
} = window.Pando_275832;
function Docs({
  onPage
}) {
  const [section, setSection] = React.useState('quickstart');
  return /*#__PURE__*/React.createElement("div", {
    style: {
      maxWidth: 'var(--site-max)',
      margin: '0 auto',
      padding: '0 var(--space-5)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)',
      padding: 'var(--space-7) 0',
      borderBottom: '1px solid var(--rule)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) 320px',
      gap: 'var(--space-7)',
      alignItems: 'center'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-3)'
    }
  }, /*#__PURE__*/React.createElement("h1", {
    style: {
      font: 'var(--type-h1)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Docs"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-lg)',
      color: 'var(--ink-secondary)'
    }
  }, "Everything Pando does, in the order you'll need it.")), /*#__PURE__*/React.createElement(ContourMap, {
    size: 320,
    rings: 6,
    style: {
      justifySelf: 'end'
    }
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: '200px minmax(0,1fr)',
      gap: 'var(--space-7)',
      paddingTop: 'var(--space-6)'
    }
  }, /*#__PURE__*/React.createElement(SidebarNav, {
    value: section,
    onChange: setSection,
    style: {
      width: '100%',
      borderRight: 'none',
      background: 'transparent',
      position: 'sticky',
      top: 'var(--space-5)',
      alignSelf: 'start'
    },
    items: [{
      value: 'quickstart',
      label: 'Quickstart'
    }, {
      value: 'cli',
      label: 'CLI reference'
    }, {
      value: 'variables',
      label: 'Environment variables'
    }, {
      value: 'sharing',
      label: 'Sharing apps'
    }]
  }), /*#__PURE__*/React.createElement("article", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)',
      paddingBottom: 'var(--space-9)'
    }
  }, section === 'quickstart' && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Quickstart"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-docs)',
      color: 'var(--ink)'
    }
  }, "Install the CLI, log in once, and deploy from any repo directory. Pando reads the repo, decides how to build and start it, and prints a URL when the app is running."), /*#__PURE__*/React.createElement(CodeBlock, {
    prompt: true,
    lines: ['brew install pando', 'pando login', 'pando deploy ./']
  }), /*#__PURE__*/React.createElement(Banner, {
    tone: "info"
  }, "The first deploy takes about a minute while Pando warms the cache on your host."), /*#__PURE__*/React.createElement("h3", {
    style: {
      font: 'var(--type-h3)'
    }
  }, "If Pando asks a question"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-docs)'
    }
  }, "Pando only asks what it can't work out, and its questions stand on their own \u2014 paste one into the tool that wrote your app and you'll get an answer. A typical question is", /*#__PURE__*/React.createElement(InlineCode, null, "What command starts this app?"))), section === 'cli' && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "CLI reference"), [['pando deploy [path]', 'Builds and runs the repo at path. Prints the app URL.'], ['pando logs <app> --follow', 'Streams build and runtime output.'], ['pando share <app> --email', 'Invites someone to open the app.'], ['pando env set <app> KEY=value', 'Sets an environment variable and redeploys.'], ['pando hosts add', 'Installs the agent on a machine you own.']].map(([cmd, desc], i) => /*#__PURE__*/React.createElement("div", {
    key: cmd,
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) minmax(0,1.2fr)',
      gap: 'var(--space-5)',
      padding: 'var(--space-3) 0',
      borderTop: i === 0 ? '1px solid var(--rule)' : '1px solid var(--rule)'
    }
  }, /*#__PURE__*/React.createElement(InlineCode, {
    style: {
      justifySelf: 'start'
    }
  }, cmd), /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, desc)))), section === 'variables' && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Environment variables"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-docs)'
    }
  }, "Variables live with the app, not the host. Set them in the console or from the CLI; Pando redeploys so the running app picks them up."), /*#__PURE__*/React.createElement(CodeBlock, {
    prompt: true,
    lines: ['pando env set inventory LOG_LEVEL=debug', 'pando env list inventory']
  }), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-docs)'
    }
  }, "Values are write-only once saved. A variable marked ", /*#__PURE__*/React.createElement(InlineCode, null, "Live only"), " is withheld from preview deploys.")), section === 'sharing' && /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Sharing apps"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-docs)'
    }
  }, "Every running app has a URL. Share it as-is, or invite people by email so the app asks them to sign in first."), /*#__PURE__*/React.createElement(CodeBlock, {
    prompt: true,
    lines: ['pando share inventory --email teammate@acme.com']
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-2)'
    }
  }, /*#__PURE__*/React.createElement(Tag, {
    mono: true
  }, "inventory.pando.app"), /*#__PURE__*/React.createElement(Tag, null, "Live")), /*#__PURE__*/React.createElement(Button, {
    onClick: () => onPage('home'),
    style: {
      alignSelf: 'flex-start'
    }
  }, "Back to the site")))));
}
function NotFound({
  onPage
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      maxWidth: 'var(--site-max)',
      margin: '0 auto',
      padding: 'var(--space-9) var(--space-5)',
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) 320px',
      gap: 'var(--space-7)',
      alignItems: 'center'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-4)',
      alignItems: 'flex-start'
    }
  }, /*#__PURE__*/React.createElement("h1", {
    style: {
      font: 'var(--type-h1)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Nothing at this address"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-lg)',
      color: 'var(--ink-secondary)'
    }
  }, "The page you asked for isn't here. The summit is missing from this map too."), /*#__PURE__*/React.createElement(Button, {
    variant: "primary",
    size: "marketing",
    onClick: () => onPage('home')
  }, "Go to the home page")), /*#__PURE__*/React.createElement(ContourMap, {
    size: 320,
    rings: 6,
    collar: true,
    summit: false,
    elevation: "",
    style: {
      justifySelf: 'end'
    }
  }));
}
Object.assign(window, {
  Docs,
  NotFound
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/site/Docs.jsx", error: String((e && e.message) || e) }); }

// ui_kits/site/Home.jsx
try { (() => {
const {
  Button,
  ContourMap,
  CodeBlock,
  InlineCode,
  StatusIndicator,
  Table,
  Tag,
  Card
} = window.Pando_275832;
function Hero({
  onPage
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      maxWidth: 'var(--site-max)',
      margin: '0 auto',
      padding: 'var(--space-9) var(--space-5)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) minmax(0,1.1fr)',
      gap: 'var(--space-7)',
      alignItems: 'center'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)',
      alignItems: 'flex-start'
    }
  }, /*#__PURE__*/React.createElement("h1", {
    style: {
      font: 'var(--type-display)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Set up once.", /*#__PURE__*/React.createElement("br", null), "Deploy everything."), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-lg)',
      color: 'var(--ink-secondary)',
      maxWidth: '38ch'
    }
  }, "Point Pando at a repo and it builds, runs, and shares the app. No pipelines, no per-app setup."), /*#__PURE__*/React.createElement(Button, {
    variant: "primary",
    size: "marketing",
    onClick: () => onPage('docs')
  }, "Install Pando"), /*#__PURE__*/React.createElement(CodeBlock, {
    prompt: true,
    lines: ['pando deploy ./'],
    style: {
      width: '100%',
      maxWidth: 360
    }
  })), /*#__PURE__*/React.createElement(ContourMap, {
    size: 520,
    rings: 8,
    collar: true,
    animate: true,
    style: {
      justifySelf: 'end'
    }
  })));
}
function HowItWorks() {
  const steps = [['Install it once', /*#__PURE__*/React.createElement(React.Fragment, null, "One binary, one login. ", /*#__PURE__*/React.createElement(InlineCode, null, "pando login"), " connects your repos and your hosts.")], ['Point it at a repo', /*#__PURE__*/React.createElement(React.Fragment, null, "Pando reads the repo, works out how to build and start it, and asks only what it genuinely can't tell.")], ['Share the URL', /*#__PURE__*/React.createElement(React.Fragment, null, "Every app gets a URL the moment it runs. Pushes redeploy it; previews get their own URL.")]];
  return /*#__PURE__*/React.createElement(Section, null, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)',
      marginBottom: 'var(--space-6)'
    }
  }, "How it works"), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column'
    }
  }, steps.map(([title, body], i) => /*#__PURE__*/React.createElement("div", {
    key: title,
    style: {
      display: 'grid',
      gridTemplateColumns: '40px minmax(0,1fr) minmax(0,1.4fr)',
      gap: 'var(--space-5)',
      padding: 'var(--space-5) 0',
      borderTop: i === 0 ? 'none' : '1px solid var(--rule)',
      alignItems: 'baseline'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      font: 'var(--type-code-sm)',
      color: 'var(--contour-text)'
    }
  }, String(i + 1).padStart(2, '0')), /*#__PURE__*/React.createElement("h3", {
    style: {
      font: 'var(--type-h3)'
    }
  }, title), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body)',
      color: 'var(--ink-secondary)'
    }
  }, body)))));
}
function WhatYouSee() {
  return /*#__PURE__*/React.createElement(Section, null, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) minmax(0,1.3fr)',
      gap: 'var(--space-7)',
      alignItems: 'start'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-4)'
    }
  }, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "One list, every app"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body)',
      color: 'var(--ink-secondary)'
    }
  }, "The console is a table, not a dashboard. Status is a shape and a word, so you can read it at a glance or in black and white."), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-5)',
      flexWrap: 'wrap'
    }
  }, /*#__PURE__*/React.createElement(StatusIndicator, {
    status: "running"
  }), /*#__PURE__*/React.createElement(StatusIndicator, {
    status: "building"
  }), /*#__PURE__*/React.createElement(StatusIndicator, {
    status: "failed"
  }), /*#__PURE__*/React.createElement(StatusIndicator, {
    status: "stopped"
  }))), /*#__PURE__*/React.createElement(Card, {
    padding: "none",
    style: {
      overflow: 'hidden'
    }
  }, /*#__PURE__*/React.createElement(Table, {
    columns: [{
      key: 'name',
      header: 'Name',
      width: 'minmax(0,1.2fr)'
    }, {
      key: 'status',
      header: 'Status',
      width: '120px',
      render: r => /*#__PURE__*/React.createElement(StatusIndicator, {
        status: r.status
      })
    }, {
      key: 'updated',
      header: 'Updated',
      width: '110px',
      muted: true
    }, {
      key: 'commit',
      header: 'Commit',
      width: '96px',
      mono: true
    }],
    rows: [{
      id: 1,
      name: 'inventory',
      status: 'running',
      updated: '2 min ago',
      commit: 'a41f9c2'
    }, {
      id: 2,
      name: 'time-off',
      status: 'building',
      updated: 'Just now',
      commit: '7be0d13'
    }, {
      id: 3,
      name: 'onboarding',
      status: 'failed',
      updated: '1 hr ago',
      commit: 'c09e7a4'
    }]
  }))));
}
function Logs() {
  return /*#__PURE__*/React.createElement(Section, null, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1.3fr) minmax(0,1fr)',
      gap: 'var(--space-7)',
      alignItems: 'center'
    }
  }, /*#__PURE__*/React.createElement(CodeBlock, {
    title: "pando logs inventory --follow",
    lines: [{
      time: '14:02:08',
      text: 'Detected Node 20. No config found.',
      tone: 'muted'
    }, {
      time: '14:02:19',
      text: 'Running the build',
      tone: 'muted'
    }, {
      time: '14:02:27',
      text: 'Build finished in 9.1s'
    }, {
      time: '14:02:31',
      text: 'Running at inventory.pando.app',
      tone: 'ok'
    }, {
      time: '14:07:52',
      text: 'GET /items 200 · 41ms',
      tone: 'muted'
    }, {
      time: '14:07:55',
      text: 'GET /items/281 200 · 12ms',
      tone: 'muted'
    }]
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-4)'
    }
  }, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "The same log, everywhere"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body)',
      color: 'var(--ink-secondary)'
    }
  }, "Build output and runtime output are one stream, in the browser and in your terminal. ", /*#__PURE__*/React.createElement(InlineCode, null, "pando logs"), " follows it."))));
}
function Hosts() {
  return /*#__PURE__*/React.createElement(Section, null, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)',
      marginBottom: 'var(--space-5)'
    }
  }, "Your hosts, your bill"), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column'
    }
  }, [['Bring your own machine', 'Pando runs apps on hosts you already pay for — a cloud VM, a box in the office, a Raspberry Pi on a shelf.', 'survey-01 · Frankfurt'], ['One agent per host', 'Install the agent once. It picks up work, builds, runs and reports back. Nothing per app.', 'survey-02 · Oregon']].map(([title, body, tag], i) => /*#__PURE__*/React.createElement("div", {
    key: title,
    style: {
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1fr) minmax(0,1.4fr) auto',
      gap: 'var(--space-5)',
      padding: 'var(--space-5) 0',
      borderTop: i === 0 ? 'none' : '1px solid var(--rule)',
      alignItems: 'baseline'
    }
  }, /*#__PURE__*/React.createElement("h3", {
    style: {
      font: 'var(--type-h3)'
    }
  }, title), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body)',
      color: 'var(--ink-secondary)'
    }
  }, body), /*#__PURE__*/React.createElement(Tag, {
    mono: true
  }, tag)))));
}
function CallToAction({
  onPage
}) {
  return /*#__PURE__*/React.createElement(Section, null, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-5)',
      alignItems: 'flex-start'
    }
  }, /*#__PURE__*/React.createElement("h2", {
    style: {
      font: 'var(--type-h2)',
      letterSpacing: 'var(--tracking-headline)'
    }
  }, "Deploy your first app"), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-body-lg)',
      color: 'var(--ink-secondary)'
    }
  }, "Install the CLI, point it at a repo, and read the URL it prints."), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: 'var(--space-3)',
      alignItems: 'center',
      flexWrap: 'wrap'
    }
  }, /*#__PURE__*/React.createElement(Button, {
    variant: "primary",
    size: "marketing",
    onClick: () => onPage('docs')
  }, "Install Pando"), /*#__PURE__*/React.createElement(Button, {
    size: "marketing",
    onClick: () => onPage('docs')
  }, "Read the docs"))));
}
function Home({
  onPage
}) {
  return /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Hero, {
    onPage: onPage
  }), /*#__PURE__*/React.createElement(HowItWorks, null), /*#__PURE__*/React.createElement(WhatYouSee, null), /*#__PURE__*/React.createElement(Logs, null), /*#__PURE__*/React.createElement(Hosts, null), /*#__PURE__*/React.createElement(CallToAction, {
    onPage: onPage
  }));
}
Object.assign(window, {
  Home
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/site/Home.jsx", error: String((e && e.message) || e) }); }

// ui_kits/site/Nav.jsx
try { (() => {
const {
  Logo,
  Button
} = window.Pando_275832;
function SiteNav({
  page,
  onPage
}) {
  const links = [{
    value: 'docs',
    label: 'Docs'
  }, {
    value: 'changelog',
    label: 'Changelog'
  }, {
    value: 'github',
    label: 'GitHub'
  }];
  return /*#__PURE__*/React.createElement("header", {
    style: {
      borderBottom: '1px solid var(--rule)',
      background: 'var(--paper)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      maxWidth: 'var(--site-max)',
      margin: '0 auto',
      padding: '0 var(--space-5)',
      height: 64,
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-5)'
    }
  }, /*#__PURE__*/React.createElement("button", {
    onClick: () => onPage('home'),
    style: {
      border: 'none',
      background: 'transparent',
      padding: 0,
      cursor: 'pointer'
    },
    "aria-label": "Pando home"
  }, /*#__PURE__*/React.createElement(Logo, {
    size: 22
  })), /*#__PURE__*/React.createElement("nav", {
    style: {
      marginLeft: 'auto',
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-5)'
    }
  }, links.map(l => /*#__PURE__*/React.createElement("button", {
    key: l.value,
    onClick: () => onPage(l.value),
    style: {
      border: 'none',
      background: 'transparent',
      padding: 0,
      cursor: 'pointer',
      font: 'var(--type-body-ui)',
      color: page === l.value ? 'var(--ink)' : 'var(--ink-secondary)',
      textDecoration: page === l.value ? 'underline' : 'none',
      textUnderlineOffset: 4
    }
  }, l.label)), /*#__PURE__*/React.createElement(Button, {
    size: "marketing",
    variant: "primary",
    onClick: () => onPage('docs')
  }, "Install Pando"))));
}
function SiteFooter({
  onPage
}) {
  const cols = [['Product', ['How it works', 'Hosts', 'Pricing', 'Changelog']], ['Docs', ['Quickstart', 'CLI reference', 'Environment variables', 'Sharing apps']], ['Company', ['About', 'Blog', 'Status', 'Contact']]];
  return /*#__PURE__*/React.createElement("footer", {
    style: {
      borderTop: '1px solid var(--rule)',
      marginTop: 'var(--space-9)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      maxWidth: 'var(--site-max)',
      margin: '0 auto',
      padding: 'var(--space-7) var(--space-5)',
      display: 'grid',
      gridTemplateColumns: 'minmax(0,1.2fr) repeat(3, minmax(0,1fr))',
      gap: 'var(--space-5)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-3)'
    }
  }, /*#__PURE__*/React.createElement(Logo, {
    size: 22
  }), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-caption)',
      color: 'var(--ink-secondary)'
    }
  }, "Set up once. Deploy everything."), /*#__PURE__*/React.createElement("p", {
    style: {
      font: 'var(--type-code-sm)',
      color: 'var(--ink-secondary)'
    }
  }, "38\xB031\u203230\u2033N \xB7 111\xB045\u203200\u2033W")), cols.map(([title, items]) => /*#__PURE__*/React.createElement("div", {
    key: title,
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 'var(--space-2)'
    }
  }, /*#__PURE__*/React.createElement("h4", {
    style: {
      font: 'var(--type-label)',
      color: 'var(--ink)'
    }
  }, title), items.map(i => /*#__PURE__*/React.createElement("button", {
    key: i,
    onClick: () => onPage('docs'),
    style: {
      border: 'none',
      background: 'transparent',
      padding: 0,
      textAlign: 'left',
      cursor: 'pointer',
      font: 'var(--type-body-ui)',
      color: 'var(--ink-secondary)'
    }
  }, i))))));
}
function Section({
  children,
  style
}) {
  return /*#__PURE__*/React.createElement("section", {
    style: {
      borderTop: '1px solid var(--rule)',
      ...style
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      maxWidth: 'var(--site-max)',
      margin: '0 auto',
      padding: 'var(--space-9) var(--space-5)'
    }
  }, children));
}
Object.assign(window, {
  SiteNav,
  SiteFooter,
  Section
});
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/site/Nav.jsx", error: String((e && e.message) || e) }); }

// ui_kits/site/SiteApp.jsx
try { (() => {
function Site() {
  const [page, setPage] = React.useState('home');
  const go = p => {
    setPage(p === 'github' || p === 'changelog' ? 'notfound' : p);
    window.scrollTo(0, 0);
  };
  return /*#__PURE__*/React.createElement("div", {
    style: {
      minHeight: '100%',
      background: 'var(--paper)',
      display: 'flex',
      flexDirection: 'column'
    }
  }, /*#__PURE__*/React.createElement(SiteNav, {
    page: page,
    onPage: go
  }), /*#__PURE__*/React.createElement("main", {
    style: {
      flex: 1
    }
  }, page === 'home' && /*#__PURE__*/React.createElement(Home, {
    onPage: go
  }), page === 'docs' && /*#__PURE__*/React.createElement(Docs, {
    onPage: go
  }), page === 'notfound' && /*#__PURE__*/React.createElement(NotFound, {
    onPage: go
  })), /*#__PURE__*/React.createElement(SiteFooter, {
    onPage: go
  }));
}
ReactDOM.createRoot(document.getElementById('root')).render(/*#__PURE__*/React.createElement(Site, null));
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/site/SiteApp.jsx", error: String((e && e.message) || e) }); }

__ds_ns.ContourMap = __ds_scope.ContourMap;

__ds_ns.Logo = __ds_scope.Logo;

__ds_ns.CodeBlock = __ds_scope.CodeBlock;

__ds_ns.InlineCode = __ds_scope.InlineCode;

__ds_ns.Badge = __ds_scope.Badge;

__ds_ns.Button = __ds_scope.Button;

__ds_ns.Card = __ds_scope.Card;

__ds_ns.Icon = __ds_scope.Icon;

__ds_ns.IconButton = __ds_scope.IconButton;

__ds_ns.Tag = __ds_scope.Tag;

__ds_ns.StatusSymbol = __ds_scope.StatusSymbol;

__ds_ns.StatusIndicator = __ds_scope.StatusIndicator;

__ds_ns.Table = __ds_scope.Table;

__ds_ns.Banner = __ds_scope.Banner;

__ds_ns.Dialog = __ds_scope.Dialog;

__ds_ns.EmptyState = __ds_scope.EmptyState;

__ds_ns.Toast = __ds_scope.Toast;

__ds_ns.Tooltip = __ds_scope.Tooltip;

__ds_ns.Checkbox = __ds_scope.Checkbox;

__ds_ns.Input = __ds_scope.Input;

__ds_ns.Radio = __ds_scope.Radio;

__ds_ns.Select = __ds_scope.Select;

__ds_ns.Switch = __ds_scope.Switch;

__ds_ns.SidebarNav = __ds_scope.SidebarNav;

__ds_ns.Tabs = __ds_scope.Tabs;

})();
