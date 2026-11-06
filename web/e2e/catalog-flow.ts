// SPDX-License-Identifier: AGPL-3.0-only

// The catalog end to end, on the fake Docker Engine of scripts/fakedocker: the rail
// to the catalog, an app through its form and its plan, and onto the working app with
// a way to reach it, what to do first and where its secrets are. Then an app that
// needs a token and reaches past its containers, and a newer release of an app, which
// the panel says is out, through its plan and its undo. scripts/docker-e2e.sh runs
// these apps on real Docker.

import { writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Locator, Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";

export interface CatalogFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  /** Says what Docker itself runs. */
  docker: DockerProbe;
  /** The directory the control plane reads more entries from, for a release newer than the first. */
  catalogDir: string;
}

const FIRST_DIGEST = `sha256:${"1".repeat(64)}`;
const SECOND_DIGEST = `sha256:${"2".repeat(64)}`;

/** A release of an entry that exists only to be upgraded. */
function release(number: number, version: string, digest: string): string {
  return `id: e2e-notes
name: E2E Notes
summary: Something to upgrade
category: storage
version: "${version}"
release: ${number}
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18300"
compose: |
  services:
    web:
      image: notes:${version}@${digest}
      ports:
        - "\${PORT}:80"
      volumes:
        - data:/data
  volumes:
    data: {}
`;
}

function fail(message: string): never {
  throw new Error(message);
}

/** Says where an app runs once there's a choice: a server that's connected, as the flow before this leaves another down. */
async function chooseServer(sheet: Locator) {
  const servers = sheet.getByRole("group", { name: "Runs on" });
  if ((await servers.count()) === 0) return;
  await servers.locator("label", { hasNotText: "Offline" }).first().getByRole("checkbox").check();
}

export async function catalogFlow(page: Page, { origin, step, shoot, docker, catalogDir }: CatalogFlowOptions) {
  step("catalog: the rail leads to it, and it lists the first set");
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Catalog" }).click();
  await page.getByRole("heading", { level: 1, name: "Catalog" }).waitFor();
  for (const app of ["PostgreSQL", "MySQL", "Nginx Proxy Manager", "Cloudflare Tunnel", "T3 Code"]) {
    await page.getByRole("link", { name: `Install ${app}` }).waitFor();
  }
  await shoot(page, "catalog");

  step("catalog: a category and a word each narrow it, and the URL keeps them");
  const chips = page.getByRole("group", { name: "Categories" });
  await chips.getByRole("button", { name: "Networking" }).click();
  await page.getByRole("link", { name: "Install PostgreSQL" }).waitFor({ state: "hidden" });
  await page.getByRole("link", { name: "Install Cloudflare Tunnel" }).waitFor();
  if (!page.url().includes("category=networking")) fail(`the URL doesn't keep the category: ${page.url()}`);
  await chips.getByRole("button", { name: "All" }).click();
  await page.getByRole("textbox", { name: "Search apps" }).fill("relational");
  await page.getByRole("link", { name: "Install Nginx Proxy Manager" }).waitFor({ state: "hidden" });
  await page.getByRole("link", { name: "Install MySQL" }).waitFor();
  await page.getByRole("textbox", { name: "Search apps" }).fill("");

  step("catalog: installing asks only what the app needs, starting from what's free");
  await page.getByRole("link", { name: "Install PostgreSQL" }).click();
  const sheet = page.getByRole("dialog", { name: "Install PostgreSQL" });
  await sheet.waitFor();
  await sheet.getByText("Version 17.11").waitFor();
  const port = await sheet.getByLabel("Adminer port").inputValue();
  if (!/^\d+$/.test(port)) fail(`Adminer's port starts as "${port}"`);
  if ((await sheet.getByLabel("Name").inputValue()) !== "postgres") fail("its name doesn't start as postgres");
  if ((await sheet.getByLabel("Database password").count()) !== 0) fail("it asks for a password it generates");
  await sheet.getByText(/Dockwell generates Database password and keeps it sealed/).waitFor();
  await shoot(page, "catalog-install");

  step("catalog: its plan shows everything it makes, with the password as a secret and no value");
  await chooseServer(sheet);
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  for (const name of ["POSTGRES_PASSWORD", "adminer", "db"])
    await plan.getByText(name, { exact: true }).first().waitFor();
  await shoot(page, "catalog-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();

  step("catalog: it lands on the working app, with a way to reach it");
  const open = page.getByRole("button", { name: "Open PostgreSQL", exact: true });
  await open.waitFor({ timeout: 120_000 });
  await shoot(page, "catalog-arrival");
  await open.click();
  await page.getByRole("heading", { level: 1, name: "adminer", exact: true }).waitFor();
  await page.getByRole("link", { name: new RegExp(`Open .*:${port}`) }).waitFor({ timeout: 30_000 });
  const from = page.getByRole("region", { name: "From the catalog" });
  await from.getByText("PostgreSQL 17.11").waitFor();
  await from.getByText(/with the password in the POSTGRES_PASSWORD secret/).waitFor();
  await shoot(page, "catalog-app");
  const running = (await docker()).filter((c) => c.service === "adminer").map((c) => c.image);
  if (!running.some((image) => /^adminer:.*@sha256:/.test(image))) {
    fail(`Docker runs ${running.join(", ")} for adminer, not pinned by digest`);
  }

  step("catalog: its secret is sealed, and one step away");
  await from.getByRole("link", { name: "Show its secrets" }).click();
  await page.getByRole("heading", { level: 1, name: "Variables and secrets" }).waitFor();
  await page.getByText("POSTGRES_PASSWORD", { exact: true }).first().waitFor();

  step("catalog: an installed app says so, and leads to the app");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Catalog" }).click();
  const card = page.getByRole("link", { name: "Open PostgreSQL" });
  await card.waitFor();
  await card.getByText("Installed").waitFor();
  await page.getByRole("link", { name: "1 installed" }).waitFor();

  step("catalog: what only a person has is asked for out of sight, and what an app reaches is said first");
  await page.getByRole("link", { name: "Install Cloudflare Tunnel" }).click();
  const tunnel = page.getByRole("dialog", { name: "Install Cloudflare Tunnel" });
  await tunnel.getByText("It reaches past its containers").waitFor();
  if (!(await tunnel.getByRole("button", { name: /Review plan/ }).isDisabled())) fail("it plans with no token");
  await tunnel.getByLabel("Tunnel token").fill("not-a-real-token");
  await chooseServer(tunnel);
  await tunnel.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("not-a-real-token").waitFor({ state: "hidden" });
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await tunnel.getByRole("button", { name: /Cancel/ }).click();
  await page.getByRole("heading", { level: 1, name: "Catalog" }).waitFor();

  step("catalog: a newer release: the panel says so where you'd look for it");
  const entry = join(catalogDir, "e2e-notes.yaml");
  writeFileSync(entry, release(1, "1.0", FIRST_DIGEST));
  await page.goto(`${origin}/catalog/e2e-notes`);
  const notes = page.getByRole("dialog", { name: "Install E2E Notes" });
  await notes.getByLabel("Port").waitFor();
  await chooseServer(notes);
  await notes.getByRole("button", { name: /Review plan/ }).click();
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("button", { name: "Open E2E Notes", exact: true }).click({ timeout: 120_000 });
  const panel = page.getByRole("region", { name: "From the catalog" });
  await panel.getByText("E2E Notes 1.0").waitFor();
  if ((await panel.getByRole("button", { name: /Upgrade to/ }).count()) !== 0)
    fail("the newest release offers an upgrade");
  writeFileSync(entry, release(2, "2.0", SECOND_DIGEST));
  await page.reload();
  const upgrade = panel.getByRole("button", { name: "Upgrade to 2.0" });
  await upgrade.waitFor();
  await panel.getByText("Version 2.0 is out.").waitFor();
  await page
    .getByRole("navigation", { name: "Main" })
    .getByRole("link", { name: /Catalog/ })
    .getByText("1")
    .waitFor();
  await shoot(page, "catalog-upgrade");

  step("catalog: upgrading is a plan that says no backup is taken, and it has an undo");
  await upgrade.click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("No backup is taken first").waitFor();
  await shoot(page, "catalog-upgrade-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const undo = page.getByRole("button", { name: /Undo/ });
  await undo.waitFor({ timeout: 120_000 });
  const now = (await docker()).filter((c) => c.image.startsWith("notes:")).map((c) => c.image);
  if (now.length !== 1 || !now[0]?.includes(SECOND_DIGEST)) fail(`after the upgrade, Docker runs ${now.join(", ")}`);
  await undo.click();
  await page
    .getByRole("dialog", { name: "Review plan" })
    .getByRole("button", { name: /Apply plan/ })
    .click();
  await page.getByRole("button", { name: /^Done/ }).click({ timeout: 120_000 });
  await panel.getByRole("button", { name: "Upgrade to 2.0" }).waitFor({ timeout: 30_000 });
  const back = (await docker()).filter((c) => c.image.startsWith("notes:")).map((c) => c.image);
  if (back.length !== 1 || !back[0]?.includes(FIRST_DIGEST)) fail(`after the undo, Docker runs ${back.join(", ")}`);
}
