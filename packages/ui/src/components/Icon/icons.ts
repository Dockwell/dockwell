// SPDX-License-Identifier: AGPL-3.0-only

// Dockwell line icons: a 24-unit grid, 1.5 stroke, round caps and joins,
// drawn in currentColor. Ported from design/system/components/bundle.js; the
// drawings are the design system's own, not copies of any library.

/** One SVG element of an icon: its tag and attributes. */
export type IconPart =
  | readonly ["path", { readonly d: string }]
  | readonly [
      "circle",
      {
        readonly cx: number;
        readonly cy: number;
        readonly r: number;
        readonly fill?: string;
        readonly stroke?: string;
      },
    ]
  | readonly ["ellipse", { readonly cx: number; readonly cy: number; readonly rx: number; readonly ry: number }]
  | readonly [
      "rect",
      { readonly x: number; readonly y: number; readonly width: number; readonly height: number; readonly rx?: number },
    ];

export const icons = {
  search: [
    ["circle", { cx: 11, cy: 11, r: 6.5 }],
    ["path", { d: "M16 16l4.5 4.5" }],
  ],
  check: [["path", { d: "M5 12.5l4.5 4.5L19 7.5" }]],
  x: [["path", { d: "M6.5 6.5l11 11M17.5 6.5l-11 11" }]],
  plus: [["path", { d: "M12 5v14M5 12h14" }]],
  minus: [["path", { d: "M5 12h14" }]],
  "chevron-right": [["path", { d: "M9.5 6l6 6-6 6" }]],
  "chevron-down": [["path", { d: "M6 9.5l6 6 6-6" }]],
  "chevron-up-down": [["path", { d: "M8 9.5l4-4 4 4M8 14.5l4 4 4-4" }]],
  "arrow-right": [["path", { d: "M5 12h14M13 6l6 6-6 6" }]],
  server: [
    ["rect", { x: 3.5, y: 4, width: 17, height: 7, rx: 2 }],
    ["rect", { x: 3.5, y: 13, width: 17, height: 7, rx: 2 }],
    ["circle", { cx: 7.5, cy: 7.5, r: 1, fill: "currentColor", stroke: "none" }],
    ["circle", { cx: 7.5, cy: 16.5, r: 1, fill: "currentColor", stroke: "none" }],
  ],
  service: [
    ["path", { d: "M12 3.2l7.8 4.4v8.8L12 20.8l-7.8-4.4V7.6z" }],
    ["path", { d: "M4.4 7.7L12 12l7.6-4.3M12 12v8.6" }],
  ],
  globe: [
    ["circle", { cx: 12, cy: 12, r: 8.5 }],
    [
      "path",
      { d: "M3.5 12h17M12 3.5c2.4 2.4 3.5 5.3 3.5 8.5s-1.1 6.1-3.5 8.5c-2.4-2.4-3.5-5.3-3.5-8.5s1.1-6.1 3.5-8.5z" },
    ],
  ],
  database: [
    ["ellipse", { cx: 12, cy: 6, rx: 7, ry: 2.6 }],
    ["path", { d: "M5 6v12c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6V6M5 12c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6" }],
  ],
  terminal: [
    ["rect", { x: 3, y: 4.5, width: 18, height: 15, rx: 2.5 }],
    ["path", { d: "M7 9.5l3 2.5-3 2.5M12.5 15H17" }],
  ],
  branch: [
    ["circle", { cx: 6.5, cy: 6, r: 2 }],
    ["circle", { cx: 6.5, cy: 18, r: 2 }],
    ["circle", { cx: 17.5, cy: 7.5, r: 2 }],
    ["path", { d: "M6.5 8v8M17.5 9.5c0 4-3 5.5-9.5 6.8" }],
  ],
  commit: [
    ["circle", { cx: 12, cy: 12, r: 3.2 }],
    ["path", { d: "M3 12h5.8M15.2 12H21" }],
  ],
  undo: [
    ["path", { d: "M8.5 5L4 9.5 8.5 14" }],
    ["path", { d: "M4 9.5h10a5.5 5.5 0 0 1 0 11h-3" }],
  ],
  clock: [
    ["circle", { cx: 12, cy: 12, r: 8.5 }],
    ["path", { d: "M12 7.5V12l3 2" }],
  ],
  shield: [["path", { d: "M12 3.5l7 2.8v5.2c0 4.3-2.9 7.7-7 9-4.1-1.3-7-4.7-7-9V6.3z" }]],
  lock: [
    ["rect", { x: 5, y: 10.5, width: 14, height: 10, rx: 2 }],
    ["path", { d: "M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5" }],
  ],
  settings: [
    ["path", { d: "M4 7h9M17 7h3M4 17h3M11 17h9" }],
    ["circle", { cx: 15, cy: 7, r: 2 }],
    ["circle", { cx: 9, cy: 17, r: 2 }],
  ],
  more: [
    ["circle", { cx: 6, cy: 12, r: 1.4, fill: "currentColor", stroke: "none" }],
    ["circle", { cx: 12, cy: 12, r: 1.4, fill: "currentColor", stroke: "none" }],
    ["circle", { cx: 18, cy: 12, r: 1.4, fill: "currentColor", stroke: "none" }],
  ],
  copy: [
    ["rect", { x: 8.5, y: 8.5, width: 12, height: 12, rx: 2 }],
    ["path", { d: "M15.5 8.5V5.5a2 2 0 0 0-2-2h-8a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h3" }],
  ],
  external: [
    ["path", { d: "M13.5 4H20v6.5M20 4l-9 9" }],
    ["path", { d: "M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4" }],
  ],
  play: [["path", { d: "M8 5.5v13l10.5-6.5z" }]],
  pause: [["path", { d: "M9 5.5v13M15 5.5v13" }]],
  stop: [["rect", { x: 6, y: 6, width: 12, height: 12, rx: 2 }]],
  deploy: [
    ["path", { d: "M12 15V4.5M7.5 9L12 4.5 16.5 9" }],
    ["path", { d: "M4.5 14.5v3a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-3" }],
  ],
  refresh: [
    ["path", { d: "M19.5 11.5a7.5 7.5 0 1 0-2.2 5.6" }],
    ["path", { d: "M19.5 5v6.5H13" }],
  ],
  activity: [["path", { d: "M3 12h4l2.5-6.5 5 13L17 12h4" }]],
  layers: [
    ["path", { d: "M12 3.5l8.5 4.5L12 12.5 3.5 8z" }],
    ["path", { d: "M3.5 12.5L12 17l8.5-4.5" }],
  ],
  folder: [["path", { d: "M3.5 7a2 2 0 0 1 2-2h4l2 2.2h7a2 2 0 0 1 2 2V17a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z" }]],
  org: [
    ["path", { d: "M4.5 20V7.5L12 4l7.5 3.5V20" }],
    ["path", { d: "M3 20h18M9.5 20v-4.5h5V20M9 10h.01M15 10h.01" }],
  ],
  alert: [
    ["path", { d: "M12 4.5l8.5 15h-17z" }],
    ["path", { d: "M12 10v4M12 17h.01" }],
  ],
  bell: [
    ["path", { d: "M6.5 16.5V11a5.5 5.5 0 0 1 11 0v5.5l1.5 2H5z" }],
    ["path", { d: "M10 20.5h4" }],
  ],
  key: [
    ["circle", { cx: 8, cy: 14, r: 4 }],
    ["path", { d: "M11 11l8.5-8.5M16.5 5.5l2 2" }],
  ],
  home: [
    ["path", { d: "M4 10.5L12 4l8 6.5V20H4z" }],
    ["path", { d: "M10 20v-5h4v5" }],
  ],
  archive: [
    ["rect", { x: 3.5, y: 4.5, width: 17, height: 4.5, rx: 1.5 }],
    ["path", { d: "M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9M10 13h4" }],
  ],
  cpu: [
    ["rect", { x: 6.5, y: 6.5, width: 11, height: 11, rx: 2 }],
    ["path", { d: "M9.5 3.5v3M14.5 3.5v3M9.5 17.5v3M14.5 17.5v3M3.5 9.5h3M3.5 14.5h3M17.5 9.5h3M17.5 14.5h3" }],
  ],
  filter: [["path", { d: "M4 6h16M7 12h10M10 18h4" }]],
  sun: [
    ["circle", { cx: 12, cy: 12, r: 4 }],
    [
      "path",
      {
        d: "M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4",
      },
    ],
  ],
  browser: [
    ["rect", { x: 3, y: 4.5, width: 18, height: 15, rx: 2.5 }],
    ["path", { d: "M3 9h18" }],
    ["circle", { cx: 6.5, cy: 6.75, r: 0.8, fill: "currentColor", stroke: "none" }],
    ["circle", { cx: 9, cy: 6.75, r: 0.8, fill: "currentColor", stroke: "none" }],
  ],
  link: [
    ["path", { d: "M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1.2 1.2" }],
    ["path", { d: "M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1.2-1.2" }],
  ],
  grid: [
    ["rect", { x: 4, y: 4, width: 6.5, height: 6.5, rx: 1.5 }],
    ["rect", { x: 13.5, y: 4, width: 6.5, height: 6.5, rx: 1.5 }],
    ["rect", { x: 4, y: 13.5, width: 6.5, height: 6.5, rx: 1.5 }],
    ["rect", { x: 13.5, y: 13.5, width: 6.5, height: 6.5, rx: 1.5 }],
  ],
  users: [
    ["circle", { cx: 9, cy: 8.5, r: 3.5 }],
    ["path", { d: "M3 19.5c.6-3.2 3-5 6-5s5.4 1.8 6 5" }],
    ["path", { d: "M15.5 5.2a3.5 3.5 0 0 1 0 6.6M17.5 14.8c1.8.7 3 2.3 3.5 4.7" }],
  ],
  user: [
    ["circle", { cx: 12, cy: 8.5, r: 3.8 }],
    ["path", { d: "M4.5 20c.8-3.6 3.8-5.8 7.5-5.8s6.7 2.2 7.5 5.8" }],
  ],
  card: [
    ["rect", { x: 3, y: 5.5, width: 18, height: 13, rx: 2.5 }],
    ["path", { d: "M3 10h18M7 15h3" }],
  ],
  chat: [
    ["path", { d: "M4.5 6.5a2 2 0 0 1 2-2h11a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H11l-4.5 3.5V16.5h0a2 2 0 0 1-2-2z" }],
    ["path", { d: "M8.5 9.5h7M8.5 12.5h4.5" }],
  ],
  trash: [
    [
      "path",
      {
        d: "M4.5 7h15M9.5 7V5h5v2M6.5 7l.8 12a1.5 1.5 0 0 0 1.5 1.4h6.4a1.5 1.5 0 0 0 1.5-1.4l.8-12M10 11v5.5M14 11v5.5",
      },
    ],
  ],
  edit: [
    ["path", { d: "M14.5 5.5l4 4L8.5 19.5H4.5v-4z" }],
    ["path", { d: "M12.5 7.5l4 4" }],
  ],
  file: [
    ["path", { d: "M6.5 3.5h7l4 4v11a2 2 0 0 1-2 2h-9a2 2 0 0 1-2-2v-13a2 2 0 0 1 2-2z" }],
    ["path", { d: "M13.5 3.5v4h4" }],
  ],
  download: [
    ["path", { d: "M12 4.5V15M7.5 10.5L12 15l4.5-4.5" }],
    ["path", { d: "M4.5 15.5v2a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-2" }],
  ],
  eye: [
    ["path", { d: "M2.5 12s3.5-6.5 9.5-6.5S21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z" }],
    ["circle", { cx: 12, cy: 12, r: 2.8 }],
  ],
  mail: [
    ["rect", { x: 3, y: 5.5, width: 18, height: 13, rx: 2.5 }],
    ["path", { d: "M3.5 7l8.5 6 8.5-6" }],
  ],
  logout: [
    ["path", { d: "M10 4.5H6.5a2 2 0 0 0-2 2v11a2 2 0 0 0 2 2H10" }],
    ["path", { d: "M14.5 8l4 4-4 4M18.5 12H9.5" }],
  ],
  menu: [["path", { d: "M4 7h16M4 12h16M4 17h16" }]],
  "arrow-left": [["path", { d: "M19 12H5M11 6l-6 6 6 6" }]],
  upload: [
    ["path", { d: "M12 15V4.5M7.5 9L12 4.5 16.5 9" }],
    ["path", { d: "M4.5 15.5v2a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-2" }],
  ],
  gauge: [
    ["path", { d: "M4.2 16.5a8.5 8.5 0 1 1 15.6 0" }],
    ["path", { d: "M12 12.5l3.5-3.5" }],
    ["circle", { cx: 12, cy: 12.5, r: 1.3, fill: "currentColor", stroke: "none" }],
  ],
  network: [
    ["circle", { cx: 12, cy: 5.5, r: 2 }],
    ["circle", { cx: 5.5, cy: 18.5, r: 2 }],
    ["circle", { cx: 18.5, cy: 18.5, r: 2 }],
    ["path", { d: "M12 7.5v4M12 11.5l-5.2 5.4M12 11.5l5.2 5.4" }],
  ],
  calendar: [
    ["rect", { x: 4, y: 5.5, width: 16, height: 14.5, rx: 2.5 }],
    ["path", { d: "M4 10h16M8.5 3.5v4M15.5 3.5v4" }],
  ],
  question: [
    ["circle", { cx: 12, cy: 12, r: 8.5 }],
    ["path", { d: "M9.6 9.5a2.5 2.5 0 0 1 4.8.8c0 1.7-2.4 2-2.4 3.7M12 17h.01" }],
  ],
  pin: [
    ["path", { d: "M12 20.5s6-5.3 6-10.5a6 6 0 0 0-12 0c0 5.2 6 10.5 6 10.5z" }],
    ["circle", { cx: 12, cy: 10, r: 2.2 }],
  ],
  moon: [["path", { d: "M19.5 14.5A8 8 0 0 1 9.5 4.5a8 8 0 1 0 10 10z" }]],
} as const satisfies Record<string, readonly IconPart[]>;

export type IconName = keyof typeof icons;

/** Every icon name, in the design system's order. */
export const iconNames = Object.keys(icons) as IconName[];
