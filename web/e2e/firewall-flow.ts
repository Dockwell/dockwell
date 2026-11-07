// SPDX-License-Identifier: AGPL-3.0-only

// A server's Firewall tab, read-only: what filters its traffic as this machine has
// it, which a CI runner's ufw, run without root, says it can't, and the ports
// Docker publishes. Nothing is changed by looking (ADR 0052).

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

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
  // Whatever this machine has: a firewall read or not, or nothing filtering.
  await page
    .getByText(/Nothing filters what comes in|read-only, as Dockwell didn't make it|needs root|failed:/)
    .first()
    .waitFor({ timeout: 15_000 });
  if ((await page.getByRole("alert").count()) > 0) {
    fail(`the Firewall tab says something went wrong: ${await page.getByRole("alert").first().innerText()}`);
  }
  await shoot(page, "server-firewall");
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 88, run: (page, run) => firewallFlow(page, run) };
