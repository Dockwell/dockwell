// SPDX-License-Identifier: AGPL-3.0-only

// Projects, once there are several (ADR 0074): the rail leads to the list, which says what's in
// each and marks one with nothing running, with Delete beside it; a project's page renames it as
// a plan; and the leftover goes in two clicks.

import type { Flow } from "./flows.ts";
import { onTop } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

export const flow: Flow = {
  order: 45,
  run: async (page, { origin, step, shoot }) => {
    step("projects: a project with nothing in it is made, as a deleted app's would be left");
    await page.goto(`${origin}/`);
    await page.getByRole("heading", { level: 1 }).waitFor();
    await page.keyboard.press("Control+k");
    const bar = page.getByRole("dialog", { name: "Command bar" });
    await bar.getByRole("option", { name: /^Create a project/ }).click();
    await bar.getByRole("combobox").fill("leftover");
    await bar.getByRole("option", { name: /^Create the project leftover/ }).click();
    await page
      .getByRole("dialog", { name: "Review plan" })
      .getByRole("button", { name: /^Apply plan/ })
      .click();
    await page.getByRole("dialog", { name: "Applied" }).getByRole("button", { name: "Done" }).click();

    step("projects: the rail leads to the list, which says what's in each and marks the leftover");
    await page
      .getByRole("navigation", { name: "Main" })
      .getByRole("link", { name: /^Projects/ })
      .click();
    await page.getByRole("heading", { level: 1, name: "Projects" }).waitFor();
    const table = page.getByRole("table", { name: "Projects" });
    const leftover = table.getByRole("row", { name: /leftover/ });
    await leftover.waitFor();
    if (!(await leftover.innerText()).includes("Nothing running")) {
      fail(`the leftover's row reads ${JSON.stringify(await leftover.innerText())}`);
    }
    const others = await table.getByRole("row").allInnerTexts();
    if (!others.some((row) => /\d+ apps?/.test(row))) fail(`no row says what runs in it: ${others.join(" | ")}`);
    await shoot(page, "projects-list");

    step("projects: its page renames it as a plan, and keeps its name");
    await leftover.getByRole("link", { name: "leftover" }).click();
    await page.getByRole("heading", { level: 1, name: "leftover" }).waitFor();
    const field = page.getByRole("textbox", { name: "Display name" });
    await field.fill("Leftover shop");
    await page.getByRole("button", { name: "Review plan" }).click();
    await page
      .getByRole("dialog", { name: "Review plan" })
      .getByRole("button", { name: /^Apply plan/ })
      .click();
    await page.getByRole("dialog", { name: "leftover is changed" }).getByRole("button", { name: "Done" }).click();
    await page.getByRole("heading", { level: 1, name: "Leftover shop" }).waitFor({ timeout: 30_000 });
    await shoot(page, "project-page");

    step("projects: Delete beside the leftover takes it in two clicks, with its environment");
    await page.getByRole("link", { name: "Projects", exact: true }).first().click();
    const renamed = page.getByRole("table", { name: "Projects" }).getByRole("row", { name: /Leftover shop/ });
    await renamed.getByRole("button", { name: "Delete the project Leftover shop" }).click();
    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await plan.waitFor();
    const text = await plan.innerText();
    if (!/2\s*to delete/.test(text) || !text.includes("production")) fail(`the plan reads ${JSON.stringify(text)}`);
    await plan.getByRole("button", { name: /^Delete 2 resources/ }).click();
    await page.getByRole("dialog", { name: "2 resources are deleted" }).getByRole("button", { name: "Done" }).click();
    await renamed.waitFor({ state: "detached", timeout: 30_000 });
  },
};
