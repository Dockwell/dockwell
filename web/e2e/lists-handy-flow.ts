// SPDX-License-Identifier: AGPL-3.0-only

import type { Locator, Page } from "playwright-core";
import type { Flow } from "./flows.ts";
import { listScreenshots } from "./lists-handy-shots.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

async function expectRightClickPlacementAndFocus(page: Page, row: Locator, kind: string) {
  const cell = row.locator("td:not(:has(a, button, input))").first();
  await cell.scrollIntoViewIfNeeded();
  const box = await cell.boundingBox();
  if (!box) throw new Error(`A ${kind} row has no cell to right-click`);
  const point = { x: box.x + Math.min(12, box.width / 2), y: box.y + box.height / 2 };
  await page.mouse.click(point.x, point.y, { button: "right" });
  const menu = page.getByRole("menu");
  await menu.waitFor();
  const placed = await menu.boundingBox();
  if (!placed || Math.abs(placed.x - point.x) > 2 || Math.abs(placed.y - point.y) > 2)
    throw new Error(`The ${kind} menu did not open at the pointer`);
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "hidden" });
  if (!(await row.evaluate((node) => node === document.activeElement)))
    throw new Error(`Closing the ${kind} menu did not return focus to the row`);
}

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

    step("lists: right-click placement and Escape focus return on Apps and Docker containers");
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/apps`);
    const appRow = page
      .getByRole("table", { name: "Apps" })
      .getByRole("row")
      .filter({ has: page.getByRole("button", { name: /^Actions for / }) })
      .first();
    await appRow.waitFor();
    await expectRightClickPlacementAndFocus(page, appRow, "app");

    const serverPath = await ownServerPath(page, origin);
    await page.goto(`${origin}${serverPath}/docker/containers`);
    const containerRow = page
      .getByRole("table", { name: "Container inventory" })
      .getByRole("row")
      .filter({ has: page.getByRole("button", { name: /^Actions for / }) })
      .first();
    await containerRow.waitFor();
    await expectRightClickPlacementAndFocus(page, containerRow, "Docker container");

    step("lists: page-count announcement, Members right-click, and phone sort always run");
    const probe = await page.context().newPage();
    try {
      const siteBody = Buffer.concat(
        Array.from({ length: 55 }, (_, i) => {
          const n = String(i + 1).padStart(2, "0");
          return bytes(
            1,
            Buffer.concat([
              bytes(1, `prj_paging_site_${i}`),
              bytes(2, `paging-site-${n}`),
              bytes(3, `paging-site-${n}.example.com`),
              bytes(4, bytes(1, bytes(1, "https://upstream.example.com"))),
              bytes(5, `env_paging_site_${i}`),
            ]),
          );
        }),
      );
      await probe.route(/\/ListWebsites(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: siteBody }),
      );
      await atWidth(probe, { width: 1440, height: 900 });
      await probe.goto(`${origin}/websites`);
      await probe.locator(".list-pager-count").getByText("1-50 of 55", { exact: true }).waitFor();
      await probe.getByRole("button", { name: "Next", exact: true }).click();
      await probe.waitForURL((url) => url.searchParams.get("page") === "2");
      await probe.locator(".list-pager-count").getByText("51-55 of 55", { exact: true }).waitFor();
      await probe
        .locator('.list-pager .dw-sr[aria-live="polite"]')
        .getByText("Page 2", { exact: true })
        .waitFor({ state: "attached" });

      const ownerUser = Buffer.concat([
        bytes(1, "usr_probe_owner"),
        bytes(2, "Owner Person"),
        bytes(3, "owner@example.com"),
        number(4, 1),
      ]);
      const secondUser = Buffer.concat([
        bytes(1, "usr_probe_member"),
        bytes(2, "Sam Rivera"),
        bytes(3, "sam@example.com"),
        number(4, 2),
      ]);
      const membersBody = Buffer.concat([
        bytes(1, Buffer.concat([bytes(1, ownerUser), number(2, 1), number(3, 1), number(9, 1)])),
        bytes(1, Buffer.concat([bytes(1, secondUser), number(9, 1)])),
      ]);
      await probe.route(/\/ListMembers(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: membersBody }),
      );
      await probe.goto(`${origin}/settings/members`);
      const memberRow = probe
        .getByRole("table", { name: "Who signs in to this install" })
        .getByRole("row", { name: /Sam Rivera/ });
      await memberRow.waitFor();
      await expectRightClickPlacementAndFocus(probe, memberRow, "member");

      await atWidth(probe, { width: 390, height: 844 });
      await probe.goto(`${origin}/websites`);
      await probe.getByRole("combobox", { name: "Sort by" }).selectOption("-website");
      await probe.waitForURL((url) => url.searchParams.get("sort") === "-website");
      await probe.getByText("paging-site-55.example.com", { exact: true }).waitFor();
    } finally {
      await probe.close();
    }

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
