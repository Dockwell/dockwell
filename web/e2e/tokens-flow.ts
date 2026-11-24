// SPDX-License-Identifier: AGPL-3.0-only

// API tokens narrowed to a project and capped at a role (ADR 0071): made in
// Settings with every project you hold, as you, until Change picks a project
// and a role below yours; listed with where each acts and as what; and, called
// as a script calls, refused what's the install's own and allowed its project's.
// Then revoked.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { atWidth } from "./sheets.ts";

interface TokensFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function tokensFlow(page: Page, { origin, step, shoot }: TokensFlowOptions) {
  step("tokens: a new token acts in every project you hold, as you, until Change narrows it");
  await atWidth(page, { width: 1440, height: 900 });
  await page.goto(`${origin}/settings/api-tokens`);
  await page.getByRole("heading", { level: 1, name: "API tokens" }).waitFor();
  const create = page.getByRole("region", { name: "Create a token" });
  await create.getByText("In every project you hold, as you", { exact: true }).waitFor();
  await create.getByRole("button", { name: "Change" }).click();
  const projects = create.getByRole("group", { name: "Projects" });
  const first = projects.getByRole("checkbox").first();
  await first.waitFor({ state: "attached" });
  const project = ((await projects.locator("label").first().innerText()) || "").trim();
  if (!project) fail("the token's projects list no project to pick");
  await projects.locator("label").first().click();
  await create.getByRole("combobox", { name: "Acts as" }).selectOption({ label: "Viewer" });
  await create.getByText(`In ${project}, as a viewer`, { exact: true }).waitFor();
  await create.getByRole("textbox", { name: "Name" }).fill("e2e narrowed");
  await shoot(page, "token-narrowed");
  await create.getByRole("button", { name: "Create token" }).click();
  const made = page.locator(".tokens-made");
  await made.waitFor();
  await made.getByRole("button", { name: "Show the whole token" }).click();
  const token = (await made.locator(".tokens-secret code").innerText()).trim();
  if (!/^dwt_[0-9A-Za-z]{49}$/.test(token)) fail(`the narrowed token looks like ${JSON.stringify(token)}`);
  await made.getByRole("button", { name: "Done" }).click();

  step("tokens: the list says where it acts and as what");
  const row = page.getByRole("table", { name: "Your API tokens" }).getByRole("row").filter({ hasText: "e2e narrowed" });
  await row.waitFor();
  const text = (await row.innerText()).replace(/\s+/g, "");
  if (!text.includes(`in:${project.replace(/\s+/g, "")}`) || !text.includes("as:viewer")) {
    fail(`the narrowed token's row reads ${JSON.stringify(text)}, want in:${project} and as:viewer`);
  }

  step("tokens: called as a script calls, it's refused the install's own and reads its project's");
  const call = async (path: string) => {
    const r = await fetch(`${origin}${path}`, { headers: { Authorization: `Bearer ${token}` } });
    await r.text();
    return { status: r.status, reason: r.headers.get("Dockwell-Error") };
  };
  const controlPlane = await call("/v1/controlPlane");
  if (controlPlane.status !== 403 || controlPlane.reason !== "token_places") {
    fail(
      `reading the control plane with a token narrowed to ${project}: ${JSON.stringify(controlPlane)}, want 403 token_places`,
    );
  }
  const services = await call("/v1/services");
  if (services.status !== 200) fail(`listing services with the narrowed token: ${JSON.stringify(services)}, want 200`);

  step("tokens: revoking it stops it at once");
  await row.getByRole("button", { name: "Actions for e2e narrowed" }).click();
  await row.getByRole("menuitem", { name: "Revoke", exact: true }).click();
  await row.getByRole("button", { name: "Revoke", exact: true }).click();
  await row.waitFor({ state: "detached" });
  const after = await call("/v1/services");
  if (after.status !== 401 || after.reason !== "token_rejected") {
    fail(`the revoked token listing services: ${JSON.stringify(after)}, want 401 token_rejected`);
  }
}

export const flow: Flow = { order: 91, run: (page, run) => tokensFlow(page, run) };
