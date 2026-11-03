// SPDX-License-Identifier: AGPL-3.0-only

import { defineConfig } from "@lingui/conf";
import { formatter } from "@lingui/format-po";

// English is the source; Simplified Chinese ships at launch (PRD: non-functional
// requirements). Catalogs are PO files, compiled by the Vite plugin on import.
export default defineConfig({
  sourceLocale: "en",
  locales: ["en", "zh-CN"],
  fallbackLocales: { default: "en" },
  // Origins without line numbers, so catalogs change only when messages do.
  format: formatter({ lineNumbers: false }),
  catalogs: [
    {
      path: "<rootDir>/src/locales/{locale}/messages",
      include: ["<rootDir>/src"],
      exclude: ["**/*.test.*", "**/routeTree.gen.ts"],
    },
  ],
});
