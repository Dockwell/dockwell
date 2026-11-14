// SPDX-License-Identifier: AGPL-3.0-only

// Builds from Git (ADR 0046), as a person does them: an app from a repository,
// through its plan, built on its server and run; a deploy whose build fails, which
// leaves what runs as it was and says which step and what it said last; and a build
// someone cancels; an app with no Dockerfile, which Railpack builds, and one it can't.
// The repository is scripts/gitserve's, and the build is fakedocker's stand-in for
// BuildKit and Railpack; CI's Docker job builds with the real ones.

import { type ChildProcess, spawn } from "node:child_process";
import { join, resolve } from "node:path";

import type { Locator, Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { onTop } from "./sheets.ts";

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
  return onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /Apply plan/ }) }));
}

export async function buildFlow(page: Page, { origin, step, shoot, git }: BuildFlowOptions) {
  step("an app builds from a Git repository with its Dockerfile, through its plan, and runs what it built");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/apps/new`);
  const sheet = page.getByRole("dialog", { name: "New app" });
  // New app opens on the catalog, as most apps start there, and Git is one step away (#617).
  if (!(await sheet.getByRole("radio", { name: "Catalog" }).isChecked())) fail("New app doesn't open on the catalog");
  await sheet.getByRole("radio", { name: "Git" }).click();
  await page.waitForURL(/\/apps\/new\?source=git$/);
  await sheet.getByLabel("Repository").fill(`${git}/web.git`);
  // The repository is checked as it's typed, and its branches are there to choose
  // from, with the default one, main, chosen already.
  await sheet.getByText(/^Public · \d+ branches$/).waitFor();
  const branch = sheet.getByRole("combobox", { name: "Branch" });
  if ((await branch.locator("option:checked").textContent()) !== "main (default)")
    fail("New app doesn't choose the repository's default branch");
  // How it builds is read and offered, and the port its Dockerfile exposes goes in the app.
  await sheet
    .getByRole("status", { name: "Build" })
    .getByText(/^Dockerfile · busybox:1\.37$/)
    .waitFor();
  const inApp = sheet.getByLabel("In the app");
  await inApp.waitFor();
  if ((await inApp.inputValue()) !== "8080") fail(`the port in the app reads ${await inApp.inputValue()}, not 8080`);
  if (!/^\d+$/.test(await sheet.getByLabel("On the server").inputValue())) fail("no port on the server was filled");
  await sheet.getByLabel(/^Name/).fill("order-api");
  await shoot(page, "build-new-app");
  // A port someone removes stays removed, and later flows publish 8080 themselves.
  await sheet.getByRole("button", { name: "Remove this port" }).click();
  await sheet.getByLabel(/^Name/).fill("order-api2");
  await sheet.getByLabel(/^Name/).fill("order-api");
  if ((await sheet.getByLabel("In the app").count()) !== 0) fail("a port removed from the form came back");
  await sheet.getByRole("button", { name: "Review plan" }).click();
  const plan = planSheet(page);
  await plan
    .getByText(/^Builds .*web at [0-9a-f]{7} \(main\) on .* first\. If the build fails, what runs stays as it is\.$/)
    .waitFor();
  await shoot(page, "build-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const live = page.getByRole("dialog", { name: /^order-api is (live|deployed|running)$/ });
  await live.waitFor({ timeout: 60_000 });
  await live.getByRole("button", { name: "Open order-api" }).click();
  await page.getByRole("heading", { level: 1, name: "order-api" }).waitFor();
  // The header says where it's built from, and on which branch.
  await page.getByText("main", { exact: true }).first().waitFor();
  const first =
    (await page.locator(".app-built-from code").nth(1).textContent()) ??
    fail("the header doesn't say which commit runs");

  await page.getByRole("link", { name: /Deployments/ }).click();
  const table = page.getByRole("table", { name: "Revisions of order-api" });
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
  await failed.getByText(new RegExp(`^order-api stay(s|ed) on ${first}$`)).waitFor();
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

  step("an app with no Dockerfile builds with Railpack, and New app and the plan say what Railpack found first");
  await page.goto(`${origin}/apps/new?source=git`);
  const next = page.getByRole("dialog", { name: "New app" });
  await next.getByLabel("Repository").fill(`${git}/web.git`);
  await next.getByText(/^Public · \d+ branches$/).waitFor();
  await next.getByRole("combobox", { name: "Branch" }).selectOption("node");
  await next.getByLabel(/^Name/).fill("catalog");
  const row = next.getByRole("status", { name: "Build" });
  await row.getByText("Railpack · Node 22", { exact: true }).waitFor({ timeout: 60_000 });
  await next.getByText("Runs npm install, npm run build, then starts with npm run start.", { exact: false }).waitFor();
  await shoot(page, "build-railpack-new-app");
  await next.getByRole("button", { name: "Review plan" }).click();
  const railpackPlan = planSheet(page);
  await railpackPlan.getByText(/^Builds .*web at [0-9a-f]{7} \(node\) on .* first\./).waitFor();
  await railpackPlan.getByText("With Railpack, as Node 22.", { exact: false }).waitFor();
  await shoot(page, "build-railpack-plan");
  await railpackPlan.getByRole("button", { name: /Apply plan/ }).click();
  const catalog = page.getByRole("dialog", { name: /^catalog is (live|deployed|running)$/ });
  await catalog.waitFor({ timeout: 60_000 });
  await catalog.getByRole("button", { name: "Open catalog" }).click();
  await page.getByRole("heading", { level: 1, name: "catalog" }).waitFor();
  await page.getByRole("link", { name: /Deployments/ }).click();
  const builtWith = page.locator(".build-facts dt", { hasText: "Built with" }).locator("xpath=following-sibling::dd");
  await builtWith.getByText("Railpack · Node 22", { exact: false }).waitFor();
  // Railpack's build step, whether it ran or came from the cache, as Railpack names it.
  await page.getByRole("log", { name: "Build log" }).getByText("npm run build", { exact: true }).waitFor();
  await shoot(page, "build-railpack-deployed");

  step("a repository Railpack can't read says so before anything builds, and its plan is refused");
  await page.goto(`${origin}/apps/new?source=git`);
  const docs = page.getByRole("dialog", { name: "New app" });
  await docs.getByLabel("Repository").fill(`${git}/web.git`);
  await docs.getByText(/^Public · \d+ branches$/).waitFor();
  await docs.getByRole("combobox", { name: "Branch" }).selectOption("docs");
  await docs.getByLabel(/^Name/).fill("docs");
  await docs
    .getByRole("status", { name: "Build" })
    .getByText("Railpack can't tell how to build it", { exact: true })
    .waitFor({ timeout: 60_000 });
  await docs
    .getByText("Add a Dockerfile, or set both a build command and a start command under Change.", { exact: false })
    .waitFor();
  await shoot(page, "build-railpack-cant");
  await docs.getByRole("button", { name: "Review plan" }).click();
  await docs.getByText(/Railpack can't tell how to build .*web at [0-9a-f]{7} for docs/).waitFor();
}

/**
 * Starts scripts/gitserve on a folder of repositories, and says where it serves and
 * how to stop it.
 */
export async function startGitServer(path: string, dir: string): Promise<{ url: string; stop: () => void }> {
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
