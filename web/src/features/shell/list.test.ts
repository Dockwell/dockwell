// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { nextSort, pagedRows, readListSearch, readSort, sortedRows, sortParam } from "./list";

describe("a list's place in its address", () => {
  it("reads only a filter, a sort and a page past the first", () => {
    expect(readListSearch({ q: "shop", sort: "-size", page: "3", other: "x" })).toEqual({
      q: "shop",
      sort: "-size",
      page: 3,
    });
    expect(readListSearch({ q: "", sort: "No Caps", page: 1 })).toEqual({});
    expect(readListSearch({ page: "0" })).toEqual({});
  });

  it("round-trips a sort, and drops one the list has no column for", () => {
    expect(readSort(sortParam({ key: "name", dir: "desc" }), ["name"])).toEqual({ key: "name", dir: "desc" });
    expect(readSort("-size", ["name"])).toBeUndefined();
    expect(sortParam(undefined)).toBeUndefined();
  });

  it("turns a column's sort the other way, and starts a new one up", () => {
    expect(nextSort(undefined, "name")).toEqual({ key: "name", dir: "asc" });
    expect(nextSort({ key: "name", dir: "asc" }, "name")).toEqual({ key: "name", dir: "desc" });
    expect(nextSort({ key: "name", dir: "desc" }, "size")).toEqual({ key: "size", dir: "asc" });
  });
});

describe("sorting and paging a list", () => {
  const rows = [
    { name: "blog", size: 10 },
    { name: "api", size: null },
    { name: "shop 2", size: 3 },
    { name: "shop 10", size: 3 },
  ];
  const by = (row: (typeof rows)[number], key: string) => (key === "size" ? row.size : row.name);

  it("sorts as people read names, keeps equals in their order, and puts no value last either way", () => {
    expect(sortedRows(rows, { key: "name", dir: "asc" }, by).map((r) => r.name)).toEqual([
      "api",
      "blog",
      "shop 2",
      "shop 10",
    ]);
    expect(sortedRows(rows, { key: "size", dir: "asc" }, by).map((r) => r.name)).toEqual([
      "shop 2",
      "shop 10",
      "blog",
      "api",
    ]);
    expect(sortedRows(rows, { key: "size", dir: "desc" }, by).map((r) => r.name)).toEqual([
      "blog",
      "shop 2",
      "shop 10",
      "api",
    ]);
    expect(sortedRows(rows, undefined, by)).toEqual(rows);
  });

  it("pages over the whole list and holds a page past the end to the last", () => {
    const many = Array.from({ length: 312 }, (_, i) => i + 1);
    const first = pagedRows(many, 0);
    expect([first.from, first.to, first.total, first.pages]).toEqual([1, 50, 312, 7]);
    const past = pagedRows(many, 99);
    expect([past.page, past.from, past.to, past.rows.length]).toEqual([6, 301, 312, 12]);
    expect(pagedRows([], 2)).toMatchObject({ page: 0, from: 0, to: 0, total: 0 });
  });
});
