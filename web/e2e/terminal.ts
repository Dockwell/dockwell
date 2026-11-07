// SPDX-License-Identifier: AGPL-3.0-only

// Walks a server's terminal in headless Chromium, against a real `dockwell run`
// and its own agent, with a virtual passkey: it claims the install, opens a
// terminal with the passkey, types into a real shell on this machine and reads
// what it prints, resizes it, finds, copies and pastes, loses the network and
// carries on from where it was with what was typed meanwhile, comes back after a
// reload, is moved to another tab and brought back, opens and closes a second
// session, ends with the shell, and finds who opened and closed it on Activity,
// and nothing typed in the audit log. The CLI's token is refused. With --shots
// <dir> it saves screenshots at 1440 and 390 px, light and dark, and in Chinese.
//
//   make e2e
//   node web/e2e/terminal.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { request } from "node:http";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, type BrowserContext, chromium, type Page } from "playwright-core";

// A real binary asks nothing beyond this machine in a test.
process.env["DOCKWELL_TEST"] = "1";

const { values, positionals } = parseArgs({
  allowPositionals: true,
  options: { shots: { type: "string" }, "fake-docker": { type: "string" } },
});
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7360 + Math.floor(Math.random() * 280);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-terminal-"));

let server: ChildProcess | undefined;
let docker: ChildProcess | undefined;
let dockerSocket = "";
let browser: Browser | undefined;
let lastPage: Page | undefined;
const serverLog: string[] = [];

function step(what: string) {
  console.log(`terminal e2e: ${what}`);
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
    ...(dockerSocket ? { DOCKER_HOST: `unix://${dockerSocket}` } : {}),
  };
}

/** Starts the fake Docker Engine, when there's one to start, and says where it listens. */
async function fakeDocker(): Promise<void> {
  const path = values["fake-docker"];
  if (!path) return;
  docker = spawn(resolve(path), [join(data, "docker")], { stdio: ["ignore", "pipe", "inherit"] });
  dockerSocket = await new Promise<string>((done, refused) => {
    const silent = setTimeout(() => refused(new Error(`${path} said nothing for 15 s, so it isn't serving`)), 15_000);
    docker?.stdout?.once("data", (b: Buffer) => {
      clearTimeout(silent);
      done(b.toString().trim());
    });
    docker?.once("exit", (code, signal) => {
      clearTimeout(silent);
      refused(new Error(`${path} ended with ${code ?? signal} before it said where it serves`));
    });
  });
}

/** Asks Docker something on its socket, as docker run by hand does. */
function dockerAPI(method: string, path: string, body?: unknown): Promise<{ status: number; text: string }> {
  return new Promise((done, failed) => {
    const req = request(
      { socketPath: dockerSocket, path, method, headers: { "Content-Type": "application/json" } },
      (res) => {
        let text = "";
        res.on("data", (d: Buffer) => {
          text += d.toString();
        });
        res.on("end", () => done({ status: res.statusCode ?? 0, text }));
      },
    );
    req.on("error", failed);
    req.end(body === undefined ? undefined : JSON.stringify(body));
  });
}

/** Runs a container behind Dockwell's back, as someone at the server would. */
async function runByHand(name: string, image: string, more: Record<string, unknown> = {}) {
  const [repo, tag] = image.split(":");
  const pulled = await dockerAPI("POST", `/images/create?fromImage=${repo}&tag=${tag}`);
  if (pulled.status >= 300) fail(`pulling ${image}: ${pulled.status} ${pulled.text}`);
  const made = await dockerAPI("POST", `/containers/create?name=${name}`, { Image: image, ...more });
  if (made.status >= 300) fail(`making ${name}: ${made.status} ${made.text}`);
  const started = await dockerAPI("POST", `/containers/${name}/start`);
  if (started.status >= 300) fail(`starting ${name}: ${started.status} ${started.text}`);
}

function dockwell(...args: string[]): string {
  return execFileSync(bin, args, { encoding: "utf8", env: env() });
}

async function start(): Promise<string> {
  await fakeDocker();
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
  await until("dockwell run to say how to claim it", () => serverLog.join("").includes("To claim it, run this"));
  const claimed = dockwell("claim", "--log-format", "json");
  return (
    claimed.match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]+/)?.[0] ?? fail(`dockwell claim printed:\n${claimed}`)
  );
}

/** A platform authenticator that does user verification, as Touch ID does. */
async function withAuthenticator(context: BrowserContext, page: Page) {
  const cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable", { enableUI: false });
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return { cdp, authenticatorId };
}

async function shoot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  for (const scheme of ["dark", "light"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: width === 1440 ? 930 : 844 });
      await sleep(400);
      await page.screenshot({ path: join(shots, `${name}-${width}-${scheme}.png`) });
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await page.setViewportSize({ width: 1440, height: 930 });
  await sleep(300);
}

/** What the terminal on screen shows, row by row, as text. */
async function screenText(page: Page): Promise<string> {
  const rows = page.locator(".terminal-screen:not([hidden]) .xterm-rows");
  if ((await rows.count()) === 0) return "";
  return (await rows.innerText()).replaceAll(" ", " ");
}

async function shows(page: Page, text: string, timeout = 15_000) {
  await until(
    `the terminal to show ${JSON.stringify(text)}`,
    async () => (await screenText(page)).includes(text),
    timeout,
  );
}

/** Types a line into the terminal on screen, as a person would. */
async function typeLine(page: Page, line: string) {
  await page.locator(".terminal-screen:not([hidden]) .xterm").click();
  await page.keyboard.type(line);
  await page.keyboard.press("Enter");
}

async function noSidewaysScroll(page: Page, what: string) {
  for (const width of [390, 360]) {
    await page.setViewportSize({ width, height: 844 });
    await sleep(200);
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    if (over > 0) fail(`${what} scrolls sideways by ${over} px at ${width} px`);
  }
  await page.setViewportSize({ width: 1440, height: 930 });
}

async function main() {
  const claimLink = await start();
  browser = await chromium.launch({ executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium" });
  const context = await browser.newContext({ viewport: { width: 1440, height: 930 }, colorScheme: "light" });
  await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin });
  const page = await context.newPage();
  lastPage = page;
  page.on("pageerror", (err) => fail(`the page threw: ${err.message}`));
  const { cdp, authenticatorId } = await withAuthenticator(context, page);

  step("claim the install with a passkey");
  await page.goto(claimLink);
  await page.getByLabel("Your name").fill("Mira Chen");
  await page.getByLabel("Email").fill("mira@example.com");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await page.getByRole("heading", { name: "Overview", level: 1 }).waitFor({ timeout: 15_000 });

  step("Terminal is a tab of the server's page, with nothing open yet");
  await page
    .getByRole("link", { name: /^Server/ })
    .first()
    .click();
  await page.getByRole("link", { name: "Terminal" }).click();
  await page.getByText("No terminal open").first().waitFor();
  const serverId = new URL(page.url()).pathname.split("/")[2] ?? fail(`no server in ${page.url()}`);
  await shoot(page, "empty");
  await noSidewaysScroll(page, "no terminal open");

  step("open a terminal, which asks for the passkey");
  const asserted = async () => {
    const { credentials } = await cdp.send("WebAuthn.getCredentials", { authenticatorId });
    return credentials[0]?.signCount ?? 0;
  };
  const before = await asserted();
  await page.getByRole("button", { name: "Open a terminal" }).click();
  await page.getByRole("tab", { selected: true }).waitFor();
  await page.getByText("Connected through the agent").waitFor({ timeout: 15_000 });
  if ((await asserted()) <= before) fail("a terminal opened without the passkey");
  await shows(page, "$");

  step("type into the shell, and read what it prints");
  await typeLine(page, "echo answer-$((6*7))");
  await shows(page, "answer-42");
  await typeLine(page, "printf 'colour \\033[31mred\\033[0m \\033[32mgreen\\033[0m \\033[34mblue\\033[0m\\n'");
  await shows(page, "colour red green blue");
  await typeLine(page, "ls -la /");
  await shows(page, " proc");
  await shoot(page, "terminal");
  // The mock's rows are 20 px apart, and the last one sits inside the well's padding.
  const rowsAt = await page.evaluate(() => {
    const screen = document.querySelector<HTMLElement>(".terminal-screen:not([hidden])");
    const rows = [...(screen?.querySelectorAll<HTMLElement>(".xterm-rows > div") ?? [])];
    const last = rows.at(-1)?.getBoundingClientRect();
    const well = screen?.getBoundingClientRect();
    return {
      apart: (rows[1]?.getBoundingClientRect().top ?? 0) - (rows[0]?.getBoundingClientRect().top ?? 0),
      under: (well?.bottom ?? 0) - (last?.bottom ?? 0),
    };
  });
  if (Math.abs(rowsAt.apart - 20) > 0.5) fail(`rows are ${rowsAt.apart} px apart, not 20`);
  if (rowsAt.under < 0) fail(`the last row runs ${-rowsAt.under} px past the well`);

  step("it fits the window, and the shell hears the size");
  const sizeOf = async () =>
    (await page.locator(".terminal-size").innerText()).match(/(\d+) × (\d+)/) ?? fail("no size in the foot");
  const [, cols, rows] = await sizeOf();
  await typeLine(page, "stty size");
  await shows(page, `${rows} ${cols}`);
  await page.setViewportSize({ width: 1100, height: 760 });
  await until("the terminal to fit the smaller window", async () => (await sizeOf())[1] !== cols);
  const [, narrower, shorter] = await sizeOf();
  await typeLine(page, "clear; stty size");
  await shows(page, `${shorter} ${narrower}`);
  await page.setViewportSize({ width: 1440, height: 930 });

  step("find in the scrollback, copy what it found, and paste");
  await typeLine(page, "echo findable-$((40+2))");
  await shows(page, "findable-42");
  await page.getByRole("button", { name: "Find in scrollback" }).click();
  await page.getByRole("textbox", { name: "Find in scrollback" }).fill("findable-42");
  await page.keyboard.press("Enter");
  const copy = page.getByRole("button", { name: "Copy selection" });
  await until("something chosen to copy", async () => !(await copy.isDisabled()));
  await copy.click();
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  if (copied !== "findable-42") fail(`copied ${JSON.stringify(copied)}`);
  await page.getByRole("button", { name: "Stop finding" }).click();
  await page.evaluate(() => navigator.clipboard.writeText("echo pasted-$((2+3))"));
  await page.getByRole("button", { name: "Session options" }).click();
  await page.getByRole("menuitem", { name: "Paste" }).click();
  await shows(page, "echo pasted-$((2+3))");
  await page.keyboard.press("Enter");
  await shows(page, "pasted-5");

  step("the platform's own keys copy and paste, and leave Ctrl+C to the shell");
  const row = page.locator(".terminal-screen:not([hidden]) .xterm-rows > div", { hasText: /^pasted-5/ }).last();
  // xterm's screen takes the mouse over its rows, so the word is chosen where it shows.
  const box = (await row.boundingBox()) ?? fail("no row with pasted-5");
  await page.mouse.dblclick(box.x + 10, box.y + box.height / 2);
  await page.keyboard.press("Control+Shift+C");
  await until("the chosen word on the clipboard", async () =>
    (await page.evaluate(() => navigator.clipboard.readText())) === "pasted-5" ? true : undefined,
  );
  await page.evaluate(() => navigator.clipboard.writeText("echo keyed-$((4+4))"));
  await page.keyboard.press("Control+Shift+V");
  await shows(page, "echo keyed-$((4+4))");
  await page.keyboard.press("Enter");
  await shows(page, "keyed-8");
  await typeLine(page, "sleep 30");
  await sleep(300);
  await page.keyboard.press("Control+C");
  await typeLine(page, "echo after-interrupt-$((2*2))");
  await shows(page, "after-interrupt-4", 5_000);

  step("a dropped connection comes back by itself, with what was typed meanwhile");
  await context.setOffline(true);
  await page.getByText("Reconnecting").waitFor({ timeout: 40_000 });
  await typeLine(page, "echo typed-offline-$((3*3))");
  // Narrowed to a phone and back, it keeps its terminal, what it printed and what was typed.
  await page.setViewportSize({ width: 390, height: 844 });
  await sleep(300);
  await page.setViewportSize({ width: 1440, height: 930 });
  await shows(page, "pasted-5");
  await shoot(page, "reconnecting");
  await context.setOffline(false);
  await page.getByText("Connected through the agent").waitFor({ timeout: 20_000 });
  await shows(page, "typed-offline-9");
  const twice = (await screenText(page)).split("findable-42").length - 1;
  if (twice > 3) fail(`what it printed came again on reconnecting: ${twice} times`);

  step("a page that's loaded again finds its terminal, and what it printed");
  await page.reload();
  await page.getByRole("tab", { selected: true }).waitFor();
  await page.getByText("Connected through the agent").waitFor({ timeout: 15_000 });
  await shows(page, "typed-offline-9");

  step("attaching from another tab moves it there, and it comes back");
  const other = await context.newPage();
  other.on("pageerror", (err) => fail(`the other page threw: ${err.message}`));
  await other.goto(page.url());
  await other.getByText("Connected through the agent").waitFor({ timeout: 15_000 });
  await page.getByText("This terminal is open somewhere else").waitFor({ timeout: 15_000 });
  await shoot(page, "moved");
  await page.getByRole("button", { name: "Show it here" }).click();
  await page.getByText("Connected through the agent").waitFor({ timeout: 15_000 });
  await other.getByText("This terminal is open somewhere else").waitFor({ timeout: 15_000 });
  await other.close();

  step("a second session, with a passkey of its own, then closed");
  const second = await asserted();
  await page.getByRole("button", { name: "New session" }).click();
  await until("a second tab", async () => (await page.getByRole("tab").count()) === 2);
  if ((await asserted()) <= second) fail("a second terminal opened without the passkey");
  await page.getByText("Connected through the agent").waitFor({ timeout: 15_000 });
  await typeLine(page, "echo second-$((1+1))");
  await shows(page, "second-2");
  await shoot(page, "two-sessions");
  await page.getByRole("button", { name: "Session options" }).click();
  await page.getByRole("menuitem", { name: "Close terminal" }).click();
  await until("one tab", async () => (await page.getByRole("tab").count()) === 1);
  await shows(page, "typed-offline-9");

  step("the shell exits, and the terminal says so");
  await typeLine(page, "exit 3");
  await page.getByText("It exited with code 3.").waitFor({ timeout: 15_000 });
  await shoot(page, "ended");
  await page.getByRole("button", { name: "Close this tab" }).click();
  await page.getByText("No terminal open").first().waitFor();

  step("its opening and closing are on Activity, and nothing typed is in the audit log");
  await page.goto(`${origin}/activity`);
  await page
    .getByText(/^Opened a terminal on /)
    .first()
    .waitFor();
  await page
    .getByText(/^Closed a terminal on .*, open /)
    .first()
    .waitFor();
  await page
    .getByText(/^Closed a terminal on .*, open /)
    .first()
    .click();
  await page.getByText("It exited with code 3.").first().waitFor();
  await shoot(page, "activity");
  // The database's own bytes, with its write-ahead log: what's on record is there as text.
  const control = join(data, "control");
  const stored = readdirSync(control)
    .filter((f) => f.startsWith("state.db"))
    .map((f) => readFileSync(join(control, f)).toString("latin1"))
    .join("");
  if (!stored.includes("terminal.close")) fail("the database doesn't read as holding the terminal's closing");
  for (const typed of ["answer-42", "findable-42", "pasted-5", "typed-offline-9", "second-2", "colour red"]) {
    if (stored.includes(typed)) fail(`the database holds what was typed or printed: ${typed}`);
  }

  if (dockerSocket) {
    step("a shell in a container, from its page, with no passkey where it reaches no further than itself");
    await runByHand("web-1", "acme/web:1", { User: "node" });
    await runByHand("admin-1", "acme/web:1", { HostConfig: { Privileged: true } });
    await runByHand("bare-1", "acme/web:distroless");
    await page.goto(`${origin}/servers/${serverId}`);
    await page
      .getByRole("link", { name: /^web-1/ })
      .first()
      .click();
    await page.getByRole("link", { name: "Open a shell" }).click();
    await page.getByRole("heading", { name: "No shell open in web-1" }).waitFor();
    await shoot(page, "container-offer");
    const before = await asserted();
    await page.getByRole("button", { name: "Open a shell" }).click();
    await page.getByRole("tab", { name: "node@web-1", selected: true }).waitFor({ timeout: 15_000 });
    await page.getByText("Connected through the agent").waitFor({ timeout: 15_000 });
    await typeLine(page, "echo in-a-container-$((6*7))");
    await shows(page, "in-a-container-42");
    if ((await asserted()) !== before)
      fail("a shell in a container that reaches no further than itself asked for the passkey");
    await shoot(page, "container-shell");

    step("one that's as good as root asks for the passkey, and one with no shell says so");
    await page.goto(`${origin}/servers/${serverId}/terminal?container=admin-1`);
    await page.getByRole("button", { name: "Open a shell" }).click();
    // It says why before the browser asks, and asks nothing until you go on.
    await page.getByText("This shell takes your passkey").waitFor();
    if ((await asserted()) !== before) fail("the passkey was asked for before the page said why");
    await shoot(page, "container-why");
    await page.getByRole("button", { name: "Confirm with your passkey" }).click();
    await page.getByRole("tab", { name: "root@admin-1", selected: true }).waitFor({ timeout: 15_000 });
    if ((await asserted()) <= before) fail("a shell in a privileged container opened without the passkey");
    await page.goto(`${origin}/servers/${serverId}/terminal?container=bare-1`);
    await page.getByRole("button", { name: "Open a shell" }).click();
    await page.getByText(/has none of \/bin\/bash, \/bin\/ash and \/bin\/sh/).waitFor({ timeout: 15_000 });
    await shoot(page, "container-no-shell");
    await page.goto(`${origin}/activity`);
    await page.getByText("Opened a shell in web-1 on").first().waitFor();
  }

  step("in Chinese, on a phone and a desktop");
  const zh = await browser.newContext({
    viewport: { width: 1440, height: 930 },
    locale: "zh-CN",
    colorScheme: "light",
  });
  for (const cookie of await context.cookies(origin)) await zh.addCookies([cookie]);
  const zhPage = await zh.newPage();
  const zhAuth = await withAuthenticator(zh, zhPage);
  await zhAuth.cdp.send("WebAuthn.addCredential", {
    authenticatorId: zhAuth.authenticatorId,
    credential: (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials[0] ?? fail("no passkey"),
  });
  zhPage.on("pageerror", (err) => fail(`the Chinese page threw: ${err.message}`));
  await zhPage.goto(`${origin}/servers/${serverId}/terminal`);
  // New session, which is there whether or not a terminal is open already.
  await zhPage.locator(".terminal-head-tool").first().locator("button").click();
  await zhPage.locator(".terminal-screen:not([hidden]) .xterm").waitFor({ timeout: 15_000 });
  await shows(zhPage, "$");
  await typeLine(zhPage, "echo 你好-$((1+1))");
  await shows(zhPage, "你好-2");
  await shoot(zhPage, "zh");
  await zh.close();

  step("a token is refused");
  dockwell("login", "--local");
  const denied = spawnSync(bin, ["terminals", "list", "--server-id", serverId, "--json"], {
    encoding: "utf8",
    env: env(),
  });
  if (denied.status === 0) fail(`the CLI's token listed terminals:\n${denied.stdout}`);
  if (!`${denied.stdout}${denied.stderr}`.includes("terminal")) {
    fail(`the CLI's token was refused for another reason:\n${denied.stdout}${denied.stderr}`);
  }
}

async function finish(code: number): Promise<never> {
  await browser?.close().catch(() => {});
  server?.kill("SIGTERM");
  docker?.kill("SIGTERM");
  await sleep(200);
  rmSync(data, { recursive: true, force: true });
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
