// SPDX-License-Identifier: AGPL-3.0-only

// Reads a server's package updates in headless Chromium, against a real `dockwell
// run` and its own agent, with stand-ins for apt (DOCKWELL_TEST_PACKAGES) and a
// releases URL of its own (DOCKWELL_RELEASES_URL), so it reads nothing of this
// machine's packages and reaches no mirror. The overview's card, the tab's count,
// the Updates tab, Check now at most once a minute, the lists refreshed once
// they're a day old and not before, a phone, Chinese, and the CLI. With --shots
// <dir> it saves screenshots at 1440 and 390 px, light and dark.
//
//   make e2e
//   node web/e2e/updates.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, chromium, type Page } from "playwright-core";

import { describePages, watchBrowser } from "./report.ts";

// A real binary asks nothing beyond this machine in a test.
process.env["DOCKWELL_TEST"] = "1";

const { values, positionals } = parseArgs({ allowPositionals: true, options: { shots: { type: "string" } } });
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7050 + Math.floor(Math.random() * 300);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-updates-"));
const stand = mkdtempSync(join(tmpdir(), "dockwell-apt-"));
const fixtures = resolve("internal/agent/updates/testdata");

let server: ChildProcess | undefined;
let releases: Server | undefined;
let browser: Browser | undefined;
let lastPage: Page | undefined;
const serverLog: string[] = [];

function step(what: string) {
  console.log(`updates e2e: ${what}`);
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

/** Puts a file in the stand, as the server's own files are laid out. */
function put(name: string, content: string, mode = 0o644) {
  const p = join(stand, name);
  mkdirSync(join(p, ".."), { recursive: true });
  writeFileSync(p, content, { mode });
}

const lists = "var/lib/apt/lists/archive.ubuntu.com_dists_noble_InRelease";

/**
 * An Ubuntu server's files with stand-ins for apt-get, dpkg-query and apt-cache,
 * which print the agent's fixtures and note each call, and lists from 30 hours ago.
 */
function makeStand() {
  put("etc/os-release", 'PRETTY_NAME="Ubuntu 24.04.1 LTS"\nNAME="Ubuntu"\nID=ubuntu\nID_LIKE=debian\n');
  put(lists, "");
  const old = new Date(Date.now() - 30 * 3600_000);
  utimesSync(join(stand, lists), old, old);
  put("usr/bin/unattended-upgrade", "", 0o755);
  put("etc/apt/apt.conf.d/20auto-upgrades", 'APT::Periodic::Unattended-Upgrade "1";\n');
  const head = `#!/bin/sh\nFIX='${fixtures}'\nROOT='${stand}'\necho "$(basename "$0") $*" >> "$ROOT/calls"\n`;
  put(
    "usr/bin/apt-get",
    // An install's simulation (ADR 0064) says what the read's does, as apt would for these packages.
    // The install itself sets up what the simulation said, pulled-in packages too.
    `${head}case "$*" in\n*" update") sleep 1; touch "$ROOT/${lists}" ;;\n*" upgrade") cat "$FIX/apt-simulate-ubuntu.txt" ;;\n*"--simulate"*" install "*) cat "$FIX/apt-simulate-ubuntu.txt" ;;\n*"--no-download"*) sed -n 's/^Inst \\([^ ]*\\) \\(\\[[^]]*\\] \\)\\{0,1\\}(\\([^ ]*\\) .*\\[\\([a-z0-9]*\\)\\].*/\\1 \\4 \\3/p' "$FIX/apt-simulate-ubuntu.txt" | while read -r n a v; do echo "Setting up $n ($v)"; printf '%s\\t%s\\t%s\\tii \\n' "$n" "$a" "$v" >> "$ROOT/installed"; sleep 0.2; done ;;\nesac\n`,
    0o755,
  );
  // What an install leaves installed, once it has run, is what dpkg reads of each package.
  put(
    "usr/bin/dpkg-query",
    `${head}case "$*" in\n*"Status-Abbrev"*) cat "$ROOT/installed" 2>/dev/null; exit 0 ;;\n*) cat "$FIX/dpkg-query.txt"; exit 1 ;;\nesac\n`,
    0o755,
  );
  put("usr/bin/dpkg", `${head}exit 0\n`, 0o755);
  put("usr/bin/apt-cache", `${head}cat "$FIX/apt-cache-show.txt"\n`, 0o755);
  // systemd, whose transient units run in the background with their output in the file
  // they name, and say how they ended (ADR 0064).
  put("run/systemd/system/.keep", "");
  put(
    "usr/bin/systemd-run",
    `${head}unit=; log=\nfor a in "$@"; do case "$a" in --unit=*) unit="\${a#--unit=}" ;; --property=StandardOutput=append:*) log="\${a#--property=StandardOutput=append:}" ;; esac; done\nwhile [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done; shift\nmkdir -p "$ROOT/units"; touch "$ROOT/units/$unit.active"\n( "$@" >> "$log" 2>&1; echo $? > "$ROOT/units/$unit.status"; rm -f "$ROOT/units/$unit.active" ) > /dev/null 2>&1 &\n`,
    0o755,
  );
  put(
    "usr/bin/systemctl",
    `${head}for u; do :; done\ncase "$*" in\n*"ActiveState"*) if [ -e "$ROOT/units/$u.active" ]; then echo active; else echo inactive; fi ;;\n*"ExecMainStatus"*) cat "$ROOT/units/$u.status" 2>/dev/null || echo 0 ;;\nesac\n`,
    0o755,
  );
}

/** How often the stand-ins were called with what starts so. */
function called(prefix: string): number {
  const file = join(stand, "calls");
  if (!existsSync(file)) return 0;
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((l) => l.length > 0 && l.startsWith(prefix)).length;
}
const refreshes = () => called("apt-get -q -o DPkg::Lock::Timeout=120 update");
const reads = () => called("apt-get -q -o Debug::NoLocking=true");

/** A releases URL in GitHub's layout, whose latest release is 0.9.5. */
async function serveReleases(): Promise<string> {
  releases = createServer((req, res) => {
    if (req.url === "/releases/latest/download/checksums.txt") {
      res.end(`${"a".repeat(64)}  dockwell_0.9.5_linux_amd64.tar.gz\n`);
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  await new Promise<void>((ok) => releases?.listen(0, "127.0.0.1", ok));
  const address = releases.address();
  if (!address || typeof address === "string") fail("the releases server has no port");
  return `http://127.0.0.1:${address.port}/releases`;
}

let releasesURL = "";

function env(): NodeJS.ProcessEnv {
  return {
    ...process.env,
    DOCKWELL_DATA_DIR: data,
    DOCKWELL_LOG_FORMAT: "text",
    DOCKWELL_TEST_PACKAGES: stand,
    DOCKWELL_RELEASES_URL: releasesURL,
    USER: "mira",
  };
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

async function shoot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: width >= 800 ? 970 : 844 });
      await sleep(250);
      await page.screenshot({ path: join(shots, `${name}-${width}-${scheme}.png`), fullPage: true });
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
    await sleep(200);
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    if (over > 0) fail(`${what} scrolls sideways by ${over} px at ${width} px`);
  }
  await page.setViewportSize({ width: 1440, height: 970 });
}

async function main() {
  makeStand();
  releasesURL = await serveReleases();
  await start();
  step("claim the install, and open a session");
  await until("dockwell run to say how to claim it", () => serverLog.join("").includes("To claim it, run this"));
  const claimed = execFileSync(bin, ["claim", "--log-format", "json"], { encoding: "utf8", env: env() });
  const link =
    claimed.match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]+/)?.[0] ?? fail(`dockwell claim printed:\n${claimed}`);

  if (refreshes() + reads() > 0) fail("the agent ran apt before anyone looked");

  browser = await chromium.launch({
    executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium",
    // /dev/shm is small in a container and shared by every browser on the machine; Chromium crashes when it fills.
    args: ["--disable-dev-shm-usage"],
  });
  watchBrowser(browser);
  const context = await browser.newContext({ viewport: { width: 1440, height: 970 }, colorScheme: "light" });
  const page = await context.newPage();
  lastPage = page;
  // A throw in a listener would end the process and skip the report and the cleaning up.
  page.on("pageerror", (err) => pageThrew(new Error(`the page threw: ${err.message}`)));
  // A platform authenticator, so the owner's passkey applies an install.
  const cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable", { enableUI: false });
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  await page.goto(link);
  await page.getByLabel("Your name").fill("Mira Chen");
  await page.getByLabel("Email").fill("mira@example.com");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await page.getByRole("heading", { name: "What do you want to run?", level: 1 }).waitFor({ timeout: 15_000 });

  step("the overview's card names the security fixes, once the day-old lists are refreshed");
  await page.goto(`${origin}/`);
  const card = page.getByRole("region", { name: "Updates" });
  // It names this machine's own distribution, which the agent's inventory says.
  await card.getByText(/^6 security updates for \S+$/).waitFor();
  await card.getByText("Needs a reboot.", { exact: false }).waitFor();
  await card.getByText(/Checked \d+ s ago/).waitFor();
  if (refreshes() !== 1) fail(`lists 30 hours old were refreshed ${refreshes()} times, want once`);
  await shoot(page, "overview-card");

  step("the server's Updates tab counts them, in a warning's tone");
  await card.getByRole("link", { name: "Review updates" }).click();
  await page.waitForURL(/\/servers\/srv_[^/]+\/updates$/);
  const tab = page.getByRole("link", { name: "Updates 6" });
  await tab.waitFor();
  if ((await tab.getAttribute("aria-current")) !== "page") fail("the Updates tab isn't the current page");
  if (!(await tab.locator(".dw-tab-count.dw-tone-warning").count())) fail("the count isn't a warning");
  const serverId = new URL(page.url()).pathname.split("/")[2] ?? fail(`no server in ${page.url()}`);

  step("it leads with the security fixes, then the rest, as Server-Updates has them");
  await page.getByText("6 security updates are ready to install").waitFor();
  // On apt, installing them is a plan away (ADR 0064).
  await page.getByRole("button", { name: "Install security updates" }).waitFor();
  await page.getByRole("button", { name: "Install every update" }).first().waitFor();
  await page.getByText("installs them within about a day.", { exact: false }).first().waitFor();
  await page.getByText("6 from noble-security", { exact: false }).waitFor();
  const table = page.getByRole("table", { name: /^Security updates for / });
  await table.waitFor();
  const names = await table.locator(".updates-name").allTextContents();
  if (names[0] !== "libssh-4" || names.length !== 6) fail(`security updates listed ${names.join(", ")}`);
  await table
    .getByRole("row", { name: /libssl3t64/ })
    .getByText("Secure Sockets Layer toolkit - shared libraries")
    .waitFor();
  await table
    .getByRole("row", { name: /linux-image-6\.8\.0-49-generic/ })
    .getByText("Needs a reboot")
    .waitFor();
  const other = page.getByRole("button", { name: /Other updates/ });
  if ((await other.getAttribute("aria-expanded")) !== "false") fail("the other updates aren't folded away");
  await other.click();
  await page
    .getByRole("table", { name: /^Other updates for / })
    .getByText("systemd")
    .waitFor();
  await page.getByText("3 more are held back", { exact: false }).waitFor();
  const versions = page.getByRole("region", { name: "Docker and Dockwell" });
  await versions.getByText("27.4.0").waitFor();
  await versions.getByText("Containers stop with it, and only those set to restart start again").waitFor();
  const automatic = page.getByRole("region", { name: "Automatic updates" });
  await automatic.getByText("unattended-upgrades").waitFor();
  await shoot(page, "updates");

  step("the newest release comes from the releases URL");
  const said = await page.evaluate(async (id) => {
    const res = await fetch("/dockwell.v1.UpdateService/GetServerUpdates", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
      body: JSON.stringify({ id }),
    });
    return (await res.json()) as { dockwell?: { newest?: string; releases?: string } };
  }, serverId);
  if (said.dockwell?.newest !== "0.9.5" || said.dockwell.releases !== releasesURL) {
    fail(`the newest release reads ${JSON.stringify(said.dockwell)}`);
  }

  step("a second look within 10 minutes asks nothing of the agent");
  const readBefore = reads();
  await page.reload();
  await page.getByText("6 security updates are ready to install").waitFor();
  await sleep(500);
  if (reads() !== readBefore) fail(`a look within 10 minutes read again (${reads() - readBefore} times)`);

  step("Check now refreshes the lists, says so while it does, and at most once a minute");
  await page.getByRole("button", { name: "Check now" }).click();
  await page.getByRole("button", { name: /Refreshing lists|Checking/ }).waitFor();
  await page.getByRole("button", { name: "Check now" }).waitFor({ timeout: 20_000 });
  if (refreshes() !== 2) fail(`Check now refreshed ${refreshes() - 1} times, want once`);
  await page.getByRole("button", { name: "Check now" }).click();
  await sleep(1500);
  if (refreshes() !== 2) fail("Check now refreshed twice within a minute");

  step("Install security updates plans what the server simulated, what it pulls in too, and what restarts");
  // The install's simulation, apart from the reads', which simulate an upgrade.
  const simulated = () =>
    readFileSync(join(stand, "calls"), "utf8")
      .split("\n")
      .filter((l) => l.startsWith("apt-get ") && l.includes("--simulate") && l.includes(" install -- ")).length;
  await page.getByRole("button", { name: "Install security updates" }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  const packages = plan.getByRole("list", { name: /^Packages it installs on / });
  await packages.waitFor({ timeout: 30_000 });
  await packages.getByRole("listitem").filter({ hasText: "openssl" }).getByText("→", { exact: false }).waitFor();
  await packages
    .getByRole("listitem")
    .filter({ hasText: "linux-image-6.8.0-49-generic" })
    .getByText("Pulled in")
    .waitFor();
  await plan.getByText(/^Docker restarts/).waitFor();
  await plan.getByText("Applying this asks for your passkey").waitFor();
  if (simulated() !== 1) fail(`the plan simulated the install ${simulated()} times, want once`);
  await shoot(page, "install-plan");

  step("applied with the passkey, it downloads, then installs in a unit of its own, and applies");
  // Docker's restart stops what runs there, so the plan asks for what it names to be typed.
  const confirmWith = await plan.locator("#plan-confirm-why code").innerText();
  await plan.getByRole("textbox", { name: /to apply it/ }).fill(confirmWith);
  await plan.getByRole("button", { name: /^Apply plan/ }).click();
  // However it ends, the sheet's heading says so; anything but Applied is a failure, said with what the sheet shows.
  const sheet = page.getByRole("dialog").last();
  const ended = await until(
    "the install to end",
    async () => {
      const title = await sheet
        .getByRole("heading")
        .first()
        .innerText()
        .catch(() => "");
      return title && !/^(Review plan|Planning|Applying|Installing updates on [^ ]+$)/.test(title) ? title : undefined;
    },
    120_000,
  );
  if (!ended.startsWith("Updates are installed on ")) fail(`the install ended "${ended}": ${await sheet.innerText()}`);
  const applied = page.getByRole("dialog", { name: ended });
  const install = called("apt-get -q -y --no-download");
  if (called("apt-get -q -y --download-only") !== 1 || install !== 1) {
    fail(
      `the install downloaded ${called("apt-get -q -y --download-only")} times and installed ${install} times, want once each`,
    );
  }
  if (!readFileSync(join(stand, "installed"), "utf8").includes("docker-ce\tamd64\t5:27.4.0-1~ubuntu.24.04~noble")) {
    fail("the install didn't set up docker-ce at its planned version");
  }
  await shoot(page, "install-applied");
  await applied.getByRole("button", { name: "Done" }).click();
  await applied.waitFor({ state: "hidden" });

  step("on a phone, a package a row, and the overview's card is one link");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("list", { name: /^Security updates for / }).waitFor();
  await noSidewaysScroll(page, "the Updates tab");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/`);
  await page.getByRole("link", { name: /6 security updates for / }).click();
  await page.waitForURL(/\/updates$/);
  await page.setViewportSize({ width: 1440, height: 970 });

  step("in Chinese");
  const zh = await browser.newContext({
    viewport: { width: 1440, height: 970 },
    colorScheme: "light",
    locale: "zh-CN",
    storageState: await context.storageState(),
  });
  const zhPage = await zh.newPage();
  await zhPage.goto(`${origin}/servers/${serverId}/updates`);
  await zhPage.getByRole("heading", { name: "安全更新" }).waitFor();
  await zhPage.getByText("6 个安全更新可以安装").waitFor();
  await shoot(zhPage, "zh-CN");
  await zh.close();

  step("the CLI reads them too");
  execFileSync(bin, ["login", "--local"], { encoding: "utf8", env: env() });
  const cli = execFileSync(bin, ["updates", "get", serverId, "--json"], { encoding: "utf8", env: env() });
  if (!cli.includes('"libssh-4"') || !cli.includes('"noble-security"')) fail(`dockwell updates get printed:\n${cli}`);
  step("all good");
}

async function finish(code: number): Promise<never> {
  await browser?.close().catch(() => {});
  server?.kill("SIGTERM");
  releases?.close();
  await sleep(200);
  rmSync(data, { recursive: true, force: true });
  rmSync(stand, { recursive: true, force: true });
  process.exit(code);
}

let pageThrew: (err: Error) => void = () => {};
const pageError = new Promise<never>((_, threw) => {
  pageThrew = threw;
});

Promise.race([main(), pageError]).then(
  () => finish(0),
  async (err: unknown) => {
    console.error(err);
    console.error(serverLog.join("").split("\n").slice(-30).join("\n"));
    console.error(await describePages());
    if (shots && lastPage) {
      mkdirSync(shots, { recursive: true });
      await lastPage.screenshot({ path: join(shots, "failure.png") }).catch(() => {});
    }
    await finish(1);
  },
);
