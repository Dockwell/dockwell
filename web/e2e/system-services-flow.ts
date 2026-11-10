// SPDX-License-Identifier: AGPL-3.0-only

// A server's system services (ADR 0067), on a real control plane whose own agent
// reads them with a stand-in systemctl (firewall-flow.ts's hostTools): the failed one
// first, found by name, its facts a row away, and the list on a phone.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";

interface SystemServicesFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function systemServicesFlow(page: Page, { origin, step, shoot }: SystemServicesFlowOptions) {
  step("a server's system services: the failed one first, live, found by name, its facts a row away");
  await page.setViewportSize({ width: 1440, height: 900 });
  const link = await ownServerPath(page, origin);
  await page.goto(`${origin}${link}`);
  await page
    .getByRole("navigation", { name: /^Server / })
    .getByRole("link", { name: "System services" })
    .click();
  await page.waitForFunction((to) => window.location.pathname === to, `${link}/services`);
  const table = page.getByRole("table", { name: /^System services on / });
  await table.waitFor({ timeout: 20_000 });
  await page.getByText("Live", { exact: true }).waitFor();
  const first = table.getByRole("row").nth(1);
  await first.getByText("nginx.service").waitFor();
  await first.getByText(/exit-code, status 1/).waitFor();
  await table
    .getByRole("row", { name: /docker\.service/ })
    .getByText("Dockwell needs it")
    .waitFor();
  await shoot(page, "server-system-services");

  // Each waits for the view it asked for, as the filter applies a moment after typing.
  await page.getByRole("radio", { name: /^Running/ }).click();
  await table
    .getByText("nginx.service")
    .waitFor({ state: "detached" })
    .catch(() => fail("Running shows a failed unit"));
  await page.getByRole("radio", { name: /^All/ }).click();
  await page.getByRole("textbox", { name: "Filter system services" }).fill("key-value");
  await table.getByText("nginx.service").waitFor({ state: "detached" });
  await table.getByText("redis-server.service").waitFor();
  if ((await table.locator("tbody tr").count()) !== 1) fail("the search for key-value found more than the one unit");
  await page.getByRole("textbox", { name: "Filter system services" }).fill("");

  await table.getByRole("row", { name: /containerd\.service/ }).click();
  const sheet = page.getByRole("dialog", { name: "containerd.service" });
  await sheet.getByText("Main process").waitFor();
  await sheet.getByText("812", { exact: true }).waitFor();
  await shoot(page, "server-system-service-sheet");
  await page.keyboard.press("Escape");
  await sheet.waitFor({ state: "hidden" });

  step("on a phone, the system services are rows");
  await page.setViewportSize({ width: 360, height: 780 });
  await page.getByRole("list", { name: "System services" }).waitFor({ timeout: 15_000 });
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 360) fail(`the system services are ${wide} px wide on a 360 px phone`);
  await page.setViewportSize({ width: 1440, height: 900 });
}

/** Where sign-in.ts runs this among the flows, beside processes (flows.ts). */
export const flow: Flow = { order: 96, run: (page, run) => systemServicesFlow(page, run) };
