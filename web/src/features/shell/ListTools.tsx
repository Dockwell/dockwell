// SPDX-License-Identifier: AGPL-3.0-only

import {
  Button,
  cx,
  Icon,
  type IconName,
  SelectField,
  Table,
  type TableColumn,
  type TableProps,
  TextField,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import {
  type CSSProperties,
  type FocusEvent,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useDeferredValue,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import type { Offer } from "../auth/may";
import { ActionMenu, type MenuAction, menuItem } from "./ActionMenu";
import { EmptyState } from "./EmptyState";
import {
  type ListPage,
  type ListSearch,
  type ListSort,
  nextSort,
  pagedRows,
  rangeIds,
  readSort,
  type SortValue,
  sortedRows,
  sortParam,
  toggledIds,
} from "./list";
import "./ListTools.css";

/** Where a list is: its filter words, its sort and its page, kept in its address. */
export interface ListPlace {
  /** What the search field holds, key by key. */
  typed: string;
  setTyped: (words: string) => void;
  /** The words to filter by: trimmed, lowercased, and a beat behind the typing. */
  query: string;
  /** Empties the search field and the filter with it. */
  clearQuery: () => void;
  sort: ListSort | undefined;
  /** A column head's click: the other way on the same column, up on a new one. Back to the first page. */
  toggleSort: (key: string) => void;
  setSort: (sort: ListSort | undefined) => void;
  /** How this column sorts the rows, for its head to say to assistive technology. */
  sortOf: (key: string) => "ascending" | "descending" | undefined;
  /** The page showing, from 0. */
  page: number;
  setPage: (page: number) => void;
}

export interface ListRouteProps<S extends ListSearch = ListSearch> {
  search: S;
  onSearch: (search: S) => void;
}

/**
 * A list's place, in its address where the page has a route to hold it (search and
 * onSearch), and on the page where it hasn't yet. The filter goes in the address a
 * moment after typing stops, so a link to the list keeps it, without a navigation
 * for each key; a new filter or sort starts again at the first page.
 */
export function useListPlace<S extends ListSearch>(
  search: S,
  onSearch: ((next: S) => void) | undefined,
  sortKeys: readonly string[],
): ListPlace {
  const [inner, setInner] = useState<S>(search);
  const here = onSearch ? search : inner;
  const write = useCallback((next: S) => (onSearch ? onSearch(next) : setInner(next)), [onSearch]);
  const addressQuery = here.q ?? "";
  const [seenQuery, setSeenQuery] = useState(addressQuery);
  const [typed, setTyped] = useState(addressQuery);
  if (addressQuery !== seenQuery) {
    setSeenQuery(addressQuery);
    setTyped(addressQuery);
  }
  const settled = useDeferredValue(typed);
  useEffect(() => {
    if (settled !== typed || settled === addressQuery) return;
    const timer = setTimeout(() => write({ ...here, q: settled || undefined, page: undefined }), 300);
    return () => clearTimeout(timer);
  }, [addressQuery, here, settled, typed, write]);
  const sort = readSort(here.sort, sortKeys);
  const setSort = (sort: ListSort | undefined) => write({ ...here, sort: sortParam(sort), page: undefined });
  return {
    typed,
    setTyped,
    query: settled.trim().toLowerCase(),
    clearQuery: () => {
      setTyped("");
      write({ ...here, q: undefined, page: undefined });
    },
    sort,
    toggleSort: (key) => setSort(nextSort(sort, key)),
    setSort,
    sortOf: (key) => (sort?.key === key ? (sort.dir === "asc" ? "ascending" : "descending") : undefined),
    page: settled === addressQuery ? (here.page ?? 1) - 1 : 0,
    setPage: (page) => write({ ...here, q: settled || undefined, page: page > 0 ? page + 1 : undefined }),
  };
}

/** Give a list's existing search field the common keyboard shortcut. */
export function useListSearchShortcut() {
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const keys = (event: globalThis.KeyboardEvent) => {
      if (event.defaultPrevented || event.key !== "/" || event.ctrlKey || event.metaKey || event.altKey) return;
      const target = event.target as HTMLElement | null;
      if (target?.closest("input, textarea, select, [contenteditable], [role=dialog], [role=menu]")) return;
      if (!input.current || input.current.disabled || document.querySelector('[role="dialog"][aria-modal="true"]'))
        return;
      event.preventDefault();
      input.current.focus();
    };
    document.addEventListener("keydown", keys);
    return () => document.removeEventListener("keydown", keys);
  }, []);
  return input;
}

/**
 * A list's search field: filters as you type, and `/` from anywhere on the page
 * puts you in it, unless you are already typing somewhere.
 */
export function ListSearchField({
  label,
  placeholder,
  value,
  onChange,
  className,
  size = "sm",
  disabled,
}: {
  /** For a screen reader, such as "Filter projects". */
  label: string;
  placeholder: string;
  value: string;
  onChange: (words: string) => void;
  className?: string;
  size?: "sm" | "lg";
  disabled?: boolean;
}) {
  const input = useListSearchShortcut();
  return (
    <TextField
      ref={input}
      className={cx("list-search", className)}
      aria-label={label}
      placeholder={placeholder}
      icon="search"
      type="search"
      size={size}
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}

/** A column's head that sorts by it: its name, and a chevron saying which way, or that it can. */
export function SortHeader({ label, sortKey, place }: { label: string; sortKey: string; place: ListPlace }) {
  const active = place.sort?.key === sortKey ? place.sort.dir : undefined;
  return (
    <button type="button" className={cx("list-sort", active && "is-sorted")} onClick={() => place.toggleSort(sortKey)}>
      <span>{label}</span>
      <Icon name={active === "asc" ? "chevron-up" : active === "desc" ? "chevron-down" : "chevron-up-down"} size={12} />
    </button>
  );
}

export function ListSortField({ place, columns }: { place: ListPlace; columns: Record<string, string> }) {
  const { t } = useLingui();
  return (
    <SelectField
      aria-label={t`Sort by`}
      size="sm"
      value={sortParam(place.sort) ?? ""}
      onChange={(event) => place.setSort(readSort(event.target.value, Object.keys(columns)))}
      options={[
        { value: "", label: t`Default order` },
        ...Object.entries(columns).flatMap(([key, label]) => [
          { value: key, label: t`${label} ascending` },
          { value: `-${key}`, label: t`${label} descending` },
        ]),
      ]}
    />
  );
}

export function sortableColumns<Row>(
  columns: readonly TableColumn<Row>[],
  place: ListPlace,
  keys: readonly string[],
): TableColumn<Row>[] {
  return columns.map((column) =>
    keys.includes(column.key) && typeof column.header === "string"
      ? {
          ...column,
          header: <SortHeader label={column.header} sortKey={column.key} place={place} />,
          sort: place.sortOf(column.key),
        }
      : column,
  );
}

export function ListToolbar({ place, label, children }: { place: ListPlace; label: string; children?: ReactNode }) {
  return (
    <div className="list-bar">
      <ListSearchField label={label} placeholder={label} value={place.typed} onChange={place.setTyped} />
      {children}
    </div>
  );
}

export function ListNoMatch({ clear }: { clear: () => void }) {
  const { t } = useLingui();
  return (
    <EmptyState
      icon="search"
      title={t`Nothing matches`}
      action={
        <Button onClick={clear}>
          <Trans>Clear the filter</Trans>
        </Button>
      }
    />
  );
}

/** Page a card section independently while sharing a page-wide filter. */
export function useListSection<Row>(rows: readonly Row[], query: string, name: (row: Row) => string) {
  const [position, setPosition] = useState({ query, page: 0 });
  if (query !== position.query) setPosition({ query, page: 0 });
  return {
    page: pagedRows(
      rows.filter((row) => name(row).toLowerCase().includes(query)),
      query === position.query ? position.page : 0,
    ),
    onPage: (page: number) => setPosition({ query, page }),
  };
}

/** Sort and page a read-only section within a larger view. */
export function ListSectionTable<Row extends object>({
  query,
  rows,
  columns,
  values,
  ...table
}: TableProps<Row> & {
  query: string;
  values: Record<string, (row: Row) => SortValue>;
}) {
  const place = useListPlace({}, undefined, Object.keys(values));
  const matched = query
    ? rows.filter((row) =>
        Object.values(values).some((value) =>
          String(value(row) ?? "")
            .toLowerCase()
            .includes(query),
        ),
      )
    : rows;
  const page = pagedRows(
    sortedRows(matched, place.sort, (row, key) => values[key]?.(row)),
    place.page,
  );
  return (
    <>
      <Table<Row> {...table} columns={sortableColumns(columns, place, Object.keys(values))} rows={page.rows} />
      <ListPager page={page} onPage={place.setPage} />
    </>
  );
}

/** Under a list too long for one page: where you are in it, and the way to the next page of it. */
export function ListPager<Row>({
  page,
  onPage,
  more = false,
  busy = false,
}: {
  page: ListPage<Row>;
  onPage: (page: number) => void;
  more?: boolean;
  busy?: boolean;
}) {
  const { t } = useLingui();
  const shown = useRef(page.page);
  const [announcement, setAnnouncement] = useState("");
  const count = more ? t`${page.from}-${page.to} of ${page.total}+` : t`${page.from}-${page.to} of ${page.total}`;
  useEffect(() => {
    if (shown.current === page.page) return;
    shown.current = page.page;
    const number = page.page + 1;
    setAnnouncement(t`Page ${number}`);
  }, [page.page, t]);
  if (page.pages <= 1 && !more) return null;
  return (
    <div className="list-pager">
      <span className="list-pager-count">{count}</span>
      <span className="dw-sr" aria-live="polite">
        {announcement}
      </span>
      <span className="list-pager-buttons">
        <Button
          variant="ghost"
          size="sm"
          icon="arrow-left"
          disabled={page.page === 0}
          onClick={() => onPage(page.page - 1)}
        >
          <Trans>Previous</Trans>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          icon="arrow-right"
          disabled={!more && page.page >= page.pages - 1}
          loading={busy}
          onClick={() => onPage(page.page + 1)}
        >
          <Trans>Next</Trans>
        </Button>
      </span>
    </div>
  );
}

/** Rows picked to act on together, by their ids in the order the list shows them. */
export interface RowSelection {
  enabled: boolean;
  list: RefObject<HTMLElement | null>;
  /** The picked ids among the rows shown, in their order. */
  selected: string[];
  count: number;
  any: boolean;
  has: (id: string) => boolean;
  /** Picks or lets go of one row, or with shift everything from the last one picked to it. */
  toggle: (id: string, on: boolean, range?: boolean) => void;
  setAll: (ids: readonly string[], on: boolean) => void;
  clear: () => void;
  /**
   * For the list's own element: Cmd or Ctrl+A picks every row shown, and Escape
   * clears. Typing somewhere in the list keeps its own keys.
   */
  onKeyDown: (event: KeyboardEvent<HTMLElement>) => void;
}

/**
 * Which rows are picked, against the ids the list shows now in their order.
 * The boxes show on hover and while anything is picked (ListTools.css); a page
 * adds `has-selection` to its table and `is-checked` to a picked row.
 */
export function useRowSelection(
  order: readonly string[],
  enabled = true,
  fallback?: RefObject<HTMLElement | null>,
): RowSelection {
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const [anchor, setAnchor] = useState<string>();
  const list = useRef<HTMLElement>(null);
  const selected = enabled ? order.filter((id) => picked.has(id)) : [];
  if (selected.length < picked.size) setPicked(new Set(selected));
  if (anchor !== undefined && (!enabled || !order.includes(anchor))) setAnchor(undefined);
  const setAll = (ids: readonly string[], on: boolean) => {
    if (enabled) setPicked((was) => toggledIds(was, ids, on));
  };
  const clear = useCallback(() => {
    setPicked(new Set());
    setAnchor(undefined);
    requestAnimationFrame(() => (list.current ?? fallback?.current)?.focus({ preventScroll: true }));
  }, [fallback]);
  return {
    enabled,
    list,
    selected,
    count: selected.length,
    any: selected.length > 0,
    has: (id) => enabled && picked.has(id),
    toggle: (id, on, range = false) => {
      setAll(range ? rangeIds(order, anchor, id) : [id], on);
      setAnchor(id);
    },
    setAll,
    clear,
    onKeyDown: (event) => {
      const target = event.target as HTMLElement | null;
      if (
        !enabled ||
        event.defaultPrevented ||
        target?.closest("input:not([type='checkbox']), textarea, select, [contenteditable], [role=dialog], [role=menu]")
      )
        return;
      if (event.key === "Escape" && picked.size > 0) {
        event.preventDefault();
        clear();
      } else if ((event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === "a") {
        if (order.length === 0) return;
        event.preventDefault();
        setAll(order, true);
      }
    },
  };
}

/** A row's box for picking it, part-checked where some of what it stands for is picked. */
export function SelectBox({
  checked,
  partly = false,
  label,
  onPick,
}: {
  checked: boolean;
  partly?: boolean;
  label: string;
  onPick: (on: boolean, range: boolean) => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = partly && !checked;
  }, [partly, checked]);
  return (
    <input
      ref={ref}
      type="checkbox"
      className="list-select-box"
      checked={checked}
      aria-label={label}
      onChange={(event) =>
        onPick(event.currentTarget.checked, (event.nativeEvent as globalThis.MouseEvent).shiftKey === true)
      }
    />
  );
}

/**
 * The column of boxes at a list's start: one a row, and the head's for every row
 * shown. A row with no id has no box: a heading row picks its own rows instead.
 */
export function selectionColumn<Row>({
  selection,
  rows,
  idOf,
  labelOf,
  allLabel,
}: {
  selection: RowSelection;
  rows: readonly Row[];
  idOf: (row: Row) => string | undefined;
  labelOf: (row: Row) => string;
  allLabel: string;
}): TableColumn<Row> {
  const ids = rows.flatMap((row) => {
    const id = idOf(row);
    return id === undefined ? [] : [id];
  });
  const on = ids.filter((id) => selection.has(id)).length;
  return {
    key: "select",
    width: 36,
    skeleton: "none",
    header: (
      <SelectBox
        checked={ids.length > 0 && on === ids.length}
        partly={on > 0}
        label={allLabel}
        onPick={(yes) => selection.setAll(ids, yes)}
      />
    ),
    render: (row) => {
      const id = idOf(row);
      if (id === undefined) return null;
      return (
        <SelectBox
          checked={selection.has(id)}
          label={labelOf(row)}
          onPick={(yes, range) => selection.toggle(id, yes, range)}
        />
      );
    },
  };
}

/** One thing the bar under a list does to every row picked. */
export interface BatchAction {
  id: string;
  label: ReactNode;
  icon?: IconName;
  danger?: boolean;
  /** Why it can't be done, kept on the button as its description. */
  off?: string | undefined;
  disabled?: boolean;
  onSelect: () => void;
}

/** A row may be left out by its state, its scope, or a check still in progress. */
export type BatchRefusal = { reason: string } | { permission: Offer } | { pending: true };

export function combineBatchActions(...batches: { actions: BatchAction[]; notes: string[] }[]) {
  const actions = batches.flatMap((batch) => batch.actions);
  const notes = batches.flatMap((batch) => (actions.length === 0 || batch.actions.length > 0 ? batch.notes : []));
  return { actions, notes: [...new Set(notes)] };
}

export function useBatchAction<Row>() {
  const { t } = useLingui();
  return (
    rows: readonly Row[],
    refusal: (row: Row) => BatchRefusal | undefined,
    action: Omit<BatchAction, "label" | "onSelect"> & {
      label: (count?: number) => string;
      onSelect: (rows: Row[], notes: string[]) => void;
    },
  ): { actions: BatchAction[]; notes: string[] } => {
    const targets: Row[] = [];
    const reasons = new Map<string, number>();
    let permission: Offer | undefined;
    let pending = false;
    for (const row of rows) {
      const no = refusal(row);
      if (!no) {
        targets.push(row);
        continue;
      }
      const reason = "permission" in no ? no.permission.reason : "reason" in no ? no.reason : "";
      if ("permission" in no) permission ??= no.permission;
      if (reason) reasons.set(reason, (reasons.get(reason) ?? 0) + 1);
      else pending = true;
    }
    const notes = [...reasons].map(([reason, count]) => t`${count} skipped (${reason})`);
    if (targets.length === 0 && !permission && !pending) return { actions: [], notes };
    return {
      actions: [
        {
          ...action,
          label: action.label(targets.length || undefined),
          ...(targets.length === 0 ? (permission?.off ?? { disabled: true }) : {}),
          onSelect: () => action.onSelect(targets, notes),
        },
      ],
      notes: targets.length > 0 ? notes : [],
    };
  };
}

/**
 * What picked rows can be done with together, once any are: how many, the verbs
 * they allow, and a line for the ones a verb leaves out ("2 of 5 can't be
 * stopped"), which the page counts. A batch that changes managed state is one
 * plan for all of them, which the verb opens.
 */
export function BatchBar({
  count,
  actions,
  notes = [],
  onClear,
}: {
  count: number;
  actions: readonly BatchAction[];
  notes?: readonly string[];
  onClear: () => void;
}) {
  const { t } = useLingui();
  const [hasSelection, setHasSelection] = useState(false);
  if (count > 0 && !hasSelection) setHasSelection(true);
  const countLabel = plural(count, { one: "# selected", other: "# selected" });
  return (
    <>
      <span className="dw-sr" aria-live="polite" aria-atomic="true">
        {count > 0 ? countLabel : hasSelection ? t`Selection cleared` : ""}
      </span>
      {count > 0 ? (
        <div
          className="list-batch-bar"
          role="toolbar"
          aria-label={t`Actions for the rows selected`}
          onKeyDown={(event) => {
            if (event.key !== "Escape") return;
            event.preventDefault();
            event.stopPropagation();
            onClear();
          }}
        >
          <span className="list-batch-count">{countLabel}</span>
          <span className="list-batch-actions">
            {actions.map((action) => (
              <Button
                key={action.id}
                size="sm"
                variant={action.danger ? "danger" : "secondary"}
                icon={action.icon}
                // MenuItem's off, for a Button: off with why keeps it in place (ADR 0059).
                {...(action.off ? { off: action.off } : {})}
                disabled={action.disabled}
                onClick={action.onSelect}
              >
                {action.label}
              </Button>
            ))}
          </span>
          {notes.length > 0 ? <span className="list-batch-note">{notes.join(" · ")}</span> : null}
          <span className="list-batch-grow" />
          <Button size="sm" variant="ghost" icon="x" aria-label={t`Clear the selection`} onClick={onClear}>
            <Trans>Clear</Trans>
          </Button>
        </div>
      ) : null}
    </>
  );
}

/** The same menu and optional hover shortcuts on tables and cards. */
export function RowActions({
  label,
  actions,
  quick = [],
  onOpen,
}: {
  label: string;
  actions: readonly MenuAction[];
  quick?: readonly string[];
  onOpen?: () => void;
}) {
  return (
    <span className="list-row-actions">
      {actions.map((action) =>
        quick.includes(action.id) &&
        !action.off &&
        !action.disabled &&
        action.onSelect &&
        typeof action.label === "string" ? (
          <Button
            key={action.id}
            className="list-row-quick"
            icon={action.icon}
            variant="ghost"
            size="sm"
            aria-label={action.label}
            title={action.label}
            onClick={action.onSelect}
          />
        ) : null,
      )}
      <ActionMenu label={label} actions={actions} onOpen={onOpen} />
    </span>
  );
}

/** Context-menu behavior for the compact layout, where there is no Table. */
export function cardContextMenu(
  event: MouseEvent<HTMLElement>,
  open: (point: { x: number; y: number }, row: HTMLElement) => void,
) {
  const target = event.target as HTMLElement;
  if (event.defaultPrevented || target.closest("input, [role=menu], [aria-haspopup=menu]")) return;
  event.preventDefault();
  const row = event.currentTarget;
  const anchor = target.closest<HTMLElement>("a[href], button") ?? row.querySelector("a[href], button") ?? row;
  open({ x: event.clientX, y: event.clientY }, anchor as HTMLElement);
}

interface RowMenuOpen<Row> {
  row: Row;
  label: string;
  actions: readonly MenuAction[];
  x: number;
  y: number;
  /** The row it was asked from, which takes focus back when it closes from the keyboard. */
  back: HTMLElement | null;
}

/**
 * A row's menu where it was asked for: on a right-click or Shift+F10, at that
 * point, with the same items its ⋯ button has. Open it from a Table's
 * onRowContextMenu, and put its element anywhere on the page.
 */
export function useRowMenu<Row>(resolve?: (row: Row) => Pick<RowMenuOpen<Row>, "label" | "actions">): {
  open: (
    row: Row,
    at: { x: number; y: number },
    rowElement: HTMLElement,
    menu?: { label: string; actions: readonly MenuAction[] },
  ) => void;
  element: ReactNode;
} {
  const [showing, setShowing] = useState<RowMenuOpen<Row> | null>(null);
  const close = useCallback(
    (back: boolean) => {
      setShowing(null);
      if (back) showing?.back?.focus();
    },
    [showing],
  );
  const open = (row: Row, at: { x: number; y: number }, rowElement: HTMLElement, menu = resolve?.(row)) => {
    if (!menu?.actions.length) return;
    setShowing({ ...menu, row, x: at.x, y: at.y, back: rowElement });
  };
  const menu = showing ? (resolve?.(showing.row) ?? showing) : undefined;
  const element =
    showing && menu ? (
      <RowContextMenu
        label={menu.label}
        actions={menu.actions}
        x={showing.x}
        y={showing.y}
        anchor={showing.back}
        onClose={close}
      />
    ) : null;
  return { open, element };
}

/** The menu itself, placed on the viewport at the point it was asked for, held inside it. */
function RowContextMenu({
  label,
  actions,
  x,
  y,
  anchor,
  onClose,
}: {
  label: string;
  actions: readonly MenuAction[];
  x: number;
  y: number;
  anchor: HTMLElement | null;
  /** True from the keyboard, so focus goes back to the row; a click elsewhere keeps its own. */
  onClose: (back: boolean) => void;
}) {
  const root = useRef<HTMLDivElement>(null);
  const [at, setAt] = useState<CSSProperties>({ left: x, top: y });
  // biome-ignore lint/correctness/useExhaustiveDependencies: A live eligibility reason changes the menu height.
  useLayoutEffect(() => {
    const box = root.current?.getBoundingClientRect();
    if (!box) return;
    const gutter = 16;
    setAt({
      left: Math.max(gutter, Math.min(x, window.innerWidth - box.width - gutter)),
      top: Math.max(gutter, Math.min(y, window.innerHeight - box.height - gutter)),
    });
  }, [actions, x, y]);
  useEffect(() => {
    const first =
      root.current?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"])') ??
      root.current?.querySelector<HTMLElement>('[role="menuitem"]');
    first?.focus({ preventScroll: true });
    const away = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) onClose(false);
    };
    const before = anchor?.getBoundingClientRect();
    const moved = () => {
      const now = anchor?.getBoundingClientRect();
      if (
        !anchor?.isConnected ||
        !before ||
        !now ||
        Math.abs(now.top - before.top) > 1 ||
        Math.abs(now.left - before.left) > 1
      ) {
        onClose(false);
      }
    };
    const resized = () => onClose(false);
    document.addEventListener("pointerdown", away);
    window.addEventListener("scroll", moved, true);
    window.addEventListener("resize", resized);
    return () => {
      document.removeEventListener("pointerdown", away);
      window.removeEventListener("scroll", moved, true);
      window.removeEventListener("resize", resized);
    };
  }, [anchor, onClose]);
  const keys = (event: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(root.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    const index = items.indexOf(document.activeElement as HTMLElement);
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      onClose(true);
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const step = event.key === "ArrowDown" ? 1 : -1;
      items[(index + step + items.length) % items.length]?.focus();
    } else if (event.key === "Home" || event.key === "End") {
      event.preventDefault();
      items[event.key === "Home" ? 0 : items.length - 1]?.focus();
    }
  };
  const left = (event: FocusEvent<HTMLDivElement>) => {
    if (!root.current?.contains(event.relatedTarget as Node | null)) onClose(false);
  };
  return (
    <div
      ref={root}
      className="action-menu is-placed list-context-menu"
      style={at}
      role="menu"
      aria-label={label}
      onKeyDown={keys}
      onBlur={left}
      onContextMenu={(event) => event.preventDefault()}
    >
      {actions.map((action) => menuItem(action, () => onClose(false)))}
    </div>
  );
}
