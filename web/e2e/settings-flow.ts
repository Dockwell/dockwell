// SPDX-License-Identifier: AGPL-3.0-only

// Settings, as Settings-General.dc.html has it for an install: reached from the rail,
// the install named and given a time zone, each on Activity, how the panel is served
// and how to change it, a language for this browser, and the sections on a phone.
// Then Control plane, as Settings-ControlPlane.dc.html has it for one control plane.

import { readFile } from "node:fs/promises";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

interface SettingsFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function settingsFlow(page: Page, { origin, step, shoot }: SettingsFlowOptions) {
  step("settings: the rail leads to General, beside the other sections");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Settings" }).click();
  await page.getByRole("heading", { level: 1, name: "General" }).waitFor();
  const nav = page.getByRole("navigation", { name: "Settings" });
  if ((await nav.getByRole("link", { name: "General" }).getAttribute("aria-current")) !== "page") {
    fail("Settings' nav doesn't mark General as the page");
  }
  const panel = page.getByRole("region", { name: "Panel address" });
  await panel.getByText("Loopback only", { exact: true }).waitFor();
  await panel.getByText("DOCKWELL_DOMAIN=panel.example.com", { exact: true }).waitFor();
  await shoot(page, "settings-general");

  step("settings: naming the install and setting its time zone are on Activity, and the tab says the name");
  const install = page.getByRole("region", { name: "This install" });
  const name = install.getByRole("textbox", { name: "Name" });
  await name.fill("Acme servers");
  await install.getByRole("combobox", { name: "Time zone" }).selectOption("Europe/Amsterdam");
  await install.getByRole("button", { name: "Save" }).click();
  await install.getByText("Saved. It's on Activity.", { exact: true }).waitFor();
  await page.waitForFunction(() => document.title === "General · Acme servers");
  await page.goto(`${origin}/activity`);
  await page.getByText("Named this install Acme servers", { exact: true }).first().waitFor();
  await page.getByText("Set the time zone for new schedules to Europe/Amsterdam", { exact: true }).first().waitFor();

  step("settings: the panel shows in the language chosen for this browser, and goes back");
  await page.goto(`${origin}/settings/general`);
  const language = page.getByRole("region", { name: "Language" });
  await language.getByRole("combobox").selectOption("zh-CN");
  await page.getByRole("heading", { level: 1, name: "常规" }).waitFor();
  await page.reload();
  await page.getByRole("heading", { level: 1, name: "常规" }).waitFor();
  await page.getByRole("region", { name: "语言" }).getByRole("combobox").selectOption("");
  await page.getByRole("heading", { level: 1, name: "General" }).waitFor();

  step("settings: Control plane says what runs it, where its state is, and how to back it up and upgrade it");
  await page.getByRole("navigation", { name: "Settings" }).getByRole("link", { name: "Control plane" }).click();
  await page.getByRole("heading", { level: 1, name: "Control plane" }).waitFor();
  const store = page.getByRole("region", { name: "State store" });
  await store.getByText(/^SQLite \d/).waitFor();
  await store.getByText(/\/control\/state\.db$/).waitFor();
  // Started by hand, as here, nothing it could name would stop it, so it says to, and only copies.
  const backups = page.getByRole("region", { name: "Backups" });
  await backups.getByText(/--exclude 'control\/key\*' -czf \/root\/dockwell-control\.tgz control agent &&/).waitFor();
  await backups.getByText(/Dockwell doesn't run as a service here/).waitFor();
  await page
    .getByRole("region", { name: "Updates" })
    .getByText(/ runs here$/)
    .waitFor();
  await page
    .getByRole("region", { name: "Agents" })
    .getByText(/^\d+ connected$/)
    .waitFor();
  const replica = page.getByRole("article").first();
  await replica.getByText("Healthy", { exact: true }).waitFor();
  await replica.getByText(/with its agent, of a 250 MB budget$/).waitFor();
  await shoot(page, "settings-control-plane");

  step("settings: Members says the owner signs in, with a passkey, and that invites need the panel's domain");
  await page.getByRole("navigation", { name: "Settings" }).getByRole("link", { name: "Members" }).click();
  await page.getByRole("heading", { level: 1, name: "Members" }).waitFor();
  await page.getByText("1 person signs in to this install, its owner", { exact: true }).waitFor();
  const people = page.getByRole("table", { name: "Who signs in to this install" });
  await people
    .getByRole("row")
    .filter({ hasText: "Owner" })
    .filter({ hasText: /passkey/ })
    .waitFor();
  await page.getByRole("region", { name: "What each role can do" }).waitFor();
  await shoot(page, "settings-members");
  // This stand serves the panel on localhost alone, where nobody else can accept an invite.
  await page.getByRole("button", { name: "Invite someone" }).click();
  await page.getByText("Invites need the panel's domain", { exact: true }).waitFor();
  await shoot(page, "settings-members-invite");

  step("settings: the audit log lists every event, narrows to an action, and saves what it keeps as CSV");
  await page.getByRole("navigation", { name: "Settings" }).getByRole("link", { name: "Audit log" }).click();
  await page.getByRole("heading", { level: 1, name: "Audit log" }).waitFor();
  const log = page.getByRole("table", { name: "The audit log, newest first" });
  await log.getByRole("cell", { name: "settings.name", exact: true }).first().waitFor();
  // Each says what happened in a sentence, as Activity does, never the JSON it records.
  await log
    .getByRole("row")
    .filter({ hasText: "settings.name" })
    .filter({ hasText: "Named this install" })
    .first()
    .waitFor();
  await page.getByRole("combobox", { name: "Time range" }).selectOption("all");
  await page.getByRole("textbox", { name: "Filter events" }).fill("owner.claim");
  await log.getByRole("cell", { name: "owner.claim", exact: true }).waitFor();
  await page.getByText("1 event, all there is.", { exact: true }).waitFor();
  // What this flow did, which the newest page has whatever came before.
  await page.getByRole("textbox", { name: "Filter events" }).fill("settings.");
  await log.getByRole("cell", { name: "settings.time_zone", exact: true }).first().waitFor();
  await shoot(page, "settings-audit-log");
  const downloading = page.waitForEvent("download");
  await page.getByRole("button", { name: "Export CSV" }).click();
  const file = await downloading;
  const path = await file.path();
  const csv = path ? await readFile(path, "utf8") : "";
  // What the filters keep, and nothing else.
  if (
    !csv.startsWith("time,action,actor_kind,") ||
    !csv.includes(",settings.time_zone,") ||
    !csv.includes("time zone for new schedules") ||
    csv.includes('""previous""') ||
    csv.includes(",owner.claim,")
  ) {
    fail(`the audit log's CSV reads ${JSON.stringify(csv.slice(0, 200))}`);
  }

  step("settings: Integrations shows where backups go, and connects storage through Backups' plan");
  await page.getByRole("navigation", { name: "Settings" }).getByRole("link", { name: "Integrations" }).click();
  await page.getByRole("heading", { level: 1, name: "Integrations" }).waitFor();
  await page.getByRole("article", { name: "Cloudflare" }).waitFor();
  await shoot(page, "settings-integrations");
  // A card to connect the first, or the section's own button once there is one.
  await page
    .getByRole("button", { name: /^Connect( storage)?$/ })
    .first()
    .click();
  const adding = page.getByRole("dialog", { name: "Add a destination" });
  await adding.getByRole("textbox", { name: "Bucket" }).waitFor();
  await adding.getByRole("button", { name: "Cancel" }).click();
  await adding.waitFor({ state: "detached" });

  step("settings: on a phone, the sections are a list reached from More");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/more`);
  await page.getByRole("link", { name: "Settings" }).click();
  await page.getByRole("heading", { level: 1, name: "Settings" }).waitFor();
  await page.getByRole("link", { name: "General" }).click();
  await page.getByRole("heading", { level: 1, name: "General" }).waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 390) fail(`General is ${wide} px wide on a 390 px phone`);
  await page.goto(`${origin}/settings`);
  await page.getByRole("link", { name: "Control plane" }).click();
  await page.getByRole("heading", { level: 1, name: "Control plane" }).waitFor();
  await page
    .getByRole("region", { name: "State store" })
    .getByText(/^SQLite \d/)
    .waitFor();
  if ((await page.getByRole("link", { name: "Back", exact: true }).getAttribute("href")) !== "/settings") {
    fail("Control plane on a phone doesn't lead back to Settings");
  }
  const cpWide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (cpWide > 390) fail(`Control plane is ${cpWide} px wide on a 390 px phone`);

  // What later steps and screens read stays as it was.
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${origin}/settings/general`);
  await page.getByRole("heading", { level: 1, name: "General" }).waitFor();
  await name.fill("");
  await install.getByRole("combobox", { name: "Time zone" }).selectOption("");
  await install.getByRole("button", { name: "Save" }).click();
  await install.getByText("Saved. It's on Activity.", { exact: true }).waitFor();
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 90, run: (page, run) => settingsFlow(page, run) };
