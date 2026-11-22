// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";

import type { Page, Route } from "playwright-core";

import type { Flow } from "./flows.ts";
import { atWidth, onTop } from "./sheets.ts";

const name = "fixture-workspace-template";
const project = "fixture-compose-templates";
const longName = "LONG_DEPLOYMENT_CONFIGURATION_INPUT_FOR_THE_TEMPLATE_EXAMPLE";
const source = `services:
  web:
    image: ghcr.io/acme/template-demo:1.4
    environment:
      APP_VALUE: \${${longName}}
      API_TOKEN: \${API_TOKEN}
`;

async function applyReview(page: Page) {
  const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await review.getByRole("button", { name: /^Apply plan/ }).click();
  await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 30000 });
}

async function deployAsDeveloper(page: Page, data: string, projectId: string, detailURL: string) {
  // The sign-in fixture keeps its passkey session and temporarily gives that
  // user only a project developer grant. The API reads the grant for each call.
  const db = new DatabaseSync(join(data, "control", "state.db"));
  const owner = db.prepare("SELECT user_id FROM grants WHERE role = 'owner'").get()?.["user_id"];
  if (typeof owner !== "string") {
    db.close();
    throw new Error("The template fixture has no owner");
  }
  try {
    db.exec("PRAGMA busy_timeout = 5000");
    db.prepare("UPDATE grants SET role = 'developer', project_id = ? WHERE user_id = ? AND role = 'owner'").run(
      projectId,
      owner,
    );
    await page.goto(detailURL);
    await page.getByRole("heading", { name, level: 1 }).waitFor();
    await page.getByRole("button", { name: "Use this revision" }).click();
    const use = onTop(page.getByRole("dialog", { name: "Use Compose template" }));
    await use.getByLabel("New environment", { exact: true }).fill("developer-template-deployment");
    await use.getByLabel(longName).fill("developer-value");
    await use.getByRole("button", { name: "Review deployment" }).click();
    await applyReview(page);
    await page.waitForURL(/\/apps\?environment=env_/);
  } finally {
    db.prepare("UPDATE grants SET role = 'owner', project_id = '' WHERE user_id = ? AND project_id = ?").run(
      owner,
      projectId,
    );
    db.close();
  }
}

export const flow: Flow = {
  order: 69,
  async run(page, { origin, shoot, step, bin, data, env }) {
    step("Compose templates: prepare, save, and use a pinned definition with a generated secret");
    const cli = (...args: string[]) =>
      execFileSync(bin, [...args, "--data-dir", data], {
        env: {
          ...process.env,
          ...env,
          DOCKWELL_URL: "",
          DOCKWELL_TOKEN: "",
          XDG_CONFIG_HOME: join(data, "template-cli"),
        },
      });
    // Deploying a template adds an environment. Keep it out of the default
    // project so other flows can still address that project's sole environment.
    cli("login", "--local");
    cli("projects", "create", project, "-y");
    await page.goto(`${origin}/apps/templates`);
    await page.getByLabel("Project", { exact: true }).selectOption({ label: project });
    const projectId = await page.getByLabel("Project", { exact: true }).inputValue();
    await page.getByText("No templates yet", { exact: true }).waitFor();
    await shoot(page, "docker-slice8-empty");
    await page.getByRole("button", { name: "New template", exact: true }).first().click();
    let form = onTop(page.getByRole("dialog", { name: "New template" }));
    await form.getByLabel("Name", { exact: true }).fill(name);
    await form.getByLabel("Description").fill("A reusable starting point for the demo workspace.");
    await form
      .getByLabel("Compose file", { exact: true })
      .fill(source.replace(`\${API_TOKEN}`, "an-inline-credential"));
    await form.getByText("Line 6", { exact: true }).waitFor();
    await shoot(page, "docker-slice8-refused");
    const rpc = "**/dockwell.v1.ComposeService/PrepareComposeTemplate";
    let release = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const delayed = async (route: Route) => {
      await held;
      await route.continue().catch(() => undefined);
    };
    await page.route(rpc, delayed);
    try {
      await form.getByLabel("Compose file", { exact: true }).fill(source);
      await form.getByText("The Compose file check is taking longer than usual", { exact: true }).waitFor();
      await shoot(page, "docker-slice8-slow");
    } finally {
      release();
      await page.unroute(rpc, delayed);
    }
    await form.getByText(longName, { exact: true }).waitFor();
    await form.getByRole("button", { name: "Review template" }).click();
    await applyReview(page);
    await page.getByRole("link", { name, exact: true }).waitFor();
    await shoot(page, "docker-slice8-list");

    await page.getByRole("button", { name: "New template", exact: true }).click();
    form = onTop(page.getByRole("dialog", { name: "New template" }));
    await form.getByLabel("Source", { exact: true }).selectOption("git");
    await form.getByLabel("Repository URL").fill("https://git.example.com/fixtures");
    await form.getByLabel("Branch, tag or commit").fill("main");
    await shoot(page, "docker-slice8-git");
    await form.getByRole("button", { name: "Cancel", exact: true }).click();

    await page.getByRole("link", { name, exact: true }).click();
    await page.getByRole("heading", { name, level: 1 }).waitFor();
    await page.getByLabel("Compose file", { exact: true }).waitFor();
    const detailURL = page.url();
    await shoot(page, "docker-slice8-detail");
    await page.getByRole("button", { name: "Use template", exact: true }).click();
    const use = onTop(page.getByRole("dialog", { name: "Use Compose template" }));
    await use.getByLabel(longName).fill("fixture-value");
    await use.getByLabel("API_TOKEN", { exact: true }).waitFor();
    await atWidth(page, { width: 390, height: 844 });
    const bounds = await use.getByLabel(longName).boundingBox();
    if (!bounds || bounds.x + bounds.width > 382) throw new Error("The template input is clipped by the phone sheet");
    await atWidth(page, { width: 1440, height: 900 });
    await shoot(page, "docker-slice8-inputs");
    await use.getByRole("button", { name: "Review deployment" }).click();
    await onTop(page.getByRole("dialog", { name: "Review plan" }))
      .getByText(/From the .* template, revision 1/i)
      .waitFor();
    await shoot(page, "docker-slice8-review");
    await applyReview(page);

    await page.getByRole("button", { name: "Template actions" }).click();
    await page.getByRole("menuitem", { name: "Save as template" }).click();
    const saved = onTop(page.getByRole("dialog", { name: "Save as template" }));
    await saved.getByLabel("Prepared Compose file").waitFor();
    await shoot(page, "docker-slice8-save");
    await saved.getByRole("button", { name: "Cancel", exact: true }).click();

    await page.goto(`${origin}/apps/new?source=compose`);
    await page.getByRole("button", { name: "From template", exact: true }).click();
    const pick = onTop(page.getByRole("dialog", { name: "Use Compose template" }));
    await pick.getByLabel("Project", { exact: true }).selectOption({ label: project });
    await pick.getByLabel("Template", { exact: true }).selectOption({ label: name });
    await pick.getByLabel(longName).waitFor();
    await pick.getByRole("button", { name: "Cancel", exact: true }).click();
    step("Compose templates: a project developer deploys an exact revision and returns to Apps");
    await deployAsDeveloper(page, data, projectId, detailURL);
    cli("projects", "delete", project, "--with-contents", "-y");
  },
};
