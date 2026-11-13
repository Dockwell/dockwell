// SPDX-License-Identifier: AGPL-3.0-only

// DNS providers (ADR 0088), through Cloudflare's stand-in beside the fake engine,
// which takes a made-up token for example.com and example.org: Settings →
// Integrations connects Cloudflare, checked live as the token is pasted, which lists
// the zones it reaches and writes and deletes a TXT record in each, as a plan that
// never shows the token.

import type { Flow } from "./flows.ts";

function fail(message: string): never {
  throw new Error(message);
}

// The stand-in's made-up token (scripts/fakedocker/cloudflare.go).
const TOKEN = "e2e-cloudflare-token";

export const flow: Flow = {
  order: 99,
  run: async (page, { origin, step, shoot }) => {
    if (!process.env["DOCKWELL_TEST_CLOUDFLARE"]) {
      step("DNS providers: there's no stand-in for Cloudflare here, so nothing writes DNS");
      return;
    }
    step("DNS providers: Integrations connects Cloudflare, checked as the token is pasted");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/settings/integrations`);
    const section = page.getByRole("region", { name: "DNS and proxies" });
    await section.waitFor({ timeout: 20_000 });
    const card = section.getByRole("article", { name: "Cloudflare" });
    await card.getByText("Not connected").waitFor({ timeout: 20_000 });
    await shoot(page, "dns-providers-none");
    await card.getByRole("button", { name: "Connect" }).click();
    const sheet = page.getByRole("dialog", { name: "Connect Cloudflare" });
    await sheet.waitFor();
    await sheet.getByLabel("API token").fill("not-the-token");
    await sheet.getByText("Wrong token").waitFor({ timeout: 20_000 });
    const review = sheet.getByRole("button", { name: "Review plan" });
    if (!(await review.isDisabled())) fail("a token Cloudflare refused goes ahead to a plan");
    await sheet.getByLabel("API token").fill(TOKEN);
    // Of the two zones it reaches, none is picked until a person picks, and it says
    // the token reaches more than it's used for.
    await sheet.getByText("Token works · pick its zones").waitFor({ timeout: 30_000 });
    await sheet.getByRole("link", { name: "Scope it to these zones" }).waitFor();
    for (const zone of ["example.com", "example.org"]) {
      const box = sheet.getByRole("checkbox", { name: zone });
      if (await box.isChecked()) fail(`${zone} is picked before anyone picked it`);
      await box.check();
    }
    await sheet.getByText("Can write DNS for 2 zones").waitFor({ timeout: 30_000 });
    await shoot(page, "dns-providers-sheet");
    await review.click();
    const plan = page.locator(".plan-sheet");
    const apply = plan.getByRole("button", { name: /Apply plan/ });
    await apply.waitFor({ timeout: 15_000 });
    if ((await plan.innerText()).includes(TOKEN)) fail("the provider's plan shows its token");
    await apply.click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();
    await card.getByText("Connected").waitFor({ timeout: 15_000 });
    await card.getByText("proves names by DNS").waitFor();
    if ((await page.locator("body").innerText()).includes(TOKEN)) fail("the page shows the provider's token");
    await shoot(page, "dns-providers-connected");

    step("DNS providers: the section on a phone");
    await page.setViewportSize({ width: 390, height: 844 });
    await section.scrollIntoViewIfNeeded();
    await shoot(page, "dns-providers-phone");
    await page.setViewportSize({ width: 1440, height: 900 });
  },
};
