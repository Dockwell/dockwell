// SPDX-License-Identifier: AGPL-3.0-only

// Builds from Git (ADR 0046), as a person does them: an app from a repository,
// through its plan, built on its server and run; a deploy whose build fails, which
// leaves what runs as it was and says which step and what it said last; and a build
// someone cancels. The repository is scripts/gitserve's, and the build is fakedocker's
// stand-in for BuildKit; CI's Docker job builds with the real one.

import { type ChildProcess, spawn } from "node:child_process";
import { join, resolve } from "node:path";

import type { Locator, Page } from "playwright-core";

import type { Flow } from "./flows.ts";

interface BuildFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
  /** Where scripts/gitserve serves, such as http://127.0.0.1:41234. */
  git: string;
}

function fail(message: string): never {
  throw new Error(message);
}

/** Points the repository's main where another branch is, as a push would. */
async function move(git: string, to: string) {
  const res = await fetch(`${git}/_move?branch=main&to=${to}`, { method: "POST" });
  if (!res.ok) fail(`the Git server didn't move main to ${to}: ${res.status} ${await res.text()}`);
}

/** The sheet that has a plan to apply, whatever it's called. */
function planSheet(page: Page): Locator {
  return page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /Apply plan/ }) });
}

export async function buildFlow(page: Page, { origin, step, shoot, git }: BuildFlowOptions) {
  step("an app builds from a Git repository with its Dockerfile, through its plan, and runs what it built");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/apps/new`);
  const sheet = page.getByRole("dialog", { name: "New app" });
  if (!(await sheet.getByRole("radio", { name: "Git" }).isChecked()))
    fail("New app doesn't open on Git, as Apps-New has it");
  await sheet.getByLabel("Repository").fill(`${git}/web.git`);
  await sheet.getByLabel("Branch").fill("main");
  await sheet.getByLabel(/^Name/).fill("storefront");
  await shoot(page, "build-new-app");
  await sheet.getByRole("button", { name: "Review plan" }).click();
  const plan = planSheet(page);
  await plan
    .getByText(/^Builds .*web at [0-9a-f]{7} \(main\) on .* first\. If the build fails, what runs stays as it is\.$/)
    .waitFor();
  await shoot(page, "build-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const live = page.getByRole("dialog", { name: /^storefront is (live|deployed|running)$/ });
  await live.waitFor({ timeout: 60_000 });
  await live.getByRole("button", { name: "Open storefront" }).click();
  await page.getByRole("heading", { level: 1, name: "storefront" }).waitFor();
  // The header says where it's built from, and on which branch.
  await page.getByText("main", { exact: true }).first().waitFor();
  const first =
    (await page.locator(".app-built-from code").nth(1).textContent()) ??
    fail("the header doesn't say which commit runs");

  await page.getByRole("link", { name: /Deployments/ }).click();
  const table = page.getByRole("table", { name: "Revisions of storefront" });
  await table.getByRole("row").filter({ hasText: "Add order export endpoint" }).first().waitFor();
  const log = page.getByRole("log", { name: "Build log" });
  // The step, whether it ran or came from the cache, where it prints nothing.
  await log.getByText('[4/4] RUN echo "installed 214 packages in 9s"', { exact: true }).waitFor();
  await page.getByText("Built", { exact: true }).first().waitFor();
  await shoot(page, "build-deployed");

  step("a build that fails leaves what runs as it was, and says which step failed and what it said last");
  await move(git, "broken");
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const deploy = planSheet(page);
  await deploy.getByText(/^Builds .*web at [0-9a-f]{7} \(main\)/).waitFor();
  await deploy.getByRole("button", { name: /Apply plan/ }).click();
  const failed = page.getByRole("dialog", { name: /^Couldn't deploy [0-9a-f]{7}$/ });
  await failed.waitFor({ timeout: 60_000 });
  // The step, quoted; what it printed last is the log's, for whoever may read logs.
  await failed
    .getByText(/The build failed at step 4 of 4, "RUN echo .*": its log marks what it printed last/)
    .waitFor();
  await failed.getByText(new RegExp(`^storefront stay(s|ed) on ${first}$`)).waitFor();
  await shoot(page, "build-failed-sheet");
  await failed.getByRole("button", { name: "Done" }).click();
  // Revisions are numbered by what the app has been through, so a row is found by its commit.
  const broken = table.getByRole("row").filter({ hasText: "Move sessions to valkey" });
  await broken.getByText("Failed", { exact: true }).waitFor();
  await broken.click();
  await log.getByText("npm ERR! missing script: build").first().waitFor();
  if ((await page.locator(".build-log-line.is-failed").count()) === 0)
    fail("the log doesn't mark the step that failed");
  // What ran is what runs.
  const still = (await page.locator(".app-built-from code").nth(1).textContent()) ?? "";
  if (still !== first) fail(`a failed build changed what runs, from ${first} to ${still}`);
  await shoot(page, "build-failed");

  step("someone cancels a build under way, and what runs runs on");
  await move(git, "slow");
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  await planSheet(page)
    .getByRole("button", { name: /Apply plan/ })
    .click();
  // The plan's sheet follows it; the revision's own panel has the build and the way to stop it.
  await page
    .getByRole("dialog")
    .getByRole("button", { name: /^(Close|Done)$/ })
    .first()
    .click();
  const building = table.getByRole("row").filter({ hasText: "Batch inventory sync with smaller pages" });
  await building.getByText("Building", { exact: true }).waitFor({ timeout: 30_000 });
  await building.click();
  await log.getByText("installing packages", { exact: true }).waitFor();
  await shoot(page, "build-building");
  await page.getByRole("button", { name: "Cancel build" }).click();
  await building.getByText("Cancelled", { exact: true }).waitFor({ timeout: 30_000 });
  await page.getByText(/^Cancelled by .*\. Nothing was deployed, and what ran before runs on\.$/).waitFor();
  await page.getByText("Build cancelled", { exact: true }).waitFor();
  const after = (await page.locator(".app-built-from code").nth(1).textContent()) ?? "";
  if (after !== first) fail(`a cancelled build changed what runs, from ${first} to ${after}`);
  await shoot(page, "build-cancelled");
  await move(git, "stable");
}

/**
 * Starts scripts/gitserve on a folder of repositories, and says where it serves and
 * how to stop it.
 */
async function startGitServer(path: string, dir: string): Promise<{ url: string; stop: () => void }> {
  const server: ChildProcess = spawn(resolve(path), [dir], { stdio: ["ignore", "pipe", "inherit"] });
  const url = await new Promise<string>((done, refused) => {
    const silent = setTimeout(() => refused(new Error(`${path} said nothing for 15 s, so it isn't serving`)), 15_000);
    server.stdout?.once("data", (b: Buffer) => {
      clearTimeout(silent);
      done(b.toString().trim());
    });
    server.once("exit", (code, signal) => {
      clearTimeout(silent);
      refused(new Error(`${path} ended with ${code ?? signal} before it said where it serves`));
    });
  });
  return { url, stop: () => server.kill("SIGTERM") };
}

/**
 * Where sign-in.ts runs this among the flows, on the fake engine (flows.ts): after
 * apps, as it starts from New app too. make e2e names scripts/gitserve's build in
 * DOCKWELL_E2E_GITSERVE.
 */
export const flow: Flow = {
  order: 15,
  run: async (page, run) => {
    const path = process.env["DOCKWELL_E2E_GITSERVE"];
    if (!path)
      fail("DOCKWELL_E2E_GITSERVE names no scripts/gitserve build, which the build flow needs. Run it with make e2e");
    const git = await startGitServer(path, join(run.data, "git"));
    try {
      await buildFlow(page, { ...run, git: git.url });
    } finally {
      git.stop();
    }
  },
};
