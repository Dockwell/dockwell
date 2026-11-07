// SPDX-License-Identifier: AGPL-3.0-only

// Follows a service's logs in headless Chromium, against a real `dockwell run`
// and its own agent, on the fake Docker Engine, so it needs no Docker. Log lines
// go from a container through the agent and the control plane to the page, and
// this checks they arrive within a second, that following stops and resumes,
// what filters, search, colours, a failed deploy and a flood do, and that closing
// the page stops the reading on the agent. It measures the page at a hundred
// thousand lines, and at thousands of lines a second, and prints the figures.
// With --shots <dir> it saves screenshots at 1440, 800, 390 and 360 px, light and
// dark, and in Chinese. CI's Service end to end on Docker job reads real
// containers' logs through the CLI (scripts/docker-e2e.sh).
//
//   make e2e
//   node web/e2e/logs.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, type BrowserContext, chromium, type Page } from "playwright-core";

// A real binary asks nothing beyond this machine in a test.
process.env["DOCKWELL_TEST"] = "1";

const { values, positionals } = parseArgs({
  allowPositionals: true,
  options: { shots: { type: "string" }, measure: { type: "boolean" } },
});
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7350 + Math.floor(Math.random() * 300);
const loadPort = port + 300;
const origin = `http://localhost:${port}`;
const load = `http://127.0.0.1:${loadPort}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-logs-"));
const socketDir = mkdtempSync(join(tmpdir(), "dw-"));

const kids: ChildProcess[] = [];
let browser: Browser | undefined;
let lastPage: Page | undefined;
const serverLog: string[] = [];

function step(what: string) {
  console.log(`logs e2e: ${what}`);
}

function fail(message: string): never {
  throw new Error(message);
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

async function until<T>(
  what: string,
  fn: () => Promise<T | undefined | false | null | ""> | T,
  timeout = 15_000,
): Promise<T> {
  const end = Date.now() + timeout;
  for (;;) {
    const got = await fn();
    if (got) return got as T;
    if (Date.now() > end) fail(`timed out waiting for ${what}`);
    await sleep(50);
  }
}

function env(): NodeJS.ProcessEnv {
  return {
    ...process.env,
    DOCKWELL_DATA_DIR: data,
    DOCKWELL_LOG_FORMAT: "text",
    USER: "mira",
  };
}

function dockwell(...args: string[]): string {
  return execFileSync(bin, args, { encoding: "utf8", env: env() });
}

async function post(path: string, body = ""): Promise<void> {
  const res = await fetch(load + path, { method: "POST", body });
  if (!res.ok) fail(`POST ${path}: ${res.status}`);
}

/** Starts the fake engine, and returns its socket. */
async function startEngine(): Promise<string> {
  const built = join(socketDir, "logload");
  execFileSync("go", ["build", "-o", built, "./scripts/logload"], { stdio: "inherit" });
  const engine = spawn(built, [socketDir, `127.0.0.1:${loadPort}`], { stdio: ["ignore", "pipe", "inherit"] });
  kids.push(engine);
  const socket = await new Promise<string>((res, rej) => {
    engine.stdout?.once("data", (d: Buffer) => res(d.toString().trim()));
    engine.once("exit", () => rej(new Error("the fake engine ended")));
  });
  return socket;
}

async function startServer(socket: string): Promise<void> {
  const server = spawn(
    bin,
    ["run", "--listen", `127.0.0.1:${port}`, "--agent-listen", "127.0.0.1:0", "--log-format", "json"],
    {
      env: { ...env(), DOCKER_HOST: `unix://${socket}` },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  kids.push(server);
  const take = (d: Buffer) => serverLog.push(d.toString());
  server.stdout?.on("data", take);
  server.stderr?.on("data", take);
  await until("the control plane", async () => {
    try {
      return (await fetch(`${origin}/healthz`)).ok;
    } catch {
      return false;
    }
  });
  await until("its agent to connect", () => serverLog.join("").includes("connected to the control plane"));
}

/** Records a page's frame times and long tasks until the function it returns is called. */
async function watchFrames(page: Page): Promise<
  () => Promise<{
    frames: number;
    p50: number;
    p95: number;
    max: number;
    longTasks: number;
    longest: number;
    heapMb: number;
  }>
> {
  await page.evaluate(() => {
    const w = window as unknown as { __frames: number[]; __long: number[]; __stop: boolean };
    w.__frames = [];
    w.__long = [];
    w.__stop = false;
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) w.__long.push(e.duration);
    }).observe({ entryTypes: ["longtask"] });
    let last = performance.now();
    const tick = (now: number) => {
      w.__frames.push(now - last);
      last = now;
      if (!w.__stop) requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });
  return async () =>
    page.evaluate(() => {
      const w = window as unknown as { __frames: number[]; __long: number[]; __stop: boolean };
      w.__stop = true;
      const sorted = [...w.__frames].sort((a, b) => a - b);
      const at = (p: number) => sorted[Math.floor(sorted.length * p)] ?? 0;
      const mem = (performance as unknown as { memory?: { usedJSHeapSize: number } }).memory;
      return {
        frames: sorted.length,
        p50: at(0.5),
        p95: at(0.95),
        max: sorted[sorted.length - 1] ?? 0,
        longTasks: w.__long.length,
        longest: Math.max(0, ...w.__long),
        heapMb: mem ? mem.usedJSHeapSize / 1e6 : 0,
      };
    });
}

function fmt(n: number): string {
  return n.toFixed(1);
}

async function shoot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  for (const scheme of ["dark", "light"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    for (const width of [1440, 800, 390, 360]) {
      await page.setViewportSize({ width, height: width >= 800 ? 970 : 844 });
      await sleep(250);
      await page.screenshot({ path: join(shots, `${name}-${width}-${scheme}.png`) });
    }
  }
  await page.emulateMedia({ colorScheme: "dark" });
  await page.setViewportSize({ width: 1440, height: 970 });
  await sleep(100);
}

/** Runs in the page: how far a chip that is on stands from one that is off, and a match from its line. */
function contrastOfMarks(): { chip: number; hit: number; hitText: number } {
  const channels = (color: string): number[] => {
    const probe = document.createElement("canvas").getContext("2d") as CanvasRenderingContext2D;
    probe.fillStyle = color;
    probe.fillRect(0, 0, 1, 1);
    return [...probe.getImageData(0, 0, 1, 1).data];
  };
  const over = (top: number[], under: number[]): number[] => {
    const a = (top[3] ?? 255) / 255;
    return [0, 1, 2].map((i) => (top[i] ?? 0) * a + (under[i] ?? 0) * (1 - a));
  };
  const ground = (el: Element | null): number[] => {
    if (!el) return [255, 255, 255];
    return over(channels(getComputedStyle(el).backgroundColor), ground(el.parentElement));
  };
  const light = (c: number[]) =>
    [0, 1, 2]
      .map((i) => {
        const v = (c[i] ?? 0) / 255;
        return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
      })
      .reduce((sum, v, i) => sum + v * ([0.2126, 0.7152, 0.0722][i] ?? 0), 0);
  const ratio = (a: number[], b: number[]) => {
    const [hi, lo] = [light(a), light(b)].sort((x, y) => y - x) as [number, number];
    return (hi + 0.05) / (lo + 0.05);
  };
  const edge = (el: Element | null): number[] =>
    el ? over(channels(getComputedStyle(el).borderTopColor), ground(el.parentElement)) : [0, 0, 0];
  const on = document.querySelector(".logs-chip.is-on .logs-chip-face");
  const off = document.querySelector(".logs-chip:not(.is-on) .logs-chip-face");
  const mark = document.querySelector("mark.dw-log-hit");
  return {
    // Its ground changes hue more than light, and its edge says the rest.
    chip: ratio(edge(on), edge(off)),
    hit: ratio(ground(mark), ground(mark?.parentElement ?? null)),
    hitText: mark ? ratio(over(channels(getComputedStyle(mark).color), ground(mark)), ground(mark)) : 0,
  };
}

/** Runs in the page: what falls short of WCAG AA's contrast, by what is on screen now. */
function contrastProblems(): string[] {
  const channels = (color: string): number[] => {
    const probe = document.createElement("canvas").getContext("2d") as CanvasRenderingContext2D;
    probe.fillStyle = color;
    probe.fillRect(0, 0, 1, 1);
    return [...probe.getImageData(0, 0, 1, 1).data];
  };
  const over = (top: number[], under: number[]): number[] => {
    const a = (top[3] ?? 255) / 255;
    return [0, 1, 2].map((i) => (top[i] ?? 0) * a + (under[i] ?? 0) * (1 - a));
  };
  const ground = (el: Element | null): number[] => {
    if (!el) return [255, 255, 255];
    return over(channels(getComputedStyle(el).backgroundColor), ground(el.parentElement));
  };
  const light = (c: number[]) =>
    [0, 1, 2]
      .map((i) => {
        const v = (c[i] ?? 0) / 255;
        return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
      })
      .reduce((sum, v, i) => sum + v * ([0.2126, 0.7152, 0.0722][i] ?? 0), 0);
  const ratio = (a: number[], b: number[]) => {
    const [hi, lo] = [light(a), light(b)].sort((x, y) => y - x) as [number, number];
    return (hi + 0.05) / (lo + 0.05);
  };
  const problems: string[] = [];
  for (const selector of [
    ".dw-log-line.is-error .dw-log-text",
    ".dw-log-line.is-error .dw-ansi-red",
    ".dw-log-line.is-error .dw-ansi-green",
    ".dw-log-line.is-error .dw-ansi-yellow",
    ".dw-log-line.is-carried .dw-log-text",
    ".dw-log-foot",
    ".logs-chip.is-on .logs-chip-face",
    ".logs-chip:not(.is-on) .logs-chip-face",
  ]) {
    const el = document.querySelector(selector);
    if (!el) continue;
    const got = ratio(over(channels(getComputedStyle(el).color), ground(el)), ground(el));
    if (got < 4.5) problems.push(`${selector} is ${got.toFixed(2)}:1`);
  }
  return problems;
}

/** Waits out colours on their way to a new theme, as a contrast read in the middle of one means nothing. */
async function colorsSettled(page: Page) {
  await sleep(50);
  await page.evaluate(() =>
    Promise.all(
      document
        .getAnimations()
        .filter((a) => a instanceof CSSTransition)
        .map((a) => a.finished.catch(() => undefined)),
    ),
  );
}

function logBody(page: Page) {
  return page.getByRole("log");
}

async function main() {
  const socket = await startEngine();
  await startServer(socket);
  step("claim the install, run a service, and open a session");
  // dockwell run prints a claim link only to a terminal, so this asks for one.
  await until("dockwell run to say how to claim it", () => serverLog.join("").includes("To claim it, run this"));
  const claimed = execFileSync(bin, ["claim", "--log-format", "json"], { encoding: "utf8", env: env() });
  const link =
    claimed.match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]+/)?.[0] ?? fail(`dockwell claim printed:\n${claimed}`);
  execFileSync("go", ["run", "./scripts/claim", link], { stdio: "inherit" });
  dockwell("login", "--local");
  dockwell("services", "create", "web", "--app-image", "ghcr.io/acme/shop:1", "--yes");
  const services = JSON.parse(dockwell("resources", "list", "--kind", "service", "--json")) as {
    resources: { id: string }[];
  };
  const serviceId = services.resources[0]?.id ?? fail("no service was made");
  const recover = execFileSync(bin, ["recover", "--log-format", "json"], { encoding: "utf8", env: env() });
  const signIn =
    recover.match(/(http:\/\/localhost:\d+\/recover#[A-Za-z0-9_-]{43})/)?.[1] ??
    fail(`dockwell recover printed:\n${recover}`);

  browser = await chromium.launch({
    executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium",
    args: ["--enable-precise-memory-info"],
  });
  const context: BrowserContext = await browser.newContext({
    viewport: { width: 1440, height: 970 },
    colorScheme: "dark",
  });
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const page = await context.newPage();
  lastPage = page;
  page.on("pageerror", (err) => fail(`the page threw: ${err.message}`));
  await page.goto(signIn);
  await page.getByText("You signed in with a one-time link").first().waitFor();
  const url = `${origin}/apps/${serviceId}/logs`;

  step("no logs yet is a state of its own");
  await page.goto(url);
  await page.getByRole("heading", { name: "No logs yet" }).waitFor({ timeout: 15_000 });
  await shoot(page, "empty");

  step("a line reaches the browser within a second");
  const latencies: number[] = [];
  for (let i = 0; i < 20; i++) {
    const marker = `marker-${i}-${Date.now()}`;
    const sawIt = page.waitForFunction(
      (m) => document.querySelector('[role="log"]')?.textContent?.includes(m),
      marker,
      { polling: 20, timeout: 5000 },
    );
    const start = Date.now();
    await post("/print", `GET /v1/products/${i} 200 ${marker}`);
    await sawIt;
    latencies.push(Date.now() - start);
    await sleep(120);
  }
  latencies.sort((a, b) => a - b);
  const worst = latencies[latencies.length - 1] ?? 0;
  step(`  written to on screen, 20 lines: median ${latencies[10]} ms, worst ${worst} ms`);
  if (worst >= 1000) fail(`a line took ${worst} ms to reach the browser, and must within 1 s`);

  step("what a line holds can't break out of it");
  await post(
    "/print",
    [
      "2026-11-04 14:19:58 INFO \u001b[32mready\u001b[0m and listening on :8080",
      "2026-11-04 14:19:59 WARN slow upstream checkout: POST /quote took 2.41 s (limit 1 s)",
      "2026-11-04 14:20:00 ERROR POST /v1/checkout/quote 502 3.00 s upstream checkout: connection reset by peer (attempt 1 of 2)",
      "    at com.acme.checkout.Quote.fetch(Quote.java:42)",
      "    at com.acme.checkout.Handler.run(Handler.java:12)",
      `<img src=x onerror="document.title='owned'"><script>window.owned=1</script>`,
      "\u001b]0;evil title\u0007\u001b[2J\u001b[Hescapes that would move things",
      `binary \u0000\u0001\u0002\u0007 junk ${"�".repeat(8)} ok`,
      `a long line ${"without-any-break-".repeat(600)}`,
    ].join("\n"),
  );
  await page.getByText("escapes that would move things").waitFor();
  const body = (await logBody(page).textContent()) ?? "";
  if (body.includes("\u001b")) fail("an escape code reached the page as text");
  if ((await page.evaluate(() => (window as unknown as { owned?: number }).owned)) || (await page.title()) === "owned")
    fail("a log line ran as markup");
  const overflow = await page.evaluate(() => {
    const el = document.querySelector('[role="log"]') as HTMLElement;
    return el.scrollWidth - el.clientWidth;
  });
  if (overflow > 0) fail(`a long line made the log ${overflow} px wider`);

  step("a source column only with more than one source");
  if (await page.locator(".dw-log-src").count()) fail("one source has a source column");
  await shoot(page, "one-source");
  await post("/replica");
  await post(
    "/print",
    "2026-11-04 14:20:01 INFO a line that both containers print\n2026-11-04 14:20:02 WARN and a warning",
  );
  await page.locator(".dw-log-src").first().waitFor({ timeout: 5000 });
  await page.getByRole("radio", { name: "All containers" }).waitFor();
  await shoot(page, "two-sources");
  await page.getByRole("radio", { name: /-2$/ }).click();
  await until(
    "the column to go with one container showing",
    async () => (await page.locator(".dw-log-src").count()) === 0,
  );
  await page.getByRole("radio", { name: "All containers" }).click();

  step("following stops when the reader scrolls up, and resumes on request");
  await post("/rate?lines=200&size=120");
  await page.getByRole("status").getByText("Following").waitFor();
  await sleep(600);
  const log = logBody(page);
  await log.hover();
  await page.mouse.wheel(0, -600);
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  // The line being read stays where it is. Its scroll position may not: lines
  // merging in from another source can move it, with the line kept in place.
  const lineAtTop = () =>
    log.evaluate((el) => {
      const box = el.getBoundingClientRect();
      const hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
      const row = hit?.closest(".dw-log-row");
      return row ? { text: row.textContent, y: Math.round(row.getBoundingClientRect().top) } : undefined;
    });
  const before = await lineAtTop();
  await sleep(600);
  const after = await lineAtTop();
  if (!before || !after) fail("no line shows while paused");
  if (before.text !== after.text || Math.abs(before.y - after.y) > 2) {
    fail(`the line being read moved while paused: ${JSON.stringify(before)} then ${JSON.stringify(after)}`);
  }
  const jump = page.getByRole("button", { name: /new line/ });
  await jump.waitFor();
  await page.screenshot({ path: shots ? join(shots, "paused.png") : join(data, "paused.png") });
  await page.getByRole("button", { name: "Follow", exact: true }).click();
  await page.getByRole("status").getByText("Following").waitFor();
  await until("the log to reach the end", () =>
    log.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight < 20),
  );
  await post("/rate?lines=0");

  step("filter by level, and search with what it found marked");
  await page.getByRole("button", { name: "Errors", exact: true }).click({ modifiers: ["Shift"] });
  await post("/print", "ERROR the filter keeps this\nWARN the filter hides this\nand this, with no level");
  await page.getByText("the filter keeps this").first().waitFor();
  await sleep(300);
  if (await page.getByText("the filter hides this").count()) fail("a warning showed with warnings off");
  if (await page.getByText("and this, with no level").count()) fail("a line with no level showed with info off");
  if (!page.url().includes("levels=error")) fail(`the filter isn't in the URL: ${page.url()}`);
  await shoot(page, "errors");
  await page.getByRole("button", { name: "Info" }).click();
  await page.getByRole("button", { name: "Warnings", exact: true }).click();
  await page.getByRole("textbox", { name: "Search logs" }).fill("checkout");
  await page.locator("mark.dw-log-hit").first().waitFor();
  await page
    .getByText(/lines loaded match/)
    .first()
    .waitFor();
  await shoot(page, "search");
  step("Enter goes through what it found, and a match shows among the lines around it");
  await page.getByRole("textbox", { name: "Search logs" }).press("Enter");
  await page.locator(".dw-log-line.is-current").first().waitFor();
  await page.getByText(/^\d+ of \d+$/).waitFor();
  await page.locator(".dw-log-line.is-current").first().hover();
  await page.getByRole("button", { name: "Show in context" }).first().click();
  await page.getByRole("dialog").locator(".dw-log-line.is-current").waitFor();
  await page.keyboard.press("Escape");
  await page.getByRole("dialog").waitFor({ state: "hidden" });
  // On a phone, tapping a line shows what can be done with it, clear of what it says.
  await page.setViewportSize({ width: 360, height: 740 });
  await sleep(300);
  await page.locator(".dw-log-line:has(.dw-log-lvl-word)").first().click();
  await page.getByRole("button", { name: "Show in context" }).first().waitFor({ timeout: 2000 });
  const covered = await page.evaluate(() => {
    const line = document.querySelector(".dw-log-line.is-active");
    const tools = line?.querySelector(".dw-log-actions")?.getBoundingClientRect();
    if (!line || !tools) return "no line was tapped";
    return [...line.querySelectorAll(".dw-log-time, .dw-log-src, .dw-log-lvl, .dw-log-text")]
      .filter((el) => {
        const b = el.getBoundingClientRect();
        return (
          b.width > 0 && b.left < tools.right && tools.left < b.right && b.top < tools.bottom && tools.top < b.bottom
        );
      })
      .map((el) => el.className)
      .join(", ");
  });
  if (covered) fail(`on a phone, a line's actions sit over its ${covered}`);
  await page.setViewportSize({ width: 1440, height: 970 });
  await sleep(300);
  await page.getByRole("textbox", { name: "Search logs" }).fill("nothing has this in it");
  await page.getByText(/^Nothing in the [\d,]+ lines loaded has "nothing has this in it"\.$/).waitFor();
  await shoot(page, "no-match");
  step("search all that the servers keep");
  await page.getByRole("textbox", { name: "Search logs" }).fill("checkout");
  await page.getByRole("radio", { name: "All the servers keep" }).click();
  await page.getByText(/That is all the servers keep\./).waitFor();
  await page.getByText(/\d+ matches? in [\d,]+ lines, back to/).waitFor();
  await shoot(page, "deep-search");
  await page.getByRole("radio", { name: "Lines loaded" }).click();
  await page.getByRole("textbox", { name: "Search logs" }).press("Escape");

  step("copy and download what shows");
  await page.getByRole("button", { name: "Copy these lines" }).click();
  await until("the copy", async () => {
    const copied = await page.evaluate(() => navigator.clipboard.readText());
    return copied.includes("connection reset by peer") && !copied.includes("\u001b");
  });
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", { name: "Download these lines" }).click(),
  ]);
  const saved = readFileSync((await download.path()) ?? fail("no download"), "utf8");
  if (!saved.includes("connection reset by peer") || saved.includes("\u001b")) fail("the download isn't what shows");

  step("a rolling deploy shows both containers, by their names, for the seconds both run, and the head says what runs");
  // What the log has its head say of how many run, and never of replicas, as there are none.
  const logHead = async () => {
    const text = await page.locator(".dw-log-name").innerText();
    if (/replica/i.test(await page.locator("main").innerText())) fail(`the page speaks of replicas: ${text}`);
    return text;
  };
  const names = () =>
    (
      JSON.parse(dockwell("logs", "sources", "web", "--json")) as {
        sources: { containerName?: string; state: string }[];
      }
    ).sources;
  const ranBefore = new Set(names().map((s) => (s.containerName ?? "").replace(/^[^-]*-/, "")));
  const deploying = (image: string) =>
    new Promise<void>((resolve, reject) => {
      spawn(bin, ["services", "update", "web", "--app-image", image, "--yes"], { env: env(), stdio: "ignore" }).on(
        "exit",
        (code) => (code === 0 ? resolve() : reject(new Error(`the deploy of ${image} exited ${code}`))),
      );
    });
  const rolled = deploying("ghcr.io/acme/shop:next");
  // The new one has the next number, whatever that is by now.
  let newOne = "";
  await until("lines from what ran and from the new one, side by side", async () => {
    await post("/print", "2026-11-04 14:21:01 INFO both of them say so");
    const shown = await page.locator(".dw-log-src").allInnerTexts();
    newOne = shown.find((n) => !ranBefore.has(n)) ?? "";
    return newOne !== "" && shown.some((n) => ranBefore.has(n));
  });
  const during = await logHead();
  if (!/\d containers/.test(during)) fail(`during the move, the head says: ${during}`);
  await shoot(page, "rolling-two-sources");
  const shownNames = await page.locator(".dw-log-src").allInnerTexts();
  if (shownNames.some((n) => n.includes("stopping_"))) fail(`a source reads ${shownNames.join(", ")}`);
  await rolled;
  const headSettles = async (when: string) => {
    const end = Date.now() + 20_000;
    while (/containers/.test(await logHead())) {
      if (Date.now() > end)
        fail(`${when}, the head says: ${await logHead()}. The server lists:\n${dockwell("logs", "sources", "web")}`);
      await sleep(100);
    }
  };
  await headSettles("once the move is over");
  if (await page.getByRole("radio", { name: "All containers" }).count())
    fail("a filter is offered, with one container");
  await shoot(page, "rolled-one-running");
  // Where the new one began, by the name it goes by. In the whole of what's loaded, as
  // only the rows on screen are drawn, and that one has scrolled away by now.
  const [marked] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", { name: "Download these lines" }).click(),
  ]);
  const loaded = readFileSync((await marked.path()) ?? fail("no download"), "utf8");
  if (!loaded.includes(`-- new container ${newOne} of `)) {
    const said = loaded.split("\n").filter((l) => / -- .* --$/.test(l));
    fail(
      `what's loaded doesn't say where ${newOne} began. What it says of places in the log:\n${said.join("\n") || "nothing"}`,
    );
  }
  // However many it has read that have gone.
  for (const image of ["ghcr.io/acme/shop:next2", "ghcr.io/acme/shop:next3"]) await deploying(image);
  await headSettles("after several deploys");
  await post("/print", "2026-11-04 14:22:01 INFO only the one says this");
  await page.getByText("only the one says this").first().waitFor();
  if (/containers/.test(await logHead())) fail(`after several deploys, the head says: ${await logHead()}`);
  await shoot(page, "after-several-deploys");

  step("a failed deploy leaves its logs, and Activity links to them");
  try {
    dockwell("services", "update", "web", "--app-image", "ghcr.io/acme/shop:broken", "--yes");
  } catch {
    // It fails, as it should, and says so.
  }
  await page.getByText("giving up after 3 tries").waitFor({ timeout: 20_000 });
  await page.goto(`${origin}/activity`);
  await page.getByText("Update service web", { exact: true }).first().click();
  const logsLink = page.getByRole("link", { name: /Read this deploy's logs/ });
  await logsLink.first().waitFor();
  await logsLink.first().click();
  await page.getByText("giving up after 3 tries").waitFor();
  await page.getByRole("alert").getByText("failed at").waitFor();
  step("  it opens at the first error, not following, with what to do next");
  await page.locator(".dw-log-line.is-current").getByText("can't reach the database").waitFor();
  await page
    .getByRole("group", { name: "Errors in this log" })
    .getByText(/1 of \d+/)
    .waitFor();
  await shoot(page, "failed-deploy");
  await page.getByRole("radio", { name: "Running now" }).click();
  await page.waitForFunction(() => !window.location.search.includes("change="));

  step("the commands docker-e2e runs on real Docker read and list what was kept");
  const table = dockwell("logs", "sources", "web");
  if (!table.includes("Running") || !table.includes("Failed")) fail(`dockwell logs sources printed:\n${table}`);
  if (dockwell("logs", "read", "web", "--page-size", "50").trim() === "") fail("dockwell logs read printed no line");
  const listed = JSON.parse(dockwell("logs", "sources", "web", "--json")) as {
    sources: { state: string; revision?: string; kept?: string }[];
  };
  const failedRevision =
    listed.sources.find((s) => s.state === "LOG_SOURCE_STATE_KEPT" && s.kept === "LOG_KEPT_FAILED")?.revision ??
    fail("no source is kept as failed");
  if (!dockwell("logs", "read", "web", "--revision", failedRevision).includes("giving up after 3 tries"))
    fail("the failed deploy's kept log doesn't read back");
  // A background job of a script, as docker-e2e runs it, starts with SIGINT ignored, and goes
  // on saying its own process ID as it becomes the binary, for an interrupt that doesn't go through sudo.
  const followLog = join(data, "follow.log");
  const interrupted = execFileSync(
    "bash",
    [
      "-c",
      `bash -c 'echo "$$"; exec "$@"' follow "${bin}" logs follow web --tail 1 >"${followLog}" 2>&1 & p=$!; sleep 1; said=$(head -n 1 "${followLog}"); [[ "$said" == "$p" ]] || echo "said $said, is $p"; kill -INT "$said"; wait $p; echo $?`,
    ],
    { encoding: "utf8", env: env(), timeout: 20_000 },
  ).trim();
  if (interrupted !== "0")
    fail(`dockwell logs follow, interrupted as a script's background job, exited ${interrupted}`);

  step("the keyboard scrolls, stops following and goes back to it");
  await page.goto(url);
  // Off the log, as a line under the pointer draws its actions, and they take a Tab of their own.
  await page.mouse.move(0, 0);
  await logBody(page).locator(".dw-log-row").first().waitFor({ timeout: 10_000 });
  await logBody(page).focus();
  const ring = await logBody(page).evaluate((el) => getComputedStyle(el).boxShadow);
  await page.keyboard.press("Tab");
  await page.keyboard.press("Shift+Tab");
  const focused = await logBody(page).evaluate(
    (el) => el === document.activeElement && getComputedStyle(el).boxShadow !== "none",
  );
  if (!focused) fail(`the log shows no focus when reached by keyboard: ${ring}`);
  await page.screenshot({ path: shots ? join(shots, "keyboard-focus.png") : join(data, "keyboard-focus.png") });
  await page.keyboard.press("PageUp");
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  await page.keyboard.press("End");
  await page.getByRole("status").getByText("Following").waitFor({ timeout: 2000 });

  step("a server that goes offline is stated, and its logs carry on when it returns");
  const ticket =
    dockwell("token", "create")
      .split("\n")
      .map((l) => l.trim().split(/\s+/))
      .find((w) => w[0] === "dockwell" && w[1] === "join")?.[2] ?? fail("dockwell token create printed no ticket");
  const secondData = join(data, "second");
  execFileSync(bin, ["join", "--data-dir", secondData, "--name", "hz-2", ticket], { env: env(), stdio: "ignore" });
  const startSecond = () => {
    const kid = spawn(bin, ["run", "--data-dir", secondData, "--log-format", "json"], {
      env: { ...env(), DOCKWELL_DATA_DIR: secondData, DOCKER_HOST: `unix://${socket}` },
      stdio: "ignore",
    });
    kids.push(kid);
    return kid;
  };
  let second = startSecond();
  const servers = async () =>
    (
      JSON.parse(dockwell("servers", "list", "--json")) as {
        servers: { id: string; name: string; connection: string }[];
      }
    ).servers;
  const hz2 = await until("hz-2 to connect", async () =>
    (await servers()).find((s) => s.name === "hz-2" && s.connection === "CONNECTION_CONNECTED"),
  );
  dockwell("services", "create", "worker", "--on", hz2.id, "--app-image", "ghcr.io/acme/worker:1", "--yes");
  const list = JSON.parse(dockwell("resources", "list", "--kind", "service", "--json")) as {
    resources: { id: string; resource: { name: string } }[];
  };
  const workerId = list.resources.find((r) => r.resource.name === "worker")?.id ?? fail("no worker service");
  await page.goto(`${origin}/apps/${workerId}/logs`);
  await post("/print", "before the server went away");
  await page.getByText("before the server went away").waitFor();
  second.kill("SIGTERM");
  await new Promise((r) => second.once("exit", r));
  await page.getByText("hz-2 is offline").waitFor({ timeout: 40_000 });
  await post("/print", "while the server was away");
  await shoot(page, "offline");
  await page.setViewportSize({ width: 360, height: 740 });
  await sleep(300);
  const offlineRoom = await logBody(page).evaluate((el) => el.getBoundingClientRect().height / window.innerHeight);
  await page.setViewportSize({ width: 1440, height: 970 });
  step(`  with a note up, the log takes ${Math.round(offlineRoom * 100)}% of a phone's screen`);
  if (offlineRoom < 0.5)
    fail(`with a server offline, the log takes ${Math.round(offlineRoom * 100)}% of a phone's screen`);
  second = startSecond();
  await page.getByText("while the server was away").waitFor({ timeout: 30_000 });
  await until("the note to go", async () => (await page.getByText("hz-2 is offline").count()) === 0);
  const seen = (await logBody(page).textContent()) ?? "";
  if (seen.split("before the server went away").length !== 2) fail("a line came twice after the server returned");
  step("  the lines written while it was away arrived, with none twice");
  await post("/stop?service=worker");
  await page.getByText("worker isn't running on hz-2").waitFor({ timeout: 10_000 });
  await shoot(page, "stopped");
  second.kill("SIGTERM");
  await new Promise((r) => second.once("exit", r));

  step("the CLI follows, reads well, and stops on Ctrl-C with no error");
  const follower = spawn(bin, ["logs", "follow", "web", "--tail", "2"], {
    env: env(),
    stdio: ["ignore", "pipe", "pipe"],
  });
  let followed = "";
  let followErr = "";
  follower.stdout?.on("data", (d: Buffer) => {
    followed += d.toString();
  });
  follower.stderr?.on("data", (d: Buffer) => {
    followErr += d.toString();
  });
  await until("the CLI's stream to start", () => followed.length > 0);
  await post("/print", "from the CLI \u001b[2Jcheck");
  await until("the CLI to print the new line", () => followed.includes("from the CLI check"));
  if (followed.includes("\u001b")) fail(`the CLI passed an escape on: ${JSON.stringify(followed)}`);
  const exited = new Promise<number | null>((r) => follower.once("exit", (code) => r(code)));
  follower.kill("SIGINT");
  const code = await exited;
  if (code !== 0 || followErr.trim() !== "") fail(`dockwell logs follow on Ctrl-C: status ${code}, said ${followErr}`);
  step(`  printed ${JSON.stringify(followed.split("\n").at(-2))}, and exited with status 0`);

  step("the page at a hundred thousand lines, at 5,000 lines a second from each replica");
  await page.goto(url);
  const written = async () => Number((await (await fetch(`${load}/written`)).text()).trim());
  const writtenBefore = await written();
  await post("/rate?lines=5000&size=120");
  await page.getByRole("status").getByText("Following").waitFor();
  const stopFilling = await watchFrames(page);
  await sleep(21_000);
  const filling = await stopFilling();
  const wrote = (await written()) - writtenBefore;
  step(
    `  filling, 21 s, ${wrote.toLocaleString("en")} lines written: ${filling.frames} frames, p50 ${fmt(filling.p50)} ms, p95 ${fmt(filling.p95)} ms, worst ${fmt(filling.max)} ms; ${filling.longTasks} long tasks, longest ${fmt(filling.longest)} ms; JS heap ${fmt(filling.heapMb)} MB`,
  );
  // Generous, as machines vary. A line drawn too dearly shows up here, as one did.
  if (filling.p95 > 50) fail(`at 5,000 lines a second from each replica, the p95 frame took ${fmt(filling.p95)} ms`);
  await post("/rate?lines=0");
  const rows = await page.locator(".dw-log-row").count();
  step(`  ${rows} rows in the DOM`);
  if (rows > 300) fail(`${rows} rows are drawn, and only those that show should be`);
  // Scrolling through them all.
  const stopScrolling = await watchFrames(page);
  await logBody(page).hover();
  for (let i = 0; i < 60; i++) {
    await page.mouse.wheel(0, -4000);
    await sleep(16);
  }
  const scrolling = await stopScrolling();
  step(
    `  scrolling up: p50 ${fmt(scrolling.p50)} ms, p95 ${fmt(scrolling.p95)} ms, worst ${fmt(scrolling.max)} ms; ${scrolling.longTasks} long tasks`,
  );
  const search = page.getByRole("textbox", { name: "Search logs" });
  const typed = Date.now();
  await search.fill("products/4242 ");
  // It really is at the limit: the figures above are worth nothing otherwise.
  // Whichever it says, by how far back the newest 100,000 reach: of them, or none in them.
  await page
    .getByText(/(of|in) the 100,000 lines loaded/)
    .first()
    .waitFor({ timeout: 10_000 });
  step(
    `  a search over all of them, from typing to the answer (120 ms of that waits for the next key): ${Date.now() - typed} ms`,
  );
  await search.fill("");

  step("a search leaves a paused reader where they were");
  const topLine = () =>
    logBody(page).evaluate((el) => {
      const top = el.getBoundingClientRect().top;
      const rows = [...el.querySelectorAll(".dw-log-row")];
      return rows.find((row) => row.getBoundingClientRect().bottom > top + 1)?.textContent ?? "";
    });
  await sleep(500);
  const place = await topLine();
  if (!place) fail("no line shows at the top of the log");
  const stillThere = async (search_: string) => {
    await search.press("Escape");
    await sleep(500);
    const back = await topLine();
    if (back !== place) fail(`after ${search_}, the log went from ${JSON.stringify(place)} to ${JSON.stringify(back)}`);
  };
  await search.fill("no line says this");
  await page.getByText("Nothing in the 100,000 lines loaded", { exact: false }).waitFor({ timeout: 10_000 });
  await stillThere("a search that found nothing");
  await search.pressSequentially("products/nothing", { delay: 180 });
  await page.getByText("Nothing in the 100,000 lines loaded", { exact: false }).waitFor({ timeout: 10_000 });
  await stillThere("a search that found nothing, typed a key at a time");
  await search.fill("products/");
  await page
    .getByText(/of the 100,000 lines loaded/)
    .first()
    .waitFor({ timeout: 10_000 });
  await stillThere("a search that found some lines");

  step("a dropped stream leaves a pause as it was, with Follow there to press");
  await page.context().setOffline(true);
  await page.getByText("Reconnecting to the control server").waitFor({ timeout: 15_000 });
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  await page.getByRole("button", { name: "Follow", exact: true }).waitFor({ timeout: 2000 });
  if ((await topLine()) !== place) fail("the stream dropping moved a paused reader");
  await page.context().setOffline(false);
  await page.getByText("Reconnecting to the control server").waitFor({ state: "hidden", timeout: 30_000 });
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  if ((await topLine()) !== place) fail("the stream coming back moved a paused reader");

  step("a flood gets a marker saying how many lines it missed");
  await post("/flood?size=200");
  await page.getByRole("button", { name: "Follow", exact: true }).click();
  await page
    .getByText(/lines? not shown/)
    .first()
    .waitFor({ timeout: 10_000 });
  await page.screenshot({ path: shots ? join(shots, "flood.png") : join(data, "flood.png") });
  await post("/flood?size=0");
  // The last marker is in the log once it stops, for a reader who goes to it.
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await sleep(1500);
  const missed = await page.evaluate(() => {
    const el = document.querySelector('[role="log"]') as HTMLElement;
    el.scrollTop = el.scrollHeight;
    return new Promise<string>((res) => setTimeout(() => res(el.textContent ?? ""), 300));
  });
  if (!/not shown/.test(missed)) step("  (the last marker is further up than the screen)");
  // Going to the end follows again, unless what shows has only just changed, and the
  // button is gone if it did. The End key follows either way.
  await logBody(page).focus();
  await page.keyboard.press("End");
  await page.getByRole("status").getByText("Following").waitFor({ timeout: 5000 });

  step("while paused, what shows holds still, however fast lines come");
  await post("/rate?lines=16000&size=120");
  await page.getByRole("status").getByText("Following").waitFor();
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await page.getByRole("status").getByText("Paused").waitFor();
  const reading = async () =>
    page.evaluate(() => {
      const el = document.querySelector('[role="log"]') as HTMLElement;
      const box = el.getBoundingClientRect();
      const row = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)?.closest(".dw-log-row");
      return row?.textContent ?? "";
    });
  const held = await reading();
  // As long as a run once took to lose Follow, which a stream dropping would do.
  let down = 0;
  for (let second = 0; second < 40; second++) {
    await sleep(1000);
    if ((await page.getByText("Reconnecting to the control server").count()) > 0) down++;
  }
  step(`  paused for 40 s at 16,000 lines a second, with the stream down for ${down} s of them`);
  if (!held || (await reading()) !== held)
    fail("the line being read changed while paused, under 16,000 lines a second");
  await page.getByText(/Paused at \d\d:\d\d:\d\d\. [\d,]+ lines since\./).waitFor();
  await page.getByRole("button", { name: /10k\+ new lines/ }).waitFor();
  await page.getByRole("button", { name: "Follow", exact: true }).waitFor({ timeout: 2000 });
  await post("/rate?lines=0");
  await page.getByRole("button", { name: "Follow", exact: true }).click();

  step("text can be chosen while lines come, survives scrolling away, and copies as lines");
  await post("/rate?lines=200&size=60");
  await page.getByRole("status").getByText("Following").waitFor();
  await sleep(800);
  const logBox = (await logBody(page).boundingBox()) ?? fail("no log to choose text in");
  const textAt = logBox.x + 180;
  await page.mouse.move(textAt, logBox.y + 100);
  await page.mouse.down();
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  await page.mouse.move(textAt + 160, logBox.y + 180, { steps: 8 });
  await page.mouse.up();
  await sleep(300);
  const chosen = await page.evaluate(() => document.getSelection()?.toString() ?? "");
  if (!chosen.includes("GET")) fail(`choosing text while lines came chose ${JSON.stringify(chosen)}`);
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  await logBody(page).hover();
  for (let i = 0; i < 12; i++) await page.mouse.wheel(0, -4000);
  await sleep(300);
  const stillChosen = await page.evaluate(() => document.getSelection()?.isCollapsed === false);
  if (!stillChosen) fail("scrolling away lost what was chosen");
  await page.keyboard.press("Control+c");
  const copiedLines = await until("the copy", async () => {
    const copied = await page.evaluate(() => navigator.clipboard.readText());
    return copied.includes("\n") && /^\d\d:\d\d:\d\d\.\d{3} \S+ .*GET/m.test(copied) ? copied : undefined;
  });
  step(
    `  copied ${copiedLines.split("\n").length} lines, as ${JSON.stringify(copiedLines.split("\n")[0]?.slice(0, 60))}`,
  );
  await page.evaluate(() => document.getSelection()?.removeAllRanges());
  await post("/rate?lines=0");
  await page.getByRole("button", { name: "Follow", exact: true }).click();

  step("nothing moves as what is said changes");
  const places = () =>
    page.evaluate(() => {
      const top = (selector: string) => document.querySelector(selector)?.getBoundingClientRect();
      return {
        log: top('[role="log"]')?.top,
        copy: top('[aria-label="Copy these lines"]')?.left,
        title: top(".dw-log-name")?.left,
      };
    });
  await sleep(300);
  const following = await places();
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await sleep(200);
  const paused = await places();
  await page.getByRole("textbox", { name: "Search logs" }).fill("products");
  await sleep(300);
  const searching = await places();
  await page.getByRole("textbox", { name: "Search logs" }).press("Escape");
  await page.getByRole("button", { name: "Follow", exact: true }).click();
  for (const [name, moved] of Object.entries({ paused, searching })) {
    if (JSON.stringify(moved) !== JSON.stringify(following)) {
      fail(`${name}, what sits around the log moved: ${JSON.stringify(following)} to ${JSON.stringify(moved)}`);
    }
  }

  step("what shows holds AA contrast, in both themes");
  await post(
    "/print",
    "ERROR \u001b[31mred\u001b[0m \u001b[32mgreen\u001b[0m \u001b[33myellow\u001b[0m on an error's ground\n    carried on",
  );
  await page.getByText("on an error's ground").first().waitFor();
  for (const scheme of ["dark", "light"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    await colorsSettled(page);
    const low = await page.evaluate(contrastProblems);
    if (low.length > 0) fail(`in the ${scheme} theme: ${low.join("; ")}`);
  }
  await page.getByRole("button", { name: "Warnings", exact: true }).click();
  await page.getByRole("textbox", { name: "Search logs" }).fill("error");
  await page.locator("mark.dw-log-hit").first().waitFor();
  for (const scheme of ["dark", "light"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    await colorsSettled(page);
    const marks = await page.evaluate(contrastOfMarks);
    step(
      `  ${scheme}: a chip's edge on against off ${marks.chip.toFixed(2)}:1; a match against its line ${marks.hit.toFixed(2)}:1, its text ${marks.hitText.toFixed(2)}:1`,
    );
    if (marks.chip < 1.5) fail(`in the ${scheme} theme, a chip on and off differ by ${marks.chip.toFixed(2)}:1`);
    if (marks.hit < 1.2) fail(`in the ${scheme} theme, a match differs from its line by ${marks.hit.toFixed(2)}:1`);
    if (marks.hitText < 4.5) fail(`in the ${scheme} theme, a match's text is ${marks.hitText.toFixed(2)}:1`);
  }
  await page.emulateMedia({ colorScheme: "dark" });
  await page.getByRole("textbox", { name: "Search logs" }).press("Escape");
  await page.getByRole("button", { name: "Warnings", exact: true }).click();

  step("a phone gives the log the room, and targets of 44 px");
  await page.setViewportSize({ width: 360, height: 740 });
  await sleep(300);
  const room = await page.evaluate(() => {
    const body = document.querySelector('[role="log"]') as HTMLElement;
    const small: string[] = [];
    for (const el of document.querySelectorAll<HTMLElement>(".logs button, .logs a, .logs [role=radio]")) {
      const box = el.getBoundingClientRect();
      if (box.height > 0 && box.height < 44 && !el.closest(".dw-log-actions, .is-held")) {
        small.push(`${el.textContent || el.getAttribute("aria-label")}: ${box.height}`);
      }
    }
    const status = document.querySelector(".dw-log-head .dw-live")?.getBoundingClientRect();
    const tools = document.querySelector(".dw-log-head .dw-log-tools")?.getBoundingClientRect();
    return {
      share: body.getBoundingClientRect().height / window.innerHeight,
      tabbar: document.querySelector(".tabbar") !== null,
      small,
      errors: document.querySelector(".logs-errors-count")?.textContent ?? "",
      crowded: status && tools ? Math.round(status.right - tools.left) : 0,
    };
  });
  step(`  the log takes ${Math.round(room.share * 100)}% of a phone's screen`);
  if (room.tabbar) fail("a phone shows the tab bar on an app's logs");
  if (room.share < 0.6) fail(`the log takes ${Math.round(room.share * 100)}% of a phone's screen`);
  if (room.small.length > 0) fail(`targets under 44 px on a phone: ${room.small.join(", ")}`);
  if (!/\d,\d/.test(room.errors))
    fail(`the phone's log head needs thousands of errors to be tested, and has ${room.errors}`);
  if (room.crowded > 0) fail(`with ${room.errors}, a phone's log head runs ${room.crowded} px into its own tools`);
  await page.setViewportSize({ width: 1440, height: 970 });

  step("closing the page stops the reading on the agent");
  const followers = async () => Number((await (await fetch(`${load}/followers`)).text()).trim());
  await until("the page's follow", async () => (await followers()) > 0);
  const closed = Date.now();
  await page.close();
  await until("the agent to stop reading", async () => (await followers()) === 0, 5000);
  step(`  Docker's follow ended ${Date.now() - closed} ms after the page closed`);

  step("in Chinese");
  const zh = await browser.newContext({
    viewport: { width: 1440, height: 970 },
    colorScheme: "dark",
    locale: "zh-CN",
    storageState: await context.storageState(),
  });
  const zhPage = await zh.newPage();
  await zhPage.goto(url);
  await zhPage.getByRole("status").first().waitFor();
  await post("/print", "2026-11-04 14:20:00 ERROR 上游服务连接被重置 connection reset");
  await sleep(600);
  await shoot(zhPage, "zh-CN");
  await zh.close();
  step("all good");
}

async function finish(code: number) {
  await browser?.close().catch(() => {});
  for (const k of kids) k.kill("SIGTERM");
  await sleep(200);
  rmSync(data, { recursive: true, force: true });
  rmSync(socketDir, { recursive: true, force: true });
  process.exit(code);
}

main().then(
  () => finish(0),
  async (err: unknown) => {
    console.error(err);
    console.error(serverLog.join("").split("\n").slice(-30).join("\n"));
    // What the page says of itself, as a step can fail for want of a button that a state took away.
    const said = await lastPage
      ?.evaluate(() => ({
        url: location.href,
        head: document.querySelector(".dw-log-head")?.textContent,
        foot: document.querySelector(".dw-log-foot")?.textContent,
        notes: [...document.querySelectorAll(".logs-note, [role=alert]")].map((el) => el.textContent),
      }))
      .catch(() => undefined);
    console.error("the page said:", said);
    if (shots && lastPage) {
      mkdirSync(shots, { recursive: true });
      await lastPage.screenshot({ path: join(shots, "failure.png") }).catch(() => {});
    }
    await finish(1);
  },
);
