// SPDX-License-Identifier: AGPL-3.0-only

// The catalog end to end, on the fake Docker Engine of scripts/fakedocker: the rail
// to the catalog, an app through its form and its plan, and onto the working app with
// a way to reach it, what to do first and where its secrets are. Then an app that
// needs a token and reaches past its containers, and a newer release of an app, which
// the panel says is out, through its plan and its undo. scripts/docker-e2e.sh runs
// these apps on real Docker. Last, its sources: Dockwell's own, the stores one click away, its apps filtered,
// and a repository refused for its address.

import { writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Locator, Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";

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
  step("catalog: the rail leads to it, and it lists the apps, n8n and pgAdmin among them");
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Catalog" }).click();
  await page.getByRole("heading", { level: 1, name: "Catalog" }).waitFor();
  for (const app of ["PostgreSQL", "MySQL", "Nginx Proxy Manager", "Cloudflare Tunnel", "T3 Code", "pgAdmin", "n8n"]) {
    await page.getByRole("link", { name: new RegExp(`^${app}\\b`) }).waitFor();
  }
  await shoot(page, "catalog");

  step("catalog: a category and a word each narrow it, and the URL keeps them");
  const chips = page.getByRole("group", { name: "Categories" });
  await chips.getByRole("button", { name: "Networking" }).click();
  await page.getByRole("link", { name: /^PostgreSQL\b/ }).waitFor({ state: "hidden" });
  await page.getByRole("link", { name: /^Cloudflare Tunnel\b/ }).waitFor();
  if (!page.url().includes("category=networking")) fail(`the URL doesn't keep the category: ${page.url()}`);
  await chips.getByRole("button", { name: "All" }).click();
  await page.getByRole("textbox", { name: "Search apps" }).fill("relational");
  await page.getByRole("link", { name: /^Nginx Proxy Manager\b/ }).waitFor({ state: "hidden" });
  await page.getByRole("link", { name: /^MySQL\b/ }).waitFor();
  await page.getByRole("textbox", { name: "Search apps" }).fill("");

  step("catalog: browse an app's logo, description and real preview before entering installation");
  let installRequests = 0;
  const countInstall = (request: { url(): string }) => {
    if (request.url().endsWith("/InstallCatalogEntry")) installRequests++;
  };
  page.on("request", countInstall);
  await page.getByRole("textbox", { name: "Search apps" }).fill("PostgreSQL");
  await page.getByRole("link", { name: /^PostgreSQL\b/ }).click();
  await page.getByRole("heading", { level: 1, name: "PostgreSQL" }).waitFor();
  if (await page.getByRole("dialog").count()) fail("browsing an app opened an install form");
  await page.getByRole("heading", { name: "About PostgreSQL" }).waitFor();
  await page.getByRole("link", { name: "Documentation", exact: true }).waitFor();
  const preview = page.getByRole("img", { name: "Table browser in Adminer, which you can add with this install" });
  await preview.scrollIntoViewIfNeeded();
  await preview.evaluate((image: HTMLImageElement) => image.decode());
  const logo = page.locator(".catalog-detail .catalog-icon img").first();
  await logo.evaluate((image: HTMLImageElement) => image.decode());
  await shoot(page, "catalog-detail");
  await page.getByRole("link", { name: "Back to catalog" }).click();
  if ((await page.getByRole("textbox", { name: "Search apps" }).inputValue()) !== "PostgreSQL")
    fail("returning from app details lost the catalog search");

  step("catalog: an app's direct address works on desktop and phone");
  await page.goto(`${origin}/catalog/postgres`);
  await page.getByRole("heading", { name: "About PostgreSQL" }).waitFor();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("link", { name: "Install PostgreSQL" }).waitFor();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
  if (overflow) fail("the app page overflows the phone's viewport");
  await shoot(page, "catalog-detail-phone");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.getByRole("link", { name: "Install PostgreSQL" }).click();
  const direct = page.getByRole("dialog", { name: "Install PostgreSQL" });
  await direct.getByRole("switch", { name: "Adminer" }).waitFor();
  const gap = await direct.locator("form").evaluate((form) => getComputedStyle(form).rowGap);
  if (gap !== "20px") fail(`the installation form's fields have a gap of ${gap}`);

  step("catalog: an app's page says what it installs before the plan, the app you open first, each by image");
  // Its admin tool is what a person opens, once it's on.
  await direct.getByRole("switch", { name: "Adminer" }).click();
  await direct.getByRole("button", { name: "Advanced" }).click();
  const parts = direct.getByRole("region", { name: "What it installs" });
  await parts.getByText("App you open").waitFor();
  for (const image of ["adminer:6.2.0", "postgres:17.11"]) await parts.getByText(image, { exact: true }).waitFor();
  const order = (await parts.textContent()) ?? "";
  if (order.indexOf("adminer") > order.indexOf("postgres:17.11"))
    fail(`What it installs puts the database before the app: ${order}`);
  await direct.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("heading", { level: 1, name: "PostgreSQL" }).waitFor();

  step("catalog: what an app reaches on the server is among what it installs, in red when it's as good as root");
  await page.goto(`${origin}/catalog/coder`);
  await page.getByRole("link", { name: "Install Coder" }).click();
  await page.getByRole("dialog", { name: "Install Coder" }).getByRole("button", { name: "Advanced" }).click();
  const coder = page.getByRole("dialog", { name: "Install Coder" }).getByRole("region", { name: "What it installs" });
  await coder.getByText("Docker's socket").waitFor();
  const socket = coder.locator(".dw-diff-critical", { hasText: "/var/run/docker.sock" });
  if ((await socket.count()) !== 1) fail("Coder's page doesn't mark Docker's socket in red among what it installs");
  await page.getByRole("dialog", { name: "Install Coder" }).getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("heading", { level: 1, name: "Coder" }).waitFor();
  if (installRequests !== 0) fail("browsing or cancelling asked to install an app");
  page.off("request", countInstall);
  await page.goto(`${origin}/catalog/postgres`);

  step("catalog: installing starts from its defaults, a line each, with a port that's free on the server");
  await page.getByRole("link", { name: "Install PostgreSQL" }).click();
  const sheet = page.getByRole("dialog", { name: "Install PostgreSQL" });
  await sheet.waitFor();
  await sheet.getByRole("group", { name: "Version" }).getByText("17.11").waitFor();
  // Its admin tool is left out unless it's turned on, and its port shows only then.
  const adminerSwitch = sheet.getByRole("switch", { name: "Adminer" });
  if ((await adminerSwitch.getAttribute("aria-checked")) !== "false") fail("Adminer is on before anyone turns it on");
  if ((await sheet.getByRole("group", { name: "Adminer port" }).count()) !== 0)
    fail("Adminer's port shows while it's off");
  await shoot(page, "catalog-install-tools-off");
  await adminerSwitch.click();
  const portLine = sheet.getByRole("group", { name: "Adminer port" });
  await portLine.getByText("Free").waitFor();
  const port = (await portLine.locator("code").innerText()).trim();
  if (!/^\d+$/.test(port)) fail(`Adminer's port starts as "${port}"`);
  const name = (await sheet.getByRole("group", { name: "Name" }).locator("code").innerText()).trim();
  if (name !== "postgres") fail(`its name starts as ${name}, not postgres`);
  if ((await sheet.getByRole("textbox").count()) !== 0) fail("it asks for something it can fill in itself");
  if ((await sheet.getByText("What to do first").count()) !== 0) fail("the install sheet still says what to do first");
  await shoot(page, "catalog-install");
  await sheet.getByRole("button", { name: "Advanced" }).click();
  await sheet.getByRole("group", { name: "Database password" }).getByText("Generated and sealed").waitFor();
  await shoot(page, "catalog-install-advanced");

  step("catalog: its plan shows everything it makes, with the password as a secret and no value");
  await chooseServer(sheet);
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  // The install form stays in the dialog, hidden under its plan, and lists what it
  // installs too: what's visible is the plan's.
  for (const name of ["POSTGRES_PASSWORD", "adminer", "db"])
    await plan.getByText(name, { exact: true }).filter({ visible: true }).first().waitFor();
  await shoot(page, "catalog-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();

  step("catalog: a database lands on its own page, with how to connect");
  const open = page.getByRole("button", { name: "Open PostgreSQL", exact: true });
  await open.waitFor({ timeout: 120_000 });
  await shoot(page, "catalog-arrival");
  await open.click();
  await page.getByRole("heading", { level: 1, name: "db", exact: true }).waitFor();
  if (!new URL(page.url()).pathname.startsWith("/databases/")) fail(`a database install lands on ${page.url()}`);
  const databasePage = page.url();
  await page.getByRole("region", { name: "Connect" }).getByText("postgres", { exact: true }).waitFor();

  step("catalog: its admin tool's page says how to sign in, with a way to reach it");
  const listed = await page.evaluate(async () => (await fetch("/v1/services")).json());
  const adminer =
    ((listed as { services?: { id: string; resource: { name: string } }[] }).services ?? []).find(
      (s) => s.resource.name === "adminer",
    )?.id ?? fail(`GET /v1/services listed no adminer: ${JSON.stringify(listed).slice(0, 300)}`);
  await page.goto(`${origin}/apps/${adminer}`);
  await page.getByRole("heading", { level: 1, name: "adminer", exact: true }).waitFor();
  await page.getByRole("link", { name: new RegExp(`Open .*:${port}`) }).waitFor({ timeout: 30_000 });
  const from = page.getByRole("region", { name: "From the catalog" });
  await from.getByText("PostgreSQL 17.11").waitFor();
  // How to sign in is a card to copy from, with the password to reveal, not a sentence naming its secret.
  const signIn = page.getByRole("region", { name: "Sign in" });
  await signIn.getByText("postgres", { exact: true }).waitFor();
  await signIn.getByText("db", { exact: true }).waitFor();
  await signIn.getByRole("button", { name: "Reveal database password" }).waitFor();
  await shoot(page, "catalog-app");
  const running = (await docker()).filter((c) => c.service === "adminer").map((c) => c.image);
  if (!running.some((image) => /^adminer:.*@sha256:/.test(image))) {
    fail(`Docker runs ${running.join(", ")} for adminer, not pinned by digest`);
  }

  step("catalog: its secret is sealed, and one step away");
  await from.getByRole("link", { name: "Its secrets" }).click();
  await page.getByRole("heading", { level: 1, name: "Variables and secrets" }).waitFor();
  await page.getByText("POSTGRES_PASSWORD", { exact: true }).first().waitFor();

  step("catalog: its admin tool is among its tools on the database's page, and removing it is a plan");
  await page.goto(databasePage);
  const tools = page.getByRole("region", { name: "Tools" });
  await tools.getByText("Installed", { exact: true }).waitFor();
  await shoot(page, "catalog-database-tools");
  await tools.getByRole("button", { name: "Remove Adminer" }).click();
  const removing = page.getByRole("dialog", { name: "Review plan" });
  await removing.getByText("Remove Adminer from postgres").waitFor({ timeout: 30_000 });
  await removing.getByText("adminer", { exact: true }).first().waitFor();
  await shoot(page, "catalog-database-tools-plan");
  // Later flows use the Adminer beside it, so this one leaves it as it is.
  await removing.getByRole("button", { name: /Cancel/ }).click();

  step("catalog: an installed app keeps its details and links to the running app");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Catalog" }).click();
  const card = page.getByRole("link", { name: /^PostgreSQL\b/ });
  await card.waitFor();
  await card.getByText("Installed").waitFor();
  await page.getByRole("link", { name: "1 installed" }).waitFor();
  await card.click();
  await page
    .getByRole("region", { name: "Your installations" })
    .getByRole("link", { name: "Open app: postgres" })
    .waitFor();
  await page.getByRole("link", { name: "Install another" }).waitFor();

  step("catalog: another install starts on a free name, says a taken one is taken, and takes a password of your own");
  await page.getByRole("link", { name: "Install another" }).click();
  const again = page.getByRole("dialog", { name: "Install PostgreSQL" });
  const nameLine = again.getByRole("group", { name: "Name" });
  await nameLine.getByText("postgres-2", { exact: true }).waitFor();
  await nameLine.getByRole("button", { name: "Change name" }).click();
  const nameField = again.getByRole("textbox", { name: "Name" });
  await nameField.fill("postgres");
  await again.getByText("A project named postgres is here already").waitFor();
  await again.getByRole("link", { name: "Open postgres" }).waitFor();
  if (!(await again.getByRole("button", { name: /Review plan/ }).isDisabled())) fail("it plans a name that's taken");
  await shoot(page, "catalog-install-taken");
  await again.getByRole("button", { name: "Use postgres-2" }).click();
  if ((await nameField.inputValue()) !== "postgres-2")
    fail(`Use postgres-2 left the name as ${await nameField.inputValue()}`);
  await again.getByRole("button", { name: "Advanced" }).click();
  await again.getByRole("button", { name: "Set my own database password" }).click();
  const own = again.getByLabel("Database password", { exact: true });
  await own.fill("short");
  await again.getByText("Use at least 8 characters.").waitFor();
  const mine = "e2e own password 1";
  await own.fill(mine);
  await again.getByText("At least 8 characters").waitFor();
  await shoot(page, "catalog-install-own");
  await chooseServer(again);
  await again.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("POSTGRES_PASSWORD", { exact: true }).filter({ visible: true }).first().waitFor();
  if ((await page.getByText(mine).count()) !== 0) fail("the plan shows the password you set");
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await again.getByRole("button", { name: /Cancel/ }).click();
  await page.getByRole("link", { name: "Back to catalog" }).click();

  step("catalog: what only a person has is asked for out of sight, and what an app reaches is said first");
  await page.getByRole("link", { name: /^Cloudflare Tunnel\b/ }).click();
  await page.getByRole("link", { name: "Install Cloudflare Tunnel" }).click();
  const tunnel = page.getByRole("dialog", { name: "Install Cloudflare Tunnel" });
  await tunnel.getByRole("group", { name: "Reaches" }).waitFor();
  if (!(await tunnel.getByRole("button", { name: /Review plan/ }).isDisabled())) fail("it plans with no token");
  await tunnel.getByLabel("Tunnel token").fill("not-a-real-token");
  await chooseServer(tunnel);
  await tunnel.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("not-a-real-token").waitFor({ state: "hidden" });
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await tunnel.getByRole("button", { name: /Cancel/ }).click();
  await page.getByRole("heading", { level: 1, name: "Cloudflare Tunnel" }).waitFor();

  step("catalog: a newer release: the panel says so where you'd look for it");
  const entry = join(catalogDir, "e2e-notes.yaml");
  writeFileSync(entry, release(1, "1.0", FIRST_DIGEST));
  await page.goto(`${origin}/catalog/e2e-notes`);
  await page.getByRole("link", { name: "Install E2E Notes" }).click();
  const notes = page.getByRole("dialog", { name: "Install E2E Notes" });
  await notes.getByRole("group", { name: "Port" }).waitFor();
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

  step("catalog: upgrading is a plan that backs up first, and it has an undo");
  await upgrade.click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("Its volumes are backed up first").waitFor();
  // An update says what it goes from and to, and backs up first unless you switch it off (ADR 0080).
  await plan.locator(".upgrade-line", { hasText: "From 1.0 to 2.0" }).waitFor();
  if (!(await plan.getByRole("switch", { name: /Back up first/ }).isChecked())) fail("an update doesn't back up first");
  await shoot(page, "catalog-upgrade-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("dialog", { name: "Updated to 2.0" }).waitFor({ timeout: 120_000 });
  // The sheet's Undo, beside the app page's own card that says it's updated.
  const undo = page.getByRole("button", { name: /Undo until/ });
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

  step("catalog: its sources start with Dockwell's own, following main, which a test leaves where the build put it");
  await page.goto(`${origin}/catalog`);
  await page.getByRole("link", { name: "Sources", exact: true }).first().click();
  const dockwells = page.locator("section.source-card", {
    has: page.getByRole("heading", { name: "Dockwell's own" }),
  });
  await dockwells.getByTitle("From the catalog dockwell").waitFor();
  await dockwells.getByText("Following main").waitFor();
  // A count and the way into the catalog, never a chip for each app (#568).
  const count = Number((await dockwells.locator(".source-card-count").textContent())?.match(/\d+/)?.[0] ?? 0);
  if (count < 30) fail(`Dockwell's own catalog shows ${count} apps, fewer than 30`);

  // The stores Dockwell reads, each one click from its plan, which this flow doesn't open: a plan reads the network.
  step("catalog: the stores Dockwell reads are listed with what each brings, each with Add");
  const gallery = page.locator("section.store-gallery");
  for (const store of ["1Panel's app store", "Dokploy's templates", "Coolify's templates"]) {
    await gallery.getByRole("button", { name: `Add ${store}` }).waitFor();
  }
  if (
    !(await gallery
      .getByText(/^About \d+ of \d+ apps install$/)
      .first()
      .isVisible())
  )
    fail("a store doesn't say how many of its apps install");
  await shoot(page, "catalog-sources");

  step("catalog: a source's apps open in the catalog, filtered to it, with the way back to every catalog");
  await dockwells.getByRole("link", { name: "Browse the apps from Dockwell's own" }).click();
  await page.getByText("From Dockwell's own").waitFor();
  await page.locator("a.catalog-card").first().waitFor();
  await shoot(page, "catalog-from-source");
  await page.getByRole("button", { name: "Show every catalog" }).click();
  await page.getByText("From Dockwell's own").waitFor({ state: "detached" });
  await page.getByRole("link", { name: "Sources", exact: true }).first().click();
  await dockwells.waitFor();

  // The form reads a repository as it's typed. An address of this server is refused
  // beside the field, before anything is asked of it, so this asks nothing of the network.
  step("catalog: a source's repository is checked as it's typed, and one that isn't public is refused, saying why");
  await page.getByRole("button", { name: "Add a source" }).click();
  const adding = page.getByRole("dialog", { name: "Add a catalog source" });
  // The stores come first, one click each, then a link for anything else.
  await adding
    .getByRole("group", { name: "App stores" })
    .getByRole("button", { name: "Add Coolify's templates" })
    .waitFor();
  await adding.getByLabel("Repository").fill("https://127.0.0.1/catalog");
  await adding
    .locator("form")
    .getByText(/this server's own address, so Dockwell reads no source there/)
    .waitFor();
  await shoot(page, "catalog-sources-refused");
  await adding.getByRole("button", { name: "Cancel" }).click();
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 40, run: (page, run) => catalogFlow(page, run) };
