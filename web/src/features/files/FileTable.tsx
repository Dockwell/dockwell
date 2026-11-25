// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type FileEntry, FileType } from "@dockwell/api";
import { Button, cx, Icon, type IconName, LoadingStatus, Skeleton } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import {
  type FormEvent,
  type KeyboardEvent,
  type MouseEvent,
  type PointerEvent,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";

import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "../servers/useFleet";
import type { ColumnWidths, ResizableColumn } from "./preferences";

export interface FileRow {
  name: string;
  entry: FileEntry;
  folder: boolean;
}

export type SortField = "name" | "size" | "modified" | "mode" | "owner";
export type SortDir = "asc" | "desc";

export interface MenuTrigger {
  position?: { x: number; y: number };
  anchor?: HTMLElement;
}

export interface FileTableProps {
  rows: readonly FileRow[];
  folderSize?: (row: FileRow) => ReactNode;
  caption: string;
  phone: boolean;
  /** Compact column defaults when a side pane is open and the user hasn't resized columns. */
  tight: boolean;
  /** Paged folders stay in server name order so Show more appends consistently. */
  sortable: boolean;
  sortField: SortField;
  sortDir: SortDir;
  onSort: (field: SortField) => void;
  columnWidths: ColumnWidths;
  onColumnWidthChange: (column: ResizableColumn, width: number) => void;
  selected: ReadonlySet<string>;
  /** Currently active editor file name, highlighted even if multi-selection is empty. */
  activeFileName?: string | undefined;
  onSelectChange: (next: ReadonlySet<string>, anchor?: string) => void;
  anchor?: string | undefined;
  onOpen: (row: FileRow) => void;
  onRowContextMenu: (row: FileRow, trigger: MenuTrigger) => void;
  onEmptyContextMenu: (position: { x: number; y: number }) => void;
  /** Row currently being renamed inline, if any. */
  renaming?: string | undefined;
  renameError?: string | undefined;
  renameBusy?: boolean;
  onStartRename: (row: FileRow) => void;
  onCommitRename: (row: FileRow, nextName: string) => void;
  onCancelRename: () => void;
  loading: boolean;
  loadingLabel: string;
  empty: ReactNode;
  maxHeight?: number | undefined;
}

const INTERACTIVE = 'a[href], button, input, select, textarea, label, [role="button"], [role="checkbox"]';

function fromControl(event: MouseEvent<HTMLElement> | KeyboardEvent<HTMLElement>): boolean {
  const target = event.target as Element;
  const control = target.closest(INTERACTIVE);
  return control !== null && control !== event.currentTarget && event.currentTarget.contains(control);
}

function changeable(row: FileRow): boolean {
  return !row.entry.refused && !row.entry.unreadableName;
}

function iconOf(row: FileRow): IconName {
  if (row.entry.type === FileType.SYMLINK) return "link";
  return row.folder ? "folder" : "file";
}

function defaultWidth(column: ResizableColumn, tight: boolean): number | undefined {
  switch (column) {
    case "name":
      return undefined;
    case "size":
      return tight ? 68 : 84;
    case "modified":
      return tight ? 78 : 92;
    case "mode":
      return tight ? 100 : 116;
    case "owner":
      return tight ? 80 : 96;
  }
}

/**
 * File manager table built on @dockwell/ui's table foundation: single-click
 * selection, double-click/Enter opening, Shift and Cmd/Ctrl multi-select,
 * hover/selection checkboxes, sortable & resizable columns, inline rename,
 * and right-click / keyboard context menus.
 */
export function FileTable({
  rows,
  folderSize,
  caption,
  phone,
  tight,
  sortable,
  sortField,
  sortDir,
  onSort,
  columnWidths,
  onColumnWidthChange,
  selected,
  activeFileName,
  onSelectChange,
  anchor,
  onOpen,
  onRowContextMenu,
  onEmptyContextMenu,
  renaming,
  renameError,
  renameBusy = false,
  onStartRename,
  onCommitRename,
  onCancelRename,
  loading,
  loadingLabel,
  empty,
  maxHeight,
}: FileTableProps) {
  const { t, i18n } = useLingui();
  const now = useControlPlaneNow();
  const words = useTimeWords();
  const wrapRef = useRef<HTMLDivElement>(null);
  const selectAllRef = useRef<HTMLInputElement>(null);
  const rowRefs = useRef(new Map<string, HTMLTableRowElement>());
  const waiting = loading && rows.length === 0;
  const hasSelection = selected.size > 0;
  const allSelected = rows.length > 0 && rows.every((r) => selected.has(r.name));
  const someSelected = hasSelection && !allSelected;

  useEffect(() => {
    if (selectAllRef.current) selectAllRef.current.indeterminate = someSelected;
  }, [someSelected]);

  const widthOf = (column: ResizableColumn): number | undefined => columnWidths[column] ?? defaultWidth(column, tight);

  const startColumnResize = (column: ResizableColumn, event: PointerEvent<HTMLSpanElement>) => {
    event.preventDefault();
    event.stopPropagation();
    const th = event.currentTarget.closest("th");
    if (!th) return;
    const startX = event.clientX;
    const startW = th.getBoundingClientRect().width;
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const minW = column === "name" ? 140 : 56;
    const maxW = column === "name" ? 720 : 320;
    const onMove = (moveEvent: globalThis.PointerEvent) => {
      const next = Math.max(minW, Math.min(maxW, Math.round(startW + (moveEvent.clientX - startX))));
      onColumnWidthChange(column, next);
    };
    const onUp = () => {
      handle.releasePointerCapture?.(event.pointerId);
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
  };

  const rangeSelect = (targetName: string, additive: boolean) => {
    const names = rows.map((r) => r.name);
    const toIdx = names.indexOf(targetName);
    if (toIdx === -1) return;
    const anchorName = anchor && names.includes(anchor) ? anchor : names.find((n) => selected.has(n));
    const fromIdx = anchorName ? names.indexOf(anchorName) : toIdx;
    const lo = Math.min(fromIdx, toIdx);
    const hi = Math.max(fromIdx, toIdx);
    const next = additive ? new Set(selected) : new Set<string>();
    for (let i = lo; i <= hi; i++) {
      const n = names[i];
      if (n !== undefined) next.add(n);
    }
    onSelectChange(next, anchorName ?? targetName);
  };

  const handleRowClick = (row: FileRow, event: MouseEvent<HTMLTableRowElement>) => {
    if (fromControl(event) || renaming === row.name) return;
    if (phone && !hasSelection) {
      onOpen(row);
      return;
    }
    if (event.shiftKey) {
      rangeSelect(row.name, event.metaKey || event.ctrlKey);
    } else if (event.metaKey || event.ctrlKey) {
      const next = new Set(selected);
      if (next.has(row.name)) next.delete(row.name);
      else next.add(row.name);
      onSelectChange(next, row.name);
    } else {
      onSelectChange(new Set([row.name]), row.name);
    }
  };

  const handleRowDoubleClick = (row: FileRow, event: MouseEvent<HTMLTableRowElement>) => {
    if (fromControl(event) || renaming === row.name) return;
    if (phone) return;
    onOpen(row);
  };

  const toggleCheckbox = (row: FileRow, shiftKey: boolean) => {
    if (shiftKey) {
      rangeSelect(row.name, true);
      return;
    }
    const next = new Set(selected);
    if (next.has(row.name)) next.delete(row.name);
    else next.add(row.name);
    onSelectChange(next, row.name);
  };

  const handleTableKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const target = event.target as Element;
    if (target.closest("input, textarea, select")) return;
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "a") {
      event.preventDefault();
      onSelectChange(new Set(rows.map((r) => r.name)), rows[0]?.name);
      return;
    }
    if (event.key === "Escape" && selected.size > 0) {
      event.preventDefault();
      onSelectChange(new Set(), undefined);
    }
  };

  const handleRowKeyDown = (row: FileRow, index: number, event: KeyboardEvent<HTMLTableRowElement>) => {
    if (event.target !== event.currentTarget || renaming === row.name) return;
    if (event.key === "Enter") {
      event.preventDefault();
      onOpen(row);
    } else if (event.key === " ") {
      event.preventDefault();
      toggleCheckbox(row, event.shiftKey);
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const nextIndex = event.key === "ArrowDown" ? Math.min(rows.length - 1, index + 1) : Math.max(0, index - 1);
      const nextRow = rows[nextIndex];
      if (!nextRow) return;
      if (event.shiftKey) rangeSelect(nextRow.name, false);
      else onSelectChange(new Set([nextRow.name]), nextRow.name);
      rowRefs.current.get(nextRow.name)?.focus();
    } else if (event.key === "F2" && changeable(row)) {
      event.preventDefault();
      onStartRename(row);
    } else if (event.key === "ContextMenu" || (event.shiftKey && event.key === "F10")) {
      event.preventDefault();
      onRowContextMenu(row, { anchor: event.currentTarget });
    }
  };

  const renderHeader = (column: ResizableColumn, label: string, align?: "right") => {
    const active = sortable && sortField === column;
    const width = widthOf(column);
    return (
      <th
        key={column}
        scope="col"
        className={cx("files-th", align === "right" && "is-num")}
        style={width !== undefined ? { width } : undefined}
        aria-sort={active ? (sortDir === "asc" ? "ascending" : "descending") : undefined}
      >
        {sortable ? (
          <button
            type="button"
            className={cx("files-sort-btn", align === "right" && "is-right")}
            onClick={() => onSort(column)}
          >
            <span>{label}</span>
            {active ? (
              <Icon name={sortDir === "asc" ? "chevron-up" : "chevron-down"} size={12} className="files-sort-icon" />
            ) : null}
          </button>
        ) : (
          label
        )}
        {!phone ? (
          <span
            className="files-col-resize"
            aria-hidden="true"
            onPointerDown={(event) => startColumnResize(column, event)}
          />
        ) : null}
      </th>
    );
  };

  const colCount = phone ? 4 : 7;

  return (
    <>
      {waiting ? <LoadingStatus label={loadingLabel} /> : null}
      {/* biome-ignore lint/a11y/noStaticElementInteractions: table wrapper handles Cmd/Ctrl-A, Escape, and empty-space context menu */}
      <div
        ref={wrapRef}
        className={cx("dw-table-wrap", "files-table-wrap", waiting && "is-loading", hasSelection && "has-selection")}
        style={maxHeight !== undefined ? { maxHeight } : undefined}
        aria-busy={waiting || undefined}
        onKeyDown={handleTableKeyDown}
        onContextMenu={(event) => {
          const target = event.target as HTMLElement;
          if (target.closest("tbody tr[data-file-row]")) return;
          event.preventDefault();
          onEmptyContextMenu({ x: event.clientX, y: event.clientY });
        }}
      >
        <table className="dw-table dw-table-compact files-table" aria-hidden={waiting || undefined}>
          {caption && !waiting ? <caption className="dw-sr">{caption}</caption> : null}
          <thead>
            <tr>
              <th scope="col" className="files-col-select" style={{ width: 36 }}>
                <input
                  ref={selectAllRef}
                  type="checkbox"
                  className="files-checkbox"
                  checked={allSelected}
                  disabled={rows.length === 0}
                  aria-label={allSelected ? t`Deselect all` : t`Select all`}
                  onChange={() => {
                    if (allSelected) onSelectChange(new Set(), undefined);
                    else onSelectChange(new Set(rows.map((r) => r.name)), rows[0]?.name);
                  }}
                />
              </th>
              {renderHeader("name", t`Name`)}
              {renderHeader("size", t`Size`, "right")}
              {phone ? null : renderHeader("modified", t`Modified`)}
              {phone ? null : renderHeader("mode", t`Permissions`)}
              {phone ? null : renderHeader("owner", t`Owner`)}
              <th scope="col" className="is-num" style={{ width: 44 }}>
                <span className="dw-sr">
                  <Trans>Actions</Trans>
                </span>
              </th>
            </tr>
          </thead>
          <tbody>
            {waiting ? (
              [168, 128, 192, 144, 112, 160].map((width) => (
                <tr key={width} className="dw-table-skeleton-row">
                  <td className="files-col-select" />
                  <td>
                    <Skeleton text="label" width={width} />
                  </td>
                  <td className="is-num">
                    <Skeleton text="label" width={40} className="dw-table-skeleton-num" />
                  </td>
                  {phone ? null : (
                    <>
                      <td>
                        <Skeleton text="label" width={64} />
                      </td>
                      <td>
                        <Skeleton text="code-sm" width={88} />
                      </td>
                      <td>
                        <Skeleton text="code-sm" width={68} />
                      </td>
                    </>
                  )}
                  <td className="is-num">
                    <Skeleton shape="block" width={28} height={28} className="dw-table-skeleton-action" />
                  </td>
                </tr>
              ))
            ) : rows.length === 0 ? (
              <tr>
                <td colSpan={colCount} className="dw-table-empty">
                  {empty}
                </td>
              </tr>
            ) : (
              rows.map((row, index) => {
                const isSelected = selected.has(row.name);
                const isActive = isSelected || activeFileName === row.name;
                const isRenaming = renaming === row.name;
                const canChange = changeable(row);
                const opens = !row.entry.unreadableName;
                return (
                  <tr
                    key={row.name}
                    ref={(el) => {
                      if (el) rowRefs.current.set(row.name, el);
                      else rowRefs.current.delete(row.name);
                    }}
                    data-file-row={row.name}
                    className={cx(opens && "is-clickable", isActive && "is-selected")}
                    aria-selected={isSelected}
                    tabIndex={opens ? 0 : undefined}
                    onClick={(event) => handleRowClick(row, event)}
                    onDoubleClick={(event) => handleRowDoubleClick(row, event)}
                    onKeyDown={(event) => handleRowKeyDown(row, index, event)}
                    onContextMenu={(event) => {
                      event.preventDefault();
                      event.stopPropagation();
                      if (!selected.has(row.name)) {
                        onSelectChange(new Set([row.name]), row.name);
                      }
                      onRowContextMenu(row, { position: { x: event.clientX, y: event.clientY } });
                    }}
                  >
                    <td className="files-col-select">
                      <input
                        type="checkbox"
                        className="files-checkbox"
                        checked={isSelected}
                        aria-label={t`Select ${row.name}`}
                        onClick={(event) => {
                          event.stopPropagation();
                          if (event.shiftKey) {
                            event.preventDefault();
                            toggleCheckbox(row, true);
                          }
                        }}
                        onChange={() => toggleCheckbox(row, false)}
                      />
                    </td>
                    <td>
                      {isRenaming ? (
                        <InlineRename
                          initial={row.name}
                          isFolder={row.folder}
                          busy={renameBusy}
                          error={renameError}
                          onCommit={(next) => onCommitRename(row, next)}
                          onCancel={onCancelRename}
                        />
                      ) : (
                        <span className="files-name">
                          <span className="files-icon">
                            <Icon name={iconOf(row)} size={15} />
                          </span>
                          <span className="files-entry-name" title={row.name}>
                            {row.name}
                          </span>
                          {row.entry.linkTarget ? (
                            <span className="files-link-target">→ {row.entry.linkTarget}</span>
                          ) : null}
                          {row.entry.refused ? (
                            <span className="files-icon" title={t`Dockwell doesn't open this`}>
                              <Icon name="lock" size={13} />
                              <span className="dw-sr">
                                <Trans>Dockwell doesn't open this</Trans>
                              </span>
                            </span>
                          ) : null}
                        </span>
                      )}
                    </td>
                    <td className="is-num is-muted">
                      {row.folder || row.entry.type === FileType.SYMLINK
                        ? (folderSize?.(row) ?? "—")
                        : formatBytes(row.entry.sizeBytes, i18n.locale)}
                    </td>
                    {phone ? null : (
                      <>
                        <td className="is-muted">
                          {row.entry.modifyTime ? words.ago(timestampDate(row.entry.modifyTime), now) : "—"}
                        </td>
                        <td className="is-mono is-muted">{row.entry.mode}</td>
                        <td className="is-mono is-muted">{row.entry.owner}</td>
                      </>
                    )}
                    <td className="is-num">
                      {canChange ? (
                        <span className="files-row-actions">
                          <Button
                            size="sm"
                            variant="ghost"
                            icon="more"
                            aria-label={t`Actions for ${row.name}`}
                            aria-haspopup="menu"
                            onClick={(event) => {
                              event.stopPropagation();
                              onRowContextMenu(row, { anchor: event.currentTarget });
                            }}
                          />
                        </span>
                      ) : null}
                    </td>
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

/** Inline rename field inside a table row's Name cell. */
function InlineRename({
  initial,
  isFolder,
  busy,
  error,
  onCommit,
  onCancel,
}: {
  initial: string;
  isFolder: boolean;
  busy: boolean;
  error?: string | undefined;
  onCommit: (nextName: string) => void;
  onCancel: () => void;
}) {
  const { t } = useLingui();
  const [value, setValue] = useState(initial);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const input = inputRef.current;
    if (!input) return;
    input.focus();
    // Select the stem before the extension for ordinary files, or the whole name for folders/dotfiles.
    const dot = !isFolder ? initial.lastIndexOf(".") : -1;
    if (dot > 0) input.setSelectionRange(0, dot);
    else input.select();
  }, [initial, isFolder]);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    event.stopPropagation();
    if (!busy) onCommit(value);
  };

  return (
    <form className="files-inline-rename" onSubmit={submit}>
      <input
        ref={inputRef}
        type="text"
        className={cx("files-inline-rename-input", error && "is-invalid")}
        aria-label={t`New name for ${initial}`}
        aria-invalid={error ? true : undefined}
        value={value}
        disabled={busy}
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={(event) => {
          event.stopPropagation();
          if (event.key === "Escape") {
            event.preventDefault();
            onCancel();
          }
        }}
      />
      <Button size="sm" variant="ghost" icon="check" type="submit" loading={busy} aria-label={t`Save name`} />
      <Button
        size="sm"
        variant="ghost"
        icon="x"
        type="button"
        disabled={busy}
        aria-label={t`Cancel rename`}
        onClick={onCancel}
      />
      {error ? (
        <span className="files-inline-rename-error" role="alert">
          {error}
        </span>
      ) : null}
    </form>
  );
}
