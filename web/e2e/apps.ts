// SPDX-License-Identifier: AGPL-3.0-only

// The app screens on this machine's Docker, for CI's Panel end to end on Docker
// job: claims a throwaway install in headless Chromium, then runs the flow make
// e2e runs on a fake engine. It uses a data directory of its own, and deletes what it made,
// on Docker too, whether or not it passes.
//
//   make docker-panel-e2e
//   node web/e2e/apps.ts bin/dockwell
//
// Chromium is the system's: CHROMIUM_PATH, or /usr/bin/chromium.

import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { type Browser, chromium } from "playwright-core";

import { appsFlow } from "./apps-flow.ts";
import { containersFlow } from "./containers-flow.ts";
import { dockerMade, dockerProbe, removeMade } from "./docker-probe.ts";
import { healthFlow } from "./health-flow.ts";
import { describePages, watchBrowser } from "./report.ts";

// A real binary asks nothing beyond this machine in a test.
process.env["DOCKWELL_TEST"] = "1";

const bin = resolve(process.argv[2] ?? "bin/dockwell");
const port = 7650 + Math.floor(Math.random() * 300);
const origin = `http://localhost:${port}`;
const data = mkdtempSync(join(tmpdir(), "dockwell-e2e-"));
const published = Number(process.env["E2E_WEB_PORT"] ?? 18081);
const socket = process.env["DOCKER_HOST"]?.replace(/^unix:\/\//, "") ?? "/var/run/docker.sock";
const docker = dockerProbe(socket);

let server: ChildProcess | undefined;
let browser: Browser | undefined;
const log: string[] = [];

/** What was there before this run, which isn't this run's to remove. */
const before = await dockerMade(socket);

/**
 * Removes what this run made, whatever became of it, so a dev machine keeps
 * nothing on the published port or in its address pools: stopping dockwell leaves
 * its containers running, as it should anywhere else.
 */
async function cleanUp() {
  const left = await removeMade(socket, before).catch((err: unknown) => [String(err)]);
  if (left.length > 0) {
    console.error(`e2e: failed: this run left on Docker: ${left.join(", ")}`);
    process.exitCode = 1;
  }
}

// Certificates come from Pebble on this machine, as in scripts/docker-e2e.sh, when it's here.
const pebbleTools = process.env["DOCKWELL_TEST_PEBBLE"];
const pebble: ChildProcess[] = [];
const acme: Record<string, string> = {};
if (pebbleTools) {
  const dir = mkdtempSync(join(tmpdir(), "dockwell-pebble-"));
  const cert = join(dir, "cert.pem");
  const key = join(dir, "key.pem");
  execFileSync("openssl", [
    ..."req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj /CN=localhost".split(" "),
    ...["-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1", "-keyout", key, "-out", cert],
  ]);
  const config = join(dir, "pebble.json");
  writeFileSync(
    config,
    JSON.stringify({
      pebble: {
        listenAddress: "127.0.0.1:14000",
        managementListenAddress: "127.0.0.1:15000",
        certificate: cert,
        privateKey: key,
        httpPort: 80,
        tlsPort: 443,
        ocspResponderURL: "",
        externalAccountBindingRequired: false,
      },
    }),
  );
  pebble.push(
    spawn(
      join(pebbleTools, "pebble-challtestsrv"),
      "-defaultIPv4 127.0.0.1 -defaultIPv6= -dnsserver 127.0.0.1:8053 -doh= -http01= -https01= -tlsalpn01= -management 127.0.0.1:8055".split(
        " ",
      ),
      { stdio: "ignore" },
    ),
    spawn(join(pebbleTools, "pebble"), ["-config", config, "-dnsserver", "127.0.0.1:8053"], {
      stdio: "ignore",
      env: { ...process.env, PEBBLE_VA_NOSLEEP: "1", PEBBLE_WFE_NONCEREJECT: "0" },
    }),
  );
  Object.assign(acme, {
    DOCKWELL_ACME_DIRECTORY_URL: "https://localhost:14000/dir",
    DOCKWELL_ACME_ROOTS: cert,
    SSL_CERT_FILE: cert,
  });
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
      env: { ...process.env, USER: "mira", ...acme },
    },
  );
  let out = "";
  server.stdout?.on("data", (b: Buffer) => {
    out += b.toString();
  });
  server.stderr?.on("data", (b: Buffer) => log.push(b.toString()));
  // dockwell run prints a claim link only to a terminal, so this asks for one.
  for (let i = 0; i < 200 && !out.includes("To claim it, run this"); i++) {
    await new Promise((r) => setTimeout(r, 50));
  }
  if (!out.includes("To claim it, run this")) throw new Error(`dockwell run didn't say how to claim it:\n${out}`);
  const claimed = execFileSync(bin, ["claim", "--data-dir", data, "--log-format", "json"], {
    encoding: "utf8",
    env: { ...process.env, ...acme },
  });
  const claim = claimed.match(/http:\/\/localhost:\d+\/claim#[A-Za-z0-9_-]{43}/)?.[0] ?? "";
  if (claim === "") throw new Error(`dockwell claim printed no claim link:\n${claimed}`);
  browser = await chromium.launch({
    executablePath: process.env["CHROMIUM_PATH"] ?? "/usr/bin/chromium",
    // /dev/shm is small in a container and shared by every browser on the machine; Chromium crashes when it fills.
    args: ["--disable-dev-shm-usage"],
    headless: true,
  });
  watchBrowser(browser);
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  // A throw in here would end the process and skip the cleaning up.
  const pageThrew = new Promise<never>((_, threw) => {
    page.on("pageerror", (err) => threw(new Error(`the page threw: ${err.message}`)));
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
  const flow = async () => {
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
      certificates: pebbleTools !== undefined,
      // It starts and exits at once, with nothing to run.
      failing: "busybox:1.37",
      step: (what) => console.log(`e2e: ${what}`),
      shoot: async () => {},
    });
    await healthFlow(page, {
      origin,
      step: (what) => console.log(`e2e: ${what}`),
      shoot: async () => {},
      docker,
      socket,
      fake: false,
      bin,
      data,
      env: {},
      browser: browser as Browser,
      context,
    });
    await containersFlow(page, {
      origin,
      step: (what) => console.log(`e2e: ${what}`),
      shoot: async () => {},
      socket,
      docker,
      image: "nginx:1.27-alpine",
    });
  };
  await Promise.race([flow(), pageThrew]);
  console.log("e2e: passed");
} catch (err) {
  console.error(`e2e: failed: ${err instanceof Error ? err.message : String(err)}`);
  console.error(log.join("").split("\n").slice(-30).join("\n"));
  console.error(await describePages());
  process.exitCode = 1;
} finally {
  await browser?.close();
  server?.kill("SIGTERM");
  for (const running of pebble) running.kill("SIGTERM");
  await cleanUp();
  rmSync(data, { recursive: true, force: true });
}
