// SPDX-License-Identifier: AGPL-3.0-only

import type { Flow } from "./flows.ts";
import { listScreenshots } from "./lists-handy-shots.ts";
import { onTop } from "./sheets.ts";

export const flow: Flow = {
  order: 99,
  run: async (page, run) => {
    const { origin, step, shoot } = run;
    step("lists: create an empty project from the Projects header");
    await page.goto(`${origin}/projects`);
    await page.getByRole("button", { name: "New project", exact: true }).click();
    const create = page.getByRole("dialog", { name: "New project" });
    await create.getByRole("textbox", { name: "Name" }).fill("List test");
    await create.getByRole("button", { name: "Review plan" }).click();
    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await plan.getByText("production", { exact: true }).waitFor();
    await plan.getByRole("button", { name: /^Apply plan/ }).click();
    await page.getByRole("dialog", { name: "Applied" }).getByRole("button", { name: "Done" }).click();
    await page.waitForURL("**/projects/list-test");

    step("lists: slash focuses search, a reload keeps the query, and both row shortcuts open the action menu");
    await page.goto(`${origin}/projects`);
    const search = page.getByRole("searchbox", { name: "Filter projects" });
    await search.waitFor();
    await page.keyboard.press("/");
    if (!(await search.evaluate((node) => node === document.activeElement)))
      throw new Error("Slash did not focus search");
    await page.keyboard.type("list test");
    await page.waitForURL((url) => url.searchParams.get("q") === "list test");
    await page.reload();
    const row = page.getByRole("row", { name: /List test/ });
    await row.waitFor();
    const table = page.getByRole("table", { name: "Projects" });
    if ((await table.getByRole("row").count()) !== 2) throw new Error("The project filter survived with extra rows");
    await table.getByRole("button", { name: "Project", exact: true }).click();
    await page.waitForURL((url) => url.searchParams.get("sort") === "name");
    await row.press("Shift+F10");
    const menu = page.getByRole("menu", { name: "Actions for List test" });
    await menu.waitFor();
    await page.keyboard.press("Escape");
    await menu.waitFor({ state: "hidden" });
    await row.getByRole("cell").nth(1).click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Rename" }).click();
    const rename = page.getByRole("dialog", { name: "Rename List test" });
    await rename.getByRole("textbox", { name: "Display name" }).fill("List test renamed");
    await rename.getByRole("button", { name: "Review plan" }).click();
    await plan.getByRole("button", { name: /^Apply plan/ }).click();
    await page.getByRole("dialog", { name: "list-test is changed" }).getByRole("button", { name: "Done" }).click();

    step("lists: delete the empty project after reviewing its environment in the plan");
    const renamed = table.getByRole("row", { name: /List test renamed/ });
    await renamed.getByRole("button", { name: "Actions for List test renamed" }).click();
    await page.getByRole("menuitem", { name: "Delete", exact: true }).click();
    await plan.getByText("production", { exact: true }).waitFor();
    await plan.getByRole("button", { name: /^Delete 2 resources/ }).click();
    await page.getByRole("dialog", { name: "2 resources are deleted" }).getByRole("button", { name: "Done" }).click();
    await renamed.waitFor({ state: "detached" });

    for (const [path, heading] of [
      ["projects", "Projects"],
      ["apps", "Apps"],
      ["websites", "Websites"],
      ["databases", "Databases"],
      ["domains", "Domains"],
      ["connections", "Connections"],
      ["backups", "Backups"],
      ["variables", "Variables"],
    ] as const) {
      await page.goto(`${origin}/${path}`);
      await page.getByRole("heading", { level: 1, name: heading, exact: true }).waitFor();
      await page.locator(".dw-table-wrap.is-loading, [aria-busy=true]").first().waitFor({ state: "hidden" });
      await shoot(page, `lists-handy-${path}`);
    }
    const directory = process.env["DOCKWELL_LISTS_ROUND2_SHOTS"];
    if (directory) await listScreenshots(page, run, directory);
  },
};
