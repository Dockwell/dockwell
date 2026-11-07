// SPDX-License-Identifier: AGPL-3.0-only

import { defineConfig, type MessageType, type OrderByFn } from "@lingui/conf";
import { formatter } from "@lingui/format-po";

// The file a message is first used in, by path, or nothing for one no file uses.
function firstFile(entry: MessageType): string {
  return (entry.origin ?? []).map(([file]) => file).sort()[0] ?? "";
}

function compare(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

// Messages in the order of the files that use them, by path, and then by their
// text: each feature's messages sit together, so pull requests in different
// features add to different parts of the catalog, and git merges them. In
// alphabetical order, two features' messages sat next to each other, and
// almost every merge made the open pull requests conflict there
// (docs/adr/0051-files-that-merge-without-conflicts.md).
const byFile: OrderByFn = (a, b) =>
  compare(firstFile(a.entry), firstFile(b.entry)) ||
  compare(a.entry.message ?? a.messageId, b.entry.message ?? b.messageId) ||
  compare(a.entry.context ?? "", b.entry.context ?? "");

// English is the source; Simplified Chinese ships at launch (PRD: non-functional
// requirements). Catalogs are PO files, compiled by the Vite plugin on import.
export default defineConfig({
  sourceLocale: "en",
  locales: ["en", "zh-CN"],
  fallbackLocales: { default: "en" },
  // No origins: a message's "#:" lines changed whenever another file used it,
  // so two pull requests that each used "Cancel" somewhere new conflicted.
  format: formatter({ origins: false }),
  orderBy: byFile,
  catalogs: [
    {
      path: "<rootDir>/src/locales/{locale}/messages",
      include: ["<rootDir>/src"],
      exclude: ["**/*.test.*", "**/routeTree.gen.ts"],
    },
  ],
});
