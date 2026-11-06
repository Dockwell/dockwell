// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { crumbs, join, lineCount, lineEnding, lineNumbers, nameOf, parentOf } from "./paths";

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

  it("numbers them", () => {
    expect(lineNumbers(3)).toBe("1\n2\n3");
  });

  it("names their endings", () => {
    expect(lineEnding("a\nb\n")).toBe("LF");
    expect(lineEnding("a\r\nb\r\n")).toBe("CRLF");
  });
});
