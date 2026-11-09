// SPDX-License-Identifier: AGPL-3.0-only

import type { ILinkHandler } from "@xterm/xterm";

import { onApple } from "../../lib/platform";

/**
 * Where a link a program printed (OSC 8) may lead: an http or https URL, and
 * nothing else, so a printed link can't run script, read a file or start an app.
 */
export function linkTarget(uri: string): string | undefined {
  try {
    const url = new URL(uri);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : undefined;
  } catch {
    return undefined;
  }
}

/**
 * What the terminal does with a link a program printed. Its text can say anything,
 * so where it leads shows on hover, and it opens only with Ctrl, or Cmd on an Apple keyboard,
 * held, as in other terminals, in a new tab that can't reach back to this one.
 */
export function linkHandler(host: () => HTMLElement | null): ILinkHandler {
  return {
    allowNonHttpProtocols: false,
    activate(event, uri) {
      const target = linkTarget(uri);
      if (!target || !(onApple() ? event.metaKey : event.ctrlKey)) return;
      window.open(target, "_blank", "noopener,noreferrer");
    },
    hover(_event, uri) {
      const el = host();
      if (el && linkTarget(uri)) el.title = linkTarget(uri) ?? "";
    },
    leave() {
      const el = host();
      if (el) el.removeAttribute("title");
    },
  };
}
