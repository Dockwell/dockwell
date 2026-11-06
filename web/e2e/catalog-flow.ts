// SPDX-License-Identifier: AGPL-3.0-only

// The catalog end to end, on the fake Docker Engine of scripts/fakedocker: the rail
// to the catalog, an app through its form and its plan, and onto the working app with
// a way to reach it, what to do first and where its secrets are. Then an app that
// needs a token and reaches past its containers. scripts/docker-e2e.sh runs these
// apps on real Docker.

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

export async function catalogFlow(page: Page, { origin, step, shoot, docker }: CatalogFlowOptions) {
  step("catalog: the rail leads to it, and it lists the first set");
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Catalog" }).click();
  await page.getByRole("heading", { level: 1, name: "Catalog" }).waitFor();
  for (const app of ["PostgreSQL", "MySQL", "Nginx Proxy Manager", "Cloudflare Tunnel", "n8n"]) {
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
  await page.getByRole("link", { name: "Install n8n" }).waitFor({ state: "hidden" });
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
}
