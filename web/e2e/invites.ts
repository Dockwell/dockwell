// SPDX-License-Identifier: AGPL-3.0-only

// Invites someone from Members in headless Chromium, against a real `dockwell
// run` that has a panel domain, as invites need one: the first invite's
// reminder to check each environment's protection, the passkey that confirms
// it, the link shown once at the domain, the open invite in the list, a newer
// invite replacing it, and revoking. The owner signs in on localhost, as the
// stand has no DNS; accepting works only at the domain, which Go covers at the
// HTTPS listener (internal/control/invites_test.go). With --shots <dir> it
// saves screenshots at 1440 and 390 px.
//
//   make e2e
//   node web/e2e/invites.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
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
const domain = "panel.example.com";
const data = mkdtempSync(join(tmpdir(), "dockwell-invites-"));

let server: ChildProcess | undefined;
let browser: Browser | undefined;
const serverLog: string[] = [];

function step(what: string) {
  console.log(`invites e2e: ${what}`);
}

function fail(message: string): never {
  throw new Error(message);
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

async function until<T>(what: string, fn: () => T | Promise<T>, timeout = 20_000): Promise<NonNullable<T>> {
  const end = Date.now() + timeout;
  for (;;) {
    const got = await fn();
    if (got) return got;
    if (Date.now() > end) fail(`timed out waiting for ${what}`);
    await sleep(50);
  }
}

function env(): NodeJS.ProcessEnv {
  return { ...process.env, DOCKWELL_DATA_DIR: data, USER: "mira" };
}

async function start(): Promise<void> {
  // The panel's domain, with its HTTPS and HTTP listeners on ports of their own: nothing asks for a certificate here.
  server = spawn(
    bin,
    [
      "run",
      "--listen",
      `127.0.0.1:${port}`,
      "--agent-listen",
      "127.0.0.1:0",
      "--domain",
      domain,
      "--https-listen",
      "127.0.0.1:0",
      "--http-listen",
      "127.0.0.1:0",
      "--log-format",
      "json",
    ],
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
}

async function shoot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: width >= 800 ? 970 : 844 });
    await sleep(250);
    await page.screenshot({ path: join(shots, `${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1440, height: 970 });
  await sleep(100);
}

async function main() {
  await start();
  step("claim the install on localhost, with a passkey");
  await until("dockwell run to say how to claim it", () => serverLog.join("").includes("To claim it, run this"));
  const claimed = execFileSync(bin, ["claim", "--log-format", "json"], { encoding: "utf8", env: env() });
  // With a domain the link is at the domain, which this stand has no DNS or certificate for: the owner opens
  // its token on localhost, over an SSH tunnel as it were, where a passkey works too.
  const token = claimed.match(/\/claim#([A-Za-z0-9_-]+)/)?.[1] ?? fail(`dockwell claim printed:\n${claimed}`);
  if (!claimed.includes(`https://${domain}`)) fail(`dockwell claim's link isn't at the panel's domain:\n${claimed}`);
  const link = `${origin}/claim#${token}`;

  browser = await chromium.launch({
    executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium",
    // /dev/shm is small in a container and shared by every browser on the machine; Chromium crashes when it fills.
    args: ["--disable-dev-shm-usage"],
  });
  watchBrowser(browser);
  const context = await browser.newContext({ viewport: { width: 1440, height: 970 }, colorScheme: "light" });
  await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin });
  const page = await context.newPage();
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
  await page.getByRole("heading", { level: 1, name: "Overview" }).waitFor({ timeout: 20_000 });

  step("Members: with one person, the invite form waits behind a button, and says nothing of roles yet");
  await page.goto(`${origin}/settings/members`);
  await page.getByRole("heading", { level: 1, name: "Members" }).waitFor();
  await page.getByText("1 person signs in to this install, its owner", { exact: true }).waitFor();
  if ((await page.getByRole("region", { name: "What each role can do" }).count()) !== 0) {
    fail("what each role can do shows before anyone is invited");
  }
  await page.getByRole("button", { name: "Invite someone" }).click();
  const form = page.getByRole("region", { name: "Invite people" });
  await form.waitFor();
  await page.getByRole("region", { name: "What each role can do" }).waitFor();

  step("the first invite reminds its maker to look at each environment's protection");
  await form.getByRole("textbox", { name: "Invite by email" }).fill(" Ada@Example.com ");
  await form.getByText("Before anyone else signs in", { exact: true }).waitFor();
  await form.getByRole("list", { name: "Environments" }).getByText("production").first().waitFor();
  await shoot(page, "invites-first");
  await form.getByRole("button", { name: "Invite", exact: true }).click();
  await form
    .getByText("Check each environment's protection first, then tick the box: it decides whose changes wait.", {
      exact: true,
    })
    .waitFor();
  await form.getByRole("checkbox", { name: "I've checked which environments are protected" }).check();

  step("a passkey confirms the invite, and its link shows once, at the panel's domain");
  await form.getByRole("button", { name: "Invite", exact: true }).click();
  // The email is the control plane's writing of it: in small letters.
  await page.getByText("Send this link to ada@example.com", { exact: true }).waitFor({ timeout: 20_000 });
  const shown = await page
    .getByText(new RegExp(`^https://${domain.replaceAll(".", "\\.")}/invite#[A-Za-z0-9_-]{40,}$`))
    .innerText();
  await page.getByRole("button", { name: "Copy link" }).click();
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  if (copied !== shown) fail(`Copy link copied ${JSON.stringify(copied.slice(0, 40))}, not the link shown`);
  await shoot(page, "invites-link");

  step("the open invite is in the list, with who made it and when it ends");
  const people = page.getByRole("table", { name: "Who signs in to this install" });
  const row = people.getByRole("row").filter({ hasText: "ada@example.com" });
  await row.getByText("Invited by Mira Chen").waitFor();
  await row.getByText("Not accepted yet").waitFor();
  await row.getByText(/^Expires in (6|7) days$/).waitFor();

  step("the link isn't shown again: after reading the page again, the invite is there and its link isn't");
  await page.reload();
  await page.getByRole("heading", { level: 1, name: "Members" }).waitFor();
  await people.getByRole("row").filter({ hasText: "ada@example.com" }).waitFor();
  if ((await page.getByText(/\/invite#/).count()) !== 0)
    fail("an invite's link shows again after the page is read again");
  if (serverLog.join("").includes(shown.split("#")[1] ?? "never"))
    fail("the control plane's log holds an invite's secret");

  step("a newer invite to the same person replaces the open one");
  const again = page.getByRole("region", { name: "Invite people" });
  await again.getByRole("textbox", { name: "Invite by email" }).fill("ada@example.com");
  await again.getByRole("checkbox", { name: "I've checked which environments are protected" }).check();
  await again.getByRole("button", { name: "Invite", exact: true }).click();
  await page.getByText("Send this link to ada@example.com", { exact: true }).waitFor({ timeout: 20_000 });
  const newer = await page.getByText(/\/invite#[A-Za-z0-9_-]{40,}$/).innerText();
  if (newer === shown) fail("a newer invite has the older one's link");
  await until(
    "one open invite for ada",
    async () => (await people.getByRole("row").filter({ hasText: "ada@example.com" }).count()) === 1,
  );

  step("revoking takes it off the list, and Activity has each of them");
  await people.getByRole("button", { name: "Revoke the invite for ada@example.com" }).click();
  await until(
    "the invite to go",
    async () => (await people.getByRole("row").filter({ hasText: "ada@example.com" }).count()) === 0,
  );
  await shoot(page, "invites-revoked");
  await page.goto(`${origin}/settings/audit-log`);
  await page.getByRole("heading", { level: 1, name: "Audit log" }).waitFor();
  await page.getByText("Invited ada@example.com as a developer of every project").first().waitFor();
  // Two revokes: the one a newer invite replaced, and the one by hand.
  await until(
    "both revokes on record",
    async () => (await page.getByText("Revoked the invite for ada@example.com").count()) >= 2,
  );
}

main()
  .then(() => console.log("invites e2e: passed"))
  .catch(async (err) => {
    console.error(err);
    console.error(await describePages());
    console.error(serverLog.join("").split("\n").slice(-30).join("\n"));
    process.exitCode = 1;
  })
  .finally(async () => {
    await browser?.close().catch(() => {});
    server?.kill();
    rmSync(data, { recursive: true, force: true });
  });
