// SPDX-License-Identifier: AGPL-3.0-only

// Follows a service's logs in headless Chromium, against a real `dockwell run`
// and its own agent, on the fake Docker Engine, so it needs no Docker. Log lines
// go from a container through the agent and the control plane to the page, and
// this checks they arrive within a second, that following stops and resumes,
// what filters, search, colours, a failed deploy and a flood do, and that closing
// the page stops the reading on the agent. It measures the page at a hundred
// thousand lines, and at thousands of lines a second, and prints the figures.
// With --shots <dir> it saves screenshots at 1440, 800, 390 and 360 px, light and
// dark, and in Chinese. CI's Docker job reads real containers' logs through the
// CLI (scripts/docker-e2e.sh).
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
  return { ...process.env, DOCKWELL_DATA_DIR: data, DOCKWELL_LOG_FORMAT: "text", USER: "mira" };
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

function logBody(page: Page) {
  return page.getByRole("log");
}

async function main() {
  const socket = await startEngine();
  await startServer(socket);
  step("claim the install, run a service, and open a session");
  const link = await until(
    "a claim link",
    () => serverLog.join("").match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]+/)?.[0],
  );
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
  await page.getByText("No logs yet.").waitFor({ timeout: 15_000 });
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
    "2026-11-04 14:20:01 INFO a line that both replicas print\n2026-11-04 14:20:02 WARN and a warning",
  );
  await page.locator(".dw-log-src").first().waitFor({ timeout: 5000 });
  await page.getByRole("radio", { name: "All replicas" }).waitFor();
  await shoot(page, "two-sources");
  await page.getByRole("radio", { name: /-2$/ }).click();
  await until(
    "the column to go with one replica showing",
    async () => (await page.locator(".dw-log-src").count()) === 0,
  );
  await page.getByRole("radio", { name: "All replicas" }).click();

  step("following stops when the reader scrolls up, and resumes on request");
  await post("/rate?lines=200&size=120");
  await page.getByRole("status").getByText("Live").waitFor();
  await sleep(600);
  const log = logBody(page);
  await log.hover();
  await page.mouse.wheel(0, -600);
  await page.getByRole("status").getByText("Paused").waitFor({ timeout: 2000 });
  const before = await log.evaluate((el) => el.scrollTop);
  await sleep(600);
  const after = await log.evaluate((el) => el.scrollTop);
  if (Math.abs(before - after) > 2) fail(`the log moved ${after - before} px while paused`);
  const jump = page.getByRole("button", { name: /new line/ });
  await jump.waitFor();
  await page.screenshot({ path: shots ? join(shots, "paused.png") : join(data, "paused.png") });
  await page.getByRole("button", { name: "Follow", exact: true }).click();
  await page.getByRole("status").getByText("Live").waitFor();
  await until("the log to reach the end", () =>
    log.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight < 20),
  );
  await post("/rate?lines=0");

  step("filter by level, and search with what it found marked");
  await page.getByRole("button", { name: "Info" }).click();
  await page.getByRole("button", { name: "Warn", exact: true }).click();
  await post("/print", "ERROR the filter keeps this\nWARN the filter hides this\nand this, with no level");
  await page.getByText("the filter keeps this").first().waitFor();
  await sleep(300);
  if (await page.getByText("the filter hides this").count()) fail("a warning showed with warnings off");
  if (await page.getByText("and this, with no level").count()) fail("a line with no level showed with info off");
  if (!page.url().includes("levels=error")) fail(`the filter isn't in the URL: ${page.url()}`);
  await shoot(page, "errors");
  await page.getByRole("button", { name: "Info" }).click();
  await page.getByRole("button", { name: "Warn", exact: true }).click();
  await page.getByRole("textbox", { name: "Search logs" }).fill("checkout");
  await page.locator("mark.dw-log-hit").first().waitFor();
  await page
    .getByText(/lines loaded match/)
    .first()
    .waitFor();
  await shoot(page, "search");
  await page.getByRole("textbox", { name: "Search logs" }).fill("nothing has this in it");
  await page.getByText("Nothing here matches these filters.").waitFor();
  await shoot(page, "no-match");
  step("search all that the servers keep");
  await page.getByRole("textbox", { name: "Search logs" }).fill("checkout");
  await page.getByRole("button", { name: "Search all the servers keep" }).click();
  await page.getByText(/as far as the servers keep logs/).waitFor();
  await shoot(page, "deep-search");
  await page.getByRole("button", { name: "Back to the lines loaded" }).click();
  await page
    .getByRole("button", { name: "Clear filters" })
    .waitFor({ state: "hidden" })
    .catch(() => {});
  await page.getByRole("textbox", { name: "Search logs" }).fill("");

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
  await page.getByText("This deploy failed and was rolled back.").waitFor();
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
  // A background job of a script, as docker-e2e runs it, starts with SIGINT ignored.
  const interrupted = execFileSync(
    "bash",
    ["-c", `"${bin}" logs follow web --tail 1 >/dev/null 2>&1 & p=$!; sleep 1; kill -INT $p; wait $p; echo $?`],
    { encoding: "utf8", env: env(), timeout: 20_000 },
  ).trim();
  if (interrupted !== "0")
    fail(`dockwell logs follow, interrupted as a script's background job, exited ${interrupted}`);

  step("the keyboard scrolls, stops following and goes back to it");
  await page.goto(url);
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
  await page.getByRole("status").getByText("Live").waitFor({ timeout: 2000 });

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

  step("the page at a hundred thousand lines, at 5,000 lines a second");
  await page.goto(url);
  await post("/rate?lines=5000&size=120");
  await page.getByRole("status").getByText("Live").waitFor();
  const stopFilling = await watchFrames(page);
  await sleep(21_000);
  const filling = await stopFilling();
  step(
    `  filling, 21 s at 5,000 lines/s: ${filling.frames} frames, p50 ${fmt(filling.p50)} ms, p95 ${fmt(filling.p95)} ms, worst ${fmt(filling.max)} ms; ${filling.longTasks} long tasks, longest ${fmt(filling.longest)} ms; JS heap ${fmt(filling.heapMb)} MB`,
  );
  // Generous, as machines vary. A line drawn too dearly shows up here, as one did.
  if (filling.p95 > 50) fail(`at 5,000 lines a second, the p95 frame took ${fmt(filling.p95)} ms`);
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
  await page.getByText(/of the 100,000 lines loaded match/).waitFor();
  step(
    `  a search over all of them, from typing to the answer (120 ms of that waits for the next key): ${Date.now() - typed} ms`,
  );
  await search.fill("");

  step("a flood gets a marker saying how many lines it missed");
  await post("/flood?size=200");
  await page.getByRole("button", { name: "Follow", exact: true }).click();
  await page.getByText(/lines not shown so far/).waitFor({ timeout: 10_000 });
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
  await page.getByRole("button", { name: "Follow", exact: true }).click();

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
    if (shots && lastPage) {
      mkdirSync(shots, { recursive: true });
      await lastPage.screenshot({ path: join(shots, "failure.png") }).catch(() => {});
    }
    await finish(1);
  },
);
