// SPDX-License-Identifier: AGPL-3.0-only

// GitHub as an App (ADR 0098), through GitHub's stand-in beside the fake engine
// (internal/control/github/ghtest), which makes the App a manifest asks for,
// installs it on acme, and serves acme/web, a private repository, from this flow's
// own scripts/gitserve to the installation's token alone. Settings → Integrations
// connects GitHub with one choice, one button and a passkey; the browser goes to
// GitHub and back twice, and the card says where the App is installed. The new app
// sheet lists the repository, reads its branches through the installation, and
// the plan says so; the private build fetches it. Deploy on push is a switch with
// nothing to set, and a push through the App's one webhook deploys it.

import { join } from "node:path";

import type { Locator, Page } from "playwright-core";

import { startGitServer } from "./build-flow.ts";
import type { Flow, FlowRun } from "./flows.ts";
import { atWidth, onTop } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

/** The sheet that has a plan to apply, whatever it's called. */
function planSheet(page: Page): Locator {
  return onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /Apply plan/ }) }));
}

async function githubAppFlow(page: Page, { origin, step, shoot }: FlowRun, stand: string, git: string) {
  step("GitHub App: Integrations connects GitHub with one choice, one button and a passkey");
  await atWidth(page, { width: 1440, height: 900 });
  await page.goto(`${origin}/settings/integrations`);
  const section = page.getByRole("region", { name: "Git" });
  const card = section.getByRole("article", { name: "GitHub" });
  await card.getByText("Not connected").waitFor({ timeout: 20_000 });
  await shoot(page, "github-none");
  await card.getByRole("button", { name: "Connect" }).click();
  const sheet = page.getByRole("dialog", { name: "Connect GitHub" });
  await sheet.waitFor();
  if (!(await sheet.getByRole("radio", { name: "Your account" }).isChecked()))
    fail("Connect GitHub doesn't start on your account");
  await sheet.getByRole("radio", { name: "An organization" }).click();
  await sheet.getByLabel("Organization").fill("acme");
  await shoot(page, "github-connect");
  // This browser's authenticator gives the passkey it asks for; then the browser goes
  // to GitHub, which makes the App and sends it back, then to install it, and back.
  await sheet.getByRole("button", { name: "Create on GitHub" }).click();
  await page.waitForURL(`${origin}/settings/integrations`, { timeout: 60_000 });
  await card.getByText("Connected").waitFor({ timeout: 20_000 });
  await card.getByText("GitHub App on acme · 1 repository").waitFor();
  if ((await page.locator("body").innerText()).includes("PRIVATE KEY")) fail("the page shows the App's key");
  await shoot(page, "github-connected");

  await card.getByRole("button", { name: "Manage" }).click();
  const manage = page.getByRole("dialog", { name: /^GitHub App / });
  await manage
    .getByRole("listitem")
    .filter({ hasText: "acme" })
    .getByText("1 repository · for every project")
    .waitFor();
  await manage.getByText("Pushes come through the GitHub App.").waitFor();
  await shoot(page, "github-manage");
  await manage.getByRole("button", { name: "Done" }).click();

  step("GitHub App: the new app sheet picks acme/web, reads it through the installation, and the plan says so");
  await page.goto(`${origin}/apps/new?source=git`);
  const add = page.getByRole("dialog", { name: "New app" });
  const field = add.getByRole("combobox", { name: "Repository" });
  await field.click();
  await add
    .getByRole("listbox", { name: "Repositories" })
    .getByRole("option", { name: /acme\/web/ })
    .click();
  if ((await field.inputValue()) !== `${stand}/acme/web`)
    fail(`the repository picked reads ${await field.inputValue()}`);
  await add.getByText(/^Private, through acme · \d+ branches$/).waitFor({ timeout: 30_000 });
  await add
    .getByRole("status", { name: "Build" })
    .getByText(/^Dockerfile · busybox:1\.37$/)
    .waitFor({ timeout: 60_000 });
  await add.getByLabel(/^Name/).fill("private-web");
  await shoot(page, "github-new-app");
  await add.getByRole("button", { name: "Review plan" }).click();
  const plan = planSheet(page);
  await plan
    .getByText(/^Builds .*acme\/web at [0-9a-f]{7} \(main\) on .* first, through the GitHub App on acme\./)
    .waitFor({ timeout: 30_000 });
  if ((await plan.innerText()).includes("ghs_")) fail("the plan shows the installation's token");
  await shoot(page, "github-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const live = page.getByRole("dialog", { name: /^private-web is (live|deployed|running)$/ });
  await live.waitFor({ timeout: 90_000 });
  await live.getByRole("button", { name: "Open private-web" }).click();
  await page.getByRole("heading", { level: 1, name: "private-web" }).waitFor();
  // Its Source is on the app's overview, where Open may not land.
  await page.goto(page.url().replace(/\/(deployments|logs|environment|settings)([/?].*)?$/, ""));
  await page.getByRole("heading", { level: 1, name: "private-web" }).waitFor();

  step("GitHub App: deploy on push is a switch with nothing to set, and a push into production waits for a person");
  const source = page.locator("section").filter({ has: page.getByRole("heading", { name: "Source", exact: true }) });
  await source.getByText("through the GitHub App on acme").waitFor({ timeout: 20_000 });
  const onPush = source.getByRole("switch", { name: "Deploy on push" });
  await onPush.click();
  await source.getByText("Pushes come through the GitHub App.").waitFor({ timeout: 20_000 });
  if ((await page.getByRole("dialog").count()) !== 0) fail("deploy on push through the App opened a sheet to set up");
  if ((await onPush.getAttribute("aria-checked")) !== "true") fail("deploy on push through the App isn't on");
  await shoot(page, "github-push-on");

  const moved = await fetch(`${git}/_move?branch=main&to=giftcards`, { method: "POST" });
  if (!moved.ok) fail(`the Git server didn't move main: ${moved.status}`);
  // The payload names a commit the branch doesn't: Dockwell reads the branch itself.
  const push = async () => {
    const pushed = await fetch(`${stand}/_push?repo=acme/web&ref=refs/heads/main&after=${"1".repeat(40)}`, {
      method: "POST",
    });
    if (pushed.status !== 202) fail(`the App's webhook answered a signed push with ${pushed.status}`);
  };
  const lineSays = async (said: RegExp, what: string) => {
    for (let i = 0; ; i++) {
      await page.reload();
      await source
        .getByText(/^Last push|Pushes come through the GitHub App\.$/)
        .first()
        .waitFor();
      if ((await source.getByText(said).count()) > 0) return;
      if (i > 60) fail(`the Source panel never said ${what}`);
      await page.waitForTimeout(1000);
    }
  };
  // Production is protected by its name, and a hook acts as a developer at most, so
  // a push through the App plans its deploy for a person to apply (ADR 0071).
  await push();
  await lineSays(/planned [0-9a-f]{7}, which waits for a person/, "the push through the App waits for a person");
  await shoot(page, "github-push-waits");

  step("GitHub App: its maker lets it deploy without waiting with a passkey, and the next push deploys");
  await source.getByRole("switch", { name: "Deploy without waiting" }).click();
  await source.getByText("Each push to main deploys private-web.").waitFor({ timeout: 20_000 });
  await push();
  await lineSays(/started a deploy of [0-9a-f]{7}/, "the push through the App started a deploy");
  await page.getByRole("link", { name: /Deployments/ }).click();
  await page
    .getByRole("table", { name: "Revisions of private-web" })
    .getByRole("row")
    .filter({ hasText: "Add gift cards" })
    .first()
    .waitFor({ timeout: 90_000 });
  await shoot(page, "github-push-deployed");
}

/**
 * Where sign-in.ts runs this among the flows, on the fake engine (flows.ts): late,
 * as it adds an app the flows before it don't expect. It runs its own
 * scripts/gitserve, which make e2e names in DOCKWELL_E2E_GITSERVE.
 */
export const flow: Flow = {
  order: 99.7,
  run: async (page, run) => {
    const stand = process.env["DOCKWELL_TEST_GITHUB"];
    if (!stand) {
      run.step("GitHub App: there's no stand-in for GitHub here, so nothing connects");
      return;
    }
    const path = process.env["DOCKWELL_E2E_GITSERVE"];
    if (!path)
      fail(
        "DOCKWELL_E2E_GITSERVE names no scripts/gitserve build, which the GitHub App flow needs. Run it with make e2e",
      );
    const git = await startGitServer(path, join(run.data, "git-github"));
    try {
      const pointed = await fetch(`${stand}/_repos`, {
        method: "POST",
        body: JSON.stringify({
          git: git.url,
          repositories: [{ full_name: "acme/web", path: "web.git", private: true, default_branch: "main" }],
        }),
      });
      if (!pointed.ok) fail(`GitHub's stand-in didn't take the repositories: ${pointed.status}`);
      await githubAppFlow(page, run, stand, git.url);
    } finally {
      git.stop();
    }
  },
};
