// SPDX-License-Identifier: AGPL-3.0-only

// Registry logins (ADR 0086), through the server's agent and the fake engine's
// registry, which keeps example/private to its made-up login: Settings → Integrations
// connects GitHub's registry, checked live as it's typed, as a plan that never shows
// the token; New app then finds the private image with it, and its plan says which
// login it pulls with, and the app runs.

import type { Flow } from "./flows.ts";
import { onTop } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

// The stand-in registry's made-up login (scripts/fakedocker/registry.go).
const USER = "ci";
const TOKEN = "e2e-registry-token";
const IMAGE = "ghcr.io/example/private:1.0";

export const flow: Flow = {
  order: 97,
  run: async (page, { origin, step, shoot }) => {
    if (!process.env["DOCKWELL_TEST_REGISTRY"]) {
      step("registry logins: there's no registry stand-in on this engine, so nothing signs in");
      return;
    }
    step("registry logins: Integrations connects GitHub's registry, checked as it's typed");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/settings/integrations`);
    const registries = page.getByRole("region", { name: "Registries" });
    await registries.waitFor({ timeout: 20_000 });
    await shoot(page, "registry-logins-none");
    await registries
      .getByRole("article", { name: "GitHub Container Registry" })
      .getByRole("button", { name: "Connect" })
      .click();
    const sheet = page.getByRole("dialog", { name: "Add a registry login" });
    await sheet.waitFor();
    await sheet.getByRole("textbox", { name: "Username" }).fill(USER);
    await sheet.getByLabel("Token").fill("not-the-token");
    await sheet.getByText("Wrong username or password").waitFor({ timeout: 20_000 });
    const review = sheet.getByRole("button", { name: "Review plan" });
    if (!(await review.isDisabled())) fail("a login the registry refused goes ahead to a plan");
    await sheet.getByLabel("Token").fill(TOKEN);
    await sheet.getByText("Signed in", { exact: true }).waitFor({ timeout: 20_000 });
    await shoot(page, "registry-logins-sheet");
    await review.click();
    const plan = page.locator(".plan-sheet");
    const apply = plan.getByRole("button", { name: /Apply plan/ });
    await apply.waitFor({ timeout: 15_000 });
    if ((await plan.innerText()).includes(TOKEN)) fail("the login's plan shows its token");
    await apply.click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();
    const card = registries.getByRole("article", { name: "GitHub Container Registry" });
    await card.getByText("Connected").waitFor({ timeout: 15_000 });
    await card.getByText("ghcr").first().waitFor();
    await shoot(page, "registry-logins-connected");

    step("registry logins: New app finds the private image with the login, and its plan pulls with it");
    await page.goto(`${origin}/apps/new?source=image`);
    const form = page.getByRole("dialog", { name: "New app" });
    await form.waitFor();
    // With a second server, as earlier flows leave one, the image is read where the app
    // runs, once the sheet knows the servers: its subtitle names the one, or it asks.
    await form
      .getByText(/Runs on/)
      .first()
      .waitFor();
    const runsOn = form.getByRole("group", { name: "Runs on" });
    if ((await runsOn.count()) > 0) await runsOn.getByRole("checkbox").first().check();
    await form.getByLabel("Image", { exact: true }).fill(IMAGE);
    await form.getByText("Found, with ghcr").waitFor({ timeout: 20_000 });
    await shoot(page, "registry-logins-found");
    await form.getByRole("textbox", { name: /^Name/ }).first().fill("private-app");
    await form.getByRole("button", { name: /Review plan/ }).click();
    const reviewing = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await reviewing.waitFor({ timeout: 30_000 });
    await reviewing.getByText(/pulls with/).waitFor({ timeout: 15_000 });
    if (!(await reviewing.innerText()).includes("ghcr")) fail("the app's plan doesn't say which login it pulls with");
    await shoot(page, "registry-logins-plan");
    await reviewing.getByRole("button", { name: /Apply plan/ }).click();
    // The private image pulled with the login: the app runs.
    await page
      .getByRole("heading", { name: /^private-app is (deployed|running|healthy|starting)$/ })
      .waitFor({ timeout: 60_000 });
    if ((await page.locator("body").innerText()).includes(TOKEN)) fail("the app's change shows the login's token");
    await shoot(page, "registry-logins-running");
    await page.getByRole("button", { name: /^Open private-app/ }).click();
    await page.getByRole("heading", { level: 1, name: "private-app" }).waitFor();

    step("registry logins: the section on a phone");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${origin}/settings/integrations`);
    await page.getByRole("region", { name: "Registries" }).waitFor({ timeout: 20_000 });
    await shoot(page, "registry-logins-phone");
    await page.setViewportSize({ width: 1440, height: 900 });
  },
};
