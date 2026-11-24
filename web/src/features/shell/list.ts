// SPDX-License-Identifier: AGPL-3.0-only

/**
 * What every list page keeps in its address: what it's filtered by, how it's
 * sorted ("name", or "-name" for the other way), and which page of it shows,
 * from 1. A page's own search fields sit beside these.
 */
export interface ListSearch {
  q?: string;
  sort?: string;
  page?: number;
}

/** Rows on one page of a list, everywhere: a screenful and a half, not a wall. */
export const LIST_PAGE_SIZE = 50;

/** Reads every cursor when a view needs a global order or a complete set of references. */
export async function readListPages<Page extends { nextPageToken: string }>(
  read: (pageToken: string) => Promise<Page>,
  append: (page: Page) => void,
): Promise<void> {
  const seen = new Set<string>();
  let token = "";
  do {
    seen.add(token);
    const page = await read(token);
    append(page);
    token = page.nextPageToken;
    if (token && seen.has(token)) throw new Error("The list returned a repeated page cursor");
  } while (token);
}

/** Reads a list's place from a route's search, leaving out what isn't one. */
export function readListSearch(search: Record<string, unknown>): ListSearch {
  const out: ListSearch = {};
  if (typeof search["q"] === "string" && search["q"] !== "") out.q = search["q"];
  if (typeof search["sort"] === "string" && /^-?[a-z][a-z0-9-]*$/.test(search["sort"])) out.sort = search["sort"];
  const page = search["page"];
  if (typeof page === "number" && Number.isSafeInteger(page) && page > 1) out.page = page;
  else if (
    typeof page === "string" &&
    /^[1-9][0-9]*$/.test(page) &&
    Number.isSafeInteger(Number(page)) &&
    Number(page) > 1
  )
    out.page = Number(page);
  return out;
}

/** How a list is sorted: by one of its columns, one way or the other. */
export interface ListSort {
  key: string;
  dir: "asc" | "desc";
}

/** A sort as the address holds it, read back. Nothing there, or nothing known, is the page's own order. */
export function readSort(param: string | undefined, keys: readonly string[]): ListSort | undefined {
  if (!param) return undefined;
  const dir = param.startsWith("-") ? "desc" : "asc";
  const key = dir === "desc" ? param.slice(1) : param;
  return keys.includes(key) ? { key, dir } : undefined;
}

/** A sort as the address holds it. */
export function sortParam(sort: ListSort | undefined): string | undefined {
  if (!sort) return undefined;
  return sort.dir === "desc" ? `-${sort.key}` : sort.key;
}

/** The next sort for a column's head: the other way on the same column, up on a new one. */
export function nextSort(sort: ListSort | undefined, key: string): ListSort {
  return sort?.key === key && sort.dir === "asc" ? { key, dir: "desc" } : { key, dir: "asc" };
}

const collator = new Intl.Collator(undefined, { sensitivity: "base", numeric: true });

/** What a row sorts by: its words, a number, or nothing it can be ordered by. */
export type SortValue = string | number | null | undefined;

/**
 * The rows in a sort's order, steady among equals, with what has no value last
 * either way, so an unread size never leads a list sorted smallest first. With
 * no sort, the rows as the page ordered them.
 */
export function sortedRows<Row>(
  rows: readonly Row[],
  sort: ListSort | undefined,
  value: (row: Row, key: string) => SortValue,
): Row[] {
  if (!sort) return [...rows];
  const way = sort.dir === "desc" ? -1 : 1;
  return rows
    .map((row, index) => ({ row, index, by: value(row, sort.key) }))
    .sort((a, b) => {
      if (a.by == null || b.by == null) {
        if (a.by == null && b.by == null) return a.index - b.index;
        return a.by == null ? 1 : -1;
      }
      const order =
        typeof a.by === "number" && typeof b.by === "number"
          ? a.by - b.by
          : collator.compare(String(a.by), String(b.by));
      return order !== 0 ? order * way : a.index - b.index;
    })
    .map(({ row }) => row);
}

/** One page of a list, and where it sits in the whole: "1-50 of 312" is from, to and total. */
export interface ListPage<Row> {
  rows: Row[];
  /** The page showing, from 0, held to the pages there are. */
  page: number;
  pages: number;
  /** The first row showing, from 1, or 0 with none. */
  from: number;
  to: number;
  total: number;
}

/** The page of the rows that shows, over the full list, for a list the API reads whole. */
export function pagedRows<Row>(rows: readonly Row[], page: number, pageSize = LIST_PAGE_SIZE): ListPage<Row> {
  const total = rows.length;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const held = Math.min(Math.max(0, page), pages - 1);
  const start = held * pageSize;
  return {
    rows: rows.slice(start, start + pageSize),
    page: held,
    pages,
    from: total === 0 ? 0 : start + 1,
    to: Math.min(total, start + pageSize),
    total,
  };
}
