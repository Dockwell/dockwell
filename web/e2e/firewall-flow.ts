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
  await page.goto(`${origin}/`);
  const link = await page.locator('a[href^="/servers/srv_"]').first().getAttribute("href");
  if (!link) fail("the overview links to no server");
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
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 88, run: (page, run) => firewallFlow(page, run) };
