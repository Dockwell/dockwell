// SPDX-License-Identifier: AGPL-3.0-only

// New app offers the catalog first (#617): the top bar's New is a short menu, an app
// opens New app on the catalog with the apps most people start with, words find
// others, a pick turns the sheet into that app's install form, Back keeps the words,
// and the install goes through its plan to the app.

import type { Locator, Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";

function fail(message: string): never {
  throw new Error(message);
}

/** Runs it on the first server that's online, where the form asks. */
async function chooseServer(sheet: Locator) {
  const servers = sheet.getByRole("group", { name: "Runs on" });
  if ((await servers.count()) === 0) return;
  await servers.locator("label", { hasNotText: "Offline" }).first().getByRole("checkbox").check();
}

export async function newAppCatalogFlow(page: Page, { origin, step, shoot }: FlowRun) {
  step("new app: the top bar's New is a menu of an app, a database, a domain and a server");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/apps`);
  await page.getByRole("banner").getByRole("button", { name: "New" }).click();
  const menu = page.getByRole("menu", { name: "New" });
  const items = await menu.getByRole("menuitem").allTextContents();
  if (items.join(",") !== "App,Database,Domain,Server") fail(`New offers ${items.join(", ")}`);
  await menu.getByRole("menuitem", { name: "App" }).click();

  step("new app: it opens on the catalog, with the apps most people start with first");
  const sheet = page.getByRole("dialog", { name: "New app" });
  await sheet.waitFor();
  if (!(await sheet.getByRole("radio", { name: "Catalog" }).isChecked())) fail("New app doesn't open on the catalog");
  const featured = sheet.getByRole("region", { name: "Featured" });
  await featured.waitFor();
  const first = await featured.locator(".new-app-entry-name").first().textContent();
  if (first !== "WordPress") fail(`the first featured app is ${first}`);
  await shoot(page, "new-app-catalog");

  step("new app: words find an app, and picking it turns the sheet into its install form");
  await sheet.getByRole("textbox", { name: "Search the catalog" }).fill("memos");
  await sheet.getByRole("button", { name: /^Memos/ }).click();
  const install = page.getByRole("dialog", { name: "Install Memos" });
  await install.waitFor();
  const at = new URL(page.url());
  if (at.searchParams.get("entry") !== "memos" || at.searchParams.get("q") !== "memos")
    fail(`picking Memos left the address at ${page.url()}`);
  await shoot(page, "new-app-install");

  step("new app: Back comes to the search with its words, and the pick installs through its plan");
  await install.getByRole("button", { name: "Back" }).click();
  await sheet.waitFor();
  if ((await sheet.getByRole("textbox", { name: "Search the catalog" }).inputValue()) !== "memos")
    fail("Back lost the words");
  await sheet.getByRole("button", { name: /^Memos/ }).click();
  await install.waitFor();
  await chooseServer(install);
  await install.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const open = page.getByRole("button", { name: "Open Memos", exact: true });
  await open.waitFor({ timeout: 120_000 });
  await open.click();
  await page.getByRole("heading", { level: 1, name: "memos" }).waitFor({ timeout: 30_000 });
  if (!new URL(page.url()).pathname.startsWith("/apps/")) fail(`Memos lands on ${page.url()}`);
}

export const flow: Flow = { order: 45, run: (page, run) => newAppCatalogFlow(page, run) };
