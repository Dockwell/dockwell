// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { keyShortcut } from "./keyShortcut";

describe("keyShortcut", () => {
  it.each([
    ["⌘↵", "Meta+Enter"],
    ["⌘Z", "Meta+Z"],
    ["⌘k", "Meta+K"],
    ["⇧⌘P", "Shift+Meta+P"],
    ["⌥⌫", "Alt+Backspace"],
    ["Ctrl+Enter", "Control+Enter"],
    ["Ctrl Shift Z", "Control+Shift+Z"],
    ["esc", "Escape"],
    ["/", "/"],
    ["F5", "F5"],
  ])("spells %s as %s", (caps, value) => {
    expect(keyShortcut(caps)).toBe(value);
  });

  it.each(["G F", "⌘", "", "Ctrl+"])("can't say %j", (caps) => {
    expect(keyShortcut(caps)).toBeUndefined();
  });
});
