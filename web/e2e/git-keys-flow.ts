// SPDX-License-Identifier: AGPL-3.0-only

// SSH deploy keys and GitLab connection repository picking for private Git (ADR 0113):
// 1. New app -> From Git: paste an SSH repository address, create and copy an
//    Ed25519 deploy key, pin the host key fingerprint, authorize the public key,
//    list branches, build and deploy the app, rotate the key safely from Source,
//    and verify the rotated key deploys a new commit.
// 2. Docker -> Images -> Build: build a standalone Docker image from the SSH
//    repository using the project's deploy key (pack streamed over ReceiveFile).
// 3. GitLab connection: connect with read_api + read_repository (no api scope),
//    pick a private GitLab project from the grouped repository picker in
//    New app -> From Git, build and deploy it, verify Source names the
//    connection, and clean up the temporary connection afterwards.
// 4. Verify 390 px phone layout with zero horizontal overflow.

import { type ChildProcess, spawn } from "node:child_process";
import { join, resolve } from "node:path";

import type { Locator, Page } from "playwright-core";

import { dockerHasImage, dockerRemoveImage } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

interface SshFixture {
  sshAddr: string;
  repoUrl: string;
  hostKeyAlgorithm: string;
  hostKeyFingerprint: string;
  controlUrl: string;
  stop: () => void;
}

function fail(message: string): never {
  throw new Error(message);
}

function planSheet(page: Page): Locator {
  return onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /Apply plan/ }) }));
}

async function startGitSshServer(path: string, dir: string): Promise<SshFixture> {
  const server: ChildProcess = spawn(resolve(path), [dir], { stdio: ["ignore", "pipe", "inherit"] });
  const raw = await new Promise<string>((done, refused) => {
    const silent = setTimeout(() => refused(new Error(`${path} said nothing for 15 s`)), 15_000);
    server.stdout?.once("data", (b: Buffer) => {
      clearTimeout(silent);
      done(b.toString().trim());
    });
    server.once("exit", (code, signal) => {
      clearTimeout(silent);
      refused(new Error(`${path} ended with ${code ?? signal} before printing its address`));
    });
  });
  const info = JSON.parse(raw) as Omit<SshFixture, "stop">;
  return { ...info, stop: () => server.kill("SIGTERM") };
}

async function setAuthorizedKeys(controlUrl: string, keys: string[]): Promise<void> {
  const res = await fetch(`${controlUrl}/_keys`, {
    method: "POST",
    body: keys.join("\n"),
  });
  if (!res.ok) fail(`gitsshserve /_keys returned ${res.status}: ${await res.text()}`);
}

async function addCommit(controlUrl: string): Promise<{ commit: string; subject: string }> {
  const res = await fetch(`${controlUrl}/_commit`, { method: "POST" });
  if (!res.ok) fail(`gitsshserve /_commit returned ${res.status}: ${await res.text()}`);
  return (await res.json()) as { commit: string; subject: string };
}

async function assertFitsPhone(page: Page, where: string): Promise<void> {
  const edge = await pastTheEdge(page);
  if (edge.wider > 0) fail(`${where} overflows 390 px phone viewport by ${edge.wider} px: ${edge.what}`);
}

export const flow: Flow = {
  order: 16,
  run: async (page, run) => {
    const bin = process.env["DOCKWELL_E2E_GITSSHSERVE"];
    if (!bin) fail("DOCKWELL_E2E_GITSSHSERVE is not set. Run e2e with make e2e");
    const ssh = await startGitSshServer(bin, join(run.data, "git-ssh"));
    const builtImages: string[] = [];
    let gitlabConnected = false;
    try {
      const { origin, socket, step, shoot } = run;

      step("New app -> From Git: paste SSH address, generate deploy key, pin host key, test and build");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}/apps/new?source=git`);
      const sheet = page.getByRole("dialog", { name: "New app" });
      await sheet.getByLabel("Repository").fill(ssh.repoUrl);

      const offer = sheet.getByTestId("deploy-key-offer");
      await offer.waitFor();
      await offer.getByRole("button", { name: "Use a deploy key" }).click();

      const setup = sheet.getByTestId("deploy-key-setup");
      await setup.waitFor();
      const pubkey = ((await setup.getByTestId("deploy-key-pubkey").textContent()) ?? "").trim();
      if (!pubkey.startsWith("ssh-ed25519 ")) fail(`unexpected deploy public key: ${pubkey}`);

      // First Test sees unverified host key and shows its SHA-256 fingerprint.
      await setup.getByRole("button", { name: "Test", exact: true }).click();
      await setup.getByText(ssh.hostKeyFingerprint).waitFor();
      await shoot(page, "git-keys-host-key");

      // Accepting host key before adding public key fails with auth error.
      await setup.getByRole("button", { name: "Accept host key and test" }).click();
      await setup.getByText("Connection test failed").waitFor();

      // Add public key to fake SSH host, then Test passes and lists branches.
      await setAuthorizedKeys(ssh.controlUrl, [pubkey]);
      await setup.getByRole("button", { name: "Test", exact: true }).click();
      await sheet.getByTestId("deploy-key-ready").waitFor();

      const branch = sheet.getByRole("combobox", { name: "Branch" });
      await branch.waitFor();
      if ((await branch.locator("option:checked").textContent()) !== "main (default)") {
        fail("SSH repository didn't select main (default)");
      }
      await sheet
        .getByRole("status", { name: "Build" })
        .getByText(/^Dockerfile · busybox:1\.37$/)
        .waitFor();

      // Remove auto-filled port so we don't collide with other flows.
      const removePort = sheet.getByRole("button", { name: "Remove this port" });
      if ((await removePort.count()) > 0) await removePort.click();
      await sheet.getByLabel(/^Name/).fill("ssh-app");
      await sheet.getByRole("button", { name: "Review plan" }).click();

      const plan = planSheet(page);
      await plan.getByText(/with deploy key /).waitFor();
      await plan.getByRole("button", { name: /Apply plan/ }).click();

      const live = page.getByRole("dialog", { name: /^ssh-app is (live|deployed|running)$/ });
      await live.waitFor({ timeout: 60_000 });
      await live.getByRole("button", { name: "Open ssh-app" }).click();
      await page.getByRole("heading", { level: 1, name: "ssh-app" }).waitFor();

      step("App Source: rotate deploy key, switch only after Test passes, and deploy a new commit");
      // Opening a freshly built Git app lands on Deployments; switch to the app's Overview tab for Source.
      await page.locator("main").getByRole("link", { name: "Overview", exact: true }).click();
      await page.getByRole("button", { name: "Manage deploy key" }).click();
      const keySheet = page.getByRole("dialog", { name: /^Deploy key / });
      await keySheet.waitFor();
      await shoot(page, "git-keys-sheet");

      await keySheet.getByRole("button", { name: "Rotate key" }).click();
      const pendingPub = ((await keySheet.getByTestId("deploy-key-pending-pubkey").textContent()) ?? "").trim();
      if (!pendingPub.startsWith("ssh-ed25519 ") || pendingPub === pubkey) {
        fail(`unexpected pending rotated public key: ${pendingPub}`);
      }

      // Test new key before adding it: fails cleanly and does not switch yet.
      await keySheet.getByRole("button", { name: "Test new key" }).click();
      await keySheet.getByText("Connection test failed").waitFor();

      // Authorize new key alongside old key, then Test new key promotes it.
      await setAuthorizedKeys(ssh.controlUrl, [pubkey, pendingPub]);
      await keySheet.getByRole("button", { name: "Test new key" }).click();
      await keySheet.getByText("Deploy key rotated").waitFor();
      await shoot(page, "git-keys-rotated");

      // Remove old key from host so only the rotated key can fetch.
      await setAuthorizedKeys(ssh.controlUrl, [pendingPub]);

      step("Phone layout at 390 px: deploy key sheet and SSH setup fit without horizontal overflow");
      await atWidth(page, { width: 390, height: 844 });
      await assertFitsPhone(page, "Deploy key sheet");
      await shoot(page, "git-keys-phone-sheet");
      await keySheet.getByRole("button", { name: "Close" }).click();
      await keySheet.waitFor({ state: "hidden" });

      // Deploy a new commit using only the rotated key.
      await atWidth(page, { width: 1440, height: 900 });
      const next = await addCommit(ssh.controlUrl);
      await page.getByRole("button", { name: "Deploy", exact: true }).click();
      const deployPlan = planSheet(page);
      await deployPlan
        .getByText(new RegExp(next.commit.slice(0, 7)))
        .first()
        .waitFor();
      await deployPlan.getByRole("button", { name: /Apply plan/ }).click();
      const deployed = page.getByRole("dialog", { name: /^ssh-app is (live|deployed|running)$/ });
      await deployed.waitFor({ timeout: 60_000 });
      await deployed.getByRole("button", { name: "Done" }).click();

      // Check phone layout on New app -> From Git with SSH deploy key prompt.
      await atWidth(page, { width: 390, height: 844 });
      await page.goto(`${origin}/apps/new?source=git`);
      const phoneNew = page.getByRole("dialog", { name: "New app" });
      await phoneNew.getByLabel("Repository").fill("git@github.com:acme/phone-check.git");
      await phoneNew.getByTestId("deploy-key-offer").waitFor();
      await assertFitsPhone(page, "New app SSH deploy key prompt");
      await atWidth(page, { width: 1440, height: 900 });

      step("Docker -> Images -> Build: build a standalone image from the SSH repository with the rotated deploy key");
      const serverPath = await ownServerPath(page, origin);
      await page.goto(`${origin}${serverPath}/docker/images`);
      await page.getByRole("button", { name: "Image actions", exact: true }).click();
      await page.getByRole("menuitem", { name: "Build image", exact: true }).click();
      const buildDialog = onTop(page.getByRole("dialog", { name: "Build image", exact: true }));
      await buildDialog.getByLabel("Source", { exact: true }).selectOption("git");
      // Index 0 is "Public repository"; index 1 is always the default project where ssh-app's key lives.
      await buildDialog.getByLabel("Git access", { exact: true }).selectOption({ index: 1 });
      await buildDialog.getByLabel("Repository", { exact: true }).fill(ssh.repoUrl);
      await buildDialog.getByTestId("deploy-key-ready").waitFor();
      await buildDialog.getByLabel("Dockerfile path").fill("Dockerfile.image");
      await buildDialog.getByLabel("Local image name").fill("fixture/ssh-build");
      await buildDialog.getByRole("button", { name: "Review plan" }).click();
      const dockerPlan = planSheet(page);
      await dockerPlan.getByText(/with deploy key /).waitFor();
      const builtTag = await dockerPlan
        .locator("code")
        .filter({ hasText: /^fixture\/ssh-build:dockwell-/ })
        .textContent();
      if (!builtTag) fail("Docker SSH build plan did not name a unique local output tag");
      builtImages.push(builtTag);
      await dockerPlan.getByRole("button", { name: /Apply plan/ }).click();
      const imageBuilt = onTop(page.getByRole("dialog", { name: "Image built" }));
      await imageBuilt.waitFor({ timeout: 60_000 });
      if (!(await dockerHasImage(socket, builtTag))) {
        fail(`Docker SSH build reported success, but ${builtTag} is not in the Docker engine`);
      }
      await shoot(page, "git-keys-docker-ssh-built");
      await imageBuilt.getByRole("button", { name: "Done" }).click();

      const gl = process.env["DOCKWELL_TEST_GITLAB"];
      if (gl) {
        step("GitLab connection: connect with read_repository + read_api, pick private project, build and deploy");
        const seeded = await fetch(`${gl}/_git-repo`, {
          method: "POST",
          body: JSON.stringify({
            project: "acme/gitlab-private",
            branch: "main",
            extra_branches: ["release"],
            web_url: "https://gitlab.example.com/acme/gitlab-private",
            files: {
              Dockerfile: 'FROM busybox:1.37\nCMD ["sh", "-c", "echo gitlab-e2e && sleep 3600"]\n',
            },
          }),
        });
        if (!seeded.ok) fail(`gltest /_git-repo returned ${seeded.status}: ${await seeded.text()}`);

        await page.goto(`${origin}/settings/integrations`);
        const gitSection = page.getByRole("region", { name: "Git" });
        const glEmpty = gitSection.getByRole("article", { name: "GitLab" });
        await glEmpty.getByRole("button", { name: "Connect" }).click();
        const glSheet = page.getByRole("dialog", { name: "Connect GitLab" });
        await glSheet.getByLabel("Name").fill("gl-e2e");
        await glSheet.getByLabel("GitLab URL").fill("https://gitlab.example.com");
        await glSheet.getByLabel("Access token").fill("glpat-e2e-repo");
        await glSheet.getByRole("button", { name: "Check connection" }).click();
        await glSheet.getByText("Connected to GitLab").waitFor({ timeout: 15_000 });
        await glSheet.getByRole("button", { name: "Review plan" }).click();
        const glPlan = planSheet(page);
        await glPlan.getByRole("button", { name: /Apply plan/ }).click();
        const glApplied = onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /^Done/ }) }));
        await glApplied.getByRole("button", { name: /^Done/ }).waitFor({ timeout: 30_000 });
        await glApplied.getByRole("button", { name: /^Done/ }).click();
        await page.waitForFunction(() => document.querySelectorAll(".dw-sheet-layer").length === 0, undefined, {
          timeout: 15_000,
        });
        gitlabConnected = true;

        const glCard = gitSection.getByRole("article", { name: "GitLab gl-e2e" });
        await glCard.getByText("Connected", { exact: true }).waitFor({ timeout: 15_000 });

        await page.goto(`${origin}/apps/new?source=git`);
        const glNew = page.getByRole("dialog", { name: "New app" });
        const repoField = glNew.getByRole("combobox", { name: "Repository" });
        await repoField.click();
        const repoList = glNew.getByRole("listbox", { name: "Repositories" });
        await repoList.getByRole("group", { name: "GitLab · gl-e2e" }).waitFor({ timeout: 15_000 });
        await repoList.getByRole("option", { name: /acme\/gitlab-private/ }).click();

        const glBranch = glNew.getByRole("combobox", { name: "Branch" });
        await glBranch.waitFor({ timeout: 15_000 });
        if ((await glBranch.locator("option:checked").textContent()) !== "main (default)") {
          fail("GitLab repository didn't select main (default)");
        }
        await glNew
          .getByRole("status", { name: "Build" })
          .getByText(/^Dockerfile · busybox:1\.37$/)
          .waitFor({ timeout: 20_000 });
        const glRemovePort = glNew.getByRole("button", { name: "Remove this port" });
        if ((await glRemovePort.count()) > 0) await glRemovePort.click();
        await glNew.getByLabel(/^Name/).fill("gitlab-app");
        await glNew.getByRole("button", { name: "Review plan" }).click();

        const glAppPlan = planSheet(page);
        await glAppPlan.getByText(/through GitLab connection gl-e2e/).waitFor();
        await shoot(page, "git-keys-gitlab-plan");
        await glAppPlan.getByRole("button", { name: /Apply plan/ }).click();

        const glLive = page.getByRole("dialog", { name: /^gitlab-app is (live|deployed|running)$/ });
        await glLive.waitFor({ timeout: 60_000 });
        await glLive.getByRole("button", { name: "Open gitlab-app" }).click();
        await page.getByRole("heading", { level: 1, name: "gitlab-app" }).waitFor();
        await page.locator("main").getByRole("link", { name: "Overview", exact: true }).click();
        await page.getByText("through GitLab connection gl-e2e").waitFor({ timeout: 15_000 });
        await shoot(page, "git-keys-gitlab-source");

        // Remove the temporary GitLab connection so later flows (github-app, ci-status)
        // start with a clean GitLab state.
        await page.goto(`${origin}/settings/integrations`);
        const cleanupGitSection = page.getByRole("region", { name: "Git" });
        const cleanupCard = cleanupGitSection.getByRole("article", { name: "GitLab gl-e2e" });
        await cleanupCard.waitFor({ timeout: 15_000 });
        await cleanupCard.getByRole("button", { name: "Manage" }).click();
        const manage = page.getByRole("dialog", { name: "GitLab connection gl-e2e" });
        await manage.getByRole("button", { name: "Remove" }).click();
        await manage.getByRole("button", { name: "Review removal" }).click();
        const rmPlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
        await rmPlan.getByRole("button", { name: /^Delete gl-e2e/ }).click();
        const rmDone = onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /^Done/ }) }));
        await rmDone.getByRole("button", { name: /^Done/ }).waitFor({ timeout: 30_000 });
        await rmDone.getByRole("button", { name: /^Done/ }).click();
        await page.waitForFunction(() => document.querySelectorAll(".dw-sheet-layer").length === 0, undefined, {
          timeout: 15_000,
        });
        await cleanupGitSection.getByRole("article", { name: "GitLab", exact: true }).waitFor({ timeout: 15_000 });
        gitlabConnected = false;
      }
    } finally {
      if (gitlabConnected) {
        try {
          await atWidth(page, { width: 1440, height: 900 });
          await page.goto(`${run.origin}/settings/integrations`);
          const gitSection = page.getByRole("region", { name: "Git" });
          const glCard = gitSection.getByRole("article", { name: "GitLab gl-e2e" });
          await glCard.waitFor({ timeout: 10_000 });
          await glCard.getByRole("button", { name: "Manage" }).click();
          const manage = page.getByRole("dialog", { name: "GitLab connection gl-e2e" });
          await manage.getByRole("button", { name: "Remove" }).click();
          await manage.getByRole("button", { name: "Review removal" }).click();
          const rmPlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
          await rmPlan.getByRole("button", { name: /^Delete gl-e2e/ }).click();
          const rmDone = onTop(page.getByRole("dialog").filter({ has: page.getByRole("button", { name: /^Done/ }) }));
          await rmDone.getByRole("button", { name: /^Done/ }).waitFor({ timeout: 30_000 });
          await rmDone.getByRole("button", { name: /^Done/ }).click();
        } catch {
          // Best-effort cleanup if the flow failed mid-way.
        }
      }
      for (const img of builtImages) await dockerRemoveImage(run.socket, img).catch(() => undefined);
      ssh.stop();
    }
  },
};
