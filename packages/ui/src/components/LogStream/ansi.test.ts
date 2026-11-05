// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { parseAnsi, plainText } from "./ansi";

describe("parseAnsi", () => {
  it("leaves plain text alone", () => {
    expect(parseAnsi("GET /healthz 200")).toEqual([{ text: "GET /healthz 200" }]);
    expect(parseAnsi("")).toEqual([]);
  });

  it("turns colour codes into runs of text", () => {
    expect(parseAnsi("\u001b[31mred\u001b[0m and plain")).toEqual([
      { text: "red", color: "red" },
      { text: " and plain" },
    ]);
    expect(parseAnsi("\u001b[1;32mok\u001b[m")).toEqual([{ text: "ok", color: "green", bold: true }]);
    expect(parseAnsi("\u001b[4munder\u001b[24m over")).toEqual([{ text: "under", underline: true }, { text: " over" }]);
    expect(parseAnsi("\u001b[2mdim\u001b[22m")).toEqual([{ text: "dim", dim: true }]);
    expect(parseAnsi("\u001b[93mbright\u001b[39m")).toEqual([{ text: "bright", color: "yellow" }]);
  });

  it("keeps a line legible, whatever colour it asks for", () => {
    // Black text on any screen, and backgrounds, read as nothing special.
    expect(parseAnsi("\u001b[30mblack")).toEqual([{ text: "black", color: "muted" }]);
    expect(parseAnsi("\u001b[41mon red")).toEqual([{ text: "on red" }]);
    expect(parseAnsi("\u001b[48;5;196mon red")).toEqual([{ text: "on red" }]);
    expect(parseAnsi("\u001b[48;2;255;0;0mon red")).toEqual([{ text: "on red" }]);
    // 256 colours and 24-bit, to the nearest of the short list.
    expect(parseAnsi("\u001b[38;5;196mx")).toEqual([{ text: "x", color: "red" }]);
    expect(parseAnsi("\u001b[38;2;0;200;0mx")).toEqual([{ text: "x", color: "green" }]);
    expect(parseAnsi("\u001b[38;2;20;20;20mx")).toEqual([{ text: "x", color: "muted" }]);
    expect(parseAnsi("\u001b[38;5;240mx")).toEqual([{ text: "x", color: "muted" }]);
  });

  it("drops every other code, never showing it raw", () => {
    expect(plainText("\u001b[2J\u001b[Hcleared")).toBe("cleared");
    expect(plainText("\u001b]0;evil title\u0007text")).toBe("text");
    expect(plainText("\u001b]8;;https://evil.test\u001b\\link\u001b]8;;\u001b\\")).toBe("link");
    expect(plainText("\u001b[?1049h")).toBe("");
    expect(plainText("cut off \u001b[3")).toBe("cut off ");
    expect(plainText("cut off \u001b]0;no end")).toBe("cut off ");
    expect(plainText("lone \u001bXescape")).toBe("lone ");
  });

  it("shows what can't be shown as a replacement character", () => {
    expect(plainText("nul\u0000 bell\u0007 bs\u0008")).toBe("nul� bell� bs�");
    // Tabs are text.
    expect(plainText("a\tb")).toBe("a\tb");
    // Direction overrides could make a line read as something else.
    expect(plainText("user‮exe.txt")).toBe("user�exe.txt");
  });

  it("shows what a line that rewrites itself ends up as", () => {
    expect(plainText("10%\r50%\r100%")).toBe("100%");
    expect(plainText("windows line\r")).toBe("windows line");
  });

  it("keeps markup as text", () => {
    expect(parseAnsi('<img src=x onerror="alert(1)">')).toEqual([{ text: '<img src=x onerror="alert(1)">' }]);
  });
});
