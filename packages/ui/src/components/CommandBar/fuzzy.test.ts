// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { fuzzyMatch, splitMatches } from "./fuzzy";

describe("fuzzyMatch", () => {
  it("matches characters in order, ignoring case and spaces in the query", () => {
    expect(fuzzyMatch("rb web", "Roll back web to r63")?.indexes).toEqual([0, 5, 10, 11, 12]);
    expect(fuzzyMatch("RBW", "roll back web")).not.toBeNull();
    expect(fuzzyMatch("wbr", "roll back web")).toBeNull();
    expect(fuzzyMatch("", "anything")).toEqual({ score: 0, indexes: [] });
  });

  it("scores consecutive characters and word starts higher, and shorter texts on ties", () => {
    const tight = fuzzyMatch("web", "web")?.score ?? 0;
    const spread = fuzzyMatch("web", "worker-eu-backup")?.score ?? 0;
    expect(tight).toBeGreaterThan(spread);
    const start = fuzzyMatch("api", "open api")?.score ?? 0;
    const middle = fuzzyMatch("api", "rapid")?.score ?? 0;
    expect(start).toBeGreaterThan(middle);
    expect(fuzzyMatch("api", "api")?.score).toBeGreaterThan(fuzzyMatch("api", "api-gateway")?.score ?? 0);
  });
});

describe("fuzzyMatch's best line-up", () => {
  it("takes the query whole where the text has it, rather than its first letters strewn along the way", () => {
    expect(fuzzyMatch("web", "Show what happened on web-1")?.indexes).toEqual([22, 23, 24]);
    expect(fuzzyMatch("back", "Roll back web to r63")?.indexes).toEqual([5, 6, 7, 8]);
  });

  it("prefers the starts of words when the letters are apart", () => {
    expect(fuzzyMatch("rbw", "carb, roll back web")?.indexes).toEqual([6, 11, 16]);
  });

  it("scores a whole match above a strewn one", () => {
    const whole = fuzzyMatch("web", "Show what happened on web-1")?.score ?? 0;
    const strewn = fuzzyMatch("web", "Show the activity of blog")?.score ?? 0;
    expect(whole).toBeGreaterThan(strewn);
  });
});

describe("splitMatches", () => {
  it("groups matched and unmatched characters into runs", () => {
    expect(splitMatches("roll back", [0, 1, 5])).toEqual([
      { text: "ro", match: true },
      { text: "ll ", match: false },
      { text: "b", match: true },
      { text: "ack", match: false },
    ]);
    expect(splitMatches("web", [])).toEqual([{ text: "web", match: false }]);
  });
});
