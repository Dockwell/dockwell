// SPDX-License-Identifier: AGPL-3.0-only

// Backups end to end, on the fake Docker Engine of scripts/fakedocker: from the rail
// to a database's dump of what the catalog installed, then a restore through a plan
// that says what it replaces, and onto Activity. The same comes from an app's page.
// scripts/docker-e2e.sh proves a restore on real Docker.

import type { Page } from "playwright-core";

export interface BackupsFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function backupsFlow(page: Page, { origin, step, shoot }: BackupsFlowOptions) {
  step("backups: the rail leads to them, and nothing is taken yet");
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Backups" }).click();
  await page.getByRole("heading", { level: 1, name: "Backups" }).waitFor();
  await page.getByRole("heading", { level: 2, name: "No backups yet" }).waitFor();
  await shoot(page, "backups-empty");

  step("backups: a database's dump says it stops nothing, and its server says how it went");
  await page.getByRole("button", { name: "Back up now" }).click();
  const sheet = page.getByRole("dialog", { name: /^Back up / });
  // The catalog's flow leaves two called data, so each says whose it is.
  const volume = sheet.getByLabel("Volume");
  const database = volume.locator("option", { hasText: /^data \(.*postgres/i });
  await volume.selectOption((await database.getAttribute("value")) ?? fail("PostgreSQL's data isn't on offer"));
  await sheet.getByText(/uses PostgreSQL's own dump/).waitFor({ timeout: 20_000 });
  await shoot(page, "backups-sheet");
  await sheet.getByRole("button", { name: "Back up", exact: true }).click();
  const table = page.getByRole("table", { name: "Backups, newest first" });
  const row = table.getByRole("row").nth(1);
  await row.getByText("Done").waitFor({ timeout: 30_000 });
  await row.getByText("PostgreSQL dump").waitFor();
  await shoot(page, "backups-list");

  step("backups: a restore is a plan, which says what it replaces and what stops, and takes a confirmation");
  await row.getByRole("button", { name: "Restore" }).click();
  const plan = page.locator(".plan-sheet");
  const risk = plan.locator(".plan-risk");
  await risk.getByText(/is replaced with the dump from/).waitFor({ timeout: 30_000 });
  await risk.getByText(/stop while this happens|stops while this happens/).waitFor();
  await risk.getByText(/starts anew on an empty volume/).waitFor();
  await plan.getByText("replaced, and undoing this puts it back").waitFor();
  await shoot(page, "backups-restore-plan");
  const apply = plan.locator(".dw-sheet-foot").getByRole("button", { name: /Apply plan/ });
  if (!(await apply.isDisabled())) fail("a restore applies with no confirmation");
  const code = await plan.locator(".plan-gate code").innerText();
  await plan.locator(".dw-sheet-foot .plan-gate input").fill(code);
  await apply.click();
  await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 60_000 });
  await shoot(page, "backups-restored");
  await plan.getByRole("button", { name: "Done" }).click();

  step("backups: it's on Activity, with the restore as a change that can be undone");
  await page.goto(`${origin}/activity?kind=backups`);
  await page
    .getByRole("row", { name: /Backed up data/ })
    .first()
    .waitFor({ timeout: 15_000 });
  await page.goto(`${origin}/activity?kind=changes`);
  const restored = page.getByRole("row", { name: /Restore data/ }).first();
  await restored.click();
  await page.getByRole("button", { name: "Undo this change" }).waitFor({ timeout: 15_000 });

  step("backups: an app's page shows its newest, and takes another");
  // journal has a db too, so go by the app. A link that has no width can't be clicked,
  // which holds the list to showing its names with two servers at this width.
  await page.goto(`${origin}/apps?q=postgres`);
  await page.getByRole("table", { name: "Apps" }).getByRole("link", { name: "db", exact: true }).click();
  const panel = page.getByRole("region", { name: "Backups" });
  await panel.getByText("PostgreSQL dump").waitFor({ timeout: 15_000 });
  await panel.getByRole("button", { name: "Back up now" }).click();
  const again = page.getByRole("dialog", { name: "Back up data" });
  await again.getByText(/uses PostgreSQL's own dump/).waitFor({ timeout: 20_000 });
  await again.getByRole("button", { name: "Cancel" }).click();
  await shoot(page, "backups-app");

  step("backups: on a phone, they stack");
  await page.setViewportSize({ width: 360, height: 780 });
  await page.goto(`${origin}/backups`);
  await page.locator(".backup-phone").getByText("data").first().waitFor();
  await page.getByRole("link", { name: "Back" }).waitFor();
  await page.getByRole("button", { name: "Back up now" }).waitFor();
  if ((await page.evaluate(() => document.documentElement.scrollWidth)) > 360) {
    fail("the backups page scrolls sideways on a phone");
  }
  await shoot(page, "backups-phone");
  await page.setViewportSize({ width: 1280, height: 900 });
}
