// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { changedLines, crumbs, editable, join, lineCount, lineEnding, nameOf, parentOf, withEndings } from "./paths";

describe("paths", () => {
  it("finds a path's folder and name", () => {
    expect(parentOf("/var/www/index.php")).toBe("/var/www");
    expect(parentOf("/etc")).toBe("/");
    expect(parentOf("/")).toBe("/");
    expect(nameOf("/var/www/index.php")).toBe("index.php");
    expect(nameOf("/")).toBe("/");
  });

  it("joins a name to a folder", () => {
    expect(join("/", "etc")).toBe("/etc");
    expect(join("/etc", "nginx")).toBe("/etc/nginx");
  });

  it("lists the folders on the way down", () => {
    expect(crumbs("/")).toEqual([{ name: "/", path: "/" }]);
    expect(crumbs("/var/www")).toEqual([
      { name: "/", path: "/" },
      { name: "var", path: "/var" },
      { name: "www", path: "/var/www" },
    ]);
  });
});

describe("lines", () => {
  it("counts them as an editor numbers them", () => {
    expect(lineCount("")).toBe(1);
    expect(lineCount("one")).toBe(1);
    expect(lineCount("one\n")).toBe(1);
    expect(lineCount("one\ntwo")).toBe(2);
    expect(lineCount("one\ntwo\n")).toBe(2);
    expect(lineCount("\n\n")).toBe(2);
  });

  it("names their endings", () => {
    expect(lineEnding("a\nb\n")).toBe("LF");
    expect(lineEnding("a\r\nb\r\n")).toBe("CRLF");
  });

  it("go through a field and come back as they were", () => {
    const crlf = "a\r\nb\r\n";
    expect(editable(crlf)).toBe("a\nb\n");
    expect(withEndings(editable(crlf), crlf)).toBe(crlf);
    expect(withEndings("a\nb\n", "a\nb\n")).toBe("a\nb\n");
  });

  it.each([
    ["one stray CRLF in a file of LF", "#!/bin/sh\necho a\r\necho b\n"],
    ["one stray LF in a file of CRLF", "a\r\nb\nc\r\n"],
    ["a lone CR", "a\rb\n"],
    ["a lone CR in a file of CRLF", "a\r\nb\rc\r\n"],
    ["a CR before a CRLF", "a\r\r\nb\r\n"],
    ["a last line that ends in a lone CR", "a\r"],
  ])("say so of %s, and bring it back byte for byte", (_, text) => {
    expect(lineEnding(text)).toBe("mixed");
    expect(withEndings(editable(text), text)).toBe(text);
  });

  it.each([
    ["no ending at all", "a"],
    ["nothing", ""],
    ["LF with no last ending", "a\nb"],
    ["CRLF with no last ending", "a\r\nb"],
  ])("bring back %s, byte for byte", (_, text) => {
    expect(withEndings(editable(text), text)).toBe(text);
  });

  it.each([
    [
      "a line typed over keeps its ending",
      "#!/bin/sh\necho a\r\necho b\n",
      "#!/bin/sh\necho a\necho c\n",
      "#!/bin/sh\necho a\r\necho c\n",
    ],
    ["a line added takes the one before's", "a\r\nb\nc\r\n", "a\nx\nb\nc\n", "a\r\nx\r\nb\nc\r\n"],
    ["a line removed takes its ending along", "a\r\nb\nc\r\n", "a\nc\n", "a\r\nc\r\n"],
    [
      "two edits far apart leave the lines between alone",
      "1\r\n2\n3\r\n4\n5\r\n",
      "x\n2\n3\n4\ny\n",
      "x\r\n2\n3\r\n4\ny\r\n",
    ],
    ["a lone CR stays", "a\rb\n", "z\nb\n", "z\rb\n"],
    ["a line added at the end of CRLF is CRLF", "a\r\nb\r\n", "a\nb\nc", "a\r\nb\r\nc"],
    ["a new last line after one with no ending takes the line before's", "a\r\nb", "a\nb\nc", "a\r\nb\r\nc"],
    ["LF stays LF", "a\nb\n", "a\nx\nb\n", "a\nx\nb\n"],
    ["lines joined into one take the last of theirs", "a\r\nb\nc\r\n", "ab\nc\n", "ab\nc\r\n"],
  ])("keep each line's ending through an edit: %s", (_, original, typed, saved) => {
    expect(withEndings(typed, original)).toBe(saved);
  });

  // The reviewer's probe: a long file of alternating endings, many lines pasted near its top, and its last
  // line edited, which the diff can't look through. Pairing by place would move the endings of every line
  // after the paste, so it gives nothing, and the person picks one ending.
  const alternating = (n: number) => Array.from({ length: n }, (_, i) => `line ${i}${i % 2 ? "\r\n" : "\n"}`).join("");
  const pasted = (n: number) => {
    const lines = editable(alternating(2000)).split("\n");
    lines.splice(3, 0, ...Array.from({ length: n }, (_, i) => `pasted ${i}`));
    lines[lines.length - 2] = "the last line, edited";
    return lines.join("\n");
  };

  it("gives nothing past the edits it looks through on a file of more than one ending", () => {
    expect(withEndings(pasted(501), alternating(2000))).toBeUndefined();
  });

  it("keeps every untouched line's ending under the edits it looks through", () => {
    const saved = withEndings(pasted(400), alternating(2000));
    if (saved === undefined) throw new Error("400 pasted lines gave nothing");
    const before = alternating(2000).split(/(?<=\n)/);
    const after = saved.split(/(?<=\n)/);
    for (const line of before.slice(0, 1999)) {
      if (!line.startsWith("line ")) continue;
      expect(after).toContain(line);
    }
  });

  it("gives every line the one ending of a file that has one, however much changed", () => {
    const crlf = Array.from({ length: 1200 }, (_, i) => `line ${i}\r\n`).join("");
    const typed = Array.from({ length: 1200 }, (_, i) => `new ${i}`).join("\n");
    expect(withEndings(`${typed}\n`, crlf)).toBe(Array.from({ length: 1200 }, (_, i) => `new ${i}\r\n`).join(""));
  });

  // The reviewer's second probe: small mixed files, a line inserted and saved, then another line retyped
  // and saved again over what the first save wrote. No line the second edit didn't touch changes its ending.
  it("keeps every untouched line's ending through a second save over the first", () => {
    let seed = 7;
    const random = (n: number) => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed % n;
    };
    const endings = ["\n", "\r\n", "\r"];
    for (let round = 0; round < 3000; round++) {
      const count = 2 + random(6);
      const lines = Array.from({ length: count }, () => (random(3) === 0 ? "x" : `l${random(4)}`));
      const original = lines.map((l) => l + endings[random(3)]).join("");
      const first = editable(original).split("\n");
      first.splice(random(first.length), 0, "inserted");
      const once = withEndings(first.join("\n"), original);
      if (once === undefined) throw new Error(`a small edit gave nothing: ${JSON.stringify(original)}`);
      const second = editable(once).split("\n");
      const at = random(second.length - 1);
      second[at] = "EDITED";
      const twice = withEndings(second.join("\n"), once);
      if (twice === undefined) throw new Error(`a small edit gave nothing: ${JSON.stringify(once)}`);
      const was = once.split(/(?<=\r\n|\n|\r(?!\n))/);
      const now = twice.split(/(?<=\r\n|\n|\r(?!\n))/);
      expect(now.length).toBe(was.length);
      for (let k = 0; k < was.length; k++) {
        if (k === at) continue;
        if (now[k] !== was[k])
          throw new Error(`round ${round}: ${JSON.stringify(once)} then line ${at} gave ${JSON.stringify(twice)}`);
      }
    }
  });

  it("count what an edit touches", () => {
    expect(changedLines("a\nb\nc\n", "a\nx\nc\n")).toBe(1);
    expect(changedLines("a\nb\nc\n", "a\nx\ny\nc\n")).toBe(2);
    expect(changedLines("a\nb\nc\n", "a\nc\n")).toBe(1);
    expect(changedLines("a\n", "a\nb\n")).toBe(1);
    expect(changedLines("a\nb\nc", "x\nb\ny")).toBe(3);
    expect(changedLines("a", "a")).toBe(0);
  });
});
