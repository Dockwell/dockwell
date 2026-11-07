// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { changedLines, crumbs, editable, join, lineCount, lineEnding, nameOf, parentOf, restoreEndings } from "./paths";

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
    expect(restoreEndings(editable(crlf), true)).toBe(crlf);
    expect(restoreEndings("a\nb\n", false)).toBe("a\nb\n");
  });

  it.each([
    ["one stray CRLF in a file of LF", "#!/bin/sh\necho a\r\necho b\n"],
    ["one stray LF in a file of CRLF", "a\r\nb\nc\r\n"],
    ["a lone CR", "a\rb\n"],
    ["a lone CR in a file of CRLF", "a\r\nb\rc\r\n"],
    ["a CR before a CRLF", "a\r\r\nb\r\n"],
    ["a last line that ends in a lone CR", "a\r"],
  ])("say so of %s, which a field can't bring back", (_, text) => {
    expect(lineEnding(text)).toBe("mixed");
    const back = (crlf: boolean) => restoreEndings(editable(text), crlf);
    expect(back(true)).not.toBe(text);
    expect(back(false)).not.toBe(text);
  });

  it.each([
    ["no ending at all", "a"],
    ["nothing", ""],
    ["LF with no last ending", "a\nb"],
    ["CRLF with no last ending", "a\r\nb"],
  ])("bring back %s, byte for byte", (_, text) => {
    expect(restoreEndings(editable(text), lineEnding(text) === "CRLF")).toBe(text);
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
