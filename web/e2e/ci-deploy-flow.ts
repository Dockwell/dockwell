// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { atWidth } from "./sheets.ts";

export const flow: Flow = {
  order: 91.5,
  run: async (page, { origin, step, shoot, bin, data, env }) => {
    const config = join(data, "ci-deploy-cli");
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
    if (!server) throw new Error("CI deploy needs the control server ID");
    const project = "ci-deploy-fixture";
    cli(["projects", "create", project, "--yes"]);
    cli(["environments", "create", "ci", "--project", project, "--yes"]);
    cli([
      "services",
      "create",
      "ci-web",
      "--project",
      project,
      "--environment",
      "ci",
      "--on",
      server,
      "--app-image",
      "nginx:1.27",
      "--yes",
    ]);
    const apps = JSON.parse(cli(["services", "list", "--json"])) as {
      services: { id: string; resource: { name: string } }[];
    };
    const app = apps.services.find((s) => s.resource.name === "ci-web")?.id;
    if (!app) throw new Error("the CI app isn't listed");

    step("CI deploy: create a deploy-only token and copy either workflow from the app");
    await atWidth(page, { width: 1440, height: 900 });
    const deployments = `${origin}/apps/${app}/deployments`;
    await page.goto(deployments);
    await page.getByRole("button", { name: "Deploy from CI" }).click();
    const sheet = page.getByRole("dialog", { name: "Deploy from CI" });
    const release = sheet.getByRole("textbox", { name: "Release tag" });
    if (await release.isVisible()) await release.fill("v0.4.0");
    await sheet.getByRole("button", { name: "Create deploy token" }).click();
    await sheet.getByRole("button", { name: "Copy token" }).waitFor();
    const workflow = sheet.locator("pre");
    const github = await workflow.innerText();
    if (!github.includes("Dockwell/dockwell/ci/github@") || !github.includes(app) || !github.includes(origin)) {
      throw new Error("the workflow is missing the action, service or panel address");
    }
    await shoot(page, "ci-deploy-1440");
    await atWidth(page, { width: 390, height: 844 });
    await sheet.getByRole("radio", { name: "GitLab CI" }).click();
    const gitlab = await workflow.innerText();
    if (!gitlab.includes("ci/gitlab/dockwell-deploy.yml") || !gitlab.includes("$DOCKWELL_DEPLOY_TOKEN")) {
      throw new Error("the GitLab workflow is missing the template or secret reference");
    }
    await shoot(page, "ci-deploy-390");
    await sheet.getByRole("button", { name: "Show the whole token" }).click();
    const token = (await sheet.locator(".ci-deploy-secret").innerText()).trim();
    if (!/^dwt_[0-9A-Za-z]{49}$/.test(token)) throw new Error("the deploy token has the wrong shape");
    await sheet.getByRole("button", { name: "Done" }).click();
    await sheet.waitFor({ state: "hidden" });

    step("CI deploy: deploy the prebuilt image and retry the same change");
    const commit = "b".repeat(40);
    const runURL = "https://github.com/acme/web/actions/runs/42";
    const deployArgs = [
      "services",
      "deploy",
      app,
      "--image",
      "nginx:1.28",
      "--wait",
      "--json",
      "--deploy-origin",
      JSON.stringify({
        repository: "https://github.com/acme/web",
        commit,
        ref: "refs/heads/main",
        runUrl: runURL,
        provider: "DEPLOY_PROVIDER_GITHUB_ACTIONS",
      }),
    ];
    const changeId = (output: string) =>
      (JSON.parse(output.split("\n")[0] ?? "{}") as { change?: { id?: string } }).change?.id;
    const original = changeId(cli(deployArgs, token));
    if (!original || changeId(cli(deployArgs, token)) !== original) throw new Error("retry planned another deploy");

    step("CI deploy: read the commit and run in Deployments and Activity");
    await page.goto(deployments);
    const commitLink = page.getByRole("link", { name: commit.slice(0, 7), exact: true }).first();
    await commitLink.waitFor();
    if ((await commitLink.getAttribute("href")) !== `https://github.com/acme/web/commit/${commit}`) {
      throw new Error("the deployment didn't link to its GitHub commit");
    }
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/activity`);
    const runLink = page.getByRole("link", { name: "CI run", exact: true }).first();
    await runLink.waitFor();
    if ((await runLink.getAttribute("href")) !== runURL) throw new Error("Activity didn't link to the CI run");
    cli(["projects", "delete", project, "--with-contents", "--yes"]);
  },
};
