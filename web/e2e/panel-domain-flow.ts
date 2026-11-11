// SPDX-License-Identifier: AGPL-3.0-only

// The panel's domain, set from Settings, General (ADR 0063), on a real control plane:
// typed and checked, saved with the passkey it asks for, which the browser's virtual
// authenticator gives, and the restart that serves it, which re-executes dockwell run
// in place. A test reaches no CA and may not hold ports 80 and 443, so after the
// restart the domain reads refused or waiting for its certificate, as this machine
// allows; either way, Stop using it brings the panel back to loopback alone. It runs
// last, as it restarts the control plane.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

interface PanelDomainFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function panelDomainFlow(page: Page, { origin, step, shoot }: PanelDomainFlowOptions) {
  step("panel domain: typed and checked, then saved with a passkey, which restarts Dockwell to serve it");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/settings/general`);
  const panel = page.getByRole("region", { name: "Panel address" });
  const field = panel.getByRole("textbox", { name: "Domain" });
  await field.waitFor({ timeout: 15_000 });
  await field.fill("panel.example.com");
  const save = panel.getByRole("button", { name: "Serve the panel there" });
  await page.waitForFunction((el) => el instanceof HTMLButtonElement && !el.disabled, await save.elementHandle(), {
    timeout: 15_000,
  });
  await shoot(page, "settings-panel-domain");
  await save.click();
  await panel.getByText("Dockwell restarts to serve panel.example.com").waitFor();
  await panel.getByRole("button", { name: "Restart and serve it" }).click();
  await panel
    .getByText(/Dockwell restarts to serve panel\.example\.com\. Apps and websites keep running/)
    .waitFor({ timeout: 15_000 });

  step("panel domain: after the restart, the panel answers again and says how the domain stands");
  const standing = panel.getByText(
    /^(panel\.example\.com isn't served|Getting a certificate for panel\.example\.com…)$/,
  );
  await standing.waitFor({ timeout: 60_000 });
  const said = await standing.textContent();
  if (said === "panel.example.com isn't served") {
    await panel.getByText(/couldn't hold the ports panel\.example\.com needs/).waitFor();
  }
  await shoot(page, "settings-panel-domain-after");

  step("panel domain: Stop using it restarts Dockwell on loopback alone again");
  await panel.getByRole("button", { name: "Stop using it" }).click();
  await field.waitFor({ timeout: 60_000 });
  if ((await panel.getByText("Loopback only").count()) === 0)
    fail("after Stop using it, the panel doesn't read loopback only");
}

/** Where sign-in.ts runs this among the flows: last, as it restarts the control plane (flows.ts). */
export const flow: Flow = { order: 99, run: (page, run) => panelDomainFlow(page, run) };
