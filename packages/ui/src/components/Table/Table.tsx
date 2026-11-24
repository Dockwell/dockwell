// SPDX-License-Identifier: AGPL-3.0-only

import { type CSSProperties, isValidElement, type KeyboardEvent, type MouseEvent, type ReactNode } from "react";

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { LoadingStatus, Skeleton, SLOW_AFTER_MS, useAfter } from "../Skeleton/Skeleton";
// Table.css loads with the foundations in styles/index.css, as pages use its classes on their own elements.

/** Keys of Row whose values React can render as they are. */
export type CellKey<Row> = {
  [K in keyof Row & string]: Row[K] extends ReactNode ? K : never;
}[keyof Row & string];

/** Keys of Row whose values identify a row. */
export type RowKey<Row> = {
  [K in keyof Row & string]: Row[K] extends string | number ? K : never;
}[keyof Row & string];

interface TableColumnBase {
  /** Sentence case, shown in the caption style. */
  header: ReactNode;
  width?: number | string;
  /** Numbers align right. */
  align?: "left" | "right";
  /** Machine values: images, IPs, domains. */
  mono?: boolean;
  muted?: boolean;
  /** How the rows are sorted by this column, for a table sorted by its header, said to assistive technology. */
  sort?: "ascending" | "descending";
  /**
   * The shape its cells take while the rows are on their way. Left out, it follows
   * the column: the first is a name, one whose header no one sees a row's action,
   * one keyed status, state or health a pill, a right-aligned one a number, a mono
   * one a machine value, and the rest text.
   */
  skeleton?: TableSkeleton;
}

/**
 * name: a name, wide; titled: a name over a mono line, as a two-line first cell;
 * text and mono: a value, of middling width; short: a word or two; number: a few
 * digits, at the right; pill: a status badge or a tag; icon: a glyph or a mark;
 * action: a small button at the row's end; none: nothing.
 */
export type TableSkeleton =
  | "name"
  | "titled"
  | "text"
  | "mono"
  | "short"
  | "number"
  | "pill"
  | "icon"
  | "action"
  | "none";

/**
 * A column shows the row's value at key, or whatever render returns. Without
 * render, key must name a value React can show; with it, key only has to be
 * unique among the columns.
 */
export type TableColumn<Row> =
  | (TableColumnBase & { key: CellKey<Row>; render?: undefined })
  | (TableColumnBase & { key: string; render: (row: Row) => ReactNode });

export interface TableProps<Row extends object = Record<string, unknown>> {
  /** Name first, status second, numbers right-aligned. */
  columns: readonly TableColumn<Row>[];
  rows: readonly Row[];
  /** The field that identifies a row. Defaults to "id". */
  rowKey?: RowKey<Row>;
  /** comfortable rows are 48px, compact 36px. */
  density?: "comfortable" | "compact";
  /** Makes rows clickable and reachable by keyboard, for rows that open details. */
  onRowClick?: (row: Row) => void;
  /**
   * A row's own menu, asked for by a right-click on the row or by Shift+F10 (or the
   * Menu key) on a focused row: where it was asked for, on the viewport, and the row
   * itself, to give focus back to. A right-click on a control in the row is the
   * control's, as a click is. Rows with one are a keyboard stop even when they don't open.
   */
  onRowContextMenu?: (row: Row, at: { x: number; y: number }, rowElement: HTMLElement) => void;
  /** Which rows have a context menu. Defaults to the same rows that can open. */
  rowHasContextMenu?: (row: Row) => boolean;
  /** Which rows onRowClick is for, where some have nothing to open. The rest are neither clickable nor a keyboard stop. */
  rowClickable?: (row: Row) => boolean;
  /** The key of the row whose details are open. */
  selectedKey?: string | number | null;
  /** A class for a row, for rows that differ, such as a group's head above its members. */
  rowClassName?: (row: Row) => string | undefined;
  /** Says what would appear and how to add the first one. */
  empty?: ReactNode;
  /** Names the table for assistive technology; the panel's title usually shows it. */
  caption?: string;
  /** Scrolls the body under a sticky header past this height. */
  maxHeight?: number | string;
  /**
   * The rows are on their way. With no rows yet, the table draws loadingRows rows
   * of each column's skeleton under its real header; rows it already has stay, as
   * a refetch keeps what it last showed.
   */
  loading?: boolean;
  /** How many rows to draw while loading: as many as last time, where the page knows. Default 3. */
  loadingRows?: number;
  /** What is on its way, said once to assistive technology, such as "Loading images". Default "Loading". */
  loadingLabel?: string;
  /** One short line said in place, above the shapes, when the wait runs long, such as "Docker is slow to list images on this server". */
  slow?: ReactNode;
  className?: string;
}

const INTERACTIVE =
  'a[href], button, input, select, textarea, label, [role="button"], [role="switch"], [role="checkbox"]';

/** True when the event came from a control inside the row, which handles it on its own. */
function fromControl(event: MouseEvent<HTMLElement> | KeyboardEvent<HTMLElement>): boolean {
  const target = event.target as Element;
  const control = target.closest(INTERACTIVE);
  return control !== null && control !== event.currentTarget && event.currentTarget.contains(control);
}

/** The shape a column's cells take while loading, where the column doesn't say. */
function skeletonOf(column: TableColumnBase & { key: string }, index: number): TableSkeleton {
  if (column.skeleton) return column.skeleton;
  if (index === 0) return "name";
  if (unseen(column.header)) return "action";
  if (/^(status|state|health)$/i.test(column.key)) return "pill";
  if (column.align === "right") return "number";
  if (column.mono) return "mono";
  return "text";
}

/** A header no one sees, as a column of row actions has: none, or words only for assistive technology. */
function unseen(header: ReactNode): boolean {
  if (header === "" || header == null) return true;
  return isValidElement<{ className?: unknown }>(header) && header.props.className === "dw-sr";
}

/** Widths that vary from row to row, as real values do, the same on every render. */
const WIDTHS: Readonly<Record<Exclude<TableSkeleton, "none" | "titled" | "icon" | "action">, readonly number[]>> = {
  name: [168, 128, 192, 144, 112],
  text: [96, 128, 80, 112, 104],
  mono: [128, 104, 144, 96, 120],
  short: [48, 40, 56, 44],
  number: [40, 32, 48, 36],
  pill: [76, 64, 84, 72],
};
const widthOf = (kind: keyof typeof WIDTHS, row: number, column: number) => {
  const widths = WIDTHS[kind];
  return widths[(row + column) % widths.length];
};

function SkeletonCell({ kind, row, column }: { kind: TableSkeleton; row: number; column: number }) {
  switch (kind) {
    case "none":
      return null;
    case "titled":
      return (
        <span className="dw-cell">
          <Skeleton text="label" width={widthOf("name", row, column)} />
          <Skeleton text="caption" width={widthOf("mono", row, column)} />
        </span>
      );
    case "icon":
      return <Skeleton shape="circle" size={16} />;
    case "action":
      return <Skeleton shape="block" width={28} height={28} className="dw-table-skeleton-action" />;
    case "pill":
      return <Skeleton shape="pill" width={widthOf("pill", row, column)} />;
    case "number":
      return <Skeleton text="label" width={widthOf("number", row, column)} className="dw-table-skeleton-num" />;
    default:
      return <Skeleton text={kind === "mono" ? "code-sm" : "label"} width={widthOf(kind, row, column)} />;
  }
}

/**
 * Dense rows of resources: tabular numbers, mono machine values, one status
 * column. A real table, named by its caption. Clickable rows are in the tab
 * order and open with Enter or Space. While its first rows are on their way
 * it draws their shapes under its real header, never a sentence.
 */
export function Table<Row extends object = Record<string, unknown>>({
  columns,
  rows,
  rowKey,
  density = "comfortable",
  onRowClick,
  onRowContextMenu,
  rowHasContextMenu,
  rowClickable,
  selectedKey,
  rowClassName,
  empty,
  caption,
  maxHeight,
  loading = false,
  loadingRows = 3,
  loadingLabel,
  slow,
  className,
}: TableProps<Row>) {
  const strings = useUIStrings();
  const waiting = loading && rows.length === 0;
  const keyField = (rowKey ?? "id") as keyof Row;
  const clickable = onRowClick !== undefined;
  const scrolls = maxHeight !== undefined;
  const wrapStyle: CSSProperties | undefined = scrolls ? { maxHeight } : undefined;
  return (
    <>
      {waiting ? <LoadingStatus label={loadingLabel ?? strings.loading} /> : null}
      <div
        className={cx("dw-table-wrap", waiting && "is-loading", className)}
        style={wrapStyle}
        aria-busy={waiting || undefined}
        // A scrolling region needs to be reachable by keyboard when nothing inside it is.
        {...(scrolls && !clickable ? { tabIndex: 0, role: "region", "aria-label": caption } : {})}
      >
        {/* Its shapes are for the eye: assistive technology hears the label, then the rows once they land. */}
        <table className={cx("dw-table", `dw-table-${density}`)} aria-hidden={waiting || undefined}>
          {/* Named once its rows are there: the region's label says what is on its way until then. */}
          {caption && !waiting ? <caption className="dw-sr">{caption}</caption> : null}
          <thead>
            <tr>
              {columns.map((column) => (
                <th
                  key={column.key}
                  scope="col"
                  className={cx(column.align === "right" && "is-num")}
                  style={column.width !== undefined ? { width: column.width } : undefined}
                  aria-sort={column.sort}
                >
                  {column.header}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {waiting ? (
              <SkeletonBody columns={columns} rows={loadingRows} slow={slow} />
            ) : rows.length === 0 ? (
              <tr>
                <td colSpan={columns.length} className="dw-table-empty">
                  {empty ?? strings.tableEmpty}
                </td>
              </tr>
            ) : (
              rows.map((row, index) => {
                const id = row[keyField] as unknown;
                const key = typeof id === "string" || typeof id === "number" ? id : index;
                const selected = selectedKey != null && selectedKey === id;
                const opens = clickable && (rowClickable?.(row) ?? true);
                const hasMenu =
                  onRowContextMenu !== undefined && (rowHasContextMenu?.(row) ?? rowClickable?.(row) ?? true);
                const stop = opens || hasMenu;
                return (
                  <tr
                    key={key}
                    className={cx(opens && "is-clickable", selected && "is-selected", rowClassName?.(row))}
                    aria-current={selected ? "true" : undefined}
                    aria-selected={opens ? selected : undefined}
                    tabIndex={stop ? 0 : undefined}
                    onClick={
                      opens
                        ? (event) => {
                            if (!fromControl(event)) onRowClick(row);
                          }
                        : undefined
                    }
                    onContextMenu={
                      hasMenu
                        ? (event) => {
                            if (fromControl(event)) return;
                            event.preventDefault();
                            onRowContextMenu(row, { x: event.clientX, y: event.clientY }, event.currentTarget);
                          }
                        : undefined
                    }
                    onKeyDown={
                      stop
                        ? (event) => {
                            if (event.target !== event.currentTarget) return;
                            if (hasMenu && ((event.shiftKey && event.key === "F10") || event.key === "ContextMenu")) {
                              event.preventDefault();
                              const box = event.currentTarget.getBoundingClientRect();
                              onRowContextMenu(row, { x: box.right - 8, y: box.bottom }, event.currentTarget);
                            } else if (opens && (event.key === "Enter" || event.key === " ")) {
                              event.preventDefault();
                              onRowClick(row);
                            }
                          }
                        : undefined
                    }
                  >
                    {columns.map((column) => (
                      <td
                        key={column.key}
                        className={cx(
                          column.mono && "is-mono",
                          column.align === "right" && "is-num",
                          column.muted && "is-muted",
                        )}
                      >
                        {column.render ? column.render(row) : (row[column.key] as ReactNode)}
                      </td>
                    ))}
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}

/** The rows on their way: the line a long wait gets, then each column's shape, row by row. */
function SkeletonBody<Row>({
  columns,
  rows,
  slow,
}: {
  columns: readonly TableColumn<Row>[];
  rows: number;
  slow: ReactNode;
}) {
  // Each row is its place, which is all a key needs to say of a row of shapes.
  const keys = Array.from({ length: Math.max(1, rows) }, (_, i) => `skeleton-${i + 1}`);
  return (
    <>
      {/* Above the shapes, so a long list of them never pushes it out of sight. */}
      {slow ? <SlowRow span={columns.length} slow={slow} /> : null}
      {keys.map((key, row) => (
        <tr key={key} className="dw-table-skeleton-row">
          {columns.map((column, index) => (
            <td key={column.key} className={cx(column.align === "right" && "is-num")}>
              <SkeletonCell kind={skeletonOf(column, index)} row={row} column={index} />
            </td>
          ))}
        </tr>
      ))}
    </>
  );
}

function SlowRow({ span, slow }: { span: number; slow: ReactNode }) {
  const late = useAfter(SLOW_AFTER_MS);
  if (!late) return null;
  return (
    <tr className="dw-table-slow">
      <td colSpan={span}>
        <p className="dw-loading-slow">{slow}</p>
      </td>
    </tr>
  );
}
