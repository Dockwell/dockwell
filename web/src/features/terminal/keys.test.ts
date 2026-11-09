// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { keySequence, withCtrl } from "./TerminalScreen";

describe("the phone's keys", () => {
  it("send what a terminal's keys do, cursor keys as the program in front asked for", () => {
    expect(keySequence("escape", false, false)).toBe("\x1b");
    expect(keySequence("tab", false, false)).toBe("\t");
    expect(keySequence("up", false, false)).toBe("\x1b[A");
    expect(keySequence("left", false, false)).toBe("\x1b[D");
    // vim and less ask for application cursor keys.
    expect(keySequence("down", true, false)).toBe("\x1bOB");
    expect(keySequence("right", true, false)).toBe("\x1bOC");
    // Ctrl with an arrow moves a word in a shell.
    expect(keySequence("left", false, true)).toBe("\x1b[1;5D");
  });

  it("send a character with Ctrl held as its control code", () => {
    expect(withCtrl("c")).toBe("\x03");
    expect(withCtrl("C")).toBe("\x03");
    expect(withCtrl("d")).toBe("\x04");
    expect(withCtrl("[")).toBe("\x1b");
    expect(withCtrl(" ")).toBe("\x00");
    expect(withCtrl("|")).toBe("\x1c");
    // What has no control form goes as it is.
    expect(withCtrl("1")).toBe("1");
    expect(withCtrl("ab")).toBe("ab");
  });
});
