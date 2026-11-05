// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { type LogChunk, LogChunkSchema, LogGapReason, LogLevel } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { type Entry, LogStore, MAX_BYTES, MAX_HELD, MAX_LINES, NO_FILTER } from "./store";

const T0 = 1_793_832_000n;

function line(seconds: number, text: string, level = LogLevel.UNSPECIFIED, nanos = 0) {
  return { time: { seconds: T0 + BigInt(seconds), nanos }, text, level };
}

function chunk(sourceId: string, ...lines: ReturnType<typeof line>[]): LogChunk {
  return create(LogChunkSchema, { sourceId, lines });
}

function texts(store: LogStore): string[] {
  const out: string[] = [];
  for (let p = store.start; p < store.start + store.length; p++) out.push(store.at(p)?.text ?? "");
  return out;
}

describe("LogStore", () => {
  it("keeps lines in time order, whichever server's came first", () => {
    const s = new LogStore();
    s.add([chunk("a", line(1, "one"), line(4, "four"))]);
    expect(s.epoch).toBe(0);
    const start = s.epoch;
    s.add([chunk("b", line(2, "two"), line(3, "three"))]);
    expect(texts(s)).toEqual(["one", "two", "three", "four"]);
    // Inserted before what is there: positions no longer mean what they did.
    expect(s.epoch).toBe(start + 1);
    s.add([chunk("a", line(5, "five"))]);
    expect(s.epoch).toBe(start + 1);
    expect(texts(s).at(-1)).toBe("five");
  });

  it("orders lines of one second by their nanoseconds", () => {
    const s = new LogStore();
    s.add([
      chunk("a", line(1, "late", LogLevel.UNSPECIFIED, 900)),
      chunk("b", line(1, "early", LogLevel.UNSPECIFIED, 100)),
    ]);
    expect(texts(s)).toEqual(["early", "late"]);
  });

  it("lets the oldest go at its limit, and says how many", () => {
    const s = new LogStore();
    const many = Array.from({ length: MAX_LINES + 5 }, (_, i) => line(i, `line ${i}`));
    s.add([chunk("a", ...many)]);
    expect(s.length).toBe(MAX_LINES);
    expect(s.evicted).toBe(5);
    expect(s.start).toBe(5);
    expect(s.at(s.start)?.text).toBe("line 5");
    expect(s.at(s.start + s.length - 1)?.text).toBe(`line ${MAX_LINES + 4}`);
    expect(s.at(s.start - 1)).toBeUndefined();
  });

  it("lets the oldest go at its limit of text too", () => {
    const s = new LogStore();
    const big = "x".repeat(1 << 20);
    for (let i = 0; i < 80; i++) s.add([chunk("a", line(i, big))]);
    expect(s.total * (1 << 20)).toBeLessThanOrEqual(MAX_BYTES);
    expect(s.evicted).toBeGreaterThan(0);
  });

  it("filters by level, source and text, and goes on as lines come", () => {
    const s = new LogStore();
    s.setSource("a", "hz-1", "srv/1");
    s.setSource("b", "hz-2", "srv2/1");
    s.add([
      chunk("a", line(1, "GET /", LogLevel.UNSPECIFIED), line(2, "slow", LogLevel.WARN)),
      chunk("b", line(3, "boom in checkout", LogLevel.ERROR), line(4, "debugging", LogLevel.DEBUG)),
    ]);
    s.setFilter({ ...NO_FILTER, levels: new Set([LogLevel.ERROR]) });
    expect(texts(s)).toEqual(["boom in checkout"]);
    // A line with no level, and a debug line, count as info.
    s.setFilter({ ...NO_FILTER, levels: new Set([LogLevel.INFO]) });
    expect(texts(s)).toEqual(["GET /", "debugging"]);
    s.setFilter({ ...NO_FILTER, label: "hz-1" });
    expect(texts(s)).toEqual(["GET /", "slow"]);
    s.setFilter({ ...NO_FILTER, query: "CHECKOUT" });
    expect(texts(s)).toEqual(["boom in checkout"]);
    s.add([chunk("a", line(5, "checkout ok"))]);
    expect(texts(s)).toEqual(["boom in checkout", "checkout ok"]);
    s.setFilter(NO_FILTER);
    expect(s.length).toBe(5);
  });

  it("takes what a search asks for as text, never as a pattern", () => {
    const s = new LogStore();
    s.add([chunk("a", line(1, "a.c"), line(2, "abc"), line(3, "(x"))]);
    s.setFilter({ ...NO_FILTER, query: "a.c" });
    expect(texts(s)).toEqual(["a.c"]);
    s.setFilter({ ...NO_FILTER, query: "(x" });
    expect(texts(s)).toEqual(["(x"]);
  });

  it("searches a line as it reads, not its colour codes", () => {
    const s = new LogStore();
    s.add([chunk("a", line(1, "\u001b[31mfail\u001b[0med"))]);
    s.setFilter({ ...NO_FILTER, query: "failed" });
    expect(s.length).toBe(1);
    s.setFilter({ ...NO_FILTER, query: "31m" });
    expect(s.length).toBe(0);
  });

  it("shows what is missing whatever the filters, and counts it", () => {
    const s = new LogStore();
    s.add([
      create(LogChunkSchema, {
        sourceId: "a",
        gap: { reason: LogGapReason.TOO_FAST, lines: 1234n },
        lines: [line(1, "after", LogLevel.UNSPECIFIED)],
      }),
    ]);
    expect(s.skipped).toBe(1234);
    expect(s.length).toBe(2);
    s.setFilter({ ...NO_FILTER, levels: new Set([LogLevel.ERROR]) });
    expect(s.length).toBe(1);
    expect(s.entryAt(s.start)?.gap?.lines).toBe(1234n);
  });

  it("notes where a new container of a replica starts", () => {
    const s = new LogStore();
    s.setSource("old", "hz-1", "srv/1");
    s.setSource("new", "hz-1", "srv/1");
    s.setSource("other", "hz-2", "srv2/1");
    s.add([chunk("old", line(1, "old line")), chunk("other", line(2, "another replica's"))]);
    expect(s.length).toBe(2);
    s.add([chunk("new", line(3, "new line"))]);
    expect(s.length).toBe(4);
    expect(s.entryAt(s.start + 2)?.started).toBe(true);
    expect(s.labels()).toEqual(["hz-1", "hz-2"]);
  });

  it("finds a line by its ID, to keep a reader's place", () => {
    const s = new LogStore();
    s.add([chunk("a", line(1, "one"), line(2, "two"), line(3, "three"))]);
    const id = s.entryAt(s.start + 1)?.id as number;
    expect(s.positionOf(id)).toBe(s.start + 1);
    s.setFilter({ ...NO_FILTER, query: "t" });
    expect(s.positionOf(id)).toBe(s.start);
    expect(s.positionOf(999)).toBeUndefined();
  });

  it("starts over when cleared, and positions don't carry over", () => {
    const s = new LogStore();
    s.add([chunk("a", line(1, "one"))]);
    const epoch = s.epoch;
    s.clear();
    expect(s.length).toBe(0);
    expect(s.epoch).toBeGreaterThan(epoch);
    s.add([chunk("a", line(2, "two"))]);
    expect(texts(s)).toEqual(["two"]);
  });

  it("goes over a hundred thousand lines quickly", () => {
    const s = new LogStore();
    const many = Array.from({ length: MAX_LINES }, (_, i) => line(i, `GET /v1/products/${i} 200 ${i % 97} ms`));
    const started = performance.now();
    s.add([chunk("a", ...many)]);
    const added = performance.now();
    s.setFilter({ ...NO_FILTER, query: "products/4242 " });
    const searched = performance.now();
    expect(s.length).toBe(1);
    // Generous, as test machines vary: a keystroke's search must not be felt.
    expect(searched - added).toBeLessThan(250);
    expect(added - started).toBeLessThan(1000);
  });

  describe("while it holds still", () => {
    const flood = (store: LogStore, from: number, n: number) =>
      store.add([chunk("a", ...Array.from({ length: n }, (_, i) => line(from + i, `line ${from + i}`)))]);

    it("keeps what shows, and only counts what comes", () => {
      const store = new LogStore();
      flood(store, 0, 3);
      const version = store.length;
      store.setHolding(true);
      flood(store, 3, 5);
      expect(store.length).toBe(version);
      expect(store.arrived).toBe(5);
      expect(store.at(store.start + 2)?.text).toBe("line 2");
    });

    it("loses none of what shows at any rate of lines", () => {
      const store = new LogStore();
      flood(store, 0, 1000);
      const start = store.start;
      store.setHolding(true);
      for (let i = 0; i < 30; i++) flood(store, 1000 + i * 10_000, 10_000);
      expect(store.start).toBe(start);
      expect(store.length).toBe(1000);
      expect(store.arrived).toBe(300_000);
    });

    it("shows what came once it moves again, the newest of it, and says what it let go", () => {
      const store = new LogStore();
      flood(store, 0, 3);
      store.setHolding(true);
      flood(store, 3, MAX_HELD + 10);
      store.setHolding(false);
      expect(store.arrived).toBe(0);
      const last = store.at(store.start + store.length - 1);
      expect(last?.text).toBe(`line ${3 + MAX_HELD + 9}`);
      const marker = store.entryAt(store.start + 3);
      expect(marker?.left).toBe(10);
      // A line can't be confused with it.
      expect(store.oldest()?.text).toBe("line 0");
    });

    it("still takes older lines a reader asked for", () => {
      const store = new LogStore();
      flood(store, 100, 2);
      store.setHolding(true);
      store.addOlder([chunk("a", line(1, "older"))]);
      expect(store.at(store.start)?.text).toBe("older");
    });
  });

  describe("errors", () => {
    const levels = (store: LogStore, lines: [string, number, LogLevel][], source = "a") =>
      store.add([chunk(source, ...lines.map(([text, at, level]) => line(at, text, level)))]);

    it("counts a stack trace as one", () => {
      const store = new LogStore();
      levels(store, [
        ["fine", 0, LogLevel.INFO],
        ["boom", 1, LogLevel.ERROR],
        ["  frame", 1, LogLevel.ERROR],
        ["  frame", 1, LogLevel.ERROR],
        ["again", 9, LogLevel.ERROR],
      ]);
      expect(store.errors).toBe(2);
      expect(store.errorAt(0)?.text).toBe("boom");
      expect(store.errorAt(1)?.text).toBe("again");
    });

    it("finds an error where it is, through a filter", () => {
      const store = new LogStore();
      levels(store, [
        ["fine", 0, LogLevel.INFO],
        ["boom", 1, LogLevel.ERROR],
        ["fine", 2, LogLevel.INFO],
        ["again", 9, LogLevel.ERROR],
      ]);
      const boom = store.errorAt(0) as Entry;
      expect(store.positionOfEntry(boom)).toBe(store.start + 1);
      store.setFilter({ levels: new Set([LogLevel.ERROR]), label: "", query: "" });
      expect(store.positionOfEntry(boom)).toBe(store.start);
      store.setFilter({ levels: new Set([LogLevel.WARN]), label: "", query: "" });
      expect(store.positionOfEntry(boom)).toBeUndefined();
    });

    it("works them out again when another source's lines merge in", () => {
      const store = new LogStore();
      levels(store, [["later", 10, LogLevel.ERROR]]);
      levels(store, [["earlier", 1, LogLevel.ERROR]], "b");
      expect(store.errors).toBe(2);
      expect(store.errorAt(0)?.text).toBe("earlier");
    });

    it("lets one go when it leaves", () => {
      const store = new LogStore();
      levels(store, [["boom", 0, LogLevel.ERROR]]);
      store.add([chunk("a", ...Array.from({ length: MAX_LINES }, (_, i) => line(i + 1, "fine")))]);
      expect(store.errors).toBe(0);
    });
  });

  it("says where the last lines left out were, and doesn't count a search's", () => {
    const store = new LogStore();
    store.add([
      create(LogChunkSchema, {
        sourceId: "a",
        gap: { reason: LogGapReason.TOO_FAST, lines: 12n, endTime: { seconds: T0 + 5n, nanos: 0 } },
        lines: [line(9, "a")],
      }),
    ]);
    expect(store.skipped).toBe(12);
    const skip = store.lastSkip as Entry;
    expect(store.positionOfEntry(skip)).toBe(store.start);
    store.add([
      create(LogChunkSchema, {
        sourceId: "a",
        gap: { reason: LogGapReason.MATCH_LIMIT, lines: 40n, endTime: { seconds: T0 + 6n, nanos: 0 } },
      }),
    ]);
    expect(store.skipped).toBe(12);
    expect(store.lastSkip).toBe(skip);
  });
});
