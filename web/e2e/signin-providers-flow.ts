// SPDX-License-Identifier: AGPL-3.0-only

// Sign-in providers (ADR 0087): Settings → Integrations connects one as a plan that
// never shows its secret. The control server reads an issuer as it's typed, and
// won't read one on its own address, which it says; nothing in a test may reach the
// internet, so a found provider's reading is answered by hand, as the domains flow
// answers ListDomains, and the plan, its apply and the list are the control plane's.

import type { Flow } from "./flows.ts";
import { aDomain, type Listed } from "./private-flow.ts";
import { onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

function fail(message: string): never {
  throw new Error(message);
}

const FOUND = 1;
const SECRET = "e2e-client-secret";

/** CheckSigninProviderResponse in its wire format: found, at auth.example.com, signing with ES256. */
function found(): Buffer {
  return Buffer.concat([
    number(1, FOUND),
    bytes(2, "https://auth.example.com/"),
    bytes(3, "auth.example.com"),
    bytes(4, "ES256"),
  ]);
}

export const flow: Flow = {
  order: 99,
  run: async (page, { origin, step, shoot }) => {
    step("sign-in providers: Integrations offers one, and won't read an issuer on the control server's own address");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/settings/integrations`);
    const section = page.getByRole("region", { name: "Sign-in" });
    await section.waitFor({ timeout: 20_000 });
    await section.getByRole("article", { name: "Sign-in provider" }).getByRole("button", { name: "Connect" }).click();
    const sheet = page.getByRole("dialog", { name: "Add a sign-in provider" });
    await sheet.waitFor();
    const issuer = sheet.getByRole("textbox", { name: "Issuer" });
    await issuer.fill("127.0.0.1:9");
    await sheet.getByText("Can't read 127.0.0.1:9").waitFor({ timeout: 20_000 });
    await sheet.getByText(/this server's own address/).waitFor();
    await sheet.getByRole("textbox", { name: "Client ID" }).fill("dockwell");
    const review = sheet.getByRole("button", { name: "Review plan" });
    if (!(await review.isDisabled())) fail("an issuer the control server won't read goes ahead to a plan");
    await shoot(page, "signin-providers-refused");

    step("sign-in providers: one that's found is planned without its secret, and listed by its host and name");
    const checked = /DomainService\/CheckSigninProvider/;
    await page.route(checked, (route) =>
      route.fulfill({ status: 200, contentType: "application/proto", body: found() }),
    );
    await issuer.fill("auth.example.com");
    await sheet.getByText("Found auth.example.com · signs with ES256").waitFor({ timeout: 20_000 });
    await sheet.getByLabel("Client secret").fill(SECRET);
    await shoot(page, "signin-providers-sheet");
    await review.click();
    const plan = page.locator(".plan-sheet");
    const apply = plan.getByRole("button", { name: /Apply plan/ });
    await apply.waitFor({ timeout: 15_000 });
    if ((await plan.innerText()).includes(SECRET)) fail("the provider's plan shows its secret");
    await apply.click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();
    await page.unroute(checked);
    const card = section.getByRole("article", { name: "auth.example.com" });
    await card.waitFor({ timeout: 15_000 });
    await card.getByText(/auth-example-com for every project/).waitFor();
    await card.getByText("dockwell", { exact: true }).waitFor();
    if ((await page.locator("body").innerText()).includes(SECRET)) fail("the list shows the provider's secret");
    await shoot(page, "signin-providers-listed");

    step("sign-in providers: on a phone, the section fits");
    await page.setViewportSize({ width: 390, height: 844 });
    await section.getByRole("article", { name: "auth.example.com" }).waitFor();
    const wider = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    if (wider > 0) fail(`on a phone, Integrations is ${wider}px wider than the page`);
    await shoot(page, "signin-providers-phone");
    await page.setViewportSize({ width: 1440, height: 900 });

    step(
      "sign-in providers: Make it private signs a route in with it, you first, with the address to register, to a plan",
    );
    const read = async <T>(path: string): Promise<T> =>
      (await (await page.request.get(`${origin}${path}`)).json()) as T;
    const { services } = await read<{ services: Listed[] }>("/v1/services");
    const app = services[0] ?? fail("no app is there");
    const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
    const server = servers[0] ?? fail("no server is there");
    // The fake engine can't run the ingress, so the domains list is answered by hand, as
    // the private routes flow does; the plan is the control plane's.
    const stubbed = /DomainService\/ListDomains/;
    await page.route(stubbed, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/proto",
        body: aDomain(app.resource.scope, server, "sso.example.com", app.resource.name, []),
      }),
    );
    await page.goto(`${origin}/domains?private=rte_sso.example.com`);
    const make = page.getByRole("dialog", { name: "Make sso.example.com private" });
    await make.waitFor({ timeout: 15_000 });
    // Through loopback there's no address of yours to offer, so once the sheet reads your
    // sessions it turns the list off and asks for a password. Wait for that, or a click
    // on the list races it and turns the list back on.
    await make.getByRole("checkbox", { name: "Ask for a password", checked: true }).waitFor({ timeout: 10_000 });
    const list = make.getByRole("checkbox", { name: "Only these addresses" });
    if (await list.isChecked()) await list.uncheck();
    await make.getByRole("checkbox", { name: "Sign in with a provider" }).check();
    // Through loopback the sheet would make a password, which signing in takes the place of.
    if (await make.getByRole("checkbox", { name: "Ask for a password" }).isChecked())
      fail("signing in leaves the password the sheet made");
    const picked = make.getByRole("combobox", { name: "Sign in with" });
    await page.waitForFunction(
      () => (document.querySelector('[role="dialog"] select') as HTMLSelectElement | null)?.value !== "",
      undefined,
      { timeout: 10_000 },
    );
    if (!(await picked.locator("option:checked").innerText()).includes("auth.example.com"))
      fail("Make it private doesn't offer the provider just connected");
    await make.getByRole("list", { name: "Who may sign in" }).getByText("mira@acme.dev").waitFor();
    await make.getByText("https://sso.example.com/.dockwell/signin/callback").waitFor();
    await shoot(page, "signin-private-sheet");
    await page.setViewportSize({ width: 390, height: 844 });
    const widerSheet = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    if (widerSheet > 0) fail(`on a phone, Make it private is ${widerSheet}px wider than the page`);
    await shoot(page, "signin-private-sheet-phone");
    await page.setViewportSize({ width: 1440, height: 900 });
    await make.getByRole("button", { name: /Review plan/ }).click();
    const routePlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await routePlan.waitFor({ timeout: 30_000 });
    await routePlan.getByText("sso.example.com").first().waitFor({ timeout: 15_000 });
    if ((await routePlan.innerText()).includes(SECRET)) fail("the route's plan shows the provider's secret");
    await shoot(page, "signin-private-plan");
    await routePlan.getByRole("button", { name: /Cancel/ }).click();
    await make.getByRole("button", { name: "Cancel" }).click();
    await make.waitFor({ state: "hidden" });
    await page.unroute(stubbed);
  },
};
