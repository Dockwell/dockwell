// SPDX-License-Identifier: AGPL-3.0-only

// The Databases section end to end, on the fake Docker Engine of scripts/fakedocker:
// from the rail to every database, with what PostgreSQL said of itself when the
// agent asked it in its own container, onto one database's page with its password
// masked, who signs in to it, and the same on a phone. scripts/docker-e2e.sh reads
// real PostgreSQL, MySQL and Redis the same way.

import type { Page } from "playwright-core";

export interface DatabasesFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
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

export async function databasesFlow(page: Page, { origin, step, shoot }: DatabasesFlowOptions) {
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

  step("databases: a database's page masks its password, and says where it is");
  await row.click();
  await page.getByRole("heading", { level: 1, name: "db" }).waitFor();
  const url = page.getByLabel("Internal URL", { exact: true });
  const shown = await url.inputValue();
  if (shown !== "postgres://postgres:••••@db:5432/app") fail(`the internal URL reads ${shown}`);
  await page
    .getByText(/The password is in the secret/)
    .getByText("POSTGRES_PASSWORD")
    .waitFor();
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

  step("databases: on a phone, each is a row that opens its page");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/databases`);
  const phoneRow = page.locator(".databases-phone-row").filter({ hasText: "PostgreSQL 17.11" }).first();
  await phoneRow.waitFor({ timeout: 20_000 });
  await fitsItsWidth(page, "the databases list on a phone");
  await phoneRow.click();
  await page.getByLabel("Internal URL", { exact: true }).waitFor();
  await fitsItsWidth(page, "a database's page on a phone");
  await page.setViewportSize({ width: 1440, height: 900 });
}
