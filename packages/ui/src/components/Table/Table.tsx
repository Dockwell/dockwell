// SPDX-License-Identifier: AGPL-3.0-only

import type { CSSProperties, KeyboardEvent, MouseEvent, ReactNode } from "react";

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import "./Table.css";

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
}

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

/**
 * Dense rows of resources: tabular numbers, mono machine values, one status
 * column. A real table, named by its caption. Clickable rows are in the tab
 * order and open with Enter or Space.
 */
export function Table<Row extends object = Record<string, unknown>>({
  columns,
  rows,
  rowKey,
  density = "comfortable",
  onRowClick,
  rowClickable,
  selectedKey,
  rowClassName,
  empty,
  caption,
  maxHeight,
  className,
}: TableProps<Row>) {
  const strings = useUIStrings();
  const keyField = (rowKey ?? "id") as keyof Row;
  const clickable = onRowClick !== undefined;
  const scrolls = maxHeight !== undefined;
  const wrapStyle: CSSProperties | undefined = scrolls ? { maxHeight } : undefined;
  return (
    <div
      className={cx("dw-table-wrap", className)}
      style={wrapStyle}
      // A scrolling region needs to be reachable by keyboard when nothing inside it is.
      {...(scrolls && !clickable ? { tabIndex: 0, role: "region", "aria-label": caption } : {})}
    >
      <table className={cx("dw-table", `dw-table-${density}`)}>
        {caption ? <caption className="dw-sr">{caption}</caption> : null}
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
          {rows.length === 0 ? (
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
              return (
                <tr
                  key={key}
                  className={cx(opens && "is-clickable", selected && "is-selected", rowClassName?.(row))}
                  aria-current={selected ? "true" : undefined}
                  aria-selected={opens ? selected : undefined}
                  tabIndex={opens ? 0 : undefined}
                  onClick={
                    opens
                      ? (event) => {
                          if (!fromControl(event)) onRowClick(row);
                        }
                      : undefined
                  }
                  onKeyDown={
                    opens
                      ? (event) => {
                          if (event.target !== event.currentTarget) return;
                          if (event.key === "Enter" || event.key === " ") {
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
  );
}
