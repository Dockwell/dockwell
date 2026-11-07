// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { foldText, fuzzyMatch, splitMatches } from "./fuzzy";

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

describe("fuzzyMatch's weights", () => {
  it("puts a match in the name an item acts on above one in its verb", () => {
    const postgres = fuzzyMatch("pg", "Open postgres")?.score ?? 0;
    const ghost = fuzzyMatch("pg", "Open ghost")?.score ?? 0;
    expect(postgres).toBeGreaterThan(ghost);
  });
});

describe("fuzzyMatch on text that isn't plain ASCII", () => {
  it("ignores accents, and lights up the accented letters", () => {
    expect(fuzzyMatch("cafe", "Open café")?.indexes).toEqual([5, 6, 7, 8]);
    expect(fuzzyMatch("café", "Open cafe")?.indexes).toEqual([5, 6, 7, 8]);
    // An accent written as its own mark goes with its letter.
    const decomposed = "Open cafe\u0301-1";
    const match = fuzzyMatch("cafe", decomposed);
    expect(match?.indexes).toEqual([5, 6, 7, 8]);
    expect(splitMatches(decomposed, match?.indexes ?? [])).toEqual([
      { text: "Open ", match: false },
      { text: "cafe\u0301", match: true },
      { text: "-1", match: false },
    ]);
  });

  it("reads full-width letters as plain ones, both ways", () => {
    expect(fuzzyMatch("ｗｅｂ", "Open web-1")?.indexes).toEqual([5, 6, 7]);
    expect(fuzzyMatch("web", "Open ｗｅｂ-1")?.indexes).toEqual([5, 6, 7]);
  });

  it("lights up the right letters after one that grows in lower case", () => {
    // "İ" is two characters in lower case; the match still points at the text's own.
    const match = fuzzyMatch("ist", "Open İstanbul");
    expect(match?.indexes).toEqual([5, 6, 7]);
    expect(splitMatches("Open İstanbul", match?.indexes ?? [])[1]).toEqual({ text: "İst", match: true });
  });

  it("keeps a character outside the basic plane whole", () => {
    const match = fuzzyMatch("𝐰eb", "Open 𝐰eb");
    expect(match?.indexes).toEqual([5, 7, 8]);
    expect(splitMatches("Open 𝐰eb", match?.indexes ?? [])).toEqual([
      { text: "Open ", match: false },
      { text: "𝐰eb", match: true },
    ]);
  });

  it("matches a folded text as it matches the text", () => {
    for (const [query, text] of [
      ["rb web", "Roll back web to r63"],
      ["cafe", "Open café"],
      ["zz", "Open web"],
    ] as const) {
      expect(fuzzyMatch(query, foldText(text))).toEqual(fuzzyMatch(query, text));
    }
  });
});

describe("fuzzyMatch's speed", () => {
  it("matches 500 options in a few milliseconds, as a keystroke needs", () => {
    const verbs = ["Open", "Restart", "Deploy", "Show what happened on", "Roll back", "Show the activity of"];
    const names = ["web", "api-gateway", "postgres", "ghost", "redis", "worker-eu-backup", "grafana", "nextcloud"];
    const options = Array.from({ length: 500 }, (_, i) =>
      foldText(`${verbs[i % verbs.length]} ${names[i % names.length]}-${i} in staging`),
    );
    const queries = ["restart api-gateway in staging", "show what happened on worker", "rstg", "zzz"];
    const runs: number[] = [];
    for (let run = 0; run < 7; run++) {
      const start = performance.now();
      for (const query of queries) for (const option of options) fuzzyMatch(query, option);
      runs.push(performance.now() - start);
    }
    // All four take about 1 ms here, where building whole tables for every option took over 20; the best run allows for a busy machine.
    expect(Math.min(...runs)).toBeLessThan(6);
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
