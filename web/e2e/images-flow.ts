// SPDX-License-Identifier: AGPL-3.0-only

// New app picks its image: a name searched, a version picked newest first, Found said
// beside it, and the form filled from what the image declares, through the server's
// agent and the fake engine's registry (ADR 0062). It makes nothing.

import type { Flow } from "./flows.ts";

function fail(message: string): never {
  throw new Error(message);
}

export const flow: Flow = {
  order: 11,
  run: async (page, { origin, step, shoot }) => {
    if (!process.env["DOCKWELL_TEST_REGISTRY"]) {
      step("images: there's no registry stand-in on this engine, so New app reads no image");
      return;
    }
    step("images: a name typed finds Docker Hub's images, and one picked lists its versions newest first");
    await page.goto(`${origin}/apps/new?source=image`);
    const sheet = page.getByRole("dialog", { name: "New app" });
    await sheet.waitFor();
    const field = sheet.getByRole("combobox", { name: "Image" });
    await field.fill("ngi");
    const nginx = sheet.getByRole("option", { name: /^nginx/ });
    await nginx.waitFor();
    if (!(await nginx.textContent())?.includes("Official")) fail("nginx isn't said to be official");
    await nginx.click();
    const versions = sheet.getByRole("option");
    await versions.first().waitFor();
    const names = await versions.allTextContents();
    if (!names[0]?.startsWith("1.28") || !names[0].includes("latest"))
      fail(`the newest version isn't first, or latest isn't said: ${names.join(", ")}`);
    await shoot(page, "images-versions");

    step("images: a version picked is found, and the form takes its port and its volume from the image");
    await versions.first().click();
    if ((await field.inputValue()) !== "nginx:1.28") fail(`the field reads ${await field.inputValue()}`);
    await sheet.getByText("Found", { exact: true }).waitFor({ timeout: 20_000 });
    const inApp = sheet.getByLabel("In the app", { exact: true });
    await inApp.waitFor();
    if ((await inApp.inputValue()) !== "80")
      fail(`the port in the app is ${await inApp.inputValue()}, want nginx's 80`);
    const onServer = await sheet.getByLabel("On the server", { exact: true }).inputValue();
    if (!/^\d+$/.test(onServer)) fail(`the server's port is ${onServer}, want a free one filled in`);
    await shoot(page, "images-filled");

    step("images: one the registry hasn't got says so, and can still be typed");
    await field.fill("acme/doesnotexist:1.0");
    await sheet.getByText("Not found, or private").waitFor({ timeout: 20_000 });
    await sheet.getByRole("button", { name: "Cancel" }).click();
    const discard = page.getByRole("button", { name: "Discard" });
    if (await discard.isVisible()) await discard.click();
    await sheet.waitFor({ state: "hidden" });
  },
};
