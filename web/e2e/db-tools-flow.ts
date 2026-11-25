// SPDX-License-Identifier: AGPL-3.0-only

// Database daily tools end to end, on the fake Docker Engine of scripts/fakedocker:
// one-click console from a database's page and row menu, row menu essentials
// (Back up now, Copy connection string, Open console), several logical databases
// in one SQL engine (Databases tab, New database with default and existing owner,
// Delete), and importing a dump file as a plan that backs the database up first.

import { mkdirSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

async function fitsItsWidth(page: Page, what: string) {
  const edge = await pastTheEdge(page);
  const width = page.viewportSize()?.width ?? 0;
  if (edge.wider > 0) fail(`${what} scrolls sideways at ${width} px, by ${edge.wider} px: ${edge.what}`);
}

async function applyPlan(page: Page) {
  const dialog = onTop(page.getByRole("dialog", { name: "Review plan" }));
  const apply = dialog.getByRole("button", { name: /Apply plan/ });
  await apply.waitFor({ timeout: 30_000 });
  const gate = dialog.locator(".plan-gate");
  if ((await gate.count()) > 0 && (await gate.isVisible())) {
    const code = (await gate.locator("code").first().innerText()).trim();
    await gate.locator("input").fill(code);
  }
  await apply.click();
  const done = page.getByRole("button", { name: "Done", exact: true });
  await done.waitFor({ timeout: 60_000 });
  await done.click();
}

export async function dbToolsFlow(page: Page, { origin, step, shoot }: FlowRun) {
  const shotsDir = process.env["DB_TOOLS_SHOTS_DIR"] ?? "";
  if (shotsDir) mkdirSync(shotsDir, { recursive: true });
  const snap = async (name: string) => {
    await shoot(page, name);
    if (shotsDir) {
      await page.screenshot({ path: join(shotsDir, `${name}.png`), fullPage: false });
    }
  };

  step("db-tools: row menu has Open console, Back up now, Copy connection string, and Databases");
  await atWidth(page, { width: 1440, height: 900 });
  await page.goto(`${origin}/databases`);
  const table = page.getByRole("table", { name: "Databases" });
  const row = table.getByRole("row").filter({ hasText: "adminer" }).first();
  await row.waitFor({ timeout: 20_000 });
  await row.getByRole("button", { name: /^Actions for / }).click();
  const menu = page.getByRole("menu", { name: /^Actions for / });
  await menu.getByRole("menuitem", { name: "Open console" }).waitFor();
  await menu.getByRole("menuitem", { name: "Back up now" }).waitFor();
  await menu.getByRole("menuitem", { name: "Copy connection string" }).waitFor();
  await menu.getByRole("menuitem", { name: "Databases" }).waitFor();
  await snap("databases-row-menu-1440");

  // Back up now opens BackUpSheet directly from the row menu.
  await menu.getByRole("menuitem", { name: "Back up now" }).click();
  const backupSheet = page.getByRole("dialog", { name: /^Back up / });
  await backupSheet.waitFor({ timeout: 15_000 });
  await backupSheet.getByRole("button", { name: "Cancel" }).click();
  await backupSheet.waitFor({ state: "hidden" });

  // Copy connection string steps up with passkey and copies or offers Copy toast.
  await row.getByRole("button", { name: /^Actions for / }).click();
  await page.getByRole("menuitem", { name: "Copy connection string" }).click();
  await page.getByText(/Connection string copied|Ready to copy/).waitFor({ timeout: 20_000 });

  step("db-tools: Open console lands on Terminal with container and console query params");
  await row.getByRole("button", { name: /^Actions for / }).click();
  await page.getByRole("menuitem", { name: "Open console" }).click();
  await page.waitForURL(/\/servers\/[^/]+\/terminal\?.*console=true/, { timeout: 20_000 });
  await snap("database-console-terminal-1440");

  step(
    "db-tools: Databases tab lists logical databases and creates one with a new owner and one with an existing owner",
  );
  await page.goto(`${origin}/databases`);
  const rowAgain = page
    .getByRole("table", { name: "Databases" })
    .getByRole("row")
    .filter({ hasText: "adminer" })
    .first();
  await rowAgain.waitFor({ timeout: 20_000 });
  await rowAgain.click();
  await page.getByRole("link", { name: "Open console" }).waitFor({ timeout: 20_000 });
  const dbPath = new URL(page.url()).pathname;

  await page
    .getByRole("navigation", { name: "Database sections" })
    .getByRole("link", { name: /Databases/ })
    .click();
  const schemasTable = page.getByRole("table", { name: /Databases in / });
  await schemasTable.waitFor({ timeout: 20_000 });
  await fitsItsWidth(page, "the Databases tab");
  await snap("database-schemas-1440");

  // Create analytics with default new owner user + secret.
  await page.getByRole("button", { name: "New database" }).click();
  const newDialog = page.getByRole("dialog", { name: /New database in / });
  await newDialog.getByLabel("Database", { exact: true }).fill("analytics");
  await snap("database-new-sheet-1440");
  await newDialog.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page);
  await schemasTable.getByText("analytics", { exact: true }).first().waitFor({ timeout: 30_000 });

  // Create reports owned by existing user analytics.
  await page.getByRole("button", { name: "New database" }).click();
  const secondDialog = page.getByRole("dialog", { name: /New database in / });
  await secondDialog.getByLabel("Database", { exact: true }).fill("reports");
  await secondDialog.getByLabel("Owner", { exact: true }).selectOption("analytics");
  await secondDialog.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page);
  await schemasTable.getByText("reports", { exact: true }).first().waitFor({ timeout: 30_000 });
  await snap("database-schemas-multi-1440");

  step("db-tools: Import dump uploads in chunks, backs up first in the plan, and applies");
  const reportsRow = schemasTable.getByRole("row").filter({ hasText: "reports" }).first();
  await reportsRow.getByRole("button", { name: "Actions for reports" }).click();
  await page.getByRole("menuitem", { name: "Import dump" }).click();
  const importDialog = page.getByRole("dialog", { name: /Import dump into / });
  await importDialog.waitFor({ timeout: 15_000 });
  await importDialog.getByLabel("Dump file").setInputFiles({
    name: "seed-reports.sql",
    mimeType: "application/sql",
    buffer: Buffer.from("CREATE TABLE e2e_reports (id int);\nINSERT INTO e2e_reports VALUES (1);\n"),
  });
  await snap("database-import-sheet-1440");
  await importDialog.getByRole("button", { name: "Upload and review plan" }).click();
  const importPlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await importPlan.getByText(/Imports seed-reports\.sql into reports/).waitFor({ timeout: 30_000 });
  await importPlan.getByText(/backed up first/).waitFor();
  await snap("database-import-plan-1440");
  await applyPlan(page);

  step("db-tools: Delete logical database drops reports through a plan while keeping analytics");
  await schemasTable
    .getByRole("row")
    .filter({ hasText: "reports" })
    .first()
    .getByRole("button", { name: "Actions for reports" })
    .click();
  await page.getByRole("menuitem", { name: "Delete" }).click();
  await applyPlan(page);
  await page.waitForFunction(
    () => {
      const t = document.querySelector('table[aria-label^="Databases in "]');
      return t !== null && !t.textContent?.includes("reports");
    },
    null,
    { timeout: 30_000 },
  );
  await schemasTable.getByText("analytics", { exact: true }).first().waitFor();

  step("db-tools: phone layout fits without sideways scroll");
  await atWidth(page, { width: 390, height: 844 });
  await page.goto(`${origin}/databases`);
  await page.locator(".databases-phone-row").first().waitFor({ timeout: 20_000 });
  await fitsItsWidth(page, "databases list on phone");
  await snap("databases-list-390");

  await page.goto(`${origin}${dbPath}/databases`);
  await page.locator(".databases-phone-row").first().waitFor({ timeout: 20_000 });
  await fitsItsWidth(page, "database schemas tab on phone");
  await snap("database-schemas-390");

  await atWidth(page, { width: 1440, height: 900 });
}

export const flow: Flow = { order: 56, run: (page, run) => dbToolsFlow(page, run) };
