// SPDX-License-Identifier: AGPL-3.0-only

import { readdirSync, readFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { describe, expect, it } from "vitest";

// Design-system guard rails (design/system/README.md + tokens.json).
// Prevents drift back into raw hex colours, off-scale font sizes, custom focus
// outlines, forbidden italics or hand-rolled pagination footers.

const roots = [resolve("src"), resolve("../packages/ui/src")];
const ALLOWED_FONT_SIZES = new Set(["12px", "13px", "14px", "16px", "22px", "28px", "32px"]);

function sources(dir: string, ending: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sources(path, ending));
    else if (entry.name.endsWith(ending) && !entry.name.includes(".test.")) out.push(path);
  }
  return out;
}

function stripComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, (match) => match.replace(/[^\n]/g, " "));
}

describe("design system guards", () => {
  it("keeps every CSS colour on tokens outside tokens.css", () => {
    const raw: string[] = [];
    for (const root of roots) {
      for (const path of sources(root, ".css")) {
        const name = relative(resolve(".."), path);
        if (name.endsWith("tokens.css")) continue;
        const lines = stripComments(readFileSync(path, "utf8")).split("\n");
        lines.forEach((line, index) => {
          // Alpha masks in mask-image use #000 for opacity only, not a painted colour.
          if (/mask-image\s*:/.test(line)) return;
          const match = line.match(/#[0-9a-fA-F]{3,8}\b/);
          if (match) raw.push(`${name}:${index + 1} (${match[0]}) ${line.trim()}`);
        });
      }
    }
    expect(raw, "use a colour or shadow token from tokens.css rather than a raw hex literal").toEqual([]);
  });

  it("keeps every px font size on the tokens.json type scale", () => {
    const off: string[] = [];
    for (const root of roots) {
      for (const path of sources(root, ".css")) {
        const name = relative(resolve(".."), path);
        if (name.endsWith("tokens.css")) continue;
        const lines = stripComments(readFileSync(path, "utf8")).split("\n");
        lines.forEach((line, index) => {
          for (const match of line.matchAll(/font(?:-size)?\s*:[^;]*?\b([0-9.]+px)\b/g)) {
            const size = match[1] ?? "";
            if (!ALLOWED_FONT_SIZES.has(size)) off.push(`${name}:${index + 1} (${size}) ${line.trim()}`);
          }
        });
      }
    }
    expect(off, "use one of the tokens.json type scale sizes (12, 13, 14, 16, 22, 28, 32px)").toEqual([]);
  });

  it("uses var(--focus-ring) instead of custom solid cobalt outlines", () => {
    const custom: string[] = [];
    for (const root of roots) {
      for (const path of sources(root, ".css")) {
        const name = relative(resolve(".."), path);
        const lines = stripComments(readFileSync(path, "utf8")).split("\n");
        lines.forEach((line, index) => {
          if (/outline\s*:\s*[^;]*solid\s+var\(--cobalt\)/.test(line)) {
            custom.push(`${name}:${index + 1} ${line.trim()}`);
          }
        });
      }
    }
    expect(custom, "use outline: none; box-shadow: var(--focus-ring) for keyboard focus").toEqual([]);
  });

  it("keeps italic styling out of the UI", () => {
    const italics: string[] = [];
    for (const root of roots) {
      for (const path of sources(root, ".css")) {
        const name = relative(resolve(".."), path);
        const lines = stripComments(readFileSync(path, "utf8")).split("\n");
        lines.forEach((line, index) => {
          if (/font-style\s*:\s*italic/.test(line)) italics.push(`${name}:${index + 1} ${line.trim()}`);
        });
      }
    }
    expect(italics, "design/system/README.md forbids italic text").toEqual([]);
  });

  it("leaves page-flipping footers to PaginationBar and ListPager", () => {
    const handRolled: string[] = [];
    for (const path of sources(resolve("src"), ".css")) {
      const name = relative(resolve(".."), path);
      const text = stripComments(readFileSync(path, "utf8"));
      if (/\.(docker-pager|list-pager)\b/.test(text)) handRolled.push(name);
    }
    for (const path of sources(resolve("src"), ".tsx")) {
      const name = relative(resolve(".."), path);
      if (name === "web/src/features/shell/ListTools.tsx") continue;
      const text = readFileSync(path, "utf8");
      if (/<Button\b[^>]*>\s*<Trans>Previous(?: page)?<\/Trans>\s*<\/Button>/.test(text)) {
        handRolled.push(name);
      }
    }
    expect(handRolled, "use PaginationBar from @dockwell/ui or ListPager from ListTools").toEqual([]);
  });
});
