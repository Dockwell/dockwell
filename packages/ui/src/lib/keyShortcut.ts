// SPDX-License-Identifier: AGPL-3.0-only

// Key names as aria-keyshortcuts spells them (UI Events key values), from
// the symbols and words people print on key caps.
const KEYS: Readonly<Record<string, string>> = {
  "⌘": "Meta",
  cmd: "Meta",
  command: "Meta",
  meta: "Meta",
  "⌃": "Control",
  ctrl: "Control",
  control: "Control",
  "⌥": "Alt",
  alt: "Alt",
  opt: "Alt",
  option: "Alt",
  "⇧": "Shift",
  shift: "Shift",
  "↵": "Enter",
  "⏎": "Enter",
  enter: "Enter",
  return: "Enter",
  esc: "Escape",
  escape: "Escape",
  "⌫": "Backspace",
  backspace: "Backspace",
  "⌦": "Delete",
  del: "Delete",
  delete: "Delete",
  "⇥": "Tab",
  tab: "Tab",
  space: "Space",
  "↑": "ArrowUp",
  "↓": "ArrowDown",
  "←": "ArrowLeft",
  "→": "ArrowRight",
};

const MODIFIERS = new Set(["Meta", "Control", "Alt", "Shift"]);

// Symbols stand alone even when written together, as in ⇧⌘P.
const SYMBOL = /([⌘⌃⌥⇧↵⏎⌫⌦⇥↑↓←→])/u;

/**
 * Turns a shortcut as printed on key caps ("⌘↵", "⇧⌘P", "Ctrl+Enter") into
 * an aria-keyshortcuts value ("Meta+Enter"). Returns undefined for what the
 * attribute can't say: a sequence of keys such as "G F", or no key at all.
 */
export function keyShortcut(caps: string): string | undefined {
  const names = caps
    .split(/[\s+]+/u)
    .flatMap((part) => part.split(SYMBOL))
    .filter((part) => part !== "")
    .map((part) => KEYS[part] ?? KEYS[part.toLowerCase()] ?? (part.length === 1 ? part.toUpperCase() : part));
  const modifiers = names.filter((name) => MODIFIERS.has(name));
  const keys = names.filter((name) => !MODIFIERS.has(name));
  const [key] = keys;
  if (keys.length !== 1 || key === undefined) return undefined;
  return [...modifiers, key].join("+");
}
