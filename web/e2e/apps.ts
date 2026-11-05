// SPDX-License-Identifier: AGPL-3.0-only

// The app screens on this machine's Docker, for CI's Docker job: claims a
// throwaway install in headless Chromium, then runs the flow make e2e runs on
// a fake engine. It uses a data directory of its own, and deletes what it made,
// on Docker too, whether or not it passes.
//
//   make docker-panel-e2e
//   node web/e2e/apps.ts bin/dockwell
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { type Browser, chromium } from "playwright-core";

import { appsFlow } from "./apps-flow.ts";
import { dockerProbe } from "./docker-probe.ts";

const bin = resolve(process.argv[2] ?? "bin/dockwell");
const port = 7650 + Math.floor(Math.random() * 300);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-e2e-"));
const published = Number(process.env["E2E_WEB_PORT"] ?? 18081);
const docker = dockerProbe(process.env["DOCKER_HOST"]?.replace(/^unix:\/\//, "") ?? "/var/run/docker.sock");

let server: ChildProcess | undefined;
let browser: Browser | undefined;
const log: string[] = [];

/** What was there before this run, which isn't this run's to remove. */
const before = new Set((await docker()).map((c) => c.id));

function dockerQuietly(...args: string[]): string {
  try {
    return execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
  } catch {
    return "";
  }
}

/**
 * Removes what this run made, whatever became of it, so a dev machine keeps
 * nothing on the published port: stopping dockwell leaves its containers
 * running, as it should anywhere else.
 */
async function cleanUp() {
  const made = (await docker().catch(() => [])).filter((c) => !before.has(c.id));
  for (const c of made) dockerQuietly("rm", "--force", c.id);
  for (const project of new Set(made.map((c) => c.project))) {
    const label = `label=com.docker.compose.project=${project}`;
    for (const net of dockerQuietly("network", "ls", "-q", "--filter", label).split("\n")) {
      if (net) dockerQuietly("network", "rm", net);
    }
    for (const vol of dockerQuietly("volume", "ls", "-q", "--filter", label).split("\n")) {
      if (vol) dockerQuietly("volume", "rm", vol);
    }
  }
}

try {
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
  let claim = "";
  for (let i = 0; i < 200 && claim === ""; i++) {
    claim = out.match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]{43}/)?.[0] ?? "";
    await new Promise((r) => setTimeout(r, 50));
  }
  if (claim === "") throw new Error(`dockwell run printed no claim link:\n${out}`);
  browser = await chromium.launch({
    executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium",
    headless: true,
  });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  page.on("pageerror", (err) => {
    throw new Error(`the page threw: ${err.message}`);
  });
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
  await page.goto(claim);
  await page.getByLabel("Your name").fill("Mira Chen");
  await page.getByLabel("Email").fill("mira@acme.dev");
  await page.getByRole("button", { name: "Create passkey" }).click();
  await page.getByRole("heading", { level: 1, name: "Overview", exact: true }).waitFor({ timeout: 15_000 });
  await page.locator(".dw-metric", { hasText: "CPU" }).waitFor({ timeout: 20_000 });
  await appsFlow(page, {
    origin,
    docker,
    port: published,
    reachable: true,
    // It starts and exits at once, with nothing to run.
    failing: "busybox:1.37",
    step: (what) => console.log(`e2e: ${what}`),
    shoot: async () => {},
  });
  console.log("e2e: passed");
} catch (err) {
  console.error(`e2e: failed: ${err instanceof Error ? err.message : String(err)}`);
  console.error(log.join("").split("\n").slice(-30).join("\n"));
  process.exitCode = 1;
} finally {
  await browser?.close();
  server?.kill("SIGTERM");
  await cleanUp();
  rmSync(data, { recursive: true, force: true });
}
