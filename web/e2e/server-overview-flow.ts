// SPDX-License-Identifier: AGPL-3.0-only

// A server's Overview, as Server-Overview.dc.html has it, on a real control plane and
// its own agent: live numbers with their trends, what runs on the server as links,
// what takes its disk, what it is, how it meets the network, and its agent. What
// takes the disk comes from the fake Docker Engine's /system/df, added up on the disk
// that holds Docker's data, which sign-in.ts puts on the checkout's disk. On a real
// Docker whose data isn't on a data disk, the panel draws each disk's used and free
// space instead, as it does while Docker can't say.

import type { Page } from "playwright-core";

interface ServerOverviewFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
  /** Docker's data is on a data disk, so the panel must draw it part by part. */
  breakdown: boolean;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function serverOverviewFlow(page: Page, { origin, step, shoot, breakdown }: ServerOverviewFlowOptions) {
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
  if (breakdown && !broken) fail(`the disk Docker's data is on isn't drawn part by part: ${await disk.innerText()}`);
  if (broken) {
    // Every part, the bar that draws them, and the disk they're of. A server with
    // another disk, as CI's runners have /mnt, draws that one's used and free
    // beside it, Free and all, so this looks inside the breakdown alone.
    const docker = disk.locator(".disk-use-docker");
    const drawn = await docker.count();
    if (drawn !== 1) fail(`Docker's disk is drawn ${drawn} times: ${await disk.innerText()}`);
    for (const part of ["Images", "Volumes", "Free"]) await docker.getByText(part, { exact: true }).waitFor();
    const bar = await docker
      .getByRole("img", { name: /^Disk usage of \/.*: Images .+, Volumes .+, Free .+, of .+$/ })
      .count();
    if (bar !== 1) fail(`the disk's bar doesn't name its parts: ${await disk.innerHTML()}`);
    const images = await docker.getByText(/^\d+ images?( · .+)?$/).count();
    if (images === 0) fail(`the images part doesn't count the fake Docker's images: ${await disk.innerText()}`);
  }
  step(broken ? "  Docker's disk, part by part" : "  each disk's used and free, as Docker's data isn't on a data disk");
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

  step("on a phone it leads with how the server is, what runs there, and what happened, and fits the narrowest");
  await page.setViewportSize({ width: 390, height: 844 });
  // The phone's page comes once the page has seen it's on a phone.
  const status = page.getByRole("region", { name: "Status" });
  await status.waitFor();
  await status.getByRole("meter", { name: "CPU" }).waitFor();
  await page.getByRole("region", { name: "Runs here" }).waitFor();
  const recent = page.getByRole("region", { name: "Recent" });
  await recent.locator("li").first().waitFor({ timeout: 15_000 });
  await shoot(page, "server-overview-phone");
  await page.setViewportSize({ width: 360, height: 780 });
  await status.waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 360) fail(`the server's page is ${wide} px wide on a 360 px phone`);
  await page.setViewportSize({ width: 1440, height: 900 });
}
