// SPDX-License-Identifier: AGPL-3.0-only

import { type CSSProperties, type ReactNode, useEffect, useState } from "react";

import { cx, hasContent } from "../../lib/cx";

// Skeleton.css loads with the foundations in styles/index.css, as every screen and Table draw these shapes.

/** How long a wait stays blank, so a fast answer never flashes its shapes. Skeleton.css holds the same delay. */
export const SKELETON_DELAY_MS = 150;

/** How long a wait runs before its one line says what it is waiting on. */
export const SLOW_AFTER_MS = 3000;

/** The text style a line stands for, which sets its height and the line box it sits in. */
export type SkeletonText = "title" | "heading" | "body" | "label" | "caption" | "code" | "code-sm" | "metric";

export interface SkeletonProps {
  /**
   * line: a line of text, in the box of its text style; block: a panel, chart, field or
   * button; pill: a status badge or tag; circle: an icon or a glyph.
   */
  shape?: "line" | "block" | "pill" | "circle";
  /** A line's text style. Default body. */
  text?: SkeletonText;
  /** Default: a line or a block fills its box; a pill is 72px; a circle is its size. */
  width?: number | string;
  /** A block's or a pill's height. Default 64px for a block, 20px for a pill. */
  height?: number | string;
  /** A circle's diameter. Default 16px, the size of an icon in a list. */
  size?: number;
  /** Sits in a line of text, as a value in a sentence does, rather than on a line of its own. */
  inline?: boolean;
  className?: string;
}

/**
 * One shape of what is on its way, drawn where the real thing will be and at its
 * size, so nothing moves when it lands. Shapes wait 150 ms before they show and
 * pulse slowly after; under reduced motion they hold still. Decorative: the
 * region they stand in says it is loading, through Loading or Table's loading.
 */
export function Skeleton({ shape = "line", text = "body", width, height, size, inline, className }: SkeletonProps) {
  const style: CSSProperties = {};
  if (shape === "circle") {
    style.width = size ?? 16;
    style.height = size ?? 16;
  } else {
    if (width !== undefined) style.width = width;
    if (height !== undefined && shape !== "line") style.height = height;
  }
  return (
    <span
      className={cx(
        "dw-skeleton",
        `is-${shape}`,
        shape === "line" && `is-text-${text}`,
        inline && "is-inline",
        className,
      )}
      style={style}
      aria-hidden="true"
    />
  );
}

/** True once ms have passed since the component mounted. */
export function useAfter(ms: number): boolean {
  const [after, setAfter] = useState(ms <= 0);
  useEffect(() => {
    if (ms <= 0) return;
    const timer = setTimeout(() => setAfter(true), ms);
    return () => clearTimeout(timer);
  }, [ms]);
  return after;
}

export interface LoadingProps {
  /**
   * What is on its way, said once and politely to assistive technology, such as
   * "Loading images". Never shown: the shapes show it.
   */
  label: string;
  /**
   * One short line said in place once the wait runs past slowAfter, in the
   * product's voice: what the wait is, such as "Docker is measuring disk use on
   * this server". Leave it out where a wait is never long.
   */
  slow?: ReactNode;
  /** How long before slow shows, in ms. Default 3 s. */
  slowAfter?: number;
  /** The shapes, laid out as the real content will be. */
  children?: ReactNode;
  className?: string;
  /** Inline styles for the region, such as a grid's columns. */
  style?: CSSProperties;
}

/**
 * A region whose content is on its way: busy for assistive technology, which
 * hears its label once, the shapes of what is coming in their final layout,
 * and, past a few seconds, one line above them that says what the wait is. Never a lone
 * sentence: the shapes carry the wait, the line only explains a long one.
 */
export function Loading({ label, slow, slowAfter = SLOW_AFTER_MS, children, className, style }: LoadingProps) {
  return (
    <>
      <LoadingStatus label={label} />
      {/* Before the region, never in it: a region that lays its shapes out as a grid would
          make the line one of its cells. First, so a long list of shapes never pushes it out of sight. */}
      <SlowLine slow={slow} after={slowAfter} />
      <div className={cx("dw-loading", className)} style={style} aria-busy="true">
        {children}
      </div>
    </>
  );
}

/**
 * The one polite announcement of a wait. It fills after the shapes' delay, so a
 * fast answer says nothing, and a live region that was there first is heard
 * when it fills. It sits outside the busy region, which a reader holds back. A
 * polite live region rather than a status, so it never stands for the page's
 * own statuses, such as a callout that says what goes on.
 */
export function LoadingStatus({ label }: { label: string }) {
  const said = useAfter(SKELETON_DELAY_MS);
  return (
    <span className="dw-sr dw-loading-said" aria-live="polite" aria-atomic="true">
      {said ? label : null}
    </span>
  );
}

/** The line a long wait gets, once it has run past after. */
export function SlowLine({ slow, after = SLOW_AFTER_MS }: { slow?: ReactNode; after?: number }) {
  const late = useAfter(after);
  if (!late || !hasContent(slow)) return null;
  return <p className="dw-loading-slow">{slow}</p>;
}

/** Widths that vary from row to row, as real names do, the same on every render. */
const pick = <T,>(values: readonly T[], index: number): T => values[index % values.length] as T;
const TITLE_WIDTHS = [168, 128, 192, 144, 112] as const;
const SUB_WIDTHS = [112, 88, 136, 96, 120] as const;

export interface SkeletonRowsProps {
  /** How many rows. Default 3. */
  rows?: number;
  /** A shape before each row's words: an icon, or a mark on a tile. */
  lead?: "icon" | "mark";
  /** Lines in each row: a title alone, or a title over a sub line. Default 2. */
  lines?: 1 | 2;
  /** A shape at each row's end: a status pill, a short value, or a chevron. */
  end?: "pill" | "value" | "chevron";
  /** Rows of 48px, as a compact list's, rather than 56px. */
  dense?: boolean;
  className?: string;
}

/** The rows of a list on its way, as phone lists and lists that aren't tables draw them. */
export function SkeletonRows({ rows = 3, lead, lines = 2, end, dense = false, className }: SkeletonRowsProps) {
  // Each row is its place, which is all a key needs to say of a shape.
  const keys = Array.from({ length: rows }, (_, i) => `row-${i + 1}`);
  return (
    <div className={cx("dw-skeleton-rows", dense && "is-dense", className)} aria-hidden="true">
      {keys.map((key, index) => (
        <div key={key} className="dw-skeleton-row">
          {lead === "icon" ? <Skeleton shape="circle" size={16} /> : null}
          {lead === "mark" ? <Skeleton shape="block" width={32} height={32} className="is-tile" /> : null}
          <span className="dw-skeleton-row-words">
            <Skeleton text="label" width={pick(TITLE_WIDTHS, index)} />
            {lines === 2 ? <Skeleton text="caption" width={pick(SUB_WIDTHS, index)} /> : null}
          </span>
          {end === "pill" ? <Skeleton shape="pill" width={pick([72, 64, 80], index)} /> : null}
          {end === "value" ? <Skeleton text="label" width={pick([48, 40, 56], index)} /> : null}
          {end === "chevron" ? <Skeleton shape="circle" size={12} /> : null}
        </div>
      ))}
    </div>
  );
}

export interface SkeletonFactsProps {
  /** How many facts. Default 4. */
  count?: number;
  /**
   * grid: label over value, in columns that fill the width, as a detail page's facts;
   * rows: label beside value, one to a row, as facts in an aside.
   */
  layout?: "grid" | "rows";
  className?: string;
}

const GRID_VALUES = ["72%", "56%", "64%", "48%"] as const;

/** Facts on their way: label and value pairs in the layout the facts will take. */
export function SkeletonFacts({ count = 4, layout = "grid", className }: SkeletonFactsProps) {
  const keys = Array.from({ length: count }, (_, i) => `fact-${i + 1}`);
  return (
    <div className={cx("dw-skeleton-facts", `is-${layout}`, className)} aria-hidden="true">
      {keys.map((key, index) => (
        <div key={key} className="dw-skeleton-fact">
          <Skeleton text="caption" width={pick([56, 72, 48, 64], index)} />
          <Skeleton
            text="body"
            width={layout === "rows" ? pick([96, 128, 80, 112], index) : pick(GRID_VALUES, index)}
          />
        </div>
      ))}
    </div>
  );
}

export interface SkeletonCardProps {
  /** The card's title where it is known before its content, as it usually is. Left out, a heading-sized line. */
  title?: ReactNode;
  /** Leave out the head, for a card that has none. */
  noHead?: boolean;
  /** Lines of text in the body. Default 3, unless height or children say otherwise. */
  lines?: number;
  /** A block of this height in the body, for a chart, a map or a log. */
  height?: number | string;
  /** The body's own shapes, such as SkeletonFacts or SkeletonRows. */
  children?: ReactNode;
  /** Drops the body padding, for rows that run edge to edge. */
  flush?: boolean;
  className?: string;
}

const LINE_WIDTHS = ["92%", "76%", "84%", "64%", "88%", "52%"] as const;

/** A panel on its way: its head, with its real title where known, and the shape of its body. */
export function SkeletonCard({
  title,
  noHead = false,
  lines,
  height,
  children,
  flush = false,
  className,
}: SkeletonCardProps) {
  let body = children;
  if (body === undefined) {
    if (height !== undefined) body = <Skeleton shape="block" height={height} />;
    else {
      const keys = Array.from({ length: lines ?? 3 }, (_, i) => `line-${i + 1}`);
      body = keys.map((key, index) => <Skeleton key={key} width={pick(LINE_WIDTHS, index)} />);
    }
  }
  return (
    <div className={cx("dw-panel", "dw-skeleton-card", className)}>
      {noHead ? null : (
        <div className="dw-panel-head">
          {hasContent(title) ? (
            <span className="dw-panel-title">{title}</span>
          ) : (
            <Skeleton text="heading" width={128} />
          )}
        </div>
      )}
      <div className={cx("dw-panel-body", flush && "is-flush")}>{body}</div>
    </div>
  );
}

export interface SkeletonFieldProps {
  /** The control's height: 32px for a field or a select, more for a group of choices. Default 32. */
  height?: number;
  /** The label's width. Default 72px. */
  label?: number;
  className?: string;
}

/** A form field on its way, where its choices come from a read: its label over its control, at their sizes. */
export function SkeletonField({ height = 32, label = 72, className }: SkeletonFieldProps) {
  return (
    <div className={cx("dw-skeleton-field", className)} aria-hidden="true">
      <Skeleton text="label" width={label} />
      <Skeleton shape="block" height={height} />
    </div>
  );
}

/** A row of tabs on its way, at the tabs' height and with their rule, for a page whose tabs depend on what loads. */
export function SkeletonTabs({ count = 4, className }: { count?: number; className?: string }) {
  const keys = Array.from({ length: count }, (_, i) => `tab-${i + 1}`);
  return (
    <div className={cx("dw-tabs", "dw-skeleton-tabs", className)} aria-hidden="true">
      {keys.map((key, index) => (
        <span key={key} className="dw-skeleton-tab">
          <Skeleton text="label" width={pick([64, 72, 48, 88, 56], index)} />
        </span>
      ))}
    </div>
  );
}
