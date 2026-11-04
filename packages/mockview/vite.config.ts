// SPDX-License-Identifier: AGPL-3.0-only

import { resolve } from "node:path";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

const repo = resolve(import.meta.dirname, "../..");

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5174,
    strictPort: true,
    // The mocks and the design system live outside this package.
    fs: { allow: [repo] },
    warmup: { clientFiles: ["./src/frame/main.tsx", "./src/viewer/main.tsx"] },
  },
  // Pre-bundled up front, so the first page load never triggers a reload.
  optimizeDeps: { include: ["react", "react-dom", "react-dom/client", "react/jsx-runtime", "react/jsx-dev-runtime"] },
  test: {
    environment: "jsdom",
    css: false,
    restoreMocks: true,
    unstubGlobals: true,
  },
});
