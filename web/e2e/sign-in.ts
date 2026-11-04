// SPDX-License-Identifier: AGPL-3.0-only

// Claims a fresh install, signs out and signs in again in headless Chromium,
// against a real `dockwell run` on a temporary data directory, with the
// DevTools virtual authenticator standing in for Touch ID. Then it turns on
// codes, signs in with a recovery code, opens a link from `dockwell recover`
// on a phone, and signs that phone out from the first browser. It makes an
// API token in the panel, calls REST and Connect with it as a script would,
// and revokes it. Everything
// after the claim goes through the panel shell: the rail's link to your
// profile on wide screens, More on phones. With --shots <dir> it also saves
// screenshots of every screen at 1440, 390 and 360 px, light and dark.
//
//   make e2e
//   node web/e2e/sign-in.ts bin/dockwell --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { createHmac } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, type BrowserContext, chromium, type Page } from "playwright-core";

const { values, positionals } = parseArgs({ allowPositionals: true, options: { shots: { type: "string" } } });
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7650 + Math.floor(Math.random() * 300);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-e2e-"));

let server: ChildProcess | undefined;
let browser: Browser | undefined;
const log: string[] = [];

function step(what: string) {
  console.log(`e2e: ${what}`);
}

function fail(message: string): never {
  throw new Error(message);
}

async function expectText(page: Page, text: string, timeout = 10_000) {
  await page.getByText(text, { exact: false }).first().waitFor({ timeout });
}

/** Waits for the page whose one h1 is name. */
async function expectPage(page: Page, name: string, timeout = 10_000) {
  await page.getByRole("heading", { level: 1, name, exact: true }).waitFor({ timeout });
}

/**
 * Starts noting which of texts the page shows from now on, however briefly,
 * and returns a function that says which it has. The page mustn't reload in
 * between.
 */
async function watchFor(page: Page, texts: string[]): Promise<() => Promise<string[]>> {
  await page.evaluate((texts) => {
    const seen = new Set<string>();
    const look = () => {
      const now = document.body.textContent ?? "";
      for (const text of texts) if (now.includes(text)) seen.add(text);
    };
    new MutationObserver(look).observe(document.body, { childList: true, subtree: true, characterData: true });
    (window as unknown as { e2eSeen: Set<string> }).e2eSeen = seen;
  }, texts);
  return () => page.evaluate(() => [...(window as unknown as { e2eSeen: Set<string> }).e2eSeen]);
}

/** The shell's navigation: the rail on wide screens, the tab bar on phones. */
function nav(page: Page) {
  return page.getByRole("navigation", { name: "Main" });
}

/** Starts dockwell run and waits for the claim link it prints. */
async function start(): Promise<string> {
  server = spawn(
    bin,
    [
      "run",
      "--data-dir",
      data,
      "--listen",
      `127.0.0.1:${port}`,
      "--agent-listen",
      "127.0.0.1:0",
      "--log-format",
      "json",
    ],
    {
      stdio: ["ignore", "pipe", "pipe"],
      env: { ...process.env, USER: "mira" },
    },
  );
  let out = "";
  server.stdout?.on("data", (b: Buffer) => {
    out += b.toString();
  });
  server.stderr?.on("data", (b: Buffer) => log.push(b.toString()));
  for (let i = 0; i < 200; i++) {
    const link = out.match(/(http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]{43})/);
    if (link?.[1]) {
      console.log(
        out
          .trim()
          .replace(/#[A-Za-z0-9_-]{43}/, "#<token>")
          .replace(/^/gm, "  | "),
      );
      return link[1];
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  fail(`dockwell run printed no claim link:\n${out}\n${log.join("")}`);
}

/** A browser context with a platform authenticator that does user verification. */
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
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    for (const width of [1440, 390, 360]) {
      await page.setViewportSize({ width, height: width === 1440 ? 900 : 844 });
      await page.waitForTimeout(100);
      await page.screenshot({ path: join(shots, `${name}-${width}-${scheme}.png`), fullPage: true });
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await page.setViewportSize({ width: 1440, height: 900 });
}

function totp(secretBase32: string, step = Math.floor(Date.now() / 30_000)): string {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const c of secretBase32.replace(/\s/g, "")) bits += alphabet.indexOf(c).toString(2).padStart(5, "0");
  const key = Buffer.from(bits.match(/.{8}/g)?.map((b) => Number.parseInt(b, 2)) ?? []);
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(step));
  const sum = createHmac("sha1", key).update(msg).digest();
  const off = (sum.at(-1) ?? 0) & 0x0f;
  return String((sum.readUInt32BE(off) & 0x7fffffff) % 1_000_000).padStart(6, "0");
}

async function main() {
  if (shots) await mkdir(shots, { recursive: true });
  step(`starting ${bin} on ${origin} with data in ${data}`);
  const claimLink = await start();

  browser = await chromium.launch({
    executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium",
    headless: true,
  });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  page.on("pageerror", (err) => fail(`the page threw: ${err.message}`));
  const { cdp, authenticatorId } = await withAuthenticator(context, page);

  step("an unclaimed panel explains how to claim it");
  await page.goto(`${origin}/`);
  await expectText(page, "has no owner yet");
  await shoot(page, "unclaimed");

  step("claim: open the printed link, create the owner with a passkey");
  await page.goto(claimLink);
  await page.getByRole("region", { name: "This server" }).waitFor();
  if (page.url().includes("#")) fail(`the claim token stayed in the address bar: ${page.url()}`);
  await shoot(page, "claim");
  await page.getByLabel("Your name").fill("Mira Chen");
  await page.getByLabel("Email").fill("mira@acme.dev");
  const claimSaid = await watchFor(page, ["The owner account wasn't created", "This install already has an owner"]);
  await page.getByRole("button", { name: "Create passkey" }).click();
  await expectPage(page, "Overview", 15_000);
  const claimFlashed = await claimSaid();
  if (claimFlashed.length > 0) fail(`the claim showed ${JSON.stringify(claimFlashed)} on its way to the overview`);
  // The control plane's own agent reports within a few seconds, and its live stats follow.
  await page.locator(".dw-metric", { hasText: "CPU" }).waitFor({ timeout: 15_000 });
  await nav(page).getByRole("link", { name: "Mira Chen Owner" }).waitFor();
  const { credentials } = await cdp.send("WebAuthn.getCredentials", { authenticatorId });
  if (credentials.length !== 1 || credentials[0]?.rpId !== "localhost" || !credentials[0]?.isResidentCredential) {
    fail(`expected one discoverable passkey for localhost, got ${JSON.stringify(credentials)}`);
  }
  const cookies = await context.cookies(origin);
  const session = cookies.find((c) => c.name === "dockwell_session");
  if (!session?.httpOnly || session.sameSite !== "Strict") fail(`session cookie: ${JSON.stringify(cookies)}`);
  step(
    `  claimed; passkey made for rpId ${credentials[0]?.rpId}; cookie ${session.name} HttpOnly SameSite=${session.sameSite}`,
  );
  await shoot(page, "overview");

  step("the claim link works only once");
  const reuse = await browser.newPage();
  await reuse.goto(claimLink);
  await expectText(reuse, "This install already has an owner");
  await reuse.close();

  step("the status page sits behind the gate too, outside the shell");
  await page.goto(`${origin}/status`);
  await expectPage(page, "The control plane is running");
  if ((await nav(page).count()) !== 0) fail("the status page shows the shell's navigation");

  step("sign out from your profile, reached from the rail, then sign in again with the passkey");
  await page.goto(`${origin}/`);
  await expectPage(page, "Overview");
  await nav(page).getByRole("link", { name: "Mira Chen Owner" }).click();
  await expectPage(page, "Profile and security");
  if ((await nav(page).getByRole("link", { name: "Mira Chen Owner" }).getAttribute("aria-current")) !== "page") {
    fail("the rail doesn't mark your profile as the current page");
  }
  await page.getByRole("region", { name: "Passkeys" }).getByRole("listitem").first().waitFor();
  await page.getByRole("region", { name: "Sessions" }).getByRole("listitem").first().waitFor();
  const signOutSaid = await watchFor(page, ["Loading"]);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expectPage(page, "Sign in to Dockwell");
  if ((await nav(page).count()) !== 0) fail("the sign-in screen shows the shell's navigation");
  if ((await signOutSaid()).length > 0) fail("the profile showed Loading on its way out after signing out");
  await shoot(page, "sign-in");
  await page.goto(`${origin}/status`);
  await expectPage(page, "Sign in to Dockwell");
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expectPage(page, "The control plane is running", 15_000);
  step("  signed out, /status asked to sign in, and the passkey returned there");
  const api = await page.evaluate(async () => {
    const r = await fetch("/dockwell.v1.AuthService/ListSessions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    return { status: r.status, body: await r.json() };
  });
  if (api.status !== 200 || api.body.sessions?.length !== 1)
    fail(`ListSessions after signing in again: ${JSON.stringify(api)}`);
  step(`  signed in again; ${api.body.sessions.length} open session`);

  step("profile: set up an authenticator app");
  await page.goto(`${origin}/`);
  await nav(page).getByRole("link", { name: "Mira Chen Owner" }).click();
  await expectPage(page, "Profile and security");
  await page.getByRole("region", { name: "Passkeys" }).getByRole("listitem").first().waitFor();
  await shoot(page, "profile");
  await page.getByRole("button", { name: "Set up" }).click();
  const sheet = page.getByRole("dialog", { name: "Set up an authenticator app" });
  const secret = (await sheet.locator(".totp-secret code").innerText()).replace(/\s/g, "");
  await shoot(page, "totp-setup");
  await sheet.getByLabel("Enter the 6-digit code it shows").fill(totp(secret));
  await sheet.getByRole("button", { name: "Turn on" }).click();
  const saved = page.getByRole("dialog", { name: "Save your recovery codes" });
  await saved.waitFor();
  const codes = await saved.locator(".totp-code-list code").allInnerTexts();
  if (codes.length !== 10) fail(`expected 10 recovery codes, got ${codes.length}`);
  await shoot(page, "recovery-codes");
  await saved.getByRole("button", { name: "I saved them" }).click();
  step(`  codes on, ${codes.length} recovery codes shown once`);

  step("sign out, then sign in with a recovery code");
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expectPage(page, "Sign in to Dockwell");
  await page.getByRole("button", { name: "Use a code from your authenticator app" }).click();
  await page.getByLabel("Email").fill("mira@acme.dev");
  await page.getByLabel("Code").fill("000000");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expectText(page, "That code didn't work");
  await shoot(page, "sign-in-code");
  await page.getByLabel("Code").fill(codes[0] ?? "");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expectText(page, "You used a recovery code");
  step("  a wrong code was refused, and the recovery code signed in");

  step("dockwell recover: a one-time link signs in a browser with no passkey");
  const recover = execFileSync(bin, ["recover", "--data-dir", data, "--log-format", "json"], {
    encoding: "utf8",
    env: { ...process.env, USER: "mira" },
  });
  const link =
    recover.match(/(http:\/\/localhost:\d+\/recover#[A-Za-z0-9_-]{43})/)?.[1] ??
    fail(`dockwell recover printed:\n${recover}`);
  const other = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const phone = await other.newPage();
  await phone.goto(link);
  await expectText(phone, "You signed in with a one-time link");
  await expectPage(phone, "Profile and security");
  if ((await phone.getByRole("link", { name: "Back" }).getAttribute("href")) !== "/more") {
    fail("the phone's profile header doesn't lead back to More");
  }
  await nav(phone).getByRole("link", { name: "More" }).click();
  await expectPage(phone, "More");
  await phone.getByRole("link", { name: "Profile and security Mira Chen" }).waitFor();
  await phone.getByRole("button", { name: "Sign out", exact: true }).waitFor();
  await shoot(phone, "more");
  await phone.goto(link);
  await expectText(phone, "This sign-in link didn't work");
  step("  signed in with the link once, More offers the profile and signing out; the second visit was refused");

  step("the first browser sees two sessions, and signs the other out");
  await page.reload();
  const sessions = page.getByRole("region", { name: "Sessions" });
  await sessions.getByRole("listitem").nth(1).waitFor();
  await sessions.getByRole("button", { name: "Sign out everywhere else" }).click();
  await sessions.getByRole("listitem").nth(1).waitFor({ state: "detached" });
  await phone.goto(`${origin}/more`);
  await expectPage(phone, "Sign in to Dockwell");
  step("  the other browser is signed out");

  step("API tokens: make one in the panel, call REST and Connect with it, then revoke it");
  const tokens = page.getByRole("region", { name: "API tokens" });
  await tokens.getByRole("button", { name: "Create token" }).click();
  const create = page.getByRole("dialog", { name: "Create an API token" });
  await create.getByLabel("Name").fill("e2e script");
  await shoot(page, "token-create");
  await create.getByRole("button", { name: "Create token" }).click();
  const made = tokens.locator(".tokens-made");
  await made.waitFor();
  const token = (await made.locator(".tokens-secret code").innerText()).trim();
  if (!/^dwt_[0-9A-Za-z]{49}$/.test(token)) fail(`the new token looks like ${JSON.stringify(token)}`);
  await shoot(page, "token-made");
  await made.getByRole("button", { name: "Done" }).click();
  if ((await page.getByText(token).count()) !== 0) fail("the token's secret stayed on the page after Done");
  // As a script calls: from outside the browser, with the token and no cookie
  // or origin, on the loopback IP, where no one can sign in.
  const withToken = async (path: string, init: RequestInit = {}) => {
    const r = await fetch(`http://127.0.0.1:${port}${path}`, {
      ...init,
      headers: { Authorization: `Bearer ${token}`, ...(init.headers as Record<string, string>) },
    });
    return { status: r.status, reason: r.headers.get("Dockwell-Error"), body: await r.text() };
  };
  const listOverConnect = () =>
    withToken("/dockwell.v1.ServerService/ListServers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  const rest = await withToken("/v1/servers");
  const restServers = rest.status === 200 ? (JSON.parse(rest.body) as { servers?: unknown[] }).servers : undefined;
  if (!restServers?.length) fail(`GET /v1/servers with the token: ${JSON.stringify(rest)}`);
  const overConnect = await listOverConnect();
  if (overConnect.status !== 200 || !overConnect.body.includes('"servers"')) {
    fail(`ListServers over Connect with the token: ${JSON.stringify(overConnect)}`);
  }
  const sessionsWithToken = await withToken("/v1/sessions");
  if (sessionsWithToken.status !== 403 || sessionsWithToken.reason !== "needs_session") {
    fail(`GET /v1/sessions with a token should need a session: ${JSON.stringify(sessionsWithToken)}`);
  }
  step(`  REST and Connect answered with the token (${restServers.length} server), and sign-in needed a session`);
  await page.reload();
  const row = tokens.getByRole("listitem").filter({ hasText: "e2e script" });
  await row.filter({ hasText: /from\s*127\.0\.0\.1/ }).waitFor();
  await shoot(page, "tokens");
  await row.getByRole("button", { name: "Revoke e2e script" }).click();
  await row.getByRole("button", { name: "Revoke", exact: true }).click();
  await row.waitFor({ state: "detached" });
  for (const [protocol, after] of [
    ["REST", await withToken("/v1/servers")],
    ["Connect", await listOverConnect()],
  ] as const) {
    if (after.status !== 401 || after.reason !== "token_rejected") {
      fail(`${protocol} with the revoked token: ${JSON.stringify(after)}`);
    }
  }
  step("  the profile showed where it was last used; once revoked, REST and Connect refused it");

  step("sign out on a phone, from More");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/more`);
  await expectPage(page, "More");
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expectPage(page, "Sign in to Dockwell");
  const left = await page.evaluate(
    async () =>
      (await fetch("/dockwell.v1.AuthService/GetAuthState", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      }).then((r) => r.json())) as { user?: unknown },
  );
  if (left.user) fail(`still signed in after signing out on More: ${JSON.stringify(left)}`);
  step("  signed out");

  try {
    const audit = execFileSync(
      "sqlite3",
      [join(data, "control", "state.db"), "SELECT action FROM audit_events ORDER BY at, id"],
      {
        encoding: "utf8",
      },
    ).trim();
    step(`audit log: ${audit.split("\n").join(", ")}`);
  } catch {
    // sqlite3 isn't installed; the audit log is covered by the Go tests.
  }
  console.log("e2e: passed");
}

try {
  await main();
} catch (err) {
  console.error(`e2e: failed: ${err instanceof Error ? err.message : String(err)}`);
  console.error(log.join("").split("\n").slice(-30).join("\n"));
  process.exitCode = 1;
} finally {
  await browser?.close();
  server?.kill("SIGTERM");
  rmSync(data, { recursive: true, force: true });
}
