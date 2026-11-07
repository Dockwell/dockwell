// SPDX-License-Identifier: AGPL-3.0-only

// Backups end to end, on the fake Docker Engine of scripts/fakedocker: from the rail
// to a database's dump of what the catalog installed, then a restore through a plan
// that says what it replaces, and onto Activity. The same comes from an app's page.
// scripts/docker-e2e.sh proves a restore on real Docker.

import type { Locator, Page } from "playwright-core";

import type { Flow } from "./flows.ts";

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

/**
 * Chooses PostgreSQL's volume in a sheet that opens on the first by name, which a
 * run with other apps makes one with nothing in it. The catalog's flow leaves two
 * called data, so each says whose it is.
 */
async function pickTheDatabase(sheet: Locator) {
  const volume = sheet.getByLabel("Volume");
  const database = volume.locator("option", { hasText: /^data \(.*postgres/i });
  await volume.selectOption((await database.getAttribute("value")) ?? fail("PostgreSQL's data isn't on offer"));
}

export async function backupsFlow(page: Page, { origin, step, shoot }: BackupsFlowOptions) {
  step("backups: the rail leads to them, with what the catalog's upgrade took");
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Backups" }).click();
  await page.getByRole("heading", { level: 1, name: "Backups" }).waitFor();
  await page.getByRole("table", { name: "Backups, newest first" }).getByRole("row").nth(1).waitFor();

  step("backups: a database's dump says it stops nothing, and its server says how it went");
  await page.getByRole("button", { name: "Back up now" }).click();
  const sheet = page.getByRole("dialog", { name: /^Back up / });
  await pickTheDatabase(sheet);
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

  step("backups: a schedule says what it costs, and is a plan");
  await page.goto(`${origin}/backups`);
  const schedules = page.getByRole("region", { name: "Schedules" });
  await schedules.getByRole("button", { name: "Add" }).click();
  const schedule = page.getByRole("dialog", { name: "Back up on a schedule" });
  await pickTheDatabase(schedule);
  await schedule.getByText(/takes about/).waitFor({ timeout: 20_000 });
  await shoot(page, "backups-schedule");
  await schedule.getByRole("button", { name: "Review plan" }).click();
  const planned = page.locator(".plan-sheet");
  await planned.getByRole("button", { name: /Apply plan/ }).click();
  await planned.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
  await planned.getByRole("button", { name: "Done" }).click();
  await schedules.getByText("Daily at 03:00").waitFor({ timeout: 15_000 });
  await schedules.getByText("Hasn't run yet").waitFor();

  step("backups: a destination asks for what signs in, keeps a passphrase out of the way, and the plan shows neither");
  const destinations = page.getByRole("region", { name: "Destinations" });
  await destinations.getByRole("button", { name: "Add" }).click();
  const adding = page.getByRole("dialog", { name: "Add a destination" });
  const signsIn = "e2e-secret-that-signs-in";
  await adding.getByLabel("Name").fill("e2e-bucket");
  await adding.getByLabel("Endpoint").fill("http://127.0.0.1:1"); // nothing answers there
  await adding.getByLabel("Bucket").fill("backups");
  await adding.getByLabel("Access key").fill("e2e-access-key");
  await adding.getByLabel("Secret key").fill(signsIn);
  const review = adding.getByRole("button", { name: "Review plan" });
  if (!(await review.isDisabled())) fail("a destination goes ahead with no word that its passphrase is kept");
  const passphrase = (await adding.locator(".backup-passphrase span").allInnerTexts()).join("");
  if (passphrase.length < 32) fail(`a destination's passphrase reads as ${passphrase.length} characters`);
  // Copied by hand, it has to be what the button copies, with no line break in it.
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  const selected = await adding.locator(".backup-passphrase").evaluate(async (code) => {
    const selection = window.getSelection();
    selection?.selectAllChildren(code);
    document.execCommand("copy");
    selection?.removeAllRanges();
    return navigator.clipboard.readText();
  });
  await adding.getByRole("button", { name: "Copy the passphrase" }).click();
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  if (selected !== copied || copied !== passphrase) {
    fail(`a passphrase selected by hand reads ${JSON.stringify(selected)}, and Copy gives ${JSON.stringify(copied)}`);
  }
  await adding.getByLabel("I've kept a copy of the passphrase").check();
  await shoot(page, "backups-destination");
  await review.click();
  const sending = page.locator(".plan-sheet");
  const applying = sending.getByRole("button", { name: /Apply plan/ });
  await applying.waitFor({ timeout: 15_000 });
  const shown = await sending.innerText();
  if (shown.includes(signsIn) || shown.includes(passphrase)) fail("a destination's plan shows what signs in");
  await applying.click();
  await sending.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
  await sending.getByRole("button", { name: "Done" }).click();
  await destinations.getByText("e2e-bucket").waitFor({ timeout: 15_000 });
  await shoot(page, "backups-destinations");

  step("backups: a copy that can't get there says why on its row, and the row keeps its buttons");
  await page.setViewportSize({ width: 1024, height: 800 });
  await page.goto(`${origin}/backups`);
  await page.getByRole("button", { name: "Back up now" }).click();
  const toStorage = page.getByRole("dialog", { name: /^Back up / });
  await pickTheDatabase(toStorage);
  await toStorage.getByText(/uses PostgreSQL's own dump/).waitFor({ timeout: 20_000 });
  await toStorage.getByLabel("Send a copy to").selectOption("e2e-bucket");
  await toStorage.getByRole("button", { name: "Back up", exact: true }).click();
  const noCopy = page.getByRole("row").filter({ hasText: "It didn't reach e2e-bucket" }).first();
  await noCopy.waitFor({ timeout: 30_000 });
  await noCopy.getByText(/Check the destination's endpoint/).waitFor();
  const edge = await noCopy.getByRole("button", { name: "Delete this backup" }).boundingBox();
  if (!edge || edge.x + edge.width > 1024) fail("a long copy error pushes a backup's buttons off the page");
  await shoot(page, "backups-copy-failed");
  await page.setViewportSize({ width: 1280, height: 900 });

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

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 50, run: (page, run) => backupsFlow(page, run) };
