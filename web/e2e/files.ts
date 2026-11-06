// SPDX-License-Identifier: AGPL-3.0-only

// Walks a server's files in headless Chromium, against a real `dockwell run` and
// its own agent, on this machine's disk, in a temporary directory: browsing, a
// file, what isn't text, what's too large, what's refused, a path that leads
// nowhere, and a phone. Each read and refusal shows on Activity, and the CLI's
// token is refused. With --shots <dir> it saves screenshots at 1440, 800, 390 and
// 360 px, light and dark.
//
//   make e2e
//   node web/e2e/files.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, chromium, type Page } from "playwright-core";

const { values, positionals } = parseArgs({ allowPositionals: true, options: { shots: { type: "string" } } });
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7050 + Math.floor(Math.random() * 300);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-files-"));
const site = mkdtempSync(join(tmpdir(), "dockwell-site-"));

let server: ChildProcess | undefined;
let browser: Browser | undefined;
let lastPage: Page | undefined;
const serverLog: string[] = [];

function step(what: string) {
  console.log(`files e2e: ${what}`);
}

function fail(message: string): never {
  throw new Error(message);
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

async function until<T>(what: string, fn: () => T | Promise<T>, timeout = 15_000): Promise<NonNullable<T>> {
  const end = Date.now() + timeout;
  for (;;) {
    const got = await fn();
    if (got) return got;
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

async function start(): Promise<void> {
  server = spawn(
    bin,
    ["run", "--listen", `127.0.0.1:${port}`, "--agent-listen", "127.0.0.1:0", "--log-format", "json"],
    { env: env(), stdio: ["ignore", "pipe", "pipe"] },
  );
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

/** A site, with what a person finds on one, and what an app could leave in it. */
function makeSite() {
  writeFileSync(join(site, "nginx.conf"), "server {\n  listen 80;\n  root /var/www;\n}\n");
  writeFileSync(join(site, "说明.txt"), "你好\n");
  writeFileSync(join(site, "logo.png"), Buffer.from([0x89, 0x50, 0x4e, 0x47, 0, 0, 0, 0]));
  writeFileSync(join(site, "huge.log"), "a line of a log\n".repeat(100_000));
  writeFileSync(join(site, "server.key"), "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n");
  mkdirSync(join(site, "conf.d"));
  writeFileSync(join(site, "conf.d", "default.conf"), "# nothing yet\n");
  symlinkSync(join(data, "control", "key"), join(site, "planted"));
  symlinkSync("conf.d", join(site, "config"));
  symlinkSync("nowhere", join(site, "broken"));
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
  await page.emulateMedia({ colorScheme: "light" });
  await page.setViewportSize({ width: 1440, height: 970 });
  await sleep(100);
}

/** Fails when the page scrolls sideways at a width: every view works from 360 px up. */
async function noSidewaysScroll(page: Page, what: string) {
  for (const width of [390, 360]) {
    await page.setViewportSize({ width, height: 844 });
    await sleep(150);
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    if (over > 0) fail(`${what} scrolls sideways by ${over} px at ${width} px`);
  }
}

async function main() {
  makeSite();
  await start();
  step("claim the install, and open a session");
  const link = await until(
    "a claim link",
    () => serverLog.join("").match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]+/)?.[0],
  );
  execFileSync("go", ["run", "./scripts/claim", link], { stdio: "inherit" });
  const recover = execFileSync(bin, ["recover", "--log-format", "json"], { encoding: "utf8", env: env() });
  const signIn =
    recover.match(/(http:\/\/localhost:\d+\/recover#[A-Za-z0-9_-]{43})/)?.[1] ??
    fail(`dockwell recover printed:\n${recover}`);

  browser = await chromium.launch({ executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium" });
  const context = await browser.newContext({ viewport: { width: 1440, height: 970 }, colorScheme: "light" });
  const page = await context.newPage();
  lastPage = page;
  page.on("pageerror", (err) => fail(`the page threw: ${err.message}`));
  await page.goto(signIn);
  await page.getByText("You signed in with a one-time link").first().waitFor();

  step("Files is a tab of the server's page");
  await page.goto(`${origin}/`);
  await page
    .getByRole("link", { name: /^Server/ })
    .first()
    .click();
  await page.getByRole("link", { name: "Files" }).click();
  await page.getByRole("table", { name: "Files in /" }).waitFor();
  await page.getByRole("cell", { name: "etc", exact: false }).first().waitFor();
  const serverId = new URL(page.url()).pathname.split("/")[2] ?? fail(`no server in ${page.url()}`);
  const at = (search: Record<string, string>) =>
    `${origin}/servers/${serverId}/files?${new URLSearchParams(search).toString()}`;
  const proc = page.getByRole("row", { name: /proc/ });
  if (!(await proc.getByText("Dockwell doesn't open this").count())) fail("/proc isn't marked as refused");

  step("browse a folder, and follow a link to another");
  await page.goto(at({ path: site }));
  await page.getByRole("table", { name: `Files in ${site}` }).waitFor();
  await page.getByText("nginx.conf").waitFor();
  await page.getByText("说明.txt").waitFor();
  await page.getByText("→ conf.d").click();
  await page.getByText("default.conf").waitFor();
  await page.getByRole("navigation", { name: "Path" }).getByText("conf.d").waitFor();
  await page.goto(at({ path: site }));
  await shoot(page, "folder");
  await noSidewaysScroll(page, "a folder");
  await page.setViewportSize({ width: 1440, height: 970 });

  step("open a file");
  await page.getByText("nginx.conf").click();
  const code = page.getByRole("region", { name: "Contents of nginx.conf" });
  await code.waitFor();
  await code.getByText("listen 80;").waitFor();
  await page.getByText("4 lines").waitFor();
  await shoot(page, "file");

  step("what isn't text, and what's too large");
  await page.getByText("logo.png").click();
  await page.getByText("This isn't a text file").waitFor();
  await page.getByText("huge.log").click();
  await page.getByText("Showing the first 1 MB").waitFor();
  await page.getByRole("region", { name: "Contents of huge.log" }).getByText("a line of a log").first().waitFor();
  await shoot(page, "large-file");

  step("what's refused, and what leads nowhere");
  await page.getByText("server.key").click();
  await page.getByText("Dockwell doesn't open this file").waitFor();
  await page.getByText("a private key", { exact: false }).waitFor();
  await page.getByRole("row", { name: /planted/ }).click();
  await page.getByText("Dockwell's own data", { exact: false }).waitFor();
  await shoot(page, "refused-file");
  await page.getByRole("row", { name: /broken/ }).click();
  await page.getByText("There's nothing at", { exact: false }).waitFor();
  await page.goto(at({ path: "/proc" }));
  await page.getByText("Dockwell doesn't open this folder").waitFor();
  await shoot(page, "refused-folder");
  await page.goto(at({ path: join(site, "nothing") }));
  await page.getByText("Nothing at this path").waitFor();
  await page.getByRole("link", { name: "Go up to" }).click();
  await page.getByText("nginx.conf").waitFor();

  step("a file on a phone takes the screen, and its header goes back");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(at({ path: site }));
  await page.getByText("nginx.conf").click();
  await page.getByRole("heading", { level: 1, name: "nginx.conf" }).waitFor();
  await page.getByRole("region", { name: "Contents of nginx.conf" }).getByText("listen 80;").waitFor();
  await noSidewaysScroll(page, "an open file");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("link", { name: `Back to ${site}` }).click();
  await page.getByText("说明.txt").waitFor();

  step("what was read, and what was refused, is on Activity");
  await page.setViewportSize({ width: 1440, height: 970 });
  await page.goto(`${origin}/activity`);
  const conf = join(site, "nginx.conf");
  await page.getByText(`Read ${conf} on`).first().waitFor();
  for (const refused of ["server.key", "planted", "broken"]) {
    await page
      .getByText(`Couldn't open ${join(site, refused)} on`)
      .first()
      .waitFor();
  }
  await page
    .getByText(`Couldn't open ${join(site, "nothing")} on`)
    .first()
    .waitFor();
  await page.getByText("Couldn't open /proc on").first().waitFor();
  await shoot(page, "activity");
  await page.getByText(`Read ${conf} on`).first().click();
  await page.getByRole("link", { name: "Open the file" }).click();
  await page.getByRole("region", { name: "Contents of nginx.conf" }).waitFor();

  step("a token is refused");
  dockwell("login", "--local");
  const denied = spawnSync(bin, ["files", "list", "--server-id", serverId, "--path", "/", "--json"], {
    encoding: "utf8",
    env: env(),
  });
  if (denied.status === 0) fail(`the CLI's token listed files:\n${denied.stdout}`);
  if (!`${denied.stdout}${denied.stderr}`.includes("reach a server's files")) {
    fail(`the CLI's token was refused for another reason:\n${denied.stdout}${denied.stderr}`);
  }
}

async function finish(code: number): Promise<never> {
  await browser?.close().catch(() => {});
  server?.kill("SIGTERM");
  await sleep(200);
  rmSync(data, { recursive: true, force: true });
  rmSync(site, { recursive: true, force: true });
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
