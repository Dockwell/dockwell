/* @ds-bundle: {"format":4,"namespace":"Dockwell","components":[{"name":"Button"},{"name":"Kbd"},{"name":"StatusBadge"},{"name":"Tag"},{"name":"TextField"},{"name":"Switch"},{"name":"SegmentedControl"},{"name":"Tabs"},{"name":"ScopeBar"},{"name":"CommandBar"},{"name":"Metric"},{"name":"Sparkline"},{"name":"Table"},{"name":"FleetGrid"},{"name":"LogStream"},{"name":"PlanDiff"},{"name":"Timeline"},{"name":"Callout"},{"name":"Toast"},{"name":"Sheet"},{"name":"Icon"},{"name":"Logo"}]} */
(function () {
  'use strict';
  var React = window.React;
  var h = React.createElement;
  var Fragment = React.Fragment;
  var useState = React.useState;
  var useEffect = React.useEffect;
  var useRef = React.useRef;
  var useMemo = React.useMemo;

  /* ---------- helpers ---------- */
  function cx() {
    var out = [];
    for (var i = 0; i < arguments.length; i++) { if (arguments[i]) out.push(arguments[i]); }
    return out.join(' ');
  }
  function omit(obj, keys) {
    var out = {};
    for (var k in obj) { if (Object.prototype.hasOwnProperty.call(obj, k) && keys.indexOf(k) === -1) out[k] = obj[k]; }
    return out;
  }
  function useControlled(value, defaultValue, onChange) {
    var s = useState(defaultValue);
    var controlled = value !== undefined;
    var current = controlled ? value : s[0];
    function set(next) {
      if (!controlled) s[1](next);
      if (onChange) onChange(next);
    }
    return [current, set];
  }
  var idSeq = 0;
  function useId(prefix) {
    var r = useRef(null);
    if (r.current === null) { idSeq += 1; r.current = (prefix || 'dw') + '-' + idSeq; }
    return r.current;
  }

  /* ---------- Icon ---------- */
  var F = { fill: 'currentColor', stroke: 'none' };
  var ICONS = {
    search: [['circle', { cx: 11, cy: 11, r: 6.5 }], ['path', { d: 'M16 16l4.5 4.5' }]],
    check: [['path', { d: 'M5 12.5l4.5 4.5L19 7.5' }]],
    x: [['path', { d: 'M6.5 6.5l11 11M17.5 6.5l-11 11' }]],
    plus: [['path', { d: 'M12 5v14M5 12h14' }]],
    minus: [['path', { d: 'M5 12h14' }]],
    'chevron-right': [['path', { d: 'M9.5 6l6 6-6 6' }]],
    'chevron-down': [['path', { d: 'M6 9.5l6 6 6-6' }]],
    'chevron-up-down': [['path', { d: 'M8 9.5l4-4 4 4M8 14.5l4 4 4-4' }]],
    'arrow-right': [['path', { d: 'M5 12h14M13 6l6 6-6 6' }]],
    server: [['rect', { x: 3.5, y: 4, width: 17, height: 7, rx: 2 }], ['rect', { x: 3.5, y: 13, width: 17, height: 7, rx: 2 }], ['circle', Object.assign({ cx: 7.5, cy: 7.5, r: 1 }, F)], ['circle', Object.assign({ cx: 7.5, cy: 16.5, r: 1 }, F)]],
    service: [['path', { d: 'M12 3.2l7.8 4.4v8.8L12 20.8l-7.8-4.4V7.6z' }], ['path', { d: 'M4.4 7.7L12 12l7.6-4.3M12 12v8.6' }]],
    globe: [['circle', { cx: 12, cy: 12, r: 8.5 }], ['path', { d: 'M3.5 12h17M12 3.5c2.4 2.4 3.5 5.3 3.5 8.5s-1.1 6.1-3.5 8.5c-2.4-2.4-3.5-5.3-3.5-8.5s1.1-6.1 3.5-8.5z' }]],
    database: [['ellipse', { cx: 12, cy: 6, rx: 7, ry: 2.6 }], ['path', { d: 'M5 6v12c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6V6M5 12c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6' }]],
    terminal: [['rect', { x: 3, y: 4.5, width: 18, height: 15, rx: 2.5 }], ['path', { d: 'M7 9.5l3 2.5-3 2.5M12.5 15H17' }]],
    branch: [['circle', { cx: 6.5, cy: 6, r: 2 }], ['circle', { cx: 6.5, cy: 18, r: 2 }], ['circle', { cx: 17.5, cy: 7.5, r: 2 }], ['path', { d: 'M6.5 8v8M17.5 9.5c0 4-3 5.5-9.5 6.8' }]],
    commit: [['circle', { cx: 12, cy: 12, r: 3.2 }], ['path', { d: 'M3 12h5.8M15.2 12H21' }]],
    undo: [['path', { d: 'M8.5 5L4 9.5 8.5 14' }], ['path', { d: 'M4 9.5h10a5.5 5.5 0 0 1 0 11h-3' }]],
    clock: [['circle', { cx: 12, cy: 12, r: 8.5 }], ['path', { d: 'M12 7.5V12l3 2' }]],
    shield: [['path', { d: 'M12 3.5l7 2.8v5.2c0 4.3-2.9 7.7-7 9-4.1-1.3-7-4.7-7-9V6.3z' }]],
    lock: [['rect', { x: 5, y: 10.5, width: 14, height: 10, rx: 2 }], ['path', { d: 'M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5' }]],
    settings: [['path', { d: 'M4 7h9M17 7h3M4 17h3M11 17h9' }], ['circle', { cx: 15, cy: 7, r: 2 }], ['circle', { cx: 9, cy: 17, r: 2 }]],
    more: [['circle', Object.assign({ cx: 6, cy: 12, r: 1.4 }, F)], ['circle', Object.assign({ cx: 12, cy: 12, r: 1.4 }, F)], ['circle', Object.assign({ cx: 18, cy: 12, r: 1.4 }, F)]],
    copy: [['rect', { x: 8.5, y: 8.5, width: 12, height: 12, rx: 2 }], ['path', { d: 'M15.5 8.5V5.5a2 2 0 0 0-2-2h-8a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h3' }]],
    external: [['path', { d: 'M13.5 4H20v6.5M20 4l-9 9' }], ['path', { d: 'M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4' }]],
    play: [['path', { d: 'M8 5.5v13l10.5-6.5z' }]],
    pause: [['path', { d: 'M9 5.5v13M15 5.5v13' }]],
    stop: [['rect', { x: 6, y: 6, width: 12, height: 12, rx: 2 }]],
    deploy: [['path', { d: 'M12 15V4.5M7.5 9L12 4.5 16.5 9' }], ['path', { d: 'M4.5 14.5v3a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-3' }]],
    refresh: [['path', { d: 'M19.5 11.5a7.5 7.5 0 1 0-2.2 5.6' }], ['path', { d: 'M19.5 5v6.5H13' }]],
    activity: [['path', { d: 'M3 12h4l2.5-6.5 5 13L17 12h4' }]],
    layers: [['path', { d: 'M12 3.5l8.5 4.5L12 12.5 3.5 8z' }], ['path', { d: 'M3.5 12.5L12 17l8.5-4.5' }]],
    folder: [['path', { d: 'M3.5 7a2 2 0 0 1 2-2h4l2 2.2h7a2 2 0 0 1 2 2V17a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z' }]],
    org: [['path', { d: 'M4.5 20V7.5L12 4l7.5 3.5V20' }], ['path', { d: 'M3 20h18M9.5 20v-4.5h5V20M9 10h.01M15 10h.01' }]],
    alert: [['path', { d: 'M12 4.5l8.5 15h-17z' }], ['path', { d: 'M12 10v4M12 17h.01' }]],
    bell: [['path', { d: 'M6.5 16.5V11a5.5 5.5 0 0 1 11 0v5.5l1.5 2H5z' }], ['path', { d: 'M10 20.5h4' }]],
    key: [['circle', { cx: 8, cy: 14, r: 4 }], ['path', { d: 'M11 11l8.5-8.5M16.5 5.5l2 2' }]],
    home: [['path', { d: 'M4 10.5L12 4l8 6.5V20H4z' }], ['path', { d: 'M10 20v-5h4v5' }]],
    archive: [['rect', { x: 3.5, y: 4.5, width: 17, height: 4.5, rx: 1.5 }], ['path', { d: 'M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9M10 13h4' }]],
    cpu: [['rect', { x: 6.5, y: 6.5, width: 11, height: 11, rx: 2 }], ['path', { d: 'M9.5 3.5v3M14.5 3.5v3M9.5 17.5v3M14.5 17.5v3M3.5 9.5h3M3.5 14.5h3M17.5 9.5h3M17.5 14.5h3' }]],
    filter: [['path', { d: 'M4 6h16M7 12h10M10 18h4' }]],
    sun: [['circle', { cx: 12, cy: 12, r: 4 }], ['path', { d: 'M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4' }]],
    browser: [['rect', { x: 3, y: 4.5, width: 18, height: 15, rx: 2.5 }], ['path', { d: 'M3 9h18' }], ['circle', Object.assign({ cx: 6.5, cy: 6.75, r: 0.8 }, F)], ['circle', Object.assign({ cx: 9, cy: 6.75, r: 0.8 }, F)]],
    link: [['path', { d: 'M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1.2 1.2' }], ['path', { d: 'M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1.2-1.2' }]],
    grid: [['rect', { x: 4, y: 4, width: 6.5, height: 6.5, rx: 1.5 }], ['rect', { x: 13.5, y: 4, width: 6.5, height: 6.5, rx: 1.5 }], ['rect', { x: 4, y: 13.5, width: 6.5, height: 6.5, rx: 1.5 }], ['rect', { x: 13.5, y: 13.5, width: 6.5, height: 6.5, rx: 1.5 }]],
    users: [['circle', { cx: 9, cy: 8.5, r: 3.5 }], ['path', { d: 'M3 19.5c.6-3.2 3-5 6-5s5.4 1.8 6 5' }], ['path', { d: 'M15.5 5.2a3.5 3.5 0 0 1 0 6.6M17.5 14.8c1.8.7 3 2.3 3.5 4.7' }]],
    user: [['circle', { cx: 12, cy: 8.5, r: 3.8 }], ['path', { d: 'M4.5 20c.8-3.6 3.8-5.8 7.5-5.8s6.7 2.2 7.5 5.8' }]],
    card: [['rect', { x: 3, y: 5.5, width: 18, height: 13, rx: 2.5 }], ['path', { d: 'M3 10h18M7 15h3' }]],
    chat: [['path', { d: 'M4.5 6.5a2 2 0 0 1 2-2h11a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H11l-4.5 3.5V16.5h0a2 2 0 0 1-2-2z' }], ['path', { d: 'M8.5 9.5h7M8.5 12.5h4.5' }]],
    trash: [['path', { d: 'M4.5 7h15M9.5 7V5h5v2M6.5 7l.8 12a1.5 1.5 0 0 0 1.5 1.4h6.4a1.5 1.5 0 0 0 1.5-1.4l.8-12M10 11v5.5M14 11v5.5' }]],
    edit: [['path', { d: 'M14.5 5.5l4 4L8.5 19.5H4.5v-4z' }], ['path', { d: 'M12.5 7.5l4 4' }]],
    file: [['path', { d: 'M6.5 3.5h7l4 4v11a2 2 0 0 1-2 2h-9a2 2 0 0 1-2-2v-13a2 2 0 0 1 2-2z' }], ['path', { d: 'M13.5 3.5v4h4' }]],
    download: [['path', { d: 'M12 4.5V15M7.5 10.5L12 15l4.5-4.5' }], ['path', { d: 'M4.5 15.5v2a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-2' }]],
    eye: [['path', { d: 'M2.5 12s3.5-6.5 9.5-6.5S21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z' }], ['circle', { cx: 12, cy: 12, r: 2.8 }]],
    mail: [['rect', { x: 3, y: 5.5, width: 18, height: 13, rx: 2.5 }], ['path', { d: 'M3.5 7l8.5 6 8.5-6' }]],
    logout: [['path', { d: 'M10 4.5H6.5a2 2 0 0 0-2 2v11a2 2 0 0 0 2 2H10' }], ['path', { d: 'M14.5 8l4 4-4 4M18.5 12H9.5' }]],
    menu: [['path', { d: 'M4 7h16M4 12h16M4 17h16' }]],
    'arrow-left': [['path', { d: 'M19 12H5M11 6l-6 6 6 6' }]],
    upload: [['path', { d: 'M12 15V4.5M7.5 9L12 4.5 16.5 9' }], ['path', { d: 'M4.5 15.5v2a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-2' }]],
    gauge: [['path', { d: 'M4.2 16.5a8.5 8.5 0 1 1 15.6 0' }], ['path', { d: 'M12 12.5l3.5-3.5' }], ['circle', Object.assign({ cx: 12, cy: 12.5, r: 1.3 }, F)]],
    network: [['circle', { cx: 12, cy: 5.5, r: 2 }], ['circle', { cx: 5.5, cy: 18.5, r: 2 }], ['circle', { cx: 18.5, cy: 18.5, r: 2 }], ['path', { d: 'M12 7.5v4M12 11.5l-5.2 5.4M12 11.5l5.2 5.4' }]],
    calendar: [['rect', { x: 4, y: 5.5, width: 16, height: 14.5, rx: 2.5 }], ['path', { d: 'M4 10h16M8.5 3.5v4M15.5 3.5v4' }]],
    question: [['circle', { cx: 12, cy: 12, r: 8.5 }], ['path', { d: 'M9.6 9.5a2.5 2.5 0 0 1 4.8.8c0 1.7-2.4 2-2.4 3.7M12 17h.01' }]],
    pin: [['path', { d: 'M12 20.5s6-5.3 6-10.5a6 6 0 0 0-12 0c0 5.2 6 10.5 6 10.5z' }], ['circle', { cx: 12, cy: 10, r: 2.2 }]],
    moon: [['path', { d: 'M19.5 14.5A8 8 0 0 1 9.5 4.5a8 8 0 1 0 10 10z' }]]
  };
  function Icon(p) {
    var size = p.size || 16;
    var parts = ICONS[p.name] || ICONS.more;
    return h('svg', {
      className: cx('dw-icon', p.className), width: size, height: size, viewBox: '0 0 24 24',
      fill: 'none', stroke: 'currentColor', strokeWidth: p.strokeWidth || 1.5,
      strokeLinecap: 'round', strokeLinejoin: 'round', focusable: 'false',
      role: p.label ? 'img' : undefined, 'aria-label': p.label || undefined, 'aria-hidden': p.label ? undefined : true
    }, parts.map(function (part, i) { return h(part[0], Object.assign({ key: i }, part[1])); }));
  }
  Icon.names = Object.keys(ICONS);

  /* ---------- status glyphs (shape + colour, never colour alone) ---------- */
  var STATUS = {
    healthy: { label: 'Healthy', tone: 'healthy', shape: 'circle' },
    degraded: { label: 'Degraded', tone: 'warning', shape: 'triangle' },
    down: { label: 'Down', tone: 'critical', shape: 'square' },
    deploying: { label: 'Deploying', tone: 'cobalt', shape: 'spinner' },
    stopped: { label: 'Stopped', tone: 'idle', shape: 'ring' },
    running: { label: 'Running', tone: 'running', shape: 'dotted-ring' },
    unknown: { label: 'Unknown', tone: 'unknown', shape: 'dashed-ring' }
  };
  var TONE_SHAPE = { healthy: 'circle', warning: 'triangle', critical: 'square', cobalt: 'spinner', idle: 'ring', running: 'dotted-ring', unknown: 'dashed-ring', info: 'info' };
  function Glyph(p) {
    var s = p.size || 10;
    var shape = p.shape || 'circle';
    var kids;
    if (shape === 'circle') kids = h('circle', { cx: 5, cy: 5, r: 4, fill: 'currentColor' });
    else if (shape === 'triangle') kids = h('path', { d: 'M5 .9l4.4 7.9H.6z', fill: 'currentColor' });
    else if (shape === 'square') kids = h('rect', { x: 1.2, y: 1.2, width: 7.6, height: 7.6, rx: 1.6, fill: 'currentColor' });
    else if (shape === 'ring') kids = h('circle', { cx: 5, cy: 5, r: 3.6, fill: 'none', stroke: 'currentColor', strokeWidth: 1.6 });
    else if (shape === 'dotted-ring') kids = h(Fragment, null,
      h('circle', { cx: 5, cy: 5, r: 3.6, fill: 'none', stroke: 'currentColor', strokeWidth: 1.6 }),
      h('circle', { cx: 5, cy: 5, r: 1.5, fill: 'currentColor' }));
    else if (shape === 'dashed-ring') kids = h(Fragment, null,
      h('circle', { cx: 5, cy: 5, r: 3.6, fill: 'none', stroke: 'currentColor', strokeWidth: 1.4, strokeDasharray: '1.9 1.87', transform: 'rotate(-45 5 5)' }),
      h('rect', { x: 3.1, y: 4.3, width: 3.8, height: 1.4, rx: 0.7, fill: 'currentColor' }));
    else if (shape === 'info') kids = h('circle', { cx: 5, cy: 5, r: 4, fill: 'currentColor' });
    else kids = h(Fragment, null,
      h('circle', { cx: 5, cy: 5, r: 3.6, fill: 'none', stroke: 'currentColor', strokeOpacity: 0.28, strokeWidth: 1.6 }),
      h('g', { className: 'dw-spin' }, h('path', { d: 'M5 1.4A3.6 3.6 0 0 1 8.6 5', fill: 'none', stroke: 'currentColor', strokeWidth: 1.6, strokeLinecap: 'round' })));
    return h('svg', { className: cx('dw-glyph', p.className), width: s, height: s, viewBox: '0 0 10 10', 'aria-hidden': true, focusable: 'false' }, kids);
  }

  /* ---------- Kbd ---------- */
  function Kbd(p) {
    return h('kbd', { className: cx('dw-kbd', p.className) }, p.children);
  }

  /* ---------- Button ---------- */
  var Button = React.forwardRef(function Button(p, ref) {
    var variant = p.variant || 'secondary';
    var size = p.size || 'md';
    var rest = omit(p, ['variant', 'size', 'icon', 'iconAfter', 'kbd', 'loading', 'className', 'children', 'type']);
    var iconOnly = p.children == null || p.children === false;
    return h('button', Object.assign({}, rest, {
      ref: ref,
      type: p.type || 'button',
      className: cx('dw-btn', 'dw-btn-' + variant, 'dw-btn-' + size, iconOnly && 'dw-btn-icon', p.loading && 'is-loading', p.className),
      disabled: p.disabled || p.loading,
      'aria-busy': p.loading || undefined
    }),
      p.loading ? h(Glyph, { shape: 'spinner', size: size === 'sm' ? 12 : 14, className: 'dw-btn-spinner' })
        : (p.icon ? h(Icon, { name: p.icon, size: size === 'sm' ? 14 : 16 }) : null),
      iconOnly ? null : h('span', { className: 'dw-btn-label' }, p.children),
      p.iconAfter ? h(Icon, { name: p.iconAfter, size: 14 }) : null,
      p.kbd ? h(Kbd, null, p.kbd) : null);
  });

  /* ---------- StatusBadge ---------- */
  function StatusBadge(p) {
    var st = STATUS[p.status] || STATUS.stopped;
    var label = p.label !== undefined ? p.label : st.label;
    var variant = p.variant || 'dot';
    return h('span', { className: cx('dw-status', 'dw-status-' + variant, 'dw-tone-' + st.tone, p.className), title: p.title },
      h(Glyph, { shape: st.shape, size: variant === 'pill' ? 8 : 10 }),
      label ? h('span', { className: 'dw-status-label' }, label) : h('span', { className: 'dw-sr' }, st.label),
      p.meta ? h('span', { className: 'dw-status-meta' }, p.meta) : null);
  }

  /* ---------- Tag ---------- */
  function Tag(p) {
    return h('span', { className: cx('dw-tag', p.tone && 'dw-tag-' + p.tone, p.className) },
      p.icon ? h(Icon, { name: p.icon, size: 12 }) : null,
      p.label != null ? h('span', { className: 'dw-tag-key' }, p.label + (p.value != null ? ':' : '')) : null,
      p.value != null ? h('span', { className: 'dw-tag-value' }, p.value) : p.children,
      p.onRemove ? h('button', { type: 'button', className: 'dw-tag-x', 'aria-label': 'Remove ' + (p.label || '') + (p.value != null ? ':' + p.value : ''), onClick: p.onRemove }, h(Icon, { name: 'x', size: 12 })) : null);
  }

  /* ---------- TextField ---------- */
  function TextField(p) {
    var id = useId('field');
    var msgId = id + '-msg';
    var inputProps = omit(p, ['label', 'hint', 'error', 'prefix', 'suffix', 'icon', 'mono', 'optional', 'className', 'size', 'action']);
    var hasMsg = !!(p.error || p.hint);
    return h('div', { className: cx('dw-field', p.error && 'is-invalid', p.className) },
      p.label ? h('label', { className: 'dw-field-label', htmlFor: id }, h('span', null, p.label), p.optional ? h('span', { className: 'dw-field-opt' }, 'Optional') : null) : null,
      h('div', { className: cx('dw-input', 'dw-input-' + (p.size || 'md'), p.icon && 'has-icon') },
        p.icon ? h(Icon, { name: p.icon, size: 16, className: 'dw-input-icon' }) : null,
        p.prefix ? h('span', { className: 'dw-input-affix dw-input-prefix' }, p.prefix) : null,
        h('input', Object.assign({ type: 'text' }, inputProps, {
          id: id,
          className: cx('dw-input-el', p.mono && 'is-mono'),
          'aria-invalid': p.error ? true : undefined,
          'aria-describedby': hasMsg ? msgId : undefined,
          spellCheck: p.mono ? false : inputProps.spellCheck
        })),
        p.suffix ? h('span', { className: 'dw-input-affix dw-input-suffix' }, p.suffix) : null,
        p.action || null),
      hasMsg ? h('div', { id: msgId, className: p.error ? 'dw-field-error' : 'dw-field-hint' },
        p.error ? h(Glyph, { shape: 'square', size: 9 }) : null,
        h('span', null, p.error || p.hint)) : null);
  }

  /* ---------- Switch ---------- */
  function Switch(p) {
    var st = useControlled(p.checked, !!p.defaultChecked, p.onChange);
    var id = useId('switch');
    return h('div', { className: cx('dw-switch', p.disabled && 'is-disabled', p.className) },
      h('button', {
        id: id, type: 'button', role: 'switch', 'aria-checked': !!st[0], disabled: p.disabled,
        'aria-describedby': p.description ? id + '-d' : undefined,
        className: 'dw-switch-track', onClick: function () { st[1](!st[0]); }
      }, h('span', { className: 'dw-switch-thumb' })),
      (p.label || p.description) ? h('div', { className: 'dw-switch-text' },
        p.label ? h('label', { className: 'dw-switch-label', htmlFor: id }, p.label) : null,
        p.description ? h('span', { id: id + '-d', className: 'dw-switch-desc' }, p.description) : null) : null);
  }

  /* ---------- roving keyboard focus for radio-like groups ---------- */
  function rovingKeys(e, values, current, set) {
    var keys = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 };
    if (!(e.key in keys) && e.key !== 'Home' && e.key !== 'End') return;
    e.preventDefault();
    var i = values.indexOf(current);
    var n = e.key === 'Home' ? 0 : e.key === 'End' ? values.length - 1 : (i + keys[e.key] + values.length) % values.length;
    set(values[n]);
    var btns = e.currentTarget.querySelectorAll('[data-value]');
    if (btns[n]) btns[n].focus();
  }

  /* ---------- SegmentedControl ---------- */
  function SegmentedControl(p) {
    var opts = p.options || [];
    var st = useControlled(p.value, p.defaultValue !== undefined ? p.defaultValue : (opts[0] && opts[0].value), p.onChange);
    var values = opts.map(function (o) { return o.value; });
    return h('div', {
      className: cx('dw-seg', 'dw-seg-' + (p.size || 'md'), p.className), role: 'radiogroup', 'aria-label': p.label,
      onKeyDown: function (e) { rovingKeys(e, values, st[0], st[1]); }
    }, opts.map(function (o) {
      var on = o.value === st[0];
      return h('button', {
        key: o.value, type: 'button', role: 'radio', 'aria-checked': on, tabIndex: on ? 0 : -1, 'data-value': o.value,
        className: cx('dw-seg-item', on && 'is-on'), onClick: function () { st[1](o.value); }
      }, o.icon ? h(Icon, { name: o.icon, size: 14 }) : null, h('span', null, o.label),
        o.count != null ? h('span', { className: 'dw-seg-count' }, o.count) : null);
    }));
  }

  /* ---------- Tabs ---------- */
  function Tabs(p) {
    var tabs = p.tabs || [];
    var st = useControlled(p.value, p.defaultValue !== undefined ? p.defaultValue : (tabs[0] && tabs[0].id), p.onChange);
    var values = tabs.map(function (t) { return t.id; });
    return h('div', {
      className: cx('dw-tabs', p.className), role: 'tablist', 'aria-label': p.label,
      onKeyDown: function (e) { rovingKeys(e, values, st[0], st[1]); }
    }, tabs.map(function (t) {
      var on = t.id === st[0];
      return h('button', {
        key: t.id, type: 'button', role: 'tab', 'aria-selected': on, tabIndex: on ? 0 : -1, 'data-value': t.id,
        className: 'dw-tab', onClick: function () { st[1](t.id); }
      }, t.icon ? h(Icon, { name: t.icon, size: 15 }) : null, h('span', null, t.label),
        t.count != null ? h('span', { className: cx('dw-tab-count', t.countTone && 'dw-tone-' + t.countTone) }, t.count) : null);
    }));
  }

  /* ---------- ScopeBar ---------- */
  function ScopeBar(p) {
    var parts = [
      { key: 'org', icon: 'org', label: p.org },
      { key: 'project', icon: 'folder', label: p.project },
      { key: 'environment', icon: 'layers', label: p.environment }
    ].filter(function (x) { return x.label; });
    return h('nav', { className: cx('dw-scope', p.className), 'aria-label': 'Scope' },
      parts.map(function (x, i) {
        var last = i === parts.length - 1;
        return h(Fragment, { key: x.key },
          i > 0 ? h('span', { className: 'dw-scope-sep', 'aria-hidden': true }, '/') : null,
          h('button', {
            type: 'button', className: cx('dw-scope-item', last && 'is-current'), 'aria-current': last ? 'page' : undefined,
            onClick: p.onSelect ? function () { p.onSelect(x.key); } : undefined
          },
            h(Icon, { name: x.icon, size: 15 }),
            h('span', null, x.label),
            x.key === 'environment' && p.protected ? h(Icon, { name: 'lock', size: 12, label: 'Protected environment', className: 'dw-scope-lock' }) : null,
            h(Icon, { name: 'chevron-up-down', size: 12, className: 'dw-scope-caret' })));
      }));
  }

  /* ---------- CommandBar ---------- */
  function fuzzy(query, text) {
    var q = query.toLowerCase().replace(/\s+/g, '');
    if (!q) return { score: 0, idx: [] };
    var t = text.toLowerCase();
    var idx = [], j = 0, score = 0, last = -2;
    for (var i = 0; i < t.length && j < q.length; i++) {
      if (t[i] === q[j]) {
        idx.push(i);
        score += i === last + 1 ? 3 : 1;
        if (i === 0 || /[\s\-_/.]/.test(t[i - 1])) score += 2;
        last = i; j++;
      }
    }
    return j === q.length ? { score: score - t.length * 0.01, idx: idx } : null;
  }
  function Highlight(p) {
    if (!p.idx || !p.idx.length) return h('span', { className: 'dw-cmd-label' }, p.text);
    var out = [], set = {}, k = 0;
    p.idx.forEach(function (i) { set[i] = true; });
    var buf = '', inMark = false;
    function flush(mark) { if (!buf) return; out.push(mark ? h('mark', { key: k++ }, buf) : h(Fragment, { key: k++ }, buf)); buf = ''; }
    for (var i = 0; i < p.text.length; i++) {
      var m = !!set[i];
      if (m !== inMark) { flush(inMark); inMark = m; }
      buf += p.text[i];
    }
    flush(inMark);
    return h('span', { className: 'dw-cmd-label' }, out);
  }
  function CommandBar(p) {
    var qs = useState(p.defaultQuery || '');
    var q = qs[0], setQ = qs[1];
    var as = useState(0);
    var active = as[0], setActive = as[1];
    var inputRef = useRef(null);
    var listRef = useRef(null);
    var items = p.items || [];
    var results = useMemo(function () {
      if (!q.trim()) return items.map(function (it) { return { item: it, idx: [] }; });
      return items.map(function (it) { var m = fuzzy(q, it.label); return m ? { item: it, idx: m.idx, score: m.score } : null; })
        .filter(Boolean).sort(function (a, b) { return b.score - a.score; });
    }, [q, items]);
    useEffect(function () { setActive(0); }, [q]);
    useEffect(function () { if (p.autoFocus !== false && inputRef.current && !p.inline) inputRef.current.focus(); }, []);
    useEffect(function () {
      var el = listRef.current && listRef.current.querySelector('[data-active="true"]');
      if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
    }, [active]);
    function run(r) { if (r && p.onSelect) p.onSelect(r.item); }
    function onKey(e) {
      if (e.key === 'ArrowDown') { e.preventDefault(); setActive(Math.min(results.length - 1, active + 1)); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); setActive(Math.max(0, active - 1)); }
      else if (e.key === 'Enter') { e.preventDefault(); run(results[active]); }
      else if (e.key === 'Escape') { e.preventDefault(); if (p.onClose) p.onClose(); }
    }
    var listId = useId('cmd');
    var rows = [], lastGroup = null;
    results.forEach(function (r, i) {
      var g = q.trim() ? null : r.item.group;
      if (g && g !== lastGroup) { rows.push(h('div', { key: 'g-' + g, className: 'dw-cmd-group', role: 'presentation' }, g)); lastGroup = g; }
      rows.push(h('div', {
        key: r.item.id, id: listId + '-' + i, role: 'option', 'aria-selected': i === active, 'data-active': i === active,
        className: cx('dw-cmd-item', i === active && 'is-active'),
        onMouseMove: function () { if (active !== i) setActive(i); }, onClick: function () { run(r); }
      },
        h(Icon, { name: r.item.icon || 'arrow-right', size: 16 }),
        h(Highlight, { text: r.item.label, idx: r.idx }),
        r.item.hint ? h('span', { className: 'dw-cmd-hint' }, r.item.hint) : null,
        r.item.kbd ? h(Kbd, null, r.item.kbd) : null));
    });
    var panel = h('div', { className: cx('dw-cmd', p.inline && 'is-inline', p.className), role: 'dialog', 'aria-label': 'Command bar' },
      h('div', { className: 'dw-cmd-input-row' },
        h(Icon, { name: 'search', size: 18 }),
        h('input', {
          ref: inputRef, className: 'dw-cmd-input', value: q, placeholder: p.placeholder || 'Search or run a command',
          onChange: function (e) { setQ(e.target.value); }, onKeyDown: onKey,
          role: 'combobox', 'aria-expanded': true, 'aria-controls': listId, 'aria-activedescendant': results.length ? listId + '-' + active : undefined
        }),
        h(Kbd, null, 'esc')),
      h('div', { ref: listRef, id: listId, className: 'dw-cmd-list', role: 'listbox' },
        rows.length ? rows : h('div', { className: 'dw-cmd-empty' }, 'Nothing matches “' + q + '”. Try a server, app or action name.')),
      h('div', { className: 'dw-cmd-foot' },
        h('span', null, h(Kbd, null, '↑'), h(Kbd, null, '↓'), ' to move'),
        h('span', null, h(Kbd, null, '↵'), ' to run'),
        p.scope ? h('span', { className: 'dw-cmd-scope' }, 'In ', h('b', null, p.scope)) : null));
    if (p.inline) return panel;
    if (!p.open) return null;
    return h('div', { className: 'dw-cmd-layer' }, h('div', { className: 'dw-scrim', onClick: p.onClose }), panel);
  }

  /* ---------- Sparkline ---------- */
  function Sparkline(p) {
    var d = (p.data || []).slice();
    if (d.length < 2) d = [0, 0];
    var W = 100, H = p.height || 32, pad = 2;
    var lo = p.min != null ? p.min : Math.min.apply(null, d);
    var hi = p.max != null ? p.max : Math.max.apply(null, d);
    if (p.threshold != null) { lo = Math.min(lo, p.threshold); hi = Math.max(hi, p.threshold); }
    if (hi === lo) { hi += 1; lo -= 1; }
    function y(v) { return pad + (1 - (v - lo) / (hi - lo)) * (H - pad * 2); }
    var pts = d.map(function (v, i) { return (i / (d.length - 1) * W).toFixed(2) + ' ' + y(v).toFixed(2); });
    var line = 'M' + pts.join('L');
    var area = line + 'L' + W + ' ' + H + 'L0 ' + H + 'Z';
    var last = d[d.length - 1];
    return h('svg', {
      className: cx('dw-spark', 'dw-tone-' + (p.tone || 'ink'), p.className), viewBox: '0 0 ' + W + ' ' + H, preserveAspectRatio: 'none',
      width: p.width || 120, height: H, role: 'img', 'aria-label': p.label || ('Trend over ' + d.length + ' samples, latest ' + last)
    },
      p.area === false ? null : h('path', { d: area, className: 'dw-spark-area' }),
      p.threshold != null ? h('line', { x1: 0, x2: W, y1: y(p.threshold), y2: y(p.threshold), className: 'dw-spark-threshold', vectorEffect: 'non-scaling-stroke' }) : null,
      h('path', { d: line, className: 'dw-spark-line', vectorEffect: 'non-scaling-stroke' }));
  }

  /* ---------- Metric ---------- */
  function Metric(p) {
    return h('div', { className: cx('dw-metric', p.className) },
      h('div', { className: 'dw-metric-label' }, p.icon ? h(Icon, { name: p.icon, size: 14 }) : null, h('span', null, p.label)),
      h('div', { className: 'dw-metric-row' },
        h('span', { className: 'dw-metric-value' }, p.value),
        p.unit ? h('span', { className: 'dw-metric-unit' }, p.unit) : null,
        p.delta ? h('span', { className: cx('dw-metric-delta', p.deltaTone && 'dw-tone-' + p.deltaTone) }, p.delta) : null),
      p.data ? h(Sparkline, { data: p.data, tone: p.tone, threshold: p.threshold, width: '100%', height: 36, min: p.min, max: p.max, label: p.label + ' trend' }) : null,
      p.caption ? h('div', { className: 'dw-metric-caption' }, p.caption) : null);
  }

  /* ---------- Table ---------- */
  function Table(p) {
    var cols = p.columns || [];
    var rows = p.rows || [];
    var key = p.rowKey || 'id';
    var clickable = !!p.onRowClick;
    return h('div', { className: cx('dw-table-wrap', p.className), style: p.maxHeight ? { maxHeight: p.maxHeight } : undefined },
      h('table', { className: cx('dw-table', 'dw-table-' + (p.density || 'comfortable')) },
        p.caption ? h('caption', { className: 'dw-sr' }, p.caption) : null,
        h('thead', null, h('tr', null, cols.map(function (c) {
          return h('th', { key: c.key, scope: 'col', className: cx(c.align === 'right' && 'is-num'), style: c.width ? { width: c.width } : undefined }, c.header);
        }))),
        h('tbody', null, rows.length ? rows.map(function (r) {
          var sel = p.selectedKey != null && p.selectedKey === r[key];
          return h('tr', {
            key: r[key], className: cx(clickable && 'is-clickable', sel && 'is-selected'), 'aria-selected': clickable ? sel : undefined,
            tabIndex: clickable ? 0 : undefined,
            onClick: clickable ? function () { p.onRowClick(r); } : undefined,
            onKeyDown: clickable ? function (e) { if (e.key === 'Enter') p.onRowClick(r); } : undefined
          }, cols.map(function (c) {
            return h('td', { key: c.key, className: cx(c.mono && 'is-mono', c.align === 'right' && 'is-num', c.muted && 'is-muted') }, c.render ? c.render(r) : r[c.key]);
          }));
        }) : h('tr', null, h('td', { colSpan: cols.length, className: 'dw-table-empty' }, p.empty || 'Nothing here yet.')))));
  }

  /* ---------- FleetGrid ---------- */
  var ORDER = ['healthy', 'degraded', 'down', 'deploying', 'stopped'];
  function FleetGrid(p) {
    var groups = p.groups || [{ label: null, servers: p.servers || p.nodes || [] }];
    function members(g) { return g.servers || g.nodes || []; }
    var all = [];
    groups.forEach(function (g) { all = all.concat(members(g)); });
    var counts = {};
    all.forEach(function (n) { counts[n.status] = (counts[n.status] || 0) + 1; });
    var hs = useState(null);
    var hover = hs[0], setHover = hs[1];
    var shown = hover || (p.selectedId != null ? all.filter(function (n) { return n.id === p.selectedId; })[0] : null);
    function tile(n) {
      var st = STATUS[n.status] || STATUS.stopped;
      return h('button', {
        key: n.id, type: 'button', role: 'listitem',
        className: cx('dw-tile', 'dw-tile-' + n.status, p.selectedId === n.id && 'is-selected'),
        'aria-label': n.name + ', ' + st.label + (n.cpu != null ? ', CPU ' + n.cpu + '%' : ''),
        onMouseEnter: function () { setHover(n); }, onMouseLeave: function () { setHover(null); },
        onFocus: function () { setHover(n); }, onBlur: function () { setHover(null); },
        onClick: p.onSelect ? function () { p.onSelect(n); } : undefined
      }, n.status === 'healthy' ? null : h(Glyph, { shape: st.shape, size: 8 }));
    }
    return h('div', { className: cx('dw-fleet', 'dw-fleet-' + (p.size || 'md'), p.className) },
      p.legend === false ? null : h('div', { className: 'dw-fleet-legend' },
        h('span', { className: 'dw-fleet-total' }, all.length + ' servers'),
        ORDER.filter(function (s) { return counts[s]; }).map(function (s) {
          return h(StatusBadge, { key: s, status: s, label: counts[s] + ' ' + STATUS[s].label.toLowerCase() });
        })),
      h('div', { className: 'dw-fleet-groups' }, groups.map(function (g, gi) {
        return h('div', { key: gi, className: 'dw-fleet-group' },
          g.label ? h('div', { className: 'dw-fleet-glabel' }, h('span', null, g.label), h('span', { className: 'dw-fleet-gmeta' }, g.meta || (members(g).length + ' servers'))) : null,
          h('div', { className: 'dw-fleet-tiles', role: 'list', 'aria-label': g.label || 'Servers' }, members(g).map(tile)));
      })),
      p.readout === false ? null : h('div', { className: 'dw-fleet-readout', 'aria-live': 'polite' }, shown ? h(Fragment, null,
        h(StatusBadge, { status: shown.status, label: shown.name }),
        h('span', null, STATUS[shown.status].label),
        shown.cpu != null ? h('span', null, 'CPU ', h('b', null, shown.cpu + '%')) : null,
        shown.mem != null ? h('span', null, 'Memory ', h('b', null, shown.mem + '%')) : null,
        shown.ip ? h('code', null, shown.ip) : null,
        shown.note ? h('span', null, shown.note) : null
      ) : h('span', null, p.hint || 'Hover or focus a server to inspect it.')));
  }

  /* ---------- LogStream ---------- */
  var LEVEL_SHAPE = { warn: 'triangle', error: 'square' };
  function LogStream(p) {
    var lines = p.lines || [];
    var f = useControlled(p.follow, true, p.onFollowChange);
    var follow = f[0], setFollow = f[1];
    var body = useRef(null);
    useEffect(function () {
      if (follow && body.current) body.current.scrollTop = body.current.scrollHeight;
    }, [lines.length, follow]);
    function onScroll() {
      var el = body.current;
      if (!el) return;
      var atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 8;
      if (!atBottom && follow) setFollow(false);
    }
    return h('div', { className: cx('dw-log', p.className) },
      h('div', { className: 'dw-log-head' },
        h('div', { className: 'dw-log-title' },
          p.live ? h('span', { className: cx('dw-live', !follow && 'is-paused') }, h('span', { className: 'dw-live-dot' }), follow ? 'Live' : 'Paused') : null,
          h('span', null, p.title)),
        h('div', { className: 'dw-log-tools' }, p.toolbar || null,
          h(Button, { size: 'sm', variant: 'ghost', icon: follow ? 'pause' : 'play', onClick: function () { setFollow(!follow); } }, follow ? 'Pause' : 'Follow'))),
      h('div', {
        ref: body, className: 'dw-log-body', style: { height: p.height || 240 }, onScroll: onScroll,
        role: 'log', 'aria-live': follow ? 'polite' : 'off', tabIndex: 0
      }, lines.map(function (l, i) {
        var lvl = l.level || 'info';
        return h('div', { key: l.id != null ? l.id : i, className: cx('dw-log-line', 'is-' + lvl) },
          h('span', { className: 'dw-log-time' }, l.time),
          h('span', { className: 'dw-log-src', title: l.source }, l.source),
          h('span', { className: 'dw-log-lvl' }, LEVEL_SHAPE[lvl] ? h(Glyph, { shape: LEVEL_SHAPE[lvl], size: 8 }) : null, h('span', { className: 'dw-sr' }, lvl)),
          h('span', { className: 'dw-log-text' }, l.text));
      })));
  }

  /* ---------- PlanDiff ---------- */
  var ACTIONS = {
    create: { sym: '+', tone: 'add', label: 'Create' },
    update: { sym: '~', tone: 'change', label: 'Update in place' },
    replace: { sym: '±', tone: 'change', label: 'Replace' },
    destroy: { sym: '−', tone: 'remove', label: 'Destroy' }
  };
  var KIND_ICON = { service: 'service', domain: 'globe', secret: 'key', database: 'database', job: 'clock', volume: 'archive', backup: 'archive', package: 'layers', node: 'server', server: 'server', app: 'service', firewall: 'shield' };
  function renderValue(v, cls) {
    if (v && typeof v === 'object' && v.secret) return h('span', { className: 'dw-diff-secret' }, h(Icon, { name: 'lock', size: 12 }), 'hidden');
    return h('span', { className: cls }, String(v));
  }
  function PlanDiff(p) {
    var res = p.resources || [];
    var os = useState(function () {
      var o = {};
      res.forEach(function (r) { o[r.id] = r.expanded !== undefined ? r.expanded : r.action !== 'destroy'; });
      return o;
    });
    var open = os[0], setOpen = os[1];
    var counts = { create: 0, update: 0, replace: 0, destroy: 0 };
    res.forEach(function (r) { counts[r.action] = (counts[r.action] || 0) + 1; });
    var summary = [
      { n: counts.create, a: 'create', w: 'to create' },
      { n: counts.update + counts.replace, a: 'update', w: 'to change' },
      { n: counts.destroy, a: 'destroy', w: 'to destroy' }
    ];
    return h('div', { className: cx('dw-plan', p.className) },
      h('div', { className: 'dw-plan-summary' },
        summary.map(function (s) {
          return h('span', { key: s.a, className: cx('dw-plan-count', !s.n && 'is-zero') },
            h('span', { className: 'dw-plan-sym is-' + ACTIONS[s.a].tone }, ACTIONS[s.a].sym), h('b', null, s.n), ' ' + s.w);
        }),
        p.impact ? h('span', { className: 'dw-plan-impact' }, h(Icon, { name: 'activity', size: 14 }), p.impact) : null),
      h('div', { className: 'dw-plan-list' }, res.map(function (r) {
        var a = ACTIONS[r.action] || ACTIONS.update;
        var isOpen = !!open[r.id];
        var hasChanges = r.changes && r.changes.length;
        return h('div', { key: r.id, className: cx('dw-plan-res', 'is-' + a.tone) },
          h('button', {
            type: 'button', className: 'dw-plan-row', 'aria-expanded': hasChanges ? isOpen : undefined, disabled: !hasChanges,
            onClick: function () { var o = Object.assign({}, open); o[r.id] = !isOpen; setOpen(o); }
          },
            h('span', { className: 'dw-plan-sym is-' + a.tone, 'aria-hidden': true }, a.sym),
            h(Icon, { name: KIND_ICON[r.kind] || 'service', size: 16, className: 'dw-plan-kicon' }),
            h('span', { className: 'dw-plan-name' }, h('code', null, r.name), h('span', { className: 'dw-plan-kind' }, r.kindLabel || r.kind)),
            h('span', { className: 'dw-plan-act' }, r.note || a.label),
            hasChanges ? h(Icon, { name: 'chevron-down', size: 14, className: cx('dw-plan-chev', isOpen && 'is-open') }) : h('span', null)),
          hasChanges && isOpen ? h('dl', { className: 'dw-plan-changes' }, r.changes.map(function (c, i) {
            return h(Fragment, { key: i },
              h('dt', { className: 'dw-plan-path' }, c.path),
              h('dd', { className: 'dw-plan-val' },
                c.from !== undefined ? renderValue(c.from, 'dw-diff-from') : null,
                c.from !== undefined && c.to !== undefined ? h(Icon, { name: 'arrow-right', size: 12, className: 'dw-plan-arrow' }) : null,
                c.to !== undefined ? renderValue(c.to, 'dw-diff-to') : null));
          })) : null);
      })));
  }

  /* ---------- Timeline ---------- */
  var EVENT = {
    deploy: { icon: 'deploy' }, config: { icon: 'settings' }, alert: { icon: 'alert' },
    rollback: { icon: 'undo' }, node: { icon: 'server' }, server: { icon: 'server' }, plan: { icon: 'commit' }, backup: { icon: 'archive' }
  };
  function Timeline(p) {
    var events = p.events || [];
    return h('ol', { className: cx('dw-tl', p.className) }, events.map(function (e) {
      var k = EVENT[e.kind] || EVENT.plan;
      return h('li', { key: e.id, className: 'dw-tl-item' },
        h('span', { className: cx('dw-tl-mark', e.tone && 'dw-tone-' + e.tone) }, h(Icon, { name: k.icon, size: 14 })),
        h('div', { className: 'dw-tl-main' },
          h('div', { className: 'dw-tl-title' }, e.title),
          h('div', { className: 'dw-tl-meta' },
            e.status ? h(StatusBadge, { status: e.status, label: e.statusLabel }) : null,
            e.revision ? h('code', { className: 'dw-tl-rev' }, e.revision) : null,
            e.actor ? h('span', null, e.actor) : null,
            e.meta ? h('span', null, e.meta) : null)),
        h('div', { className: 'dw-tl-side' },
          h('time', { className: 'dw-tl-time', dateTime: e.datetime }, e.time),
          e.undoable && p.onUndo ? h(Button, { size: 'sm', variant: 'ghost', icon: 'undo', onClick: function () { p.onUndo(e); } }, e.undoLabel || 'Undo') : null));
    }));
  }

  /* ---------- Callout ---------- */
  function Callout(p) {
    var tone = p.tone || 'info';
    var t = tone === 'info' ? 'cobalt' : tone;
    return h('div', { className: cx('dw-callout', 'dw-tone-' + t, p.className), role: tone === 'critical' ? 'alert' : 'status' },
      h('span', { className: 'dw-callout-glyph' }, h(Glyph, { shape: TONE_SHAPE[tone] || 'circle', size: 10 })),
      h('div', { className: 'dw-callout-body' },
        p.title ? h('div', { className: 'dw-callout-title' }, p.title) : null,
        p.children ? h('div', { className: 'dw-callout-text' }, p.children) : null),
      p.actions ? h('div', { className: 'dw-callout-actions' }, p.actions) : null);
  }

  /* ---------- Toast ---------- */
  function Toast(p) {
    var tone = p.tone || 'healthy';
    var t = tone === 'info' ? 'cobalt' : tone;
    return h('div', {
      className: cx('dw-toast', 'dw-tone-' + t, p.className), role: tone === 'critical' ? 'alert' : 'status',
      style: p.duration ? { '--dw-toast-d': p.duration + 'ms' } : undefined
    },
      h('span', { className: 'dw-toast-glyph' }, h(Glyph, { shape: TONE_SHAPE[tone] || 'circle', size: 10 })),
      h('div', { className: 'dw-toast-body' },
        h('div', { className: 'dw-toast-title' }, p.title),
        p.description ? h('div', { className: 'dw-toast-desc' }, p.description) : null),
      h('div', { className: 'dw-toast-actions' },
        p.action ? h(Button, { size: 'sm', variant: 'secondary', icon: p.action.icon, kbd: p.action.kbd, onClick: p.action.onClick }, p.action.label) : null,
        p.onClose ? h(Button, { size: 'sm', variant: 'ghost', icon: 'x', 'aria-label': 'Dismiss', onClick: p.onClose }) : null),
      p.duration ? h('span', { className: 'dw-toast-timer', 'aria-hidden': true }) : null);
  }

  /* ---------- Sheet ---------- */
  function Sheet(p) {
    var id = useId('sheet');
    var panelRef = useRef(null);
    useEffect(function () {
      if (p.inline || !p.open) return undefined;
      function onKey(e) { if (e.key === 'Escape' && p.onClose) p.onClose(); }
      document.addEventListener('keydown', onKey);
      if (panelRef.current) panelRef.current.focus();
      return function () { document.removeEventListener('keydown', onKey); };
    }, [p.open, p.inline]);
    var panel = h('section', {
      ref: panelRef, tabIndex: -1, className: cx('dw-sheet', p.inline && 'is-inline', p.className),
      role: 'dialog', 'aria-modal': p.inline ? undefined : true, 'aria-labelledby': id,
      style: p.width ? { width: p.width } : undefined
    },
      h('header', { className: 'dw-sheet-head' },
        h('div', { className: 'dw-sheet-heading' },
          h('h2', { id: id, className: 'dw-sheet-title' }, p.title),
          p.subtitle ? h('p', { className: 'dw-sheet-sub' }, p.subtitle) : null),
        p.onClose ? h(Button, { variant: 'ghost', size: 'sm', icon: 'x', 'aria-label': 'Close', onClick: p.onClose }) : null),
      h('div', { className: 'dw-sheet-body' }, p.children),
      p.footer ? h('footer', { className: 'dw-sheet-foot' }, p.footer) : null);
    if (p.inline) return panel;
    if (!p.open) return null;
    return h('div', { className: 'dw-sheet-layer' }, h('div', { className: 'dw-scrim', onClick: p.onClose }), panel);
  }


  /* ---------- Logo ---------- */
  // The D is the dock; the cobalt below its waterline is the well. Geometry in font units (cap height 700).
  var LOGO_D = 'M53.6 -700H300.1A350 350 0 0 1 300.1 0H53.6ZM178.6 -575V-125H300.1A225 225 0 0 0 300.1 -575Z';
  var LOGO_WATER = 'M178.6 -311.5H521.8A225 225 0 0 1 300.1 -125H178.6Z';
  var LOGO_OCK = 'M937.8 12Q863.4 12 803.7 -20.8Q743.9 -53.6 709.1 -112.5Q674.2 -171.4 674.2 -248Q674.2 -325.6 709.2 -384Q744.1 -442.4 804.4 -475.2Q864.6 -508 938.2 -508Q1012.8 -508 1072.4 -475.2Q1132.1 -442.4 1166.9 -384Q1201.8 -325.6 1201.8 -248Q1201.8 -171.4 1166.8 -112.5Q1131.9 -53.6 1072.1 -20.8Q1012.4 12 937.8 12ZM937.7 -95.9Q975 -95.9 1005.8 -112.9Q1036.6 -129.9 1054.9 -163.7Q1073.2 -197.5 1073.2 -247.6Q1073.2 -298.5 1054.6 -332.3Q1036 -366.1 1005.6 -383.1Q975.2 -400.1 938.6 -400.1Q902 -400.1 871 -383Q840.1 -365.8 821.4 -332.4Q802.8 -298.9 802.8 -247.9Q802.8 -197.7 821.1 -163.8Q839.4 -129.9 870.3 -112.9Q901.1 -95.9 937.7 -95.9Z M1487 12Q1411.7 12 1353.5 -21.3Q1295.2 -54.6 1261.9 -113.1Q1228.6 -171.6 1228.6 -248Q1228.6 -324.6 1261.9 -383Q1295.2 -441.4 1353.5 -474.7Q1411.7 -508 1487 -508Q1581.1 -508 1645.5 -458.7Q1709.9 -409.4 1728.2 -323.6H1596.9Q1587.5 -359.4 1557.4 -380.1Q1527.3 -400.9 1486.2 -400.9Q1450.9 -400.9 1421.1 -382.7Q1391.3 -364.5 1373.9 -330.7Q1356.4 -296.9 1356.4 -248Q1356.4 -200.1 1373.9 -165.4Q1391.3 -130.7 1421.1 -112.5Q1450.9 -94.4 1486.2 -94.4Q1527.3 -94.4 1557.4 -114.7Q1587.5 -135 1596.9 -172.4H1728.2Q1710.9 -89.4 1646 -38.7Q1581.1 12 1487 12Z M1893.1 -144.4 1837.6 -223.1 2085.9 -496H2233ZM1776.2 0V-720H1901.2V0ZM2112.8 0 1920.4 -249.4 1996.3 -325.1 2266.6 0Z M2382.6 0 2236.9 -496H2360.5L2463 -76.4L2432.4 -77.2L2550.1 -496H2689.1L2807.6 -77.2L2776.2 -76.4L2878.7 -496H3003.1L2857.4 0H2728.2L2602.4 -432.9H2636.9L2511.8 0Z M3243.7 12Q3168.2 12 3110.3 -20.5Q3052.4 -53 3019.8 -110.7Q2987.2 -168.4 2987.2 -244.3Q2987.2 -321.7 3019.3 -381.3Q3051.4 -440.9 3109.5 -474.4Q3167.6 -508 3244.1 -508Q3317.7 -508 3373.4 -475.9Q3429.1 -443.8 3460.1 -389Q3491 -334.3 3491 -265.6Q3491 -255.6 3491 -243.5Q3491 -231.3 3489.2 -218.1H3077.4V-296.9H3364.9Q3361.9 -347.1 3328 -376.8Q3294.2 -406.4 3244.3 -406.4Q3207.9 -406.4 3177.2 -390.3Q3146.4 -374.2 3128.3 -342Q3110.2 -309.8 3110.2 -260.6V-232.1Q3110.2 -187.9 3127.7 -155.9Q3145.3 -124 3175.4 -107.2Q3205.4 -90.4 3242.9 -90.4Q3282.4 -90.4 3309.4 -108.2Q3336.4 -125.9 3350.1 -155.4H3477Q3463.4 -108.1 3430.9 -70.1Q3398.4 -32.1 3350.8 -10.1Q3303.3 12 3243.7 12Z M3543.5 0V-720H3668.5V0Z M3735.5 0V-720H3860.5V0Z';
  function Logo(p) {
    var variant = p.variant || 'logotype';
    var hgt = Number(p.height) || (variant === 'logotype' ? 20 : 24);
    var water = p.mono ? 'currentColor' : 'var(--cobalt)';
    var label = p.title || 'Dockwell';
    var common = { className: cx('dw-logo', p.className), role: 'img', 'aria-label': label, focusable: 'false' };
    if (variant === 'icon') {
      return h('svg', Object.assign({}, common, { viewBox: '0 0 1024 1024', width: hgt, height: hgt }),
        h('rect', { x: 4, y: 4, width: 1016, height: 1016, rx: 225, fill: '#191c1a', stroke: 'rgba(255,255,255,0.12)', strokeWidth: 8 }),
        h('g', { transform: 'translate(236 798.7) scale(0.81920)' },
          h('path', { fill: '#f3f4f1', fillRule: 'evenodd', d: LOGO_D }),
          h('path', { fill: '#809dff', d: LOGO_WATER })));
    }
    var mark = variant === 'mark';
    var vbW = mark ? 596.4 : 3850.4;
    return h('svg', Object.assign({}, common, { viewBox: '53.6 -700 ' + vbW + ' 700', height: hgt, width: hgt * vbW / 700, fill: 'currentColor' }),
      h('path', { fillRule: 'evenodd', d: LOGO_D }),
      h('path', { fill: water, d: LOGO_WATER }),
      mark ? null : h('path', { d: LOGO_OCK }));
  }

  var api = {
    Button: Button, Kbd: Kbd, StatusBadge: StatusBadge, Tag: Tag, TextField: TextField, Switch: Switch,
    SegmentedControl: SegmentedControl, Tabs: Tabs, ScopeBar: ScopeBar, CommandBar: CommandBar,
    Metric: Metric, Sparkline: Sparkline, Table: Table, FleetGrid: FleetGrid, LogStream: LogStream,
    PlanDiff: PlanDiff, Timeline: Timeline, Callout: Callout, Toast: Toast, Sheet: Sheet, Icon: Icon,
    Glyph: Glyph, Logo: Logo, STATUS: STATUS
  };
  window.Dockwell = Object.assign(window.Dockwell || {}, api);
})();
