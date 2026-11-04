// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { type LogChunk, LogChunkSchema, LogGapReason, LogLevel } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { LogStore, MAX_BYTES, MAX_LINES, NO_FILTER } from "./store";

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
});
