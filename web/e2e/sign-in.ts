// SPDX-License-Identifier: AGPL-3.0-only

// Claims a fresh install, signs out and signs in again in headless Chromium,
// against a real `dockwell run` on a temporary data directory, with the
// DevTools virtual authenticator standing in for Touch ID. It adds a second
// passkey, from a second authenticator, confirmed with the first: a
// step-up, as every change to how you sign in takes. Then it turns on
// codes, signs in with a recovery code, opens a link from `dockwell recover`
// on a phone, and signs that phone out from the first browser. It makes an
// API token in the panel, calls REST and Connect with it as a script would,
// makes a project with a plan and undoes it, and revokes the token. Then it
// creates a project from the command bar, reviews its plan in the plan sheet,
// applies it, and undoes it from Activity. The CLI plans a project with local
// access, and the panel applies it at the address the CLI sends a person to,
// /changes/<id>. It adds a secret in the panel, reveals it with the passkey, and
// checks an API token can't. Then it runs the app screens from apps-flow.ts, the
// plan sheet's every state from plan-flow.ts, and an install from
// catalog-flow.ts, on the fake Docker Engine of scripts/fakedocker. Everything
// after the claim goes through the panel shell: the rail's link to your
// profile on wide screens, More on phones. With --shots <dir> it also saves
// screenshots of every screen at 1440, 800, 390 and 360 px, light and dark.
//
//   make e2e
//   node web/e2e/sign-in.ts bin/dockwell --fake-docker bin/fakedocker --shots /tmp/shots
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { createHmac } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

import { type Browser, type BrowserContext, chromium, type Page } from "playwright-core";

import { appsFlow } from "./apps-flow.ts";
import { catalogFlow } from "./catalog-flow.ts";
import { containersFlow } from "./containers-flow.ts";
import { dockerMade, dockerProbe, dockerStop, removeMade } from "./docker-probe.ts";
import { healthFlow } from "./health-flow.ts";
import { planFlow } from "./plan-flow.ts";

const { values, positionals } = parseArgs({
  allowPositionals: true,
  options: { shots: { type: "string" }, "fake-docker": { type: "string" } },
});
const bin = resolve(positionals[0] ?? "bin/dockwell");
const shots = values.shots ? resolve(values.shots) : undefined;
const port = 7650 + Math.floor(Math.random() * 300);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-e2e-"));
const catalogDir = mkdtempSync(join(tmpdir(), "dockwell-e2e-catalog-"));

let server: ChildProcess | undefined;
let docker: ChildProcess | undefined;
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

/** Where the fake Docker Engine's socket is, once it has started. */
let fakeSocket = "";

/** Starts the fake Docker Engine and says where its socket is, for dockwell to reach it. */
async function fakeDocker(): Promise<NodeJS.ProcessEnv> {
  const path = values["fake-docker"];
  if (!path) return {};
  docker = spawn(resolve(path), [join(data, "docker")], { stdio: ["ignore", "pipe", "inherit"] });
  // A fake that dies, or says nothing, would otherwise hang until the job's timeout.
  const socket = await new Promise<string>((done, refused) => {
    const silent = setTimeout(() => refused(new Error(`${path} said nothing for 15 s, so it isn't serving`)), 15_000);
    docker?.stdout?.once("data", (b: Buffer) => {
      clearTimeout(silent);
      done(b.toString().trim());
    });
    docker?.once("error", (err) => {
      clearTimeout(silent);
      refused(err);
    });
    docker?.once("exit", (code, signal) => {
      clearTimeout(silent);
      refused(new Error(`${path} ended with ${code ?? signal} before it said where it serves`));
    });
  });
  fakeSocket = socket;
  return { DOCKER_HOST: `unix://${socket}` };
}

/** Starts dockwell run and waits for the claim link it prints. */
async function start(): Promise<string> {
  const engine = await fakeDocker();
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
      env: { ...process.env, ...engine, USER: "mira", DOCKWELL_CATALOG_DIR: catalogDir },
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
    for (const width of [1440, 800, 390, 360]) {
      await page.setViewportSize({ width, height: width === 1440 ? 900 : 844 });
      // A full-page shot of a long page starts at its top, as someone arriving would see it.
      await page.evaluate(() => window.scrollTo(0, 0));
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

let pageThrew: (err: Error) => void = () => {};
const pageError = new Promise<never>((_, threw) => {
  pageThrew = threw;
});

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
  // A throw in here would end the process and skip the cleaning up.
  page.on("pageerror", (err) => pageThrew(new Error(`the page threw: ${err.message}`)));
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

  step("add a second passkey, confirmed with the first: a step-up");
  await page.goto(`${origin}/`);
  await nav(page).getByRole("link", { name: "Mira Chen Owner" }).click();
  await expectPage(page, "Profile and security");
  const passkeyList = page.getByRole("region", { name: "Passkeys" });
  await passkeyList.getByRole("listitem").first().waitFor();
  // The first authenticator holds the passkey that confirms it. A second,
  // as a security key would, makes the new one: the first can't, as it
  // holds one for this account already. They swap once the step-up is done,
  // as the panel asks to begin adding with its proof, the second time it
  // asks: the first, without one, is answered needs_step_up.
  const beginAdd = "**/dockwell.v1.AuthService/BeginAddPasskey";
  let begins = 0;
  let second: string | undefined;
  await page.route(beginAdd, async (route) => {
    begins += 1;
    if (begins === 2) {
      await cdp.send("WebAuthn.setAutomaticPresenceSimulation", { authenticatorId, enabled: false });
      ({ authenticatorId: second } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
        options: {
          protocol: "ctap2",
          transport: "usb",
          hasResidentKey: true,
          hasUserVerification: true,
          isUserVerified: true,
          automaticPresenceSimulation: true,
        },
      }));
    }
    await route.continue();
  });
  const stepUps = await watchFor(page, ["needs your passkey just now", "The passkey prompt closed"]);
  await passkeyList.getByRole("button", { name: "Add passkey" }).click();
  await passkeyList.getByRole("listitem").nth(1).waitFor({ timeout: 15_000 });
  await page.unroute(beginAdd);
  await cdp.send("WebAuthn.setAutomaticPresenceSimulation", { authenticatorId, enabled: true });
  if (begins !== 2) fail(`the panel began adding a passkey ${begins} times; want twice, before and after its step-up`);
  const flashed = await stepUps();
  if (flashed.length > 0) fail(`adding a passkey showed ${JSON.stringify(flashed)}`);
  const onSecond = await cdp.send("WebAuthn.getCredentials", {
    authenticatorId: second ?? fail("the second authenticator was never added"),
  });
  if (onSecond.credentials.length !== 1 || onSecond.credentials[0]?.rpId !== "localhost") {
    fail(`the second authenticator holds ${JSON.stringify(onSecond.credentials)}, want one passkey for localhost`);
  }
  const added = await page.evaluate(async () => {
    const r = await fetch("/dockwell.v1.AuthService/ListPasskeys", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    return (await r.json()) as { passkeys?: unknown[] };
  });
  if (added.passkeys?.length !== 2) fail(`ListPasskeys after adding one = ${JSON.stringify(added)}`);
  await shoot(page, "passkey-added");
  step("  the first passkey confirmed it; the second authenticator made a passkey for localhost, and either signs in");

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

  step("changes: make a project through the API with a plan, apply it, then undo it");
  const readOnly = await withToken("/v1/changes", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      summary: "Add the shop project",
      edits: [{ resource: { name: "shop" }, spec: { project: {} } }],
    }),
  });
  if (readOnly.status !== 403 || readOnly.reason !== "token_scope") {
    fail(`proposing a change with the read-only token: ${JSON.stringify(readOnly)}`);
  }
  type Change = {
    id: string;
    state: string;
    reverts?: string;
    proposedBy?: { kind: string; name: string };
    plan?: {
      fingerprint: string;
      summary?: { create?: number; destroy?: number };
      resources?: { action: string; resource: { kind: string; name: string } }[];
      servers?: unknown[];
    };
  };
  // From the panel's page, with its session, as the panel calls the API.
  const changeCall = async (path: string, body?: unknown): Promise<Change> => {
    const r = await page.evaluate(
      async ({ path, body }) => {
        const res = await fetch(
          path,
          body === undefined
            ? {}
            : { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) },
        );
        return { status: res.status, reason: res.headers.get("Dockwell-Error"), body: await res.text() };
      },
      { path, body },
    );
    if (r.status !== 200) fail(`${body === undefined ? "GET" : "POST"} ${path}: ${JSON.stringify(r)}`);
    return (JSON.parse(r.body) as { change: Change }).change;
  };
  const applied = async (change: Change): Promise<Change> => {
    await changeCall(`/v1/changes/${change.id}:apply`, {});
    for (let i = 0; i < 100; i++) {
      const now = await changeCall(`/v1/changes/${change.id}`);
      if (now.state === "CHANGE_STATE_APPLIED") return now;
      if (now.state !== "CHANGE_STATE_APPLYING")
        fail(`applying ${change.id} ended ${now.state}: ${JSON.stringify(now)}`);
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    return fail(`${change.id} didn't finish applying`);
  };
  const shop = await changeCall("/v1/changes", {
    summary: "Add the shop project",
    edits: [{ resource: { name: "shop" }, spec: { project: { displayName: "Shop" } } }],
  });
  const kinds = (shop.plan?.resources ?? []).map((r) => `${r.action} ${r.resource.kind} ${r.resource.name}`).join(", ");
  if (
    shop.state !== "CHANGE_STATE_PLANNED" ||
    shop.plan?.summary?.create !== 2 ||
    kinds !== "PLAN_ACTION_CREATE project shop, PLAN_ACTION_CREATE environment production" ||
    shop.proposedBy?.kind !== "ACTOR_KIND_USER" ||
    shop.proposedBy?.name !== "Mira Chen"
  ) {
    fail(`proposing a project planned ${JSON.stringify(shop)}`);
  }
  step(`  planned ${kinds}, fingerprint ${shop.plan?.fingerprint}`);
  await applied(shop);
  const undo = await changeCall(`/v1/changes/${shop.id}:undo`, {});
  if (undo.reverts !== shop.id || undo.plan?.summary?.destroy !== 2)
    fail(`undoing the project planned ${JSON.stringify(undo)}`);
  await applied(undo);
  const history = await page.evaluate(async () => (await fetch("/v1/changes?pageSize=3")).json());
  const ids = ((history as { changes?: Change[] }).changes ?? []).map((c) => c.id);
  if (ids[0] !== undo.id || ids[1] !== shop.id)
    fail(`GET /v1/changes lists ${JSON.stringify(ids)}, want the undo, then the project`);
  step(`  the read-only token was refused; the panel applied ${shop.id}, then undid it in ${undo.id}`);
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

  step("the panel: create a project from the command bar, review and apply its plan, then undo it from Activity");
  await page.goto(`${origin}/activity`);
  await expectPage(page, "Activity");
  // The changes the token made are on the timeline already, with who made them.
  await page
    .getByRole("row", { name: /Add the shop project/ })
    .first()
    .waitFor();
  await page.keyboard.press("Control+k");
  const bar = page.getByRole("dialog", { name: "Command bar" });
  await bar.getByRole("option", { name: /^Create a project/ }).click();
  await bar.getByRole("combobox").fill("garden");
  await shoot(page, "command-bar");
  await bar.getByRole("option", { name: /^Create the project garden/ }).click();
  const review = page.getByRole("dialog", { name: "Review plan" });
  await review.waitFor();
  await expectText(review, "Create the project garden");
  await review.getByText("2", { exact: true }).first().waitFor();
  const plannedText = await review.innerText();
  if (!/2\s*to create/.test(plannedText) || !plannedText.includes("production")) {
    fail(`the plan sheet for a new project shows ${JSON.stringify(plannedText)}`);
  }
  await shoot(page, "plan-sheet");
  await review.getByRole("button", { name: /^Apply plan/ }).click();
  // A project runs on no server, so the change has no steps: it goes from planned to applied in the sheet.
  const done = page.getByRole("dialog", { name: "Applied" });
  await done.waitFor();
  await expectText(done, "Recorded and applied");
  await shoot(page, "plan-applied");
  await done.getByRole("button", { name: "Done" }).click();
  // The timeline has it at once, from its watch.
  const garden = page.getByRole("row", { name: /Create the project garden/ });
  await garden.waitFor();
  await garden.click();
  const detail = page.getByRole("region", { name: "Create the project garden" });
  await expectText(detail, "Mira Chen");
  await shoot(page, "activity-selected");
  await detail.getByRole("button", { name: "Undo this change" }).click();
  const undoReview = page.getByRole("dialog", { name: "Review plan" });
  await undoReview.waitFor();
  if (!/2\s*to delete/.test(await undoReview.innerText())) {
    fail(`the undo's plan shows ${JSON.stringify(await undoReview.innerText())}`);
  }
  await shoot(page, "plan-undo");
  await undoReview.getByRole("button", { name: /^Delete 2 resources/ }).click();
  await page.getByRole("dialog", { name: "2 resources are deleted" }).getByRole("button", { name: "Done" }).click();
  await page.getByRole("row", { name: /Undo: Create the project garden/ }).waitFor();
  await detail.getByText("Undone by").waitFor();
  if ((await detail.getByRole("button", { name: "Undo this change" }).count()) !== 0) {
    fail("Activity still offers to undo a change that was undone");
  }
  await page.getByRole("button", { name: "Close details" }).click();
  await shoot(page, "activity");
  step("  planned, applied and undone in the panel; Activity followed it live and stopped offering undo");

  step("the CLI: local access plans a project, and the panel applies it at the address the CLI gives a person");
  const cli = (...args: string[]) =>
    execFileSync(bin, [...args, "--data-dir", data, "--log-format", "json"], {
      encoding: "utf8",
      // No login of the person running this: local access, as on a fresh control server.
      env: { ...process.env, USER: "mira", DOCKWELL_URL: "", DOCKWELL_TOKEN: "", XDG_CONFIG_HOME: join(data, "cli") },
    });
  cli("login", "--local");
  const proposed = JSON.parse(
    cli("projects", "create", "tools", "--summary", "Add the tools project", "--plan-only", "--json"),
  ) as { change?: { id?: string } };
  const changeId = proposed.change?.id ?? fail(`dockwell projects create printed ${JSON.stringify(proposed)}`);
  // The CLI calls the loopback address dockwell run recorded, and sends a
  // person to the panel there on localhost, where passkeys work: the
  // origin this browser signed in on.
  const local = JSON.parse(readFileSync(join(data, "control", "local-token.json"), "utf8")) as { url?: string };
  if (local.url !== `http://127.0.0.1:${port}`)
    fail(`local access calls ${local.url}, not the panel's loopback address`);
  await page.goto(`${origin}/changes/${changeId}`);
  const fromCLI = page.getByRole("dialog", { name: "Review plan" });
  await fromCLI.waitFor();
  await expectText(fromCLI, "Add the tools project");
  await shoot(page, "plan-from-cli");
  await fromCLI.getByRole("button", { name: /^Apply plan/ }).click();
  const cliDone = page.getByRole("dialog", { name: "Applied" });
  await cliDone.waitFor();
  await cliDone.getByRole("button", { name: "Done" }).click();
  if (new URL(page.url()).pathname !== "/activity") fail(`closing the sheet left ${page.url()}, not Activity`);
  // Applied here, by Mira Chen from the panel; proposed by local access's token, from the CLI.
  const tools = page.getByRole("region", { name: "Add the tools project" });
  await expectText(tools, "Proposed by");
  await expectText(tools, "Mira Chen");
  await expectText(tools, "From the CLI");
  await expectText(tools, "Local access on");
  await shoot(page, "activity-from-cli");
  // Local access's token, on the timeline as the shell account that made it.
  await page
    .getByRole("row", { name: /Created the API token Local access on .*, read-write/ })
    .first()
    .waitFor();
  cli("logout", "--local");
  step(
    `  ${changeId}, planned by the CLI's local access, applied at ${origin}/changes/${changeId}; Activity names both`,
  );

  step("secrets: add one in the panel, reveal it with the passkey, and check an API token can't");
  await page.goto(`${origin}/variables`);
  await expectPage(page, "Variables and secrets");
  // By now there are two projects, so the environment goes by default / production.
  const valueForm = page.getByRole("form", { name: /^Add a variable or secret to (default \/ )?production$/ });
  await valueForm.getByRole("textbox", { name: "Key" }).fill("DB_PASSWORD");
  await valueForm.getByRole("switch", { name: "Secret" }).click();
  const secretValue = "e2e-s3cret-correct-horse";
  await valueForm.getByLabel("Value").fill(secretValue);
  await valueForm.getByRole("button", { name: "Add" }).click();
  const secretPlan = page.getByRole("dialog", { name: "Review plan" });
  await secretPlan.waitFor();
  await expectText(secretPlan, "Add the secret DB_PASSWORD");
  if ((await secretPlan.innerText()).includes(secretValue)) fail("the plan sheet shows the secret's value");
  await shoot(page, "plan-secret");
  await secretPlan.getByRole("button", { name: /^Apply plan/ }).click();
  const secretDone = page.getByRole("dialog", { name: /is created$/ });
  await secretDone.waitFor();
  await secretDone.getByRole("button", { name: "Done" }).click();
  const revealButton = page.getByRole("button", { name: "Reveal DB_PASSWORD" });
  await revealButton.waitFor();
  if ((await page.content()).includes(secretValue)) fail("the page has the secret's value before it was revealed");
  await shoot(page, "variables");
  // The virtual authenticator answers the passkey prompt, as Touch ID would.
  await revealButton.click();
  await page.getByText(secretValue, { exact: true }).waitFor();
  await shoot(page, "variables-revealed");
  const listed = await page.evaluate(async () => (await fetch("/v1/variables?project=default")).json());
  const secretId =
    ((listed as { variables?: { id: string; name: string; value?: string }[] }).variables ?? []).find(
      (v) => v.name === "DB_PASSWORD",
    )?.id ?? fail(`GET /v1/variables listed ${JSON.stringify(listed)}`);
  if (JSON.stringify(listed).includes(secretValue)) fail("GET /v1/variables carries the secret's value");
  await page.getByRole("button", { name: "Hide DB_PASSWORD" }).click();
  // A script's token, even read-write, is refused, and so is its step-up.
  // Making the token takes the passkey too, so a page's script can't: the
  // panel makes it, and the browser asks.
  const withoutStepUp = await page.evaluate(async () => {
    const res = await fetch("/v1/tokens", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: "e2e sneaky", scope: "TOKEN_SCOPE_READ_WRITE" }),
    });
    return { status: res.status, reason: res.headers.get("Dockwell-Error") };
  });
  if (withoutStepUp.status !== 403 || withoutStepUp.reason !== "needs_step_up") {
    fail(`POST /v1/tokens from the page without a step-up = ${JSON.stringify(withoutStepUp)}`);
  }
  await page.goto(`${origin}/profile`);
  const revealTokens = page.getByRole("region", { name: "API tokens" });
  await revealTokens.getByRole("button", { name: "Create token" }).click();
  const revealCreate = page.getByRole("dialog", { name: "Create an API token" });
  await revealCreate.getByLabel("Name").fill("e2e reveal");
  await revealCreate.getByRole("radio", { name: /Read and write/ }).check();
  await revealCreate.getByRole("button", { name: "Create token" }).click();
  const revealMade = revealTokens.locator(".tokens-made");
  await revealMade.waitFor();
  const revealer = (await revealMade.locator(".tokens-secret code").innerText()).trim();
  await revealMade.getByRole("button", { name: "Done" }).click();
  const revealTokenId = await page.evaluate(async () => {
    const res = await fetch("/v1/tokens");
    const body = (await res.json()) as { tokens?: { id: string; name: string }[] };
    return body.tokens?.find((t) => t.name === "e2e reveal")?.id ?? "";
  });
  for (const [path, body] of [
    [`/v1/secrets/${secretId}:reveal`, { proof: "anything" }],
    ["/v1/sessions:beginStepUp", { action: "STEP_UP_ACTION_REVEAL_SECRET", target: secretId }],
    ["/v1/sessions:beginStepUp", { action: "STEP_UP_ACTION_CREATE_TOKEN" }],
    ["/v1/sessions:beginStepUp", { action: "STEP_UP_ACTION_ADD_PASSKEY" }],
  ] as const) {
    const r = await fetch(`http://127.0.0.1:${port}${path}`, {
      method: "POST",
      headers: { Authorization: `Bearer ${revealer}`, "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const text = await r.text();
    if (r.status !== 403 || r.headers.get("Dockwell-Error") !== "needs_session" || text.includes(secretValue)) {
      fail(`POST ${path} with a read-write token = ${r.status} ${r.headers.get("Dockwell-Error")}: ${text}`);
    }
  }
  await page.evaluate(
    async (id) =>
      fetch(`/v1/tokens/${id}:revoke`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }),
    revealTokenId || fail("the token made for the reveal isn't listed"),
  );
  await page.goto(`${origin}/activity`);
  await page
    .getByRole("row", { name: /Revealed the secret DB_PASSWORD/ })
    .first()
    .waitFor();
  step(
    `  ${secretId} added through a plan, revealed with the passkey and on the timeline; a token, and a page's script, were refused`,
  );
  if (fakeSocket !== "") {
    const before = await dockerMade(fakeSocket);
    await appsFlow(page, { origin, step, shoot, docker: dockerProbe(fakeSocket), failing: "nginx:broken" });
    await planFlow(page, {
      origin,
      browser,
      context,
      step,
      shoot,
      stopByHand: (id) => dockerStop(fakeSocket, id),
      docker: dockerProbe(fakeSocket),
    });
    await healthFlow(page, {
      origin,
      step,
      shoot,
      docker: dockerProbe(fakeSocket),
      socket: fakeSocket,
      fake: true,
      bin,
      data,
      env: { DOCKER_HOST: `unix://${fakeSocket}` },
      browser,
      context,
      // A daemon that hangs, as a stuck storage driver makes Docker: it takes calls and answers none.
      hangDocker: () => {
        docker?.kill("SIGSTOP");
        return () => docker?.kill("SIGCONT");
      },
      // A control plane that stops answering and keeps its connections, as a frozen
      // process or a proxy holding them open does.
      freezeControlPlane: () => {
        server?.kill("SIGSTOP");
        return () => server?.kill("SIGCONT");
      },
    });
    await catalogFlow(page, { origin, step, shoot, docker: dockerProbe(fakeSocket), catalogDir });
    await containersFlow(page, {
      origin,
      step,
      shoot,
      socket: fakeSocket,
      docker: dockerProbe(fakeSocket),
      image: "acme/legacy:prints",
    });
    // What make docker-panel-e2e does at its end, where only CI could see it.
    step("Docker: what the run made comes off, networks and volumes too");
    const made = await dockerMade(fakeSocket);
    if (made.networks.length === 0) fail("the flow left no network, so this shows nothing about removing one");
    const left = await removeMade(fakeSocket, before);
    if (left.length > 0) fail(`removing what the run made left ${left.join(", ")}`);
  }

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
  await Promise.race([main(), pageError]);
} catch (err) {
  console.error(`e2e: failed: ${err instanceof Error ? err.message : String(err)}`);
  console.error(log.join("").split("\n").slice(-30).join("\n"));
  process.exitCode = 1;
} finally {
  await browser?.close();
  server?.kill("SIGTERM");
  docker?.kill("SIGTERM");
  rmSync(data, { recursive: true, force: true });
}
