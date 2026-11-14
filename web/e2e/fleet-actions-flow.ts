// SPDX-License-Identifier: AGPL-3.0-only

// Fleet actions (ADR 0094), on a real control plane: servers picked on Servers go to
// New fleet action, which restarts a system service on each through one plan. The
// second server's agent, which the health flow left stopped, is left out of the
// change, as the page and the plan both say; with it connected, the plan has its
// steps wave by wave. Then New fleet action on a phone.

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";

const SEEN_WITHIN = 20_000;

function fail(message: string): never {
  throw new Error(message);
}

export async function fleetActionsFlow(
  page: Page,
  { origin, step, shoot }: Pick<FlowRun, "origin" | "step" | "shoot">,
) {
  step("fleet actions: servers picked on Servers go to New fleet action, and a restart runs on each through a plan");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/servers?closed=true`);
  const table = page.getByRole("table", { name: /^Servers/ });
  await table.waitFor({ timeout: SEEN_WITHIN });
  const listed = (await page.evaluate(async () => (await fetch("/v1/servers")).json())) as {
    servers?: { id: string; name: string; controlServer?: boolean; connection?: string }[];
  };
  const control = listed.servers?.find((s) => s.controlServer) ?? fail(`no control server: ${JSON.stringify(listed)}`);
  const second =
    listed.servers?.find((s) => s.name === "second") ?? fail(`no second server: ${JSON.stringify(listed)}`);
  const away = second.connection !== "CONNECTION_CONNECTED";

  await table.getByRole("checkbox", { name: control.name, exact: true }).check();
  await table.getByRole("checkbox", { name: second.name, exact: true }).check();
  const bar = page.getByRole("region", { name: "Picked servers" });
  await bar.getByText("2 selected").waitFor();
  await shoot(page, "servers-picked");
  await bar.getByRole("link", { name: "Run fleet action" }).click();
  await page.waitForFunction(() => window.location.pathname === "/fleet-actions");
  await page.getByText("2 servers picked").waitFor({ timeout: SEEN_WITHIN });

  await page.getByRole("radio", { name: /Act on a system service/ }).check();
  await page.getByRole("textbox", { name: "System service" }).fill("nginx.service");
  const target = page.getByRole("region", { name: "Target" });
  if (away) await target.getByText(`${second.name} isn't connected, so it isn't in this change`).waitFor();
  await page.getByRole("complementary", { name: "Summary" }).getByText("Restart nginx.service").waitFor();
  await shoot(page, "fleet-action-new");

  await page.getByRole("button", { name: "Review plan" }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan
    .locator(".plan-step-line")
    .getByText(/Restart\s*nginx\.service on /)
    .first()
    .waitFor({ timeout: SEEN_WITHIN });
  if (away) {
    await plan.getByText(`${second.name} isn't connected, so it isn't in this change`).waitFor();
  } else {
    await plan.getByText("Steps by wave").waitFor();
  }
  await shoot(page, "fleet-action-plan");
  await plan.getByRole("button", { name: /^Apply plan/ }).click();
  await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });
  await plan.waitFor({ state: "hidden" });

  step("on a phone, New fleet action fits its width");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/fleet-actions`);
  await page.getByRole("heading", { name: "New fleet action" }).first().waitFor({ timeout: SEEN_WITHIN });
  await page.getByRole("radio", { name: /Apply OS updates/ }).waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 390) fail(`New fleet action is ${wide} px wide on a 390 px phone`);
  await shoot(page, "fleet-action-phone");
  await page.setViewportSize({ width: 1440, height: 900 });
}

/** Where sign-in.ts runs this among the flows: after system services, before the second server leaves (flows.ts). */
export const flow: Flow = { order: 96.5, run: (page, run) => fleetActionsFlow(page, run) };
