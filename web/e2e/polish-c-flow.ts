// SPDX-License-Identifier: AGPL-3.0-only

import type { Locator } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { proposeFixture } from "./row-verbs-shots.ts";
import { atWidth, onTop } from "./sheets.ts";

async function apply(plan: Locator) {
  const button = plan.getByRole("button", { name: /Apply plan|^Delete / });
  await button.waitFor();
  const gate = plan.locator(".plan-gate input");
  if (await gate.isVisible()) await gate.fill(await plan.locator(".plan-gate code").first().innerText());
  await button.click();
}

export const flow: Flow = {
  order: 103,
  async run(page, { origin, step }) {
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/apps`);
    const server = (await ownServerPath(page, origin)).split("/").at(-1);
    const response = await page.request.get(`${origin}/v1/resources?kind=environment`);
    const { resources } = (await response.json()) as { resources: { id: string; resource: { name: string } }[] };
    const scope = resources.find(({ resource }) => resource.name === "production")?.id;
    if (!scope || !server) throw new Error("The polish fixture needs the default environment and its local server");

    step("polish-c: the app header opens a workload plan from its menu");
    const app = "polish-web";
    await proposeFixture(page, "Create an app for the header menu", [
      {
        resource: { kind: "service", name: app, scope },
        spec: { service: { app: { image: "nginx:1.27" } } },
        servers: [server],
      },
    ]);
    await page.goto(`${origin}/apps?q=${app}`);
    await page
      .getByRole("table", { name: "Apps", exact: true })
      .getByRole("row", { name: /polish-web/ })
      .click();
    await page.getByRole("heading", { level: 1, name: app }).waitFor();
    const appUrl = page.url();
    await page.getByRole("button", { name: "Deploy", exact: true }).waitFor();
    const menuTrigger = page.getByRole("button", { name: `Actions for ${app}` });
    await menuTrigger.click();
    const menu = page.getByRole("menu", { name: `Actions for ${app}` });
    await menu.getByRole("menuitem", { name: "Restart", exact: true }).waitFor();
    const stop = menu.getByRole("menuitem", { name: "Stop", exact: true });
    await stop.waitFor();
    await menu.getByRole("menuitem", { name: "Delete app" }).waitFor();
    await menu.getByRole("menuitem", { name: "Pause", exact: true }).click();
    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await plan.getByText(`Pause ${app}`, { exact: true }).waitFor();
    await plan.getByRole("button", { name: "Cancel", exact: true }).click();
    await menuTrigger.click();
    await stop.click();
    await plan.getByText(`Stop ${app}`, { exact: true }).waitFor();
    await plan.getByRole("button", { name: "Cancel", exact: true }).click();

    step("polish-c: the task panel can open an older run's output");
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Scheduled tasks" }).click();
    await page.getByRole("button", { name: "New task" }).first().click();
    const task = page.getByRole("dialog", { name: "New task" });
    await task.getByLabel("Command").fill('echo "polish-output-$$"');
    await task.getByRole("textbox", { name: /^Name/ }).fill("Polish run history");
    await task.getByRole("button", { name: "Review plan" }).click();
    await plan.locator(".plan-gate").getByText("Applying this asks for your passkey").waitFor({ timeout: 30_000 });
    await apply(plan);
    await page.getByRole("button", { name: "Open Polish run history", exact: true }).click({ timeout: 30_000 });

    const panel = page.getByRole("region", { name: "Polish run history" });
    const run = panel.getByRole("button", { name: "Run now" });
    const dots = panel.getByRole("button", { name: /^Show output:/ });
    const output = panel.locator(".tasks-output");
    const log = output.getByText(/^polish-output-\d+$/);
    await run.click();
    await log.waitFor({ timeout: 30_000 });
    const first = await log.innerText();
    await run.click();
    await dots.nth(1).waitFor({ timeout: 30_000 });
    const firstLine = output.getByText(first, { exact: true });
    await firstLine.waitFor({ state: "hidden" });
    await log.waitFor({ timeout: 30_000 });
    const second = await log.innerText();
    if (first === second) throw new Error("The two run logs have identical output");
    await dots.first().click();
    await firstLine.waitFor();
    if ((await dots.first().getAttribute("aria-pressed")) !== "true") throw new Error("The older run was not selected");
    await dots.nth(1).click();
    await output.getByText(second, { exact: true }).waitFor();

    step("polish-c: remove the app and task fixtures");
    await panel.getByRole("button", { name: "Delete task" }).click();
    await apply(plan);
    const done = page.getByRole("button", { name: "Done", exact: true });
    await done.click({ timeout: 30_000 });
    await panel.waitFor({ state: "hidden" });

    await page.goto(appUrl);
    await menuTrigger.click();
    await menu.getByRole("menuitem", { name: "Delete app" }).click();
    await apply(plan);
    await done.click({ timeout: 30_000 });
    await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor();
  },
};
