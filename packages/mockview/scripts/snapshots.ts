// SPDX-License-Identifier: AGPL-3.0-only

// Writes a PNG of every artboard in design/mocks, in the light and the dark
// theme, to packages/mockview/snapshots/<theme>/<artboard>.png. It fails when
// an artboard reports a problem, logs an error or a warning, or throws.
//
//   make mock-snapshots
//   pnpm --filter @dockwell/mockview snapshots -- --only Overview --theme dark
//
// Options: --only <name> (repeatable; that artboard, or every file name containing it),
// --theme light|dark (repeatable), --out <dir>, --jobs <n>.
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { existsSync } from "node:fs";
import { mkdir, readFile, rm } from "node:fs/promises";
import { join, relative, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, type ConsoleMessage, chromium, type Page } from "playwright-core";
import { createServer, type ViteDevServer } from "vite";

import { type Canvas, canvasBoards } from "../src/canvas.ts";

const here = resolve(import.meta.dirname, "..");
const repo = resolve(here, "../..");

interface Job {
  file: string;
  theme: "light" | "dark";
  width: number;
  height: number;
}

/** Taller than any mock's full page; a taller capture fails instead of repeating tiles. */
const WINDOW_HEIGHT = 4000;

interface Failure {
  job: Job;
  messages: string[];
}

function fail(message: string): never {
  console.error(message);
  process.exit(1);
}

const options = {
  only: { type: "string", multiple: true },
  theme: { type: "string", multiple: true },
  out: { type: "string" },
  jobs: { type: "string" },
} as const;

function readArgs() {
  // pnpm passes a leading "--" through to the script.
  const args = process.argv.slice(2);
  if (args[0] === "--") args.shift();
  try {
    return parseArgs({ args, options }).values;
  } catch (error) {
    return fail(
      `${error instanceof Error ? error.message : String(error)}\n` +
        "Use --only <name>, --theme light|dark, --out <dir> or --jobs <n>.",
    );
  }
}

const values = readArgs();

const themes = values.theme?.length ? values.theme : ["light", "dark"];
for (const theme of themes) {
  if (theme !== "light" && theme !== "dark") fail(`--theme ${theme} isn't a theme. Use light or dark.`);
}
const out = resolve(values.out ?? join(here, "snapshots"));
const workers = Math.max(1, Number.parseInt(values.jobs ?? "4", 10) || 4);
const executablePath = process.env["CHROMIUM_PATH"] || "/usr/bin/chromium";
if (!existsSync(executablePath)) {
  fail(
    `There's no Chromium at ${executablePath}. Install Chromium, or set CHROMIUM_PATH to a Chrome or Chromium binary.`,
  );
}

const canvas = JSON.parse(await readFile(join(repo, "design/mocks/canvas.json"), "utf8")) as Canvas;
const boards = canvasBoards(canvas);
const only = (values.only ?? []).map((s) => s.toLowerCase().replace(/\.dc\.html$/, ""));
const base = (file: string) => file.toLowerCase().replace(/\.dc\.html$/, "");
// An exact artboard name picks that artboard; anything else picks every file name that contains it.
const picked = only.length
  ? boards.filter((b) =>
      only.some((s) => (boards.some((g) => base(g.file) === s) ? base(b.file) === s : base(b.file).includes(s))),
    )
  : boards;
if (picked.length === 0)
  fail(`No artboard matches --only ${values.only?.join(", ")}. Use part of a file name in design/mocks.`);

const queue: Job[] = picked.flatMap(({ file, width, height }) =>
  themes.map((theme) => ({ file, theme: theme as Job["theme"], width, height })),
);

async function shoot(page: Page, base: string, job: Job): Promise<string[]> {
  const messages: string[] = [];
  const onConsole = (msg: ConsoleMessage) => {
    if (msg.type() !== "error" && msg.type() !== "warning") return;
    const { url } = msg.location();
    messages.push(url && !msg.text().includes(url) ? `${msg.text()} (${url})` : msg.text());
  };
  const onPageError = (error: Error) => messages.push(error.message);
  page.on("console", onConsole);
  page.on("pageerror", onPageError);
  try {
    await page.setViewportSize({ width: job.width, height: job.height });
    const url = new URL("frame.html", base);
    url.searchParams.set("board", job.file);
    url.searchParams.set("theme", job.theme);
    await page.goto(url.href, { waitUntil: "load" });
    await page.waitForFunction(() => window.__mockview && window.__mockview.status !== "loading", undefined, {
      timeout: 30_000,
    });
    const status = await page.evaluate(() => window.__mockview);
    const tall = await page.evaluate(() => document.documentElement.scrollHeight);
    if (tall > WINDOW_HEIGHT)
      messages.push(
        `The page is ${tall} px tall, more than the ${WINDOW_HEIGHT} px a snapshot can hold. Raise WINDOW_HEIGHT.`,
      );
    if (status?.status !== "ready") messages.push(...(status?.problems ?? []).map((p) => `${p.file}: ${p.message}`));
    const dir = join(out, job.theme);
    await mkdir(dir, { recursive: true });
    await page.screenshot({
      path: join(dir, job.file.replace(/\.dc\.html$/, ".png")),
      fullPage: true,
      animations: "disabled",
    });
  } catch (error) {
    messages.push(error instanceof Error ? error.message : String(error));
  } finally {
    page.off("console", onConsole);
    page.off("pageerror", onPageError);
  }
  return [...new Set(messages)];
}

let server: ViteDevServer | undefined;
let browser: Browser | undefined;
const started = performance.now();
const failures: Failure[] = [];
try {
  if (!only.length) {
    for (const theme of themes) await rm(join(out, theme), { recursive: true, force: true });
  }
  server = await createServer({
    root: here,
    configFile: join(here, "vite.config.ts"),
    logLevel: "warn",
    server: { port: 0, strictPort: false, hmr: false },
  });
  await server.listen();
  const origin = server.resolvedUrls?.local[0];
  if (!origin) fail("The Vite server started but didn't report a URL.");

  // Headless Chromium repeats tiles in a capture taller than its window, so
  // the window fits the largest page, including pages that scroll past their canvas height.
  const widest = Math.max(...picked.map((b) => b.width));
  browser = await chromium.launch({
    executablePath,
    headless: true,
    args: [`--window-size=${widest},${WINDOW_HEIGHT}`],
  });
  const context = await browser.newContext({ deviceScaleFactor: 1, colorScheme: "light", reducedMotion: "reduce" });
  const total = queue.length;
  let done = 0;
  await Promise.all(
    Array.from({ length: Math.min(workers, total) }, async () => {
      const page = await context.newPage();
      for (let job = queue.shift(); job; job = queue.shift()) {
        const messages = await shoot(page, origin, job);
        done++;
        if (messages.length) {
          failures.push({ job, messages });
          console.error(`${job.theme} ${job.file}:\n  ${messages.join("\n  ")}`);
        }
        if (process.stdout.isTTY) process.stdout.write(`\r${done}/${total}`);
      }
      await page.close();
    }),
  );
  if (process.stdout.isTTY) process.stdout.write("\r");
  const seconds = Math.round((performance.now() - started) / 1000);
  console.log(
    `Wrote ${total} ${total === 1 ? "snapshot" : "snapshots"} to ${relative(process.cwd(), out) || "."} in ${seconds} s.`,
  );
} finally {
  await browser?.close();
  await server?.close();
}

if (failures.length) {
  fail(
    `${failures.length} of ${picked.length * themes.length} snapshots had problems, listed above. ` +
      "Open them with make mocks to see where they go wrong.",
  );
}
