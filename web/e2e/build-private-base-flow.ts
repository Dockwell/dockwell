// SPDX-License-Identifier: AGPL-3.0-only

// A Git build whose Dockerfile starts from an image its registry keeps private
// (ADR 0086, slice 2): with GitHub's registry connected, as registry-logins-flow.ts
// connects it, New app offers its login for the base image, picked as the only one,
// the plan says the Dockerfile can read any image the login can, the build pulls the
// private base with it, and the app runs. The repository is scripts/gitserve's
// private-base branch, and the build is fakedocker's stand-in for BuildKit, which
// pulls example/private only with the stand-in registry's login.

import { join } from "node:path";

import type { Page } from "playwright-core";

import { startGitServer } from "./build-flow.ts";
import type { Flow } from "./flows.ts";
import { onTop } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

// The stand-in registry's made-up login (scripts/fakedocker/registry.go).
const USER = "ci";
const TOKEN = "e2e-registry-token";

interface Options {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
  git: string;
}

/** Connects GitHub's registry with the stand-in's login, unless it's connected. */
async function connectGHCR(page: Page, origin: string) {
  await page.goto(`${origin}/settings/integrations`);
  const registries = page.getByRole("region", { name: "Registries" });
  await registries.waitFor({ timeout: 20_000 });
  const card = registries.getByRole("article", { name: "GitHub Container Registry" });
  await card.waitFor();
  if ((await card.getByText("Connected").count()) > 0) return;
  await card.getByRole("button", { name: "Connect" }).click();
  const sheet = page.getByRole("dialog", { name: "Add a registry login" });
  await sheet.getByRole("textbox", { name: "Username" }).fill(USER);
  await sheet.getByLabel("Token").fill(TOKEN);
  await sheet.getByText("Signed in", { exact: true }).waitFor({ timeout: 20_000 });
  await sheet.getByRole("button", { name: "Review plan" }).click();
  const plan = page.locator(".plan-sheet");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: "Done" }).click();
  await card.getByText("Connected").waitFor({ timeout: 15_000 });
}

export async function buildPrivateBaseFlow(page: Page, { origin, step, shoot, git }: Options) {
  step("build from a private base: GitHub's registry is connected, so the build's base image pulls with its login");
  await page.setViewportSize({ width: 1440, height: 900 });
  await connectGHCR(page, origin);

  step("build from a private base: New app picks the login, the plan says what it gives, and the app runs");
  await page.goto(`${origin}/apps/new?source=git`);
  const sheet = page.getByRole("dialog", { name: "New app" });
  await sheet.waitFor();
  // With a second server, as earlier flows leave one, it builds where it runs.
  await sheet
    .getByText(/Runs on/)
    .first()
    .waitFor();
  const runsOn = sheet.getByRole("group", { name: "Runs on" });
  if ((await runsOn.count()) > 0) await runsOn.getByRole("checkbox").first().check();
  await sheet.getByLabel("Repository").fill(`${git}/web.git`);
  await sheet.getByText(/^Public · \d+ branches$/).waitFor({ timeout: 20_000 });
  await sheet.getByRole("combobox", { name: "Branch" }).selectOption("private-base");
  await sheet
    .getByRole("status", { name: "Build" })
    .getByText(/^Dockerfile · ghcr\.io\/example\/private:1\.0$/)
    .waitFor({ timeout: 60_000 });
  // The base image is private, so the build pulls it with the only login for its
  // registry, picked, which it could do without.
  const pick = sheet.getByRole("combobox", { name: "Pull ghcr.io images with" });
  await pick.waitFor({ timeout: 20_000 });
  await sheet.getByText("Its Dockerfile can read any image the login can.").waitFor();
  for (let i = 0; (await pick.inputValue()) === "" && i < 50; i++) await page.waitForTimeout(100);
  const picked = await pick.inputValue();
  if (!picked) fail("New app didn't pick the only login for the private base image");
  await sheet.getByLabel(/^Name/).fill("private-base");
  await sheet.getByRole("button", { name: "Review plan" }).click();
  const plan = onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /Apply plan/ }) }));
  await plan.getByText(/^Builds .*web at [0-9a-f]{7} \(private-base\)/).waitFor({ timeout: 30_000 });
  await plan
    .getByText(/its Dockerfile can read any image/i)
    .first()
    .waitFor();
  if (!(await plan.innerText()).includes(picked)) fail("the plan doesn't say which login the build pulls with");
  if ((await plan.innerText()).includes(TOKEN)) fail("the build's plan shows the login's token");
  await shoot(page, "build-private-base-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const live = page.getByRole("dialog", { name: /^private-base is (live|deployed|running)$/ });
  await live.waitFor({ timeout: 60_000 });
  await live.getByRole("button", { name: "Open private-base" }).click();
  await page.getByRole("heading", { level: 1, name: "private-base" }).waitFor();
  await page.getByRole("link", { name: /Deployments/ }).click();
  const log = page.getByRole("log", { name: "Build log" });
  await log
    .getByText(/FROM ghcr\.io\/example\/private:1\.0/)
    .first()
    .waitFor({ timeout: 20_000 });
  await log.getByText("built on our own base", { exact: true }).first().waitFor();
  if ((await log.innerText()).includes(TOKEN)) fail("the build's log shows the login's token");
  await shoot(page, "build-private-base-deployed");
}

/** After registry-logins-flow.ts, which connects GitHub's registry, as it reads private images too. */
export const flow: Flow = {
  order: 98,
  run: async (page, run) => {
    if (!process.env["DOCKWELL_TEST_REGISTRY"]) {
      run.step("build from a private base: there's no registry stand-in on this engine, so nothing signs in");
      return;
    }
    const path = process.env["DOCKWELL_E2E_GITSERVE"];
    if (!path) fail("DOCKWELL_E2E_GITSERVE names no scripts/gitserve build. Run it with make e2e");
    const git = await startGitServer(path, join(run.data, "git"));
    try {
      await buildPrivateBaseFlow(page, { ...run, git: git.url });
    } finally {
      git.stop();
    }
  },
};
