// SPDX-License-Identifier: AGPL-3.0-only

// A server's Firewall tab, read-only: what filters its traffic, and the ports
// Docker publishes. Under DOCKWELL_TEST the agent runs none of the machine's own
// tools, only the stand-ins hostTools writes: a ufw that answers as Ubuntu's does
// after ufw allow OpenSSH and ufw allow "Nginx Full", and an nft with nothing in
// its ruleset. Nothing is changed by looking (ADR 0052).

import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";

/** ufw status verbose as ufw prints it, its rows "%-26s %-12s%-26s", app profiles named after their ports. */
const ufwStatus = [
  "Status: active",
  "Logging: on (low)",
  "Default: deny (incoming), allow (outgoing), disabled (routed)",
  "New profiles: skip",
  "",
  "To                         Action      From",
  "--                         ------      ----",
  "22/tcp (OpenSSH)           ALLOW IN    Anywhere",
  "80,443/tcp (Nginx Full)    ALLOW IN    Anywhere",
  "22/tcp (OpenSSH (v6))      ALLOW IN    Anywhere (v6)",
  "80,443/tcp (Nginx Full (v6)) ALLOW IN    Anywhere (v6)",
  "",
].join("\n");

/**
 * Writes stand-ins for the server's own tools into dir, laid out as the paths
 * they stand in for, for DOCKWELL_TEST_TOOLS, and returns dir.
 */
export function hostTools(dir: string): string {
  const sbin = join(dir, "usr", "sbin");
  mkdirSync(sbin, { recursive: true });
  writeFileSync(join(sbin, "ufw"), `#!/bin/sh\ncat <<'EOF'\n${ufwStatus}EOF\n`, { mode: 0o755 });
  writeFileSync(join(sbin, "nft"), `#!/bin/sh\necho '{"nftables": []}'\n`, { mode: 0o755 });
  return dir;
}

interface FirewallFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function firewallFlow(page: Page, { origin, step, shoot }: FirewallFlowOptions) {
  step("a server's Firewall tab says what filters its traffic, read-only, and what Docker publishes past it");
  // The run has a second server by now, with no agent connected: read the control server's.
  const link = await ownServerPath(page, origin);
  await page.goto(`${origin}${link}`);
  await page
    .getByRole("navigation", { name: /^Server / })
    .getByRole("link", { name: "Firewall" })
    .click();
  await page.waitForFunction((to) => window.location.pathname === to, `${link}/firewall`);
  // ufw's app profiles read as the ports they allow, as ufw status verbose says them.
  const ufw = page.getByRole("region", { name: "ufw" });
  await ufw.waitFor({ timeout: 15_000 });
  await ufw
    .getByRole("row", { name: /22\/tcp \(OpenSSH\)/ })
    .first()
    .waitFor();
  await ufw
    .getByRole("row", { name: /80,443\/tcp \(Nginx Full\)/ })
    .first()
    .waitFor();
  if ((await page.getByText(/Nothing filters what comes in/).count()) > 0) {
    fail("the Firewall tab says nothing filters, beside ufw");
  }
  if ((await page.getByRole("alert").count()) > 0) {
    fail(`the Firewall tab says something went wrong: ${await page.getByRole("alert").first().innerText()}`);
  }
  await shoot(page, "server-firewall");

  step(
    "a rule of Dockwell's own applies with a passkey, waits for you to say you can still reach the server, and stays",
  );
  const rules = page.getByRole("region", { name: "Dockwell's rules" });
  await rules.getByRole("button", { name: "Add a rule" }).click();
  const add = page.getByRole("dialog", { name: /^Add a rule to / });
  await add.getByLabel("Port").fill("8443");
  await add.getByRole("button", { name: "Plan it" }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  // The browser's virtual authenticator gives the passkey the change takes. Then the server waits.
  const keep = page.getByRole("button", { name: "Keep the change" });
  await keep.waitFor({ timeout: 30_000 });
  const test = await page.getByText(/^Open a new SSH session to port /).innerText();
  if (!test.includes("with your key")) fail(`the confirmation's test says ${test}`);
  await shoot(page, "server-firewall-confirm");
  await keep.click();
  await page.getByRole("button", { name: "Keep the change" }).waitFor({ state: "detached", timeout: 15_000 });
  await page
    .getByRole("button", { name: /^(Done|Close)$/ })
    .first()
    .click();
  await rules.getByRole("row", { name: /8443\/tcp/ }).waitFor({ timeout: 15_000 });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 88, run: (page, run) => firewallFlow(page, run) };
