// SPDX-License-Identifier: AGPL-3.0-only

// Settings, as Settings-General.dc.html has it for an install: reached from the rail,
// the install named and given a time zone, each on Activity, how the panel is served
// and how to change it, a language for this browser, and the sections on a phone.

import type { Page } from "playwright-core";

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
  await panel.getByText("Environment=DOCKWELL_DOMAIN=panel.example.com", { exact: true }).waitFor();
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

  step("settings: on a phone, the sections are a list reached from More");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/more`);
  await page.getByRole("link", { name: "Settings" }).click();
  await page.getByRole("heading", { level: 1, name: "Settings" }).waitFor();
  await page.getByRole("link", { name: "General" }).click();
  await page.getByRole("heading", { level: 1, name: "General" }).waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth);
  if (wide > 390) fail(`General is ${wide} px wide on a 390 px phone`);

  // What later steps and screens read stays as it was.
  await page.setViewportSize({ width: 1440, height: 900 });
  await name.fill("");
  await install.getByRole("combobox", { name: "Time zone" }).selectOption("UTC");
  await install.getByRole("button", { name: "Save" }).click();
  await install.getByText("Saved. It's on Activity.", { exact: true }).waitFor();
}
