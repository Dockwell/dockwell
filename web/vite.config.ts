// SPDX-License-Identifier: AGPL-3.0-only

import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { brotliCompressSync, gzipSync, constants as zlib } from "node:zlib";

import { lingui } from "@lingui/vite-plugin";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

// The build lands where the Go binary embeds it (internal/panel).
const outDir = resolve(import.meta.dirname, "../internal/panel/dist/app");

// `pnpm dev` proxies API calls to a local `dockwell run`.
const apiOrigin = process.env["DOCKWELL_API"] ?? "http://127.0.0.1:7600";

/** Preloads the woff2 fonts, so text renders in Host Grotesk from the first paint. */
function preloadFonts(): Plugin {
  return {
    name: "dockwell:preload-fonts",
    apply: "build",
    transformIndexHtml: {
      order: "post",
      handler(_html, ctx) {
        return Object.keys(ctx.bundle ?? {})
          .filter((file) => file.endsWith(".woff2"))
          .map((file) => ({
            tag: "link",
            attrs: { rel: "preload", href: `/${file}`, as: "font", type: "font/woff2", crossorigin: "" },
            injectTo: "head" as const,
          }));
      },
    },
  };
}

const compressible = new Set([".js", ".css", ".html", ".svg", ".json", ".webmanifest", ".txt"]);

/**
 * Writes .br and .gz next to every compressible file, so the Go server can
 * send them as they are instead of compressing on each request.
 */
function precompress(): Plugin {
  const walk = (dir: string): string[] =>
    readdirSync(dir).flatMap((name) => {
      const path = join(dir, name);
      return statSync(path).isDirectory() ? walk(path) : [path];
    });
  return {
    name: "dockwell:precompress",
    apply: "build",
    closeBundle() {
      for (const file of walk(outDir)) {
        if (!compressible.has(extname(file))) continue;
        const raw = readFileSync(file);
        if (raw.length < 1024) continue;
        const br = brotliCompressSync(raw, {
          params: { [zlib.BROTLI_PARAM_QUALITY]: 11, [zlib.BROTLI_PARAM_SIZE_HINT]: raw.length },
        });
        const gz = gzipSync(raw, { level: 9 });
        if (br.length < raw.length) writeFileSync(`${file}.br`, br);
        if (gz.length < raw.length) writeFileSync(`${file}.gz`, gz);
      }
    },
  };
}

export default defineConfig(({ command }) => ({
  plugins: [
    tanstackRouter({ target: "react", autoCodeSplitting: true }),
    react(),
    lingui({ macroTransform: true, failOnMissing: command === "build" }),
    preloadFonts(),
    precompress(),
  ],
  build: {
    outDir,
    emptyOutDir: true,
    target: "es2023",
    reportCompressedSize: false,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      // Connect and gRPC procedures live under /<package>.<Service>/.
      "^/(dockwell|grpc)\\.": apiOrigin,
      "/healthz": apiOrigin,
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    restoreMocks: true,
    unstubGlobals: true,
  },
}));
