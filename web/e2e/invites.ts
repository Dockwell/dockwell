// SPDX-License-Identifier: AGPL-3.0-only

// Invites someone from Members in headless Chromium, against a real `dockwell
// run` that has a panel domain, as invites need one: the first invite's
// confirmation of each environment's protection, on record, the passkey that confirms
// it, the link shown once at the domain, the open invite in the list, a newer
// invite replacing it, and revoking. The owner signs in on localhost, as the
// stand has no DNS; accepting works only at the domain, which Go covers at the
// HTTPS listener (internal/control/invites_test.go), and here in a second
// browser that is told the domain is this machine and given a certificate
// made for the run. Then the owner manages who joined: the sheets that change
// a role, remove someone and transfer ownership say what they do as rows, and
// removing them takes a passkey. With --shots <dir> it saves screenshots at
// 1440 and 390 px, the sheets' in light and dark.
//
//   make e2e
//   node web/e2e/invites.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, chromium, type Locator, type Page } from "playwright-core";

import { describePages, watchBrowser } from "./report.ts";

// A real binary asks nothing beyond this machine in a test.
process.env["DOCKWELL_TEST"] = "1";

const { values, positionals } = parseArgs({ allowPositionals: true, options: { shots: { type: "string" } } });
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7050 + Math.floor(Math.random() * 300);
const httpsPort = port + 400;
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

// A certificate for the panel's domain, made for this run and put where the
// control plane keeps the one it gets from its CA, so it asks no CA for one:
// the key and then the certificate, as it caches them. It lasts ten years, so
// nothing tries to renew it.
function seedCertificate(): string {
  const dir = join(data, "control", "acme");
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const key = join(data, "panel.key");
  const cert = join(data, "panel.crt");
  execFileSync(
    "openssl",
    [
      "req",
      "-x509",
      "-newkey",
      "ec",
      "-pkeyopt",
      "ec_paramgen_curve:prime256v1",
      "-nodes",
      "-keyout",
      key,
      "-out",
      cert,
      "-days",
      "3650",
      "-subj",
      `/CN=${domain}`,
      "-addext",
      `subjectAltName=DNS:${domain}`,
    ],
    { stdio: "ignore" },
  );
  writeFileSync(join(dir, domain), readFileSync(key, "utf8") + readFileSync(cert, "utf8"), { mode: 0o600 });
  // Its key's SHA-256, as Chromium takes it to trust this one certificate and no other.
  const spki = execFileSync("openssl", ["x509", "-in", cert, "-pubkey", "-noout"]);
  const der = execFileSync("openssl", ["pkey", "-pubin", "-outform", "der"], { input: spki });
  rmSync(key);
  rmSync(cert);
  return createHash("sha256").update(der).digest("base64");
}

let trusted = "";

async function start(): Promise<void> {
  trusted = seedCertificate();
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
      `127.0.0.1:${httpsPort}`,
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

/** Waits until what a sheet's rows say holds each of these, a row being its term and then what becomes of it. */
async function rowsSay(rows: Locator, what: string, want: readonly string[]) {
  await rows.waitFor();
  await until(`${what} to say ${want.join(", ")}`, async () => {
    const text = (await rows.textContent()) ?? "";
    return want.every((w) => text.includes(w));
  });
}

/** A sheet at both widths, in light and dark. A phone draws Members as a list, so each width opens it afresh. */
async function shootSheet(page: Page, name: string, open: () => Promise<Locator>) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: width >= 800 ? 970 : 844 });
    const sheet = await open();
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      await sleep(250);
      await page.screenshot({ path: join(shots, `${name}-${width}-${scheme}.png`) });
    }
    await page.emulateMedia({ colorScheme: "light" });
    await sheet.getByRole("button", { name: "Cancel" }).click();
    await sheet.waitFor({ state: "detached" });
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
    // The panel's domain is this machine, at the HTTPS listener, and Chromium trusts the run's certificate
    // by its key: a page with a certificate error may not make a passkey, so ignoring errors isn't enough.
    args: [
      "--disable-dev-shm-usage",
      `--host-resolver-rules=MAP ${domain}:443 127.0.0.1:${httpsPort}`,
      `--ignore-certificate-errors-spki-list=${trusted}`,
    ],
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
  // A new owner starts where the first app comes from.
  await page.getByRole("heading", { level: 1, name: "What do you want to run?" }).waitFor({ timeout: 20_000 });

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

  step("the first invite waits until its maker confirms each environment's protection, on record");
  await form.getByRole("textbox", { name: "Invite by email" }).fill(" Ada@Example.com ");
  // One state with its action in it, and the environments a step away.
  await form.getByText("Protected environments aren't confirmed", { exact: true }).waitFor();
  const confirm = form.getByRole("button", { name: /^Confirm: \d+ protected, \d+ open$/ });
  await confirm.waitFor();
  await form.getByRole("button", { name: "Review", exact: true }).click();
  await form.getByRole("list", { name: "Environments" }).getByText("production").first().waitFor();
  await shoot(page, "invites-first");
  // Inviting first brings you to the button that confirms.
  await form.getByRole("button", { name: "Invite", exact: true }).click();
  await page.waitForFunction(() => document.activeElement?.textContent?.startsWith("Confirm: "));
  // Confirming is on record: the page reads it back from the control plane, and it stays after reading again.
  await confirm.click();
  await form.getByText(/^Protection confirmed by Mira Chen, /).waitFor();

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
  // Nothing is asked again: the confirmation was recorded, and the page says by whom.
  await again.getByText(/^Protection confirmed by Mira Chen, /).waitFor();
  if ((await again.getByRole("button", { name: /^Confirm: \d+ protected, \d+ open$/ }).count()) !== 0) {
    fail("the protections are asked for again after they were confirmed");
  }
  await again.getByRole("button", { name: "Invite", exact: true }).click();
  await page.getByText("Send this link to ada@example.com", { exact: true }).waitFor({ timeout: 20_000 });
  const newer = await page.getByText(/\/invite#[A-Za-z0-9_-]{40,}$/).innerText();
  if (newer === shown) fail("a newer invite has the older one's link");
  await until(
    "one open invite for ada",
    async () => (await people.getByRole("row").filter({ hasText: "ada@example.com" }).count()) === 1,
  );

  step("revoking takes it off the list, and Activity has each of them");
  // A revoked link can't come back, so it takes a second press, which says so.
  await people.getByRole("button", { name: "Revoke the invite for ada@example.com", exact: true }).click();
  await people.getByRole("button", { name: "Revoke the invite for ada@example.com: its link stops working" }).click();
  await until(
    "the invite to go",
    async () => (await people.getByRole("row").filter({ hasText: "ada@example.com" }).count()) === 0,
  );
  await shoot(page, "invites-revoked");
  await page.goto(`${origin}/settings/audit-log`);
  await page.getByRole("heading", { level: 1, name: "Audit log" }).waitFor();
  await page.getByText("Invited ada@example.com as a developer of every project").first().waitFor();
  // Two revokes: the one a newer invite replaced, and the one by hand.
  await page.getByText("Confirmed which environments are protected").first().waitFor();
  await until(
    "both revokes on record",
    async () => (await page.getByText("Revoked the invite for ada@example.com").count()) >= 2,
  );

  step("the person invited accepts at the panel's domain with a passkey of their own, and is on Members");
  await page.goto(`${origin}/settings/members`);
  // With both invites revoked it's one person again, so the form waits behind its button.
  await page.getByRole("button", { name: "Invite someone" }).click();
  const invite = page.getByRole("region", { name: "Invite people" });
  await invite.getByRole("textbox", { name: "Invite by email" }).fill("ada@example.com");
  await invite.getByRole("button", { name: "Invite", exact: true }).click();
  await page.getByText("Send this link to ada@example.com", { exact: true }).waitFor({ timeout: 20_000 });
  const theirs = await page.getByText(/\/invite#[A-Za-z0-9_-]{40,}$/).innerText();
  const elsewhere = await browser.newContext({ viewport: { width: 1440, height: 970 } });
  const joining = await elsewhere.newPage();
  const theirKeys = await elsewhere.newCDPSession(joining);
  await theirKeys.send("WebAuthn.enable", { enableUI: false });
  await theirKeys.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  await joining.goto(theirs);
  await joining.getByLabel("Your name").fill("Ada Park");
  await joining.getByRole("button", { name: "Create passkey and join" }).click();
  await joining.getByRole("heading", { level: 1, name: "Overview" }).waitFor({ timeout: 20_000 });
  await page.reload();
  await page.getByText("2 people sign in to this install", { exact: true }).waitFor();
  const ada = people.getByRole("row").filter({ hasText: "ada@example.com" });
  await ada.getByText("Invited by Mira Chen").waitFor();

  // Each sheet opens from the menu on their row, or on a phone, on their card.
  const manage = async (item: string, title: string) => {
    await page.getByRole("button", { name: "Manage Ada Park" }).click();
    await page.getByRole("menuitem", { name: item, exact: true }).click();
    const sheet = page.getByRole("dialog", { name: title });
    await sheet.waitFor();
    return sheet;
  };
  const roleSheet = async (role: string) => {
    const sheet = await manage("Change role", "Change what Ada Park holds");
    await sheet.getByLabel("Role").selectOption({ label: role });
    return sheet;
  };

  step("changing a role says what it does as rows: a raise keeps their sessions and tokens, a lowering ends them");
  let sheet = await manage("Change role", "Change what Ada Park holds");
  // What they hold now changes nothing, so there's nothing to press and nothing to say.
  if (!(await sheet.getByRole("button", { name: "Change role" }).isDisabled())) {
    fail("Change role can be pressed before anything is changed");
  }
  const effects = sheet.getByLabel("What this changes for Ada Park");
  if ((await effects.count()) !== 0) fail("the role sheet says what changes before anything is chosen");
  await sheet.getByLabel("Role").selectOption({ label: "Admin" });
  await rowsSay(effects, "raising to admin", ["SessionsKept", "API tokensKept"]);
  await sheet.getByLabel("Role").selectOption({ label: "Viewer" });
  await rowsSay(effects, "lowering to viewer", ["SessionsSigned out everywhere", "API tokensRevoked"]);

  step("taking back the last thing someone holds offers Remove as a button, which opens it");
  await sheet.getByLabel("Role").selectOption({ label: "No role there" });
  await sheet.getByText("That's the last thing Ada Park holds", { exact: true }).waitFor();
  if (!(await sheet.getByRole("button", { name: "Change role" }).isDisabled())) {
    fail("Change role can be pressed for the last thing someone holds");
  }
  await sheet.getByRole("button", { name: "Remove Ada Park instead" }).click();
  sheet = page.getByRole("dialog", { name: "Remove Ada Park" });

  step("removing says what it ends, read from the control plane, before it asks");
  await rowsSay(sheet.getByLabel("What removing Ada Park ends"), "the removal", [
    "Sign-inPasskeys and codes deleted",
    "Sessions1 browser is signed out",
    "API tokens revokedNone",
    "Invites revokedNone",
    "Planned changes discardedNone",
    "Apps and tasks they set upKeep running",
    "Their name on ActivityStays",
  ]);
  await sheet.getByRole("button", { name: "Cancel" }).click();
  await sheet.waitFor({ state: "detached" });

  step("transferring ownership says what changes for both, and only the owner is offered it");
  sheet = await manage("Transfer ownership", "Make Ada Park the owner");
  await rowsSay(sheet.getByLabel("What this changes", { exact: true }), "the transfer", [
    "Ada ParkBecomes the owner",
    "YouBecome an admin, and stay signed in",
    "Your CLI access on the control serverRevoked",
    "Undoing thisOnly Ada Park can",
  ]);
  await sheet.getByRole("button", { name: "Cancel" }).click();
  await sheet.waitFor({ state: "detached" });

  await shootSheet(page, "members-role-raise", () => roleSheet("Admin"));
  await shootSheet(page, "members-role-lower", () => roleSheet("Viewer"));
  await shootSheet(page, "members-role-last", () => roleSheet("No role there"));
  await shootSheet(page, "members-remove", async () => {
    const s = await manage("Remove", "Remove Ada Park");
    await s.getByLabel("What removing Ada Park ends").waitFor();
    return s;
  });
  await shootSheet(page, "members-transfer", () => manage("Transfer ownership", "Make Ada Park the owner"));

  step("removing them takes a passkey, signs them out and takes them off Members");
  sheet = await manage("Remove", "Remove Ada Park");
  await sheet.getByLabel("What removing Ada Park ends").waitFor();
  await sheet.getByRole("button", { name: "Remove Ada Park" }).click();
  await sheet.waitFor({ state: "detached", timeout: 20_000 });
  await until(
    "ada to go from Members",
    async () => (await people.getByRole("row").filter({ hasText: "ada@example.com" }).count()) === 0,
  );
  await joining.reload();
  await joining.getByRole("heading", { level: 1, name: "Overview" }).waitFor({ state: "detached" });
  if ((await joining.getByText("Ada Park").count()) !== 0) fail("someone removed is still signed in at the domain");
  await elsewhere.close();
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
