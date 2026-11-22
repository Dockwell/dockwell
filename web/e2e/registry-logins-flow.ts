// SPDX-License-Identifier: AGPL-3.0-only

// Registry logins (ADR 0086), through the server's agent and the fake engine's
// registry, which keeps example/private to its made-up login: Settings → Integrations
// connects GitHub's registry, checked live as it's typed, as a plan that never shows
// the token; New app then finds the private image with it, and its plan says which
// login it pulls with, and the app runs.

import { dockerPull, dockerRemoveImage } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

// The stand-in registry's made-up login (scripts/fakedocker/registry.go).
const USER = "ci";
const TOKEN = "e2e-registry-token";
const IMAGE = "ghcr.io/example/private:1.0";

export const flow: Flow = {
  order: 97,
  run: async (page, { origin, socket, step, shoot }) => {
    if (!process.env["DOCKWELL_TEST_REGISTRY"]) {
      step("registry logins: there's no registry stand-in on this engine, so nothing signs in");
      return;
    }
    step("registry logins: Integrations connects GitHub's registry, checked as it's typed");
    await atWidth(page, { width: 1440, height: 900 });
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

    step("registry logins: allow pushes in place with a passkey, then turn them off in Settings");
    const server = await ownServerPath(page, origin);
    const localImage = "fixture/push-permission:local";
    await dockerPull(socket, localImage);
    try {
      await page.goto(`${origin}${server}/docker/images`);
      // Earlier flows can leave more images than the first page holds.
      await page.getByRole("textbox", { name: "Search images" }).fill(localImage);
      await page.getByRole("button", { name: `Actions for ${localImage}`, exact: true }).click();
      await page.getByRole("menuitem", { name: "Push image" }).click();
      const push = onTop(page.getByRole("dialog", { name: "Push image" }));
      await push.getByRole("textbox", { name: "Tag" }).fill("next");
      const allow = push.getByRole("button", { name: "Allow pushes with this login" });
      await allow.waitFor();
      const reviewPush = push.getByRole("button", { name: "Review push" });
      if (!(await reviewPush.isDisabled())) fail("a pull-only login can review a push");
      await shoot(page, "registry-logins-push-permission");
      await allow.click();
      const grant = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await grant.getByText("Applying this asks for your passkey", { exact: true }).waitFor();
      await shoot(page, "registry-logins-push-review");
      const approved = page.waitForResponse((response) => response.url().endsWith("/FinishStepUp") && response.ok());
      await grant.getByRole("button", { name: /Apply plan/ }).click();
      await approved;
      const done = page.getByRole("button", { name: "Done", exact: true });
      await done.click();
      await reviewPush.waitFor();
      await page.waitForFunction(() =>
        [...document.querySelectorAll("button")].some(
          (button) => button.textContent?.trim() === "Review push" && !button.disabled,
        ),
      );
      if ((await push.getByRole("textbox", { name: "Tag" }).inputValue()) !== "next") {
        fail("enabling the login discarded the destination");
      }
      await push.getByRole("button", { name: "Cancel", exact: true }).click();
    } finally {
      await dockerRemoveImage(socket, localImage);
    }
    await page.goto(`${origin}/settings/integrations`);
    const pushPermission = card.getByRole("switch", { name: "Allow pushes" });
    await pushPermission.waitFor();
    if (!(await pushPermission.isChecked())) fail("the login's push permission was not saved");
    await shoot(page, "registry-logins-push-setting");
    await pushPermission.click();
    const revoke = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await revoke.getByText("Applying this asks for your passkey", { exact: true }).waitFor();
    await revoke.getByRole("button", { name: /Apply plan/ }).click();
    await page.getByRole("button", { name: "Done", exact: true }).click();
    if (await pushPermission.isChecked()) {
      await card.getByRole("switch", { name: "Allow pushes", checked: false }).waitFor();
    }

    step("registry logins: a standalone image pull offers the only eligible login and names it in review");
    await page.goto(`${origin}${server}/docker/images`);
    await page.getByRole("button", { name: "Pull image", exact: true }).click();
    const pull = onTop(page.getByRole("dialog", { name: "Pull image" }));
    const registry = pull.getByRole("combobox", { name: "Registry", exact: true });
    await registry.selectOption({ label: "ghcr.io · ghcr" });
    await pull.getByText("ghcr", { exact: true }).last().waitFor();
    await pull.getByRole("textbox", { name: "Image", exact: true }).fill(IMAGE);
    await pull
      .getByRole("region", { name: "Repository tags" })
      .getByRole("button", { name: /^1\.0(?: |$)/ })
      .waitFor();
    await shoot(page, "registry-logins-docker-pull");
    await pull.getByRole("button", { name: "Review pull" }).click();
    const pullPlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await pullPlan.locator(".plan-image-evidence").getByText("ghcr", { exact: true }).waitFor();
    if ((await pullPlan.innerText()).includes(TOKEN)) fail("the pull's plan shows the login's token");
    await pullPlan.getByRole("button", { name: "Cancel" }).click();

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
    // A server is chosen at once, in one line with Change (#792); boxes only once changed.
    if (
      (await runsOn.count()) > 0 &&
      (await runsOn.getByRole("button", { name: "Change where it runs" }).count()) === 0
    )
      await runsOn.locator("label", { hasNotText: "Offline" }).first().getByRole("checkbox").check();
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
    await atWidth(page, { width: 390, height: 844 });
    await page.goto(`${origin}/settings/integrations`);
    await page.getByRole("region", { name: "Registries" }).waitFor({ timeout: 20_000 });
    await shoot(page, "registry-logins-phone");
    await atWidth(page, { width: 1440, height: 900 });
  },
};
