// SPDX-License-Identifier: AGPL-3.0-only

// A server's Overview, as Server-Overview.dc.html has it, on a real control plane and
// its own agent: live numbers with their trends, what runs on the server as links,
// what takes its disk, what it is, how it meets the network, and its agent. What
// takes the disk comes from the fake Docker Engine's /system/df, added up on the disk
// that holds Docker's data, which a machine whose / isn't a data disk hasn't got: then
// the panel draws each disk's used and free space, as it does while Docker can't say.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

interface ServerOverviewFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function serverOverviewFlow(page: Page, { origin, step, shoot }: ServerOverviewFlowOptions) {
  step("a server's Overview: live numbers, what runs here, what takes the disk, and what the server is");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/`);
  const link = await page.locator('a[href^="/servers/srv_"]').first().getAttribute("href");
  if (!link) fail("the overview links to no server");
  await page.goto(`${origin}${link}`);
  await page.locator(".dw-metric", { hasText: "Load average" }).waitFor({ timeout: 15_000 });
  await page
    .locator(".dw-metric", { hasText: "CPU" })
    .getByText(/on average over the last|Across \d+ vCPU/)
    .waitFor();

  const runs = page.getByRole("region", { name: "What runs here" });
  const apps = runs.getByRole("link", { name: /^(\d+ apps?|No apps)/ });
  await apps.waitFor();
  const domains = runs.getByRole("link", { name: /^(\d+ domains?|No domains)/ });
  await domains.waitFor();
  // Once it has read how they run, the card says so, never only that it is checking.
  await runs
    .getByText(/healthy|running|unknown|deploying|failed|not running|paused|starting|need|Deploy one/)
    .first()
    .waitFor({ timeout: 30_000 });

  const disk = page.getByRole("region", { name: "Disk usage" });
  await disk.waitFor();
  await disk
    .getByText("Images", { exact: true })
    .or(disk.getByText(/% of .+ used$/).first())
    .first()
    .waitFor({ timeout: 70_000 });
  const broken = (await disk.getByText("Images", { exact: true }).count()) > 0;
  step(broken ? "  Docker's disk, part by part" : "  each disk's used and free, as / here isn't a data disk");
  await page.getByRole("region", { name: "System" }).getByText("Swap", { exact: true }).waitFor();
  await page.getByRole("region", { name: "Network" }).getByText("Traffic", { exact: true }).waitFor();
  await page.getByRole("region", { name: "Agent" }).getByText("Memory", { exact: true }).waitFor();
  await shoot(page, "server-overview");

  step("each card in What runs here opens its list");
  const to = await apps.getAttribute("href");
  await apps.click();
  // The panel moves without loading a page, so this waits for where it went.
  // With more than one server, the list it opens is filtered to this one, in its query.
  await page.waitForFunction((to) => window.location.pathname + window.location.search === to, to);
  await page.goBack();
  await runs.waitFor();

  step("the Overview fits the narrowest phone");
  await page.setViewportSize({ width: 360, height: 780 });
  // The numbers go two by two once the page has seen it's on a phone.
  await page.locator(".metrics.is-compact").waitFor();
  await runs.waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 360) fail(`the Overview is ${wide} px wide on a 360 px phone`);
  await page.setViewportSize({ width: 1440, height: 900 });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 85, run: (page, run) => serverOverviewFlow(page, run) };
