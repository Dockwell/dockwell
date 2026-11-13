// SPDX-License-Identifier: AGPL-3.0-only

import { expect } from "vitest";

/**
 * Fails for each piece of text under root that is one of names and isn't inside a
 * link, as every name of a thing Dockwell keeps opens its page (#566). It reads text
 * nodes, so a name split by markup inside one link still counts as linked.
 */
export function expectNamesLinked(root: HTMLElement, names: readonly string[]): void {
  const unlinked: string[] = [];
  const walker = root.ownerDocument.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const text = node.textContent?.trim() ?? "";
    const at = node.parentElement;
    if (!names.includes(text) || !at || at.closest("a[href]")) continue;
    // Words for screen readers, or a menu's choice, are no reference to open.
    if (at.closest(".dw-sr, option, [aria-hidden='true']")) continue;
    unlinked.push(`${text} in <${at.tagName.toLowerCase()} class="${at.className}">`);
  }
  expect(unlinked, "names that should be links to their pages").toEqual([]);
}
