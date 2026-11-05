// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "../../lib/cx";
import type { GlyphShape } from "../../lib/status";
import "./Glyph.css";

export interface GlyphProps {
  /** The status shape. Shape carries meaning with colour, so state never depends on colour alone. */
  shape?: GlyphShape;
  /** Pixel size of the square glyph. Colour comes from currentColor. */
  size?: number;
  className?: string;
}

/** The bare status shape used inside StatusBadge, for custom rows and legends. Always decorative. */
export function Glyph({ shape = "circle", size = 10, className }: GlyphProps) {
  return (
    <svg
      className={cx("dw-glyph", className)}
      width={size}
      height={size}
      viewBox="0 0 10 10"
      aria-hidden="true"
      focusable="false"
    >
      <GlyphShapeElement shape={shape} />
    </svg>
  );
}

function GlyphShapeElement({ shape }: { shape: GlyphShape }) {
  switch (shape) {
    case "circle":
    case "info":
      return <circle cx={5} cy={5} r={4} fill="currentColor" />;
    case "triangle":
      return <path d="M5 .9l4.4 7.9H.6z" fill="currentColor" />;
    case "square":
      return <rect x={1.2} y={1.2} width={7.6} height={7.6} rx={1.6} fill="currentColor" />;
    case "ring":
      return <circle cx={5} cy={5} r={3.6} fill="none" stroke="currentColor" strokeWidth={1.6} />;
    case "dotted-ring":
      return (
        <>
          <circle cx={5} cy={5} r={3.6} fill="none" stroke="currentColor" strokeWidth={1.6} />
          <circle cx={5} cy={5} r={1.5} fill="currentColor" />
        </>
      );
    case "dashed-ring":
      // A dash across it as well as the gaps, so it can't pass for a hollow ring at 10 px.
      return (
        <>
          <circle
            cx={5}
            cy={5}
            r={3.6}
            fill="none"
            stroke="currentColor"
            strokeWidth={1.4}
            strokeDasharray="1.9 1.87"
            transform="rotate(-45 5 5)"
          />
          <rect x={3.1} y={4.3} width={3.8} height={1.4} rx={0.7} fill="currentColor" />
        </>
      );
    case "spinner":
      return (
        <>
          <circle cx={5} cy={5} r={3.6} fill="none" stroke="currentColor" strokeOpacity={0.28} strokeWidth={1.6} />
          <g className="dw-spin">
            <path
              d="M5 1.4A3.6 3.6 0 0 1 8.6 5"
              fill="none"
              stroke="currentColor"
              strokeWidth={1.6}
              strokeLinecap="round"
            />
          </g>
        </>
      );
  }
}
