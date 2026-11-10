// SPDX-License-Identifier: AGPL-3.0-only

// The Databases section end to end, on the fake Docker Engine of scripts/fakedocker:
// from the rail to every database, with what PostgreSQL said of itself when the
// agent asked it in its own container, onto one database's page with its password
// masked, who signs in to it, its backups and a restore of one, an upgrade the catalog
// offers, and the same on a phone. scripts/docker-e2e.sh reads real PostgreSQL, MySQL
// and Redis the same way.

import { readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

export interface DatabasesFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  /** The directory the control plane reads more entries from, for a release newer than the built-in one. */
  catalogDir: string;
}

function fail(message: string): never {
  throw new Error(message);
}

/** Fails if the page scrolls sideways at its width. */
async function fitsItsWidth(page: Page, what: string) {
  const { scroll, width } = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    width: window.innerWidth,
  }));
  if (scroll > width) fail(`${what} scrolls sideways at ${width} px: it's ${scroll} px wide`);
}

export async function databasesFlow(page: Page, { origin, step, shoot, catalogDir }: DatabasesFlowOptions) {
  step("databases: the rail leads to them, with what the catalog's PostgreSQL says of itself");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Databases" }).click();
  await page.getByRole("heading", { level: 1, name: "Databases" }).waitFor();
  if (new URL(page.url()).pathname !== "/databases") fail(`the rail's Databases opens ${page.url()}`);
  const table = page.getByRole("table", { name: "Databases" });
  // The catalog's flow upgraded its install, so its PostgreSQL is the one with Adminer beside it.
  const row = table.getByRole("row").filter({ hasText: "adminer" }).first();
  await row.waitFor({ timeout: 20_000 });
  await row.getByText("PostgreSQL 17.11").waitFor({ timeout: 20_000 });
  await row.getByText("3 of 100").waitFor();
  const engines = page.getByRole("list", { name: "Engines" });
  await engines.getByRole("link", { name: /PostgreSQL/ }).waitFor();
  await page.getByRole("region", { name: "Backups" }).getByText("Last run").waitFor();
  await fitsItsWidth(page, "the databases list");
  await shoot(page, "databases-list");

  step("databases: a database's page lists what to connect with, its password masked until a passkey reveals it");
  await row.click();
  await page.getByRole("heading", { level: 1, name: "db" }).waitFor();
  const connect = page.getByRole("region", { name: "Connect" });
  await connect.getByText("postgres://postgres:••••••••••••@db:5432/app", { exact: true }).waitFor();
  await connect.getByText("Kept in the secret POSTGRES_PASSWORD").waitFor();
  // The virtual authenticator answers the passkey prompt, as Touch ID would.
  await connect.getByRole("button", { name: "Reveal password" }).click();
  await connect.getByRole("button", { name: "Copy password" }).waitFor({ timeout: 20_000 });
  const revealed = (await connect.locator(".dw-conn-code.is-revealed").innerText()).trim();
  if (!/^[A-Za-z0-9]{32}$/.test(revealed)) fail(`the revealed password reads ${revealed.length} characters`);
  await connect.getByText(`postgres://postgres:${revealed}@db:5432/app`, { exact: true }).waitFor();
  await connect.getByRole("button", { name: "Hide" }).click();
  if ((await page.content()).includes(revealed)) fail("the page keeps the password after it's hidden");

  step("databases: changing the password is a plan, with a new one generated in the database and its secret at once");
  await connect.getByRole("button", { name: "Change password" }).click();
  const changing = page.getByRole("dialog", { name: "Review plan" });
  await changing.getByRole("button", { name: /Apply plan/ }).waitFor({ timeout: 30_000 });
  await shoot(page, "database-change-password-plan");
  await changing.getByRole("button", { name: /Apply plan/ }).click();
  const changed = page.getByRole("button", { name: "Done", exact: true });
  await changed.waitFor({ timeout: 60_000 });
  await changed.click();
  await connect.getByRole("button", { name: "Reveal password" }).click();
  await connect.getByRole("button", { name: "Copy password" }).waitFor({ timeout: 20_000 });
  const renewed = (await connect.locator(".dw-conn-code.is-revealed").innerText()).trim();
  if (!/^[A-Za-z0-9]{32}$/.test(renewed) || renewed === revealed)
    fail("changing the password left the secret as it was");
  await connect.getByRole("button", { name: "Hide" }).click();
  await page.getByText("99.2").waitFor();
  await page.getByText(/PostgreSQL's own dump, while it runs/).waitFor();
  const users = page.getByRole("table", { name: "Users of db" });
  await users.getByRole("row").filter({ hasText: "Can do anything" }).waitFor();
  await fitsItsWidth(page, "a database's page");
  await shoot(page, "database-detail");

  step("databases: its users have a tab of their own");
  await page.getByRole("navigation", { name: "Database sections" }).getByRole("link", { name: /Users/ }).click();
  await page.getByRole("table", { name: "Databases in db" }).getByText("app", { exact: true }).first().waitFor();
  await shoot(page, "database-users");

  step("databases: a user with a database of their own is a plan, with a password no one typed");
  await page.getByRole("button", { name: "Add a user" }).click();
  const adding = page.getByRole("dialog", { name: "Add a user to db" });
  await adding.getByLabel("User", { exact: true }).fill("orders");
  await shoot(page, "database-add-user");
  await adding.getByRole("button", { name: /Review plan/ }).click();
  const planned = page.getByRole("dialog", { name: "Review plan" });
  await planned.getByRole("button", { name: /Apply plan/ }).waitFor({ timeout: 30_000 });
  await shoot(page, "database-add-user-plan");
  const shownPlan = await planned.innerText();
  if (!shownPlan.includes("orders")) fail(`the plan for a user doesn't name them: ${shownPlan.slice(0, 400)}`);
  if (/[A-Za-z0-9]{32}/.test(shownPlan.replace(/ORDERS_DATABASE_PASSWORD|chg_[A-Z0-9]+|sha256:[0-9a-f]+/g, ""))) {
    fail("the plan for a user shows what may be its password");
  }
  await planned.getByRole("button", { name: /Apply plan/ }).click();
  const done = page.getByRole("button", { name: "Done", exact: true });
  await done.waitFor({ timeout: 60_000 });
  await done.click();
  const orders = page
    .getByRole("table", { name: "Users of db" })
    .getByRole("row")
    .filter({ hasText: "ORDERS_DATABASE_PASSWORD" });
  await orders.waitFor({ timeout: 30_000 });
  await page.getByRole("table", { name: "Databases in db" }).getByText("orders", { exact: true }).first().waitFor();
  await shoot(page, "database-user-added");

  step("databases: its backups are a tab, where one is taken and one restores through a plan");
  await page.getByRole("navigation", { name: "Database sections" }).getByRole("link", { name: "Backups" }).click();
  const backups = page.getByRole("table", { name: "Backups of db, newest first" });
  await backups.getByRole("row").filter({ hasText: "PostgreSQL dump" }).first().waitFor({ timeout: 15_000 });
  const before = await backups.getByRole("row").count();
  await page.getByRole("button", { name: "Back up now" }).first().click();
  const sheet = page.getByRole("dialog", { name: /^Back up / });
  await sheet.getByText(/uses PostgreSQL's own dump/).waitFor({ timeout: 20_000 });
  await sheet.getByRole("button", { name: "Back up", exact: true }).click();
  const deadline = Date.now() + 30_000;
  while ((await backups.getByRole("row").count()) <= before) {
    if (Date.now() > deadline) fail("the backup taken from a database's page isn't on its list");
    await page.waitForTimeout(250);
  }
  await backups.getByRole("row").nth(1).getByText("Done").waitFor({ timeout: 30_000 });
  await shoot(page, "database-backups");
  await backups.getByRole("row").nth(1).getByRole("button", { name: "Restore" }).click();
  const plan = page.locator(".plan-sheet");
  await plan
    .locator(".plan-risk")
    .getByText(/is replaced with the dump from/)
    .waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: "Cancel" }).click();
  await plan.waitFor({ state: "hidden" });

  step("databases: a newer release of its entry is an upgrade, a plan that backs it up first");
  const entry = join(catalogDir, "postgres.yaml");
  const builtIn = readFileSync("internal/catalog/entries/postgres.yaml", "utf8");
  const newer = builtIn
    .replace('version: "17.11"', 'version: "17.12"')
    .replace(/^release: (\d+)$/m, (_, current: string) => `release: ${Number(current) + 1}`)
    .replace(/postgres:17\.11@sha256:[0-9a-f]{64}/, `postgres:17.12@sha256:${"3".repeat(64)}`);
  if (newer === builtIn || !newer.includes("postgres:17.12@"))
    fail("the newer release of PostgreSQL reads the same as the built-in one");
  writeFileSync(entry, newer);
  try {
    await page.getByRole("navigation", { name: "Database sections" }).getByRole("link", { name: "Overview" }).click();
    await page.reload();
    await page.getByText("PostgreSQL 17.12 is out").waitFor({ timeout: 20_000 });
    await shoot(page, "database-upgrade");
    await page.getByRole("button", { name: "Upgrade to 17.12" }).click();
    await plan.getByText("Its volumes are backed up first").waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Cancel" }).click();
    await plan.waitFor({ state: "hidden" });
  } finally {
    rmSync(entry, { force: true });
  }

  step("databases: on a phone, each is a row that opens its page");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/databases`);
  const phoneRow = page.locator(".databases-phone-row").filter({ hasText: "PostgreSQL 17.11" }).first();
  await phoneRow.waitFor({ timeout: 20_000 });
  await fitsItsWidth(page, "the databases list on a phone");
  await phoneRow.click();
  const phoneConnect = page.getByRole("region", { name: "Connect" });
  const masked = phoneConnect.locator(".dw-conn-code.is-masked");
  await masked.waitFor({ timeout: 20_000 });
  await fitsItsWidth(page, "a database's page on a phone");
  // The masked password keeps to one line beside its actions, rather than a dot to a line.
  const tall = await masked.evaluate((el) => el.getBoundingClientRect().height);
  if (tall > 28) fail(`the masked password on a phone is ${Math.round(tall)} px tall, more than one line`);
  await page.setViewportSize({ width: 1440, height: 900 });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 55, run: (page, run) => databasesFlow(page, run) };
