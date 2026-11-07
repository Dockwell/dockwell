// SPDX-License-Identifier: AGPL-3.0-only

// A server's Network section, on the page a person reads it on. What a real NAT
// does can't be shown here, as this runs on one machine: internal/control/fleet
// holds the sources to their order, and this walks what a person sees and does.

import type { Page } from "playwright-core";

interface NetworkFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

export async function networkFlow(page: Page, { origin, step, shoot }: NetworkFlowOptions) {
  step("a server's Network section keeps its facts apart, and takes its public address from a person");
  await page.goto(`${origin}/`);
  const link = await page.locator('a[href^="/servers/srv_"]').first().getAttribute("href");
  await page.goto(`${origin}${link}`);
  const network = page.getByRole("region", { name: "Network" });
  await network.getByText("Reachable", { exact: true }).waitFor();
  await network.getByText("Not checked", { exact: true }).waitFor();
  await shoot(page, "network-before");

  await network.getByRole("button", { name: /^(Set|Change)$/ }).click();
  const sheet = page.getByRole("dialog");
  const field = sheet.getByLabel("Public address");
  await field.fill("10.0.0.25");
  await sheet.getByRole("button", { name: "Set address" }).click();
  await sheet.getByText("10.0.0.25 is private, so the internet can't reach it there", { exact: false }).waitFor();
  await shoot(page, "network-refused");

  await field.fill("198.51.100.7");
  await sheet.getByRole("button", { name: "Set address" }).click();
  await sheet.waitFor({ state: "hidden" });
  await network.getByText("198.51.100.7", { exact: true }).waitFor();
  await network.getByText(/^Set by /).waitFor();
  // Setting an address says nothing of whether the internet can reach it.
  await network.getByText("Not checked", { exact: true }).waitFor();
  await shoot(page, "network-set");

  await page.goto(`${origin}/activity`);
  await page.getByText("Set the public address of", { exact: false }).first().waitFor();

  await page.goBack();
  await network.getByRole("button", { name: "Change" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Clear" }).click();
  await page.getByRole("dialog").waitFor({ state: "hidden" });
  await network.getByText("198.51.100.7", { exact: true }).waitFor({ state: "hidden" });
}
