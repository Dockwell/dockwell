// SPDX-License-Identifier: AGPL-3.0-only

// SSH deploy keys for private Git (ADR 0113): paste an SSH repository address
// in New app -> From Git, create and copy an Ed25519 deploy key, test against
// a local fake SSH Git host, accept the host key fingerprint, authorize the
// public key, list branches, build and deploy the app, rotate the key safely
// from the Source panel, verify the rotated key deploys a new commit, and check
// phone layout at 390 px with zero horizontal overflow.

import { type ChildProcess, spawn } from "node:child_process";
import { join, resolve } from "node:path";

import type { Locator, Page } from "playwright-core";

import type { Flow } from "./flows.ts";
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
    try {
      const { origin, step, shoot } = run;

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
    } finally {
      ssh.stop();
    }
  },
};
