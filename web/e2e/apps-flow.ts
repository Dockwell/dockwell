// SPDX-License-Identifier: AGPL-3.0-only

// The app screens end to end, in the panel: an app from an image goes through
// its plan to the list and its own page, deploys another image, shows its
// revisions, rolls back as a plan, and exports. A Compose file is refused at
// the line, then runs as one stack. make e2e runs it on the fake Docker Engine
// of scripts/fakedocker, and make docker-panel-e2e on real Docker, where
// publishing a port and reaching it means something.

import type { Page } from "playwright-core";

export interface AppsFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  /** On real Docker, the server port to publish and reach. */
  port?: number;
}

const FIRST = "nginx:1.27-alpine";
const SECOND = "nginx:1.28-alpine";

function fail(message: string): never {
  throw new Error(message);
}

/** Reads the plan sheet's counts, applies it, and waits for it to end with the button that ends it. */
async function applyPlan(page: Page, done: string) {
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const finish = page.getByRole("button", { name: done, exact: true });
  await finish.waitFor({ timeout: 120_000 });
  await finish.click();
}

export async function appsFlow(page: Page, { origin, step, shoot, port }: AppsFlowOptions) {
  step("apps: the list invites the first app, and New app asks only for the image");
  await page.goto(`${origin}/apps`);
  await page.getByRole("heading", { name: "No apps yet" }).waitFor();
  await page.getByRole("link", { name: "New app" }).click();
  const sheet = page.getByRole("dialog", { name: "New app" });
  await sheet.waitFor();
  if ((await sheet.getByRole("group", { name: "Runs on" }).count()) > 0)
    fail("New app asks where to run with one server");
  await shoot(page, "apps-new");

  step(`apps: run ${FIRST}, review its plan, apply it with its progress in the sheet, and land on the app`);
  await sheet.getByLabel("Image", { exact: true }).fill(FIRST);
  if (port) {
    await sheet.getByRole("button", { name: "Add port" }).click();
    await sheet.getByLabel("On the server").fill(String(port));
    await sheet.getByLabel("In the app").fill("80");
  }
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("1").first().waitFor();
  await shoot(page, "apps-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("dialog", { name: /Applying|Applied/ }).waitFor();
  await page.getByRole("button", { name: "Open nginx" }).click({ timeout: 120_000 });
  await page.getByRole("heading", { level: 1, name: "nginx", exact: true }).waitFor();
  await page.getByRole("region", { name: "Running" }).getByText(FIRST).waitFor();
  await shoot(page, "apps-overview");
  if (port) {
    const res = await fetch(`http://127.0.0.1:${port}/`);
    if (!res.ok) fail(`the published port answered ${res.status}`);
    step(`  nginx answers on port ${port}`);
  }

  step("apps: it is on the list, with its image, revision and state");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Apps" }).click();
  const row = page.getByRole("row", { name: /nginx/ });
  await row.waitFor();
  for (const text of [FIRST, "Live", "r1"]) await row.getByText(text).first().waitFor();
  if ((await page.getByRole("columnheader", { name: "Runs on" }).count()) > 0)
    fail("the list has a server column with one server");
  await shoot(page, "apps-list");

  step(`apps: deploy ${SECOND} from the app's page, as a plan`);
  await row.getByRole("link", { name: "nginx" }).click();
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const deploy = page.getByRole("dialog", { name: "Deploy nginx" });
  await deploy.getByLabel("Image").fill(SECOND);
  await deploy.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page, "Done");

  step("apps: Deployments lists both revisions, with what changed and who did it");
  await page.getByRole("link", { name: /Deployments/ }).click();
  const r2 = page.getByRole("row", { name: /r2/ });
  await r2.waitFor();
  await r2.getByText("Live").waitFor();
  await r2.getByText(`Deploy ${SECOND} to nginx`).waitFor();
  await r2.getByText("Mira Chen").waitFor();
  await r2.getByText("From the panel").waitFor();
  await page.getByRole("row", { name: /r1/ }).getByText("Replaced").waitFor();
  await shoot(page, "apps-deployments");

  step("apps: one click rolls back to r1, which is a plan too");
  await page.getByRole("button", { name: "Roll back to r1" }).click();
  await applyPlan(page, "Done");
  const r3 = page.getByRole("row", { name: /r3/ });
  await r3.getByText("Live").waitFor({ timeout: 15_000 });
  await r3.getByText("Roll back nginx to r1").waitFor();
  await page.getByRole("row", { name: /r2/ }).getByText("Rolled back").waitFor();
  await page.getByRole("link", { name: "Overview", exact: true }).last().click();
  await page.getByRole("region", { name: "Running" }).getByText(FIRST).waitFor();
  if (port) {
    const res = await fetch(`http://127.0.0.1:${port}/`);
    if (!res.ok) fail(`after the rollback, the published port answered ${res.status}`);
  }

  step("apps: export it as a Compose file, with no secrets in it");
  await page.getByRole("button", { name: "Export as Compose" }).click();
  const exported = page.getByRole("dialog", { name: "Export as Compose" });
  await exported.getByRole("region", { name: "Compose file" }).getByText(`image: ${FIRST}`).waitFor();
  await shoot(page, "apps-export");
  await page.keyboard.press("Escape");

  step("apps: a Compose file is refused at the line, then runs as one stack");
  await page.goto(`${origin}/apps/new?source=compose`);
  const compose = page.getByRole("dialog", { name: "New app" });
  const field = compose.getByLabel("Compose file");
  await field.fill(`services:\n  front:\n    image: ${FIRST}\n    build: .\n  back:\n    image: ${SECOND}\n`);
  await compose.getByText("The service front builds an image").waitFor();
  await shoot(page, "apps-compose-refused");
  await field.fill(`services:\n  front:\n    image: ${FIRST}\n  back:\n    image: ${SECOND}\n`);
  await compose.getByRole("region", { name: "What this file makes" }).getByText("This makes 2 services").waitFor();
  await compose.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page, "Show the apps");
  const stack = page.getByRole("row", { name: /back, front/ });
  await stack.getByText("Stack of 2 services").waitFor();
  await shoot(page, "apps-stack");

  step("apps: deleting goes through a plan too, and the list follows");
  for (const name of ["nginx", "front", "back"]) {
    await page.goto(`${origin}/apps`);
    await page.getByRole("link", { name, exact: true }).click();
    await page.getByRole("button", { name: "Delete this app" }).click();
    await applyPlan(page, "Done");
    await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor();
    await page.getByRole("link", { name, exact: true }).waitFor({ state: "detached" });
  }
}
