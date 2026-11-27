// SPDX-License-Identifier: AGPL-3.0-only

// CI deploy status reporting (ADR 0111, issue #948): exercises token-bound
// status repositories in Deploy from CI and Settings -> API tokens, missing
// GitHub Checks permission on Settings -> Integrations, granting & refreshing
// it, retrying a failed/skipped deploy status from Deployments, connecting a
// GitLab instance with live token scope checks and automatic post-save check,
// and verifying Check Runs + GitLab Commit Statuses (env-scoped names, no
// private listen URL leak) + Deployments/Activity UI links at 1440 and 390.

import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { atWidth, onTop } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

async function pollUntil<T>(label: string, fn: () => Promise<T | undefined>, timeoutMs = 30_000): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const got = await fn();
    if (got !== undefined) return got;
    await new Promise((r) => setTimeout(r, 250));
  }
  fail(`timed out waiting for ${label}`);
}

export const flow: Flow = {
  order: 99.8,
  run: async (page, { origin, step, shoot, bin, data, env }) => {
    const gh = process.env["DOCKWELL_TEST_GITHUB"];
    const gl = process.env["DOCKWELL_TEST_GITLAB"];
    if (!gh || !gl) {
      step("CI status: stand-in GitHub/GitLab servers unavailable, skipping");
      return;
    }

    const shotDir = process.env["DOCKWELL_CI_STATUS_SHOTS"];
    if (shotDir) mkdirSync(shotDir, { recursive: true });
    const saveShot = async (p: Page, name: string) => {
      await shoot(p, name);
      if (shotDir) await p.screenshot({ path: join(shotDir, `${name}.png`), fullPage: false });
    };

    const config = join(data, "ci-status-cli");
    const cli = (args: string[], token = "") =>
      execFileSync(bin, [...args, "--data-dir", data], {
        encoding: "utf8",
        timeout: 30_000,
        env: {
          ...process.env,
          ...env,
          USER: "mira",
          DOCKWELL_URL: token ? origin : "",
          DOCKWELL_TOKEN: token,
          XDG_CONFIG_HOME: config,
        },
      });
    cli(["login", "--local"]);
    const fleet = JSON.parse(cli(["servers", "list", "--json"])) as {
      servers: { id: string; controlServer: boolean }[];
    };
    const server = fleet.servers.find((s) => s.controlServer)?.id;
    if (!server) fail("CI status needs the control server ID");

    const project = "ci-status-fixture";
    cli(["projects", "create", project, "--yes"]);
    cli(["environments", "create", "ci", "--project", project, "--yes"]);
    cli([
      "services",
      "create",
      "status-web",
      "--project",
      project,
      "--environment",
      "ci",
      "--on",
      server,
      "--app-image",
      "nginx:1.26",
      "--yes",
    ]);
    const apps = JSON.parse(cli(["services", "list", "--json"])) as {
      services: { id: string; resource: { name: string } }[];
    };
    const app = apps.services.find((s) => s.resource.name === "status-web")?.id;
    if (!app) fail("status-web isn't listed");

    const deploymentsUrl = `${origin}/apps/${app}/deployments`;

    try {
      await atWidth(page, { width: 1440, height: 900 });
      // Ensure the fake GitHub App on acme with acme/web is connected first so
      // Deploy from CI offers acme/web in its Status repository picker.
      await page.goto(`${origin}/settings/integrations`);
      const gitRegion = page.getByRole("region", { name: "Git" });
      const ghInitial = gitRegion.getByRole("article", { name: "GitHub" });
      await ghInitial.waitFor({ timeout: 20_000 });
      if ((await ghInitial.getByText("Not connected").count()) > 0) {
        const pointed = await fetch(`${gh}/_repos`, {
          method: "POST",
          body: JSON.stringify({
            git: "http://127.0.0.1:1",
            repositories: [{ full_name: "acme/web", path: "web.git", private: true, default_branch: "main" }],
          }),
        });
        if (!pointed.ok) fail(`couldn't seed acme/web on ghtest: ${pointed.status}`);
        await ghInitial.getByRole("button", { name: "Connect" }).click();
        const ghConnect = page.getByRole("dialog", { name: "Connect GitHub" });
        await ghConnect.getByRole("radio", { name: "An organization" }).click();
        await ghConnect.getByLabel("Organization").fill("acme");
        await ghConnect.getByRole("button", { name: "Create on GitHub" }).click();
        await ghInitial.getByText("GitHub App on acme · 1 repository").waitFor({ timeout: 60_000 });
        await ghInitial.getByText("Connected", { exact: true }).waitFor({ timeout: 20_000 });
      }

      step("CI status: create a GitHub deploy token bound to acme/web from Deploy from CI");
      await page.goto(deploymentsUrl);
      await page.getByRole("button", { name: "Deploy from CI" }).click();
      const ciSheet = page.getByRole("dialog", { name: "Deploy from CI" });
      const ghRepoSelect = ciSheet.getByLabel("Status repository");
      await ghRepoSelect.waitFor({ timeout: 15_000 });
      await ghRepoSelect.selectOption({ label: "acme/web" });
      await ciSheet.getByRole("button", { name: "Create deploy token" }).click();
      await ciSheet.getByRole("button", { name: "Show the whole token" }).click();
      const ghToken = (await ciSheet.locator(".ci-deploy-secret").innerText()).trim();
      await ciSheet.getByRole("button", { name: "Done" }).click();
      await ciSheet.waitFor({ state: "hidden" });

      step("CI status: remove GitHub checks:write permission and deploy to surface missing permission + Retry");
      const revoke = await fetch(`${gh}/_permissions`, {
        method: "POST",
        body: JSON.stringify({ contents: "read", metadata: "read" }),
      });
      if (!revoke.ok) fail(`couldn't revoke checks permission on ghtest: ${revoke.status}`);

      const ghCommit = "c".repeat(40);
      const ghDeploy = JSON.parse(
        cli(
          [
            "services",
            "deploy",
            app,
            "--image",
            "nginx:1.27",
            "--wait",
            "--json",
            "--deploy-origin",
            JSON.stringify({
              repository: "https://github.com/acme/web",
              commit: ghCommit,
              ref: "refs/heads/main",
              runUrl: "https://github.com/acme/web/actions/runs/501",
              provider: "DEPLOY_PROVIDER_GITHUB_ACTIONS",
            }),
          ],
          ghToken,
        ).split("\n")[0] ?? "{}",
      ) as { change?: { id?: string } };
      const ghChangeId = ghDeploy.change?.id;
      if (!ghChangeId) fail("GitHub CI deploy returned no change ID");

      // Wait for Deployments to display the localized missing Checks permission reason + Retry button.
      await page.goto(deploymentsUrl);
      await pollUntil("missing Checks permission reason on Deployments", async () => {
        const retryBtn = page.getByRole("button", { name: "Retry" }).first();
        if ((await retryBtn.count()) > 0 && (await retryBtn.isVisible())) return true;
        await page.reload();
        return undefined;
      });
      await page
        .getByText(/needs Checks permission/)
        .first()
        .waitFor({ timeout: 15_000 });
      await saveShot(page, "deployments-retry-1440");
      await atWidth(page, { width: 390, height: 844 });
      await page.getByRole("button", { name: "Retry" }).first().waitFor({ timeout: 15_000 });
      await saveShot(page, "deployments-retry-390");

      step("CI status: Settings -> Integrations flags missing Checks permission and clears after refresh");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/settings/integrations`);
      const gitSection = page.getByRole("region", { name: "Git" });
      const ghCard = gitSection.getByRole("article", { name: "GitHub" });
      await ghCard.getByText("Missing Checks").waitFor({ timeout: 20_000 });
      await ghCard.getByRole("link", { name: "Update permissions on GitHub" }).waitFor();
      await saveShot(page, "integrations-github-missing-1440");
      await atWidth(page, { width: 390, height: 844 });
      await ghCard.getByText("Missing Checks").waitFor({ timeout: 15_000 });
      await saveShot(page, "integrations-github-missing-390");

      // Grant checks:write on fake GitHub and click Check again.
      const grant = await fetch(`${gh}/_permissions`, {
        method: "POST",
        body: JSON.stringify({ contents: "read", metadata: "read", checks: "write" }),
      });
      if (!grant.ok) fail(`couldn't grant checks permission on ghtest: ${grant.status}`);
      await atWidth(page, { width: 1440, height: 900 });
      await ghCard.getByRole("button", { name: "Check again" }).click();
      await ghCard.getByText("Connected", { exact: true }).waitFor({ timeout: 20_000 });

      step("CI status: Retry from Deployments delivers the GitHub Check Run");
      await page.goto(deploymentsUrl);
      await page.getByRole("button", { name: "Retry" }).first().click();
      const runs = await pollUntil("GitHub check run on ghtest", async () => {
        const res = await fetch(`${gh}/_check-runs`);
        const list = ((await res.json()) ?? []) as {
          name: string;
          status: string;
          conclusion: string;
          details_url: string;
          external_id: string;
        }[];
        const match = list.find(
          (r) => r.external_id === ghChangeId && r.status === "completed" && r.conclusion === "success",
        );
        return match ? list : undefined;
      });
      const ghRun = runs.find((r) => r.external_id === ghChangeId);
      // M2: env-scoped check name; L2: no private listen address leaked when panel_domain is unset.
      if (ghRun?.name !== "Dockwell / ci / status-web" || ghRun.details_url.includes("127.0.0.1")) {
        fail(`unexpected GitHub check run: ${JSON.stringify(ghRun)}`);
      }
      await page.reload();
      await page.getByRole("link", { name: "Reported to GitHub" }).first().waitFor({ timeout: 15_000 });

      step("CI status: connect GitLab in Settings -> Integrations with token scope check");
      await page.goto(`${origin}/settings/integrations`);
      const glEmpty = gitSection.getByRole("article", { name: "GitLab" });
      await glEmpty.getByRole("button", { name: "Connect" }).click();
      const glSheet = page.getByRole("dialog", { name: "Connect GitLab" });
      await glSheet.getByLabel("Name").fill("self-managed");
      await glSheet.getByLabel("GitLab URL").fill("https://gitlab.example.com");
      await glSheet.getByLabel("Access token").fill("glpat-e2e-read");
      await glSheet.getByRole("button", { name: "Check connection" }).click();
      await glSheet.getByText("Missing api scope").waitFor({ timeout: 15_000 });

      await glSheet.getByLabel("Access token").fill("glpat-e2e-api");
      await glSheet.getByRole("button", { name: "Check connection" }).click();
      await glSheet.getByText("Connected to GitLab").waitFor({ timeout: 15_000 });
      await glSheet.getByRole("button", { name: "Review plan" }).click();
      const plan = onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /Apply plan/ }) }));
      await plan.getByRole("button", { name: /Apply plan/ }).click();
      const applied = onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /^Done/ }) }));
      await applied.getByRole("button", { name: /^Done/ }).waitFor({ timeout: 30_000 });
      await applied.getByRole("button", { name: /^Done/ }).click();
      await page.waitForFunction(() => document.querySelectorAll(".dw-sheet-layer").length === 0, undefined, {
        timeout: 15_000,
      });

      // N4: post-save check runs automatically so the card shows Connected right away.
      const glCard = gitSection.getByRole("article", { name: "GitLab self-managed" });
      await glCard.getByText("Connected", { exact: true }).waitFor({ timeout: 15_000 });
      await saveShot(page, "integrations-connected-1440");
      await atWidth(page, { width: 390, height: 844 });
      await glCard.getByText("Connected", { exact: true }).waitFor({ timeout: 15_000 });
      await saveShot(page, "integrations-connected-390");

      step("CI status: create a GitLab deploy token bound to acme/web and deploy via GitLab CI");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(deploymentsUrl);
      await page.getByRole("button", { name: "Deploy from CI" }).click();
      const glCiSheet = page.getByRole("dialog", { name: "Deploy from CI" });
      await glCiSheet.getByRole("radio", { name: "GitLab CI" }).click();
      const glRepoSelect = glCiSheet.getByLabel("Status repository");
      await glRepoSelect.waitFor({ timeout: 15_000 });
      await glRepoSelect.selectOption({ label: "acme/web" });
      await glCiSheet.getByRole("button", { name: "Create deploy token" }).click();
      await glCiSheet.getByRole("button", { name: "Show the whole token" }).click();
      const glToken = (await glCiSheet.locator(".ci-deploy-secret").innerText()).trim();
      await glCiSheet.getByRole("button", { name: "Done" }).click();
      await glCiSheet.waitFor({ state: "hidden" });

      const glCommit = "d".repeat(40);
      const glDeploy = JSON.parse(
        cli(
          [
            "services",
            "deploy",
            app,
            "--image",
            "nginx:1.28",
            "--wait",
            "--json",
            "--deploy-origin",
            JSON.stringify({
              repository: "https://gitlab.example.com/acme/web",
              commit: glCommit,
              ref: "refs/heads/main",
              runUrl: "https://gitlab.example.com/acme/web/-/jobs/77",
              provider: "DEPLOY_PROVIDER_GITLAB_CI",
            }),
          ],
          glToken,
        ).split("\n")[0] ?? "{}",
      ) as { change?: { id?: string } };
      const glChangeId = glDeploy.change?.id;
      if (!glChangeId) fail("GitLab CI deploy returned no change ID");

      const statuses = await pollUntil("GitLab commit status on gltest", async () => {
        const res = await fetch(`${gl}/_statuses`);
        const list = ((await res.json()) ?? []) as {
          project: string;
          sha: string;
          state: string;
          name: string;
          target_url: string;
        }[];
        const match = list.find((s) => s.sha === glCommit && s.state === "success");
        return match ? list : undefined;
      });
      const glSuccess = statuses.find((s) => s.sha === glCommit && s.state === "success");
      if (
        glSuccess?.name !== "Dockwell / ci / status-web" ||
        glSuccess.project !== "acme/web" ||
        glSuccess.target_url.includes("127.0.0.1")
      ) {
        fail(`unexpected GitLab commit status: ${JSON.stringify(glSuccess)}`);
      }

      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(deploymentsUrl);
      await page.getByRole("link", { name: "Reported to GitLab" }).first().waitFor({ timeout: 15_000 });
      await saveShot(page, "deployments-reported-1440");
      await atWidth(page, { width: 390, height: 844 });
      await page.getByRole("link", { name: "Reported to GitLab" }).first().waitFor({ timeout: 15_000 });
      await saveShot(page, "deployments-reported-390");

      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/activity`);
      await page.getByRole("link", { name: "Reported to GitLab" }).first().waitFor({ timeout: 15_000 });
      await saveShot(page, "activity-reported-1440");
      await atWidth(page, { width: 390, height: 844 });
      await page.getByRole("link", { name: "Reported to GitLab" }).first().waitFor({ timeout: 15_000 });
      await saveShot(page, "activity-reported-390");
    } finally {
      // Restore full GitHub permissions and remove the fixture project.
      await fetch(`${gh}/_permissions`, {
        method: "POST",
        body: JSON.stringify({ contents: "read", metadata: "read", checks: "write" }),
      }).catch(() => {});
      try {
        cli(["projects", "delete", project, "--with-contents", "--yes"]);
      } catch {
        // Ignore cleanup errors if the flow failed before creating the project.
      }
    }
  },
};
