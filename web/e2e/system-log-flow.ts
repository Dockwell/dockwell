// SPDX-License-Identifier: AGPL-3.0-only

// A server's system log (ADR 0067), on a real control plane whose own agent reads
// its journal with a stand-in journalctl (firewall-flow.ts's hostTools): the newest
// lines, then a line as the journal writes it, narrowed to a unit picked from the
// server's own, searched, opened from a unit's sheet, and on a phone.

import { appendFileSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";

function fail(message: string): never {
  throw new Error(message);
}

export async function systemLogFlow(
  page: Page,
  { origin, step, shoot, env }: Pick<FlowRun, "origin" | "step" | "shoot" | "env">,
) {
  step("a server's system log: its newest lines, then what the journal writes as it comes");
  await page.setViewportSize({ width: 1440, height: 900 });
  const link = await ownServerPath(page, origin);
  await page.goto(`${origin}${link}`);
  await page
    .getByRole("navigation", { name: /^Server / })
    .getByRole("link", { name: "System log" })
    .click();
  await page.waitForFunction((to) => window.location.pathname === to, `${link}/log`);
  const log = page.getByRole("region", { name: "System log" });
  await log.getByText(/unknown directive in \/etc\/nginx/).waitFor({ timeout: 20_000 });
  await page.getByText("Live", { exact: true }).first().waitFor({ timeout: 20_000 });
  await shoot(page, "server-system-log");
  const tools = env["DOCKWELL_TEST_TOOLS"];
  if (tools) {
    // The follow opens a moment after the newest page: a line written then comes live.
    await page.waitForTimeout(1_500);
    appendFileSync(
      join(tools, "var", "lib", "stand", "follow"),
      `${JSON.stringify({ __CURSOR: "e2e-live", __REALTIME_TIMESTAMP: String(Date.now() * 1000), _SYSTEMD_UNIT: "cron.service", _PID: "790", PRIORITY: "6", MESSAGE: "cron: ran the hourly jobs" })}\n`,
    );
    await log
      .getByText("cron: ran the hourly jobs")
      .waitFor({ timeout: 20_000 })
      .catch(() => fail("a line the journal wrote didn't come live"));
  }

  step("narrowed to a unit picked from the server's own, and searched");
  const picker = page.getByRole("combobox", { name: "System service" });
  await picker.locator("option", { hasText: "nginx.service" }).first().waitFor({ state: "attached", timeout: 20_000 });
  await picker.selectOption("nginx.service");
  await page.waitForFunction(() => new URL(window.location.href).searchParams.get("unit") === "nginx.service");
  await page.getByRole("textbox", { name: "Search the system log" }).fill("test failed");
  await page.waitForFunction(() => new URL(window.location.href).searchParams.get("q") === "test failed");
  await log.getByText(/configuration file/).waitFor({ timeout: 20_000 });
  if ((await log.getByText(/unknown directive/).count()) !== 0) fail("the search shows a line that doesn't hold it");
  await shoot(page, "server-system-log-search");

  step("a unit's sheet opens its system log");
  await page.goto(`${origin}${link}/services`);
  const table = page.getByRole("table", { name: /^System services on / });
  await table.getByRole("row", { name: /nginx\.service/ }).click();
  const sheet = page.getByRole("dialog", { name: "nginx.service" });
  await sheet.getByRole("link", { name: "Open in the system log" }).click();
  await page.waitForFunction(
    (to) =>
      window.location.pathname === to && new URL(window.location.href).searchParams.get("unit") === "nginx.service",
    `${link}/log`,
  );
  await page.getByRole("combobox", { name: "System service" }).waitFor();
  if ((await page.getByRole("combobox", { name: "System service" }).inputValue()) !== "nginx.service") {
    fail("the system log didn't open narrowed to nginx.service");
  }

  step("on a phone, the system log's filters wrap above it");
  await page.setViewportSize({ width: 390, height: 844 });
  await log
    .getByText(/unknown directive/)
    .first()
    .waitFor({ timeout: 20_000 });
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 390) fail(`the system log is ${wide} px wide on a 390 px phone`);
  await shoot(page, "server-system-log-phone");
  await page.setViewportSize({ width: 1440, height: 900 });
}

/** Where sign-in.ts runs this among the flows, beside the system services. */
export const flow: Flow = { order: 96, run: (page, run) => systemLogFlow(page, run) };
