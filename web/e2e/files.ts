// SPDX-License-Identifier: AGPL-3.0-only

// Walks a server's files in headless Chromium, against a real `dockwell run` and
// its own agent, on this machine's disk, in a temporary directory: browsing, a
// file, what isn't text, what's too large, what's refused, a path that leads
// nowhere, and a phone. It edits and saves a file, on a desktop and a phone, finds
// a file changed under it, keeps what it typed, stops at a link swapped in, keeps a
// byte order mark, which this build's binary format loses on the way in, and finds
// what it did on Activity.
// The CLI's token is refused. With --shots <dir> it saves screenshots at 1440, 800,
// 390 and 360 px, light and dark.
//
//   make e2e
//   node web/e2e/files.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, chromium, type Page } from "playwright-core";

// A real binary asks nothing beyond this machine in a test.
process.env["DOCKWELL_TEST"] = "1";

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
  writeFileSync(join(site, "app.ini"), "\uFEFFname = app\n");
  writeFileSync(join(site, "mixed.sh"), "#!/bin/sh\necho a\r\necho b\n");
  writeFileSync(join(site, "config.yml"), "deploy_key: AAAA\n");
  writeFileSync(join(site, "authorized_keys"), "deploy_key: AAAA\n");
  symlinkSync(join(data, "control", "key"), join(site, "planted"));
  symlinkSync("conf.d", join(site, "config"));
  symlinkSync("nowhere", join(site, "broken"));
  // A folder longer than its pane, as /etc is.
  mkdirSync(join(site, "many"));
  for (let i = 0; i < 120; i++) writeFileSync(join(site, "many", `part-${String(i).padStart(3, "0")}.conf`), "");
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
  // dockwell run prints a claim link only to a terminal, so this asks for one.
  await until("dockwell run to say how to claim it", () => serverLog.join("").includes("To claim it, run this"));
  const claimed = execFileSync(bin, ["claim", "--log-format", "json"], { encoding: "utf8", env: env() });
  const link =
    claimed.match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]+/)?.[0] ?? fail(`dockwell claim printed:\n${claimed}`);
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

  step("a folder longer than its pane scrolls there, and the page is no taller for it");
  const pageHeight = () => page.evaluate(() => document.documentElement.scrollHeight);
  const short = await pageHeight();
  await page.goto(at({ path: join(site, "many") }));
  await page.getByText("part-119.conf").waitFor({ state: "attached" });
  const long = await pageHeight();
  if (long > short)
    fail(`a folder of 120 files makes the page ${long - short} px taller than a short one, past its pane`);
  await page.goto(at({ path: site }));
  await page.getByText("nginx.conf").waitFor();

  step("open a file");
  await page.getByText("nginx.conf").click();
  const code = page.getByRole("textbox", { name: "Contents of nginx.conf" });
  await code.waitFor();
  if (!(await code.inputValue()).includes("listen 80;")) fail("the file reads as something else");
  await page.getByText("4 lines").waitFor();
  await shoot(page, "file");

  step("edit a file and save it, in place");
  const conf = join(site, "nginx.conf");
  const inode = statSync(conf).ino;
  await code.fill("server {\n  listen 8080;\n}\n");
  await page.getByText("Edited").waitFor();
  await shoot(page, "edited");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("Saved nginx.conf").first().waitFor();
  if (readFileSync(conf, "utf8") !== "server {\n  listen 8080;\n}\n") fail("the save isn't on the disk");
  if (statSync(conf).ino !== inode) fail("the save replaced the file, so a container that mounts it keeps the old one");

  step("a file that changed since is noticed, and what you typed stays");
  await code.fill("mine\n");
  writeFileSync(conf, "theirs\n");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("This file changed on the server").waitFor();
  if ((await code.inputValue()) !== "mine\n") fail("a refused save lost what was typed");
  if (readFileSync(conf, "utf8") !== "theirs\n") fail("a refused save wrote the file");
  await shoot(page, "conflict");
  await page.getByRole("button", { name: "Save over it" }).click();
  await page.getByText("Saved nginx.conf").first().waitFor();
  if (readFileSync(conf, "utf8") !== "mine\n") fail("saving over it didn't write the file");

  step("leaving what you typed asks first");
  await code.fill("unsaved\n");
  page.once("dialog", (dialog) => void dialog.dismiss());
  await page.getByRole("button", { name: "Close nginx.conf" }).click();
  await sleep(200);
  if ((await code.inputValue()) !== "unsaved\n") fail("leaving took what was typed without asking");
  await page.getByRole("button", { name: "Discard" }).click();
  await page.getByRole("button", { name: "Close nginx.conf" }).click();
  await code.waitFor({ state: "detached" });
  await page.getByText("nginx.conf").click();
  await code.waitFor();

  step("a byte order mark stays through a save, on the format the panel runs on");
  await page.getByText("app.ini").click();
  const ini = page.getByRole("textbox", { name: "Contents of app.ini" });
  await ini.waitFor();
  await ini.fill("name = changed\n");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("Saved app.ini").first().waitFor();
  if (!readFileSync(join(site, "app.ini")).equals(Buffer.from("\uFEFFname = changed\n"))) {
    fail(`a save changed bytes nobody touched: ${readFileSync(join(site, "app.ini")).toString("hex")}`);
  }

  step("a file with more than one kind of line ending reads, and doesn't edit");
  await page.getByText("mixed.sh").click();
  await page.getByText("This file can't be edited here").waitFor();
  await page.getByRole("button", { name: "Close mixed.sh" }).click();

  step("a link swapped in under what you typed isn't saved through, whatever its text");
  const swapped = join(site, "config.yml");
  const keys = join(site, "authorized_keys");
  await page.getByText("config.yml").click();
  const yml = page.getByRole("textbox", { name: "Contents of config.yml" });
  await yml.waitFor();
  await yml.fill("deploy_key: what a person typed\n");
  rmSync(swapped);
  symlinkSync(keys, swapped);
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("This file changed on the server").waitFor();
  await page.getByRole("button", { name: "Save over it" }).click();
  await page.getByText("This isn't the file you opened").waitFor();
  await page.getByText(keys).first().waitFor();
  await shoot(page, "swapped-link");
  if (readFileSync(keys, "utf8") !== "deploy_key: AAAA\n") fail("a save went through a link swapped in");
  await page.getByRole("button", { name: "Discard" }).click();
  await page.getByRole("button", { name: "Close config.yml" }).click();
  await yml.waitFor({ state: "detached" });

  step("a file that's a way back in asks for a passkey, and saves nothing without one");
  await page.getByRole("row", { name: /authorized_keys/ }).click();
  const authorized = page.getByRole("textbox", { name: "Contents of authorized_keys" });
  await authorized.waitFor();
  await authorized.fill("ssh-ed25519 a key typed here\n");
  await page.getByText("It asks for your passkey", { exact: false }).waitFor();
  // This browser has no authenticator, so the prompt closes, as a person can close it.
  await page.evaluate(() => {
    navigator.credentials.get = () => Promise.reject(new DOMException("closed", "NotAllowedError"));
  });
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("The passkey prompt closed", { exact: false }).waitFor();
  await shoot(page, "way-back-in");
  if (readFileSync(keys, "utf8") !== "deploy_key: AAAA\n") fail("a way back in saved with no passkey");
  await page.getByRole("button", { name: "Discard" }).click();
  await page.getByRole("button", { name: "Close authorized_keys" }).click();
  await authorized.waitFor({ state: "detached" });

  step("what isn't text, and what's too large");
  await page.getByText("logo.png").click();
  await page.getByText("This isn't a text file").waitFor();
  await page.getByText("huge.log").click();
  await page.getByText("Showing the first 1 MB").waitFor();
  await page.getByRole("textbox", { name: "Contents of huge.log" }).waitFor();
  await page.getByText("it can't be edited here", { exact: false }).waitFor();
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
  await noSidewaysScroll(page, "an open file");
  await page.setViewportSize({ width: 390, height: 844 });
  await code.fill("edited on a phone\n");
  const save = page.getByRole("button", { name: "Save", exact: true });
  await save.click();
  await page.getByText("Saved nginx.conf").first().waitFor();
  if (readFileSync(conf, "utf8") !== "edited on a phone\n") fail("a phone's save isn't on the disk");
  await page.getByRole("link", { name: `Back to ${site}` }).click();
  await page.getByText("说明.txt").waitFor();

  step("what was read, saved and refused is on Activity, and opens the file");
  await page.setViewportSize({ width: 1440, height: 970 });
  await page.goto(`${origin}/activity`);
  await page.getByText(`Read ${conf} on`).first().waitFor();
  await page.getByText(`Couldn't save ${conf} on`).first().waitFor();
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
  await page.getByText(`Saved ${conf} on`).first().click();
  await page.getByRole("link", { name: "Open the file" }).click();
  await page.getByRole("textbox", { name: "Contents of nginx.conf" }).waitFor();

  step("a token is refused");
  dockwell("login", "--local");
  // The control plane refuses a list. The CLI knows a save takes a person before it asks.
  for (const [says, ...args] of [
    ["reach a server's files", "files", "list", "--server-id", serverId, "--path", "/", "--json"],
    [
      "no API token may do",
      "files",
      "save",
      "--server-id",
      serverId,
      "--path",
      conf,
      "--content",
      "x",
      "--version",
      "v",
    ],
  ] as [string, ...string[]][]) {
    const denied = spawnSync(bin, args, { encoding: "utf8", env: env() });
    if (denied.status === 0) fail(`the CLI's token ran ${args.join(" ")}:\n${denied.stdout}`);
    if (!`${denied.stdout}${denied.stderr}`.includes(says)) {
      fail(`the CLI's token was refused for another reason:\n${denied.stdout}${denied.stderr}`);
    }
  }
  if (readFileSync(conf, "utf8") !== "edited on a phone\n") fail("a token changed the file");
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
