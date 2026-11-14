// SPDX-License-Identifier: AGPL-3.0-only

// Who gets in to a route (ADR 0087), on the fake Docker Engine: from a row of Domains,
// the sheet offers a password Dockwell makes, shown once, and an address list, and
// reaches a plan on the real control plane, which never shows the password or its
// hash. A route already private says what a stranger got when its server last asked,
// and keeps its users' passwords. The fake engine can't run the ingress, so ListDomains
// is answered by hand, for a real app and a real server, as the domains flow does.

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { coveredHidden, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

function fail(message: string): never {
  throw new Error(message);
}

const ISSUED = 2;
const ASKED_FOR_PASSWORD = 2;

/** A domain in ListDomains' wire format: one route to service on server, private to users if any, with what a stranger got. */
function aDomain(
  environment: string,
  server: { id: string; name: string },
  name: string,
  service: string,
  users: string[],
) {
  const route = Buffer.concat([
    bytes(1, `rte_${name}`),
    bytes(2, name.replaceAll(".", "-")),
    bytes(4, service),
    number(5, 80),
    ...users.map((user) => bytes(111, user)),
  ]);
  const seconds = Math.floor(Date.now() / 1000) + 40 * 86_400;
  const certificate = Buffer.concat([number(1, ISSUED), bytes(4, number(1, seconds))]);
  const stranger =
    users.length > 0
      ? bytes(110, Buffer.concat([number(2, ASKED_FOR_PASSWORD), number(3, 401), bytes(4, "401 Unauthorized")]))
      : Buffer.alloc(0);
  const status = Buffer.concat([bytes(1, name), bytes(2, certificate), stranger]);
  const on = Buffer.concat([bytes(1, server.id), bytes(2, server.name), bytes(3, status)]);
  return bytes(
    1,
    Buffer.concat([bytes(1, name), bytes(2, environment), bytes(3, `https://${name}`), bytes(4, route), bytes(5, on)]),
  );
}

/** An app as GET /v1/services lists it, with the ports it publishes. */
interface Listed {
  resource: { name: string; scope: string };
  spec?: { app?: { ports?: { hostIp?: string }[] } };
}

/** Whether an app publishes a port on every address of its server, a way in around any route to it. */
function publishes(service: Listed): boolean {
  return (service.spec?.app?.ports ?? []).some((p) => !p.hostIp || p.hostIp === "0.0.0.0" || p.hostIp === "::");
}

async function privateFlow(page: Page, { origin, step, shoot }: FlowRun) {
  const read = async <T>(path: string): Promise<T> => (await (await page.request.get(`${origin}${path}`)).json()) as T;
  const { services } = await read<{ services: Listed[] }>("/v1/services");
  // The catalog flow's adminer, or any app there is: a route to it is all this needs.
  const adminer =
    services.find((service) => service.resource.name === "adminer") ?? services[0] ?? fail("no app is there");
  // A private route reads as a stranger's outcome only with no way around it: an app that
  // publishes no port, and that no public route reaches (#606's review, M2).
  const quiet =
    services.find(
      (service) =>
        service.resource.scope === adminer.resource.scope &&
        service.resource.name !== adminer.resource.name &&
        !publishes(service),
    ) ?? fail(`no app beside ${adminer.resource.name} publishes no port`);
  const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
  const server = servers[0] ?? fail("no server is there");
  const stubbed = /DomainService\/ListDomains/;
  await page.route(stubbed, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/proto",
      body: Buffer.concat([
        aDomain(adminer.resource.scope, server, "open.example.com", adminer.resource.name, []),
        aDomain(adminer.resource.scope, server, "around.example.com", adminer.resource.name, ["ci"]),
        aDomain(adminer.resource.scope, server, "locked.example.com", quiet.resource.name, ["ci"]),
      ]),
    }),
  );
  const row = (name: string) => page.getByRole("row", { name: new RegExp(`^${name.replaceAll(".", "\\.")}`) });

  step("private: a private route says what a stranger got, as its server last asked");
  await page.goto(`${origin}/domains`);
  await row("locked.example.com")
    .getByText("Private · A stranger is asked for a password")
    .waitFor({ timeout: 15_000 });

  step("private: one whose app is open around it says where, over what a stranger got");
  const around = publishes(adminer)
    ? /^Private here · open on port \d+$/
    : /^Private here · open at open\.example\.com/;
  await row("around.example.com").getByText(around).waitFor();
  if ((await row("around.example.com").getByText("A stranger is asked for a password").count()) !== 0)
    fail("a route its app is open around reads as keeping strangers out");
  await shoot(page, "private-list");

  step("private: from a row, the sheet makes a password, shown once, as this browser reaches the panel on loopback");
  await row("open.example.com").getByRole("button", { name: "Make open.example.com private" }).click();
  const sheet = page.getByRole("dialog", { name: "Make open.example.com private" });
  await sheet.waitFor();
  await page.waitForURL(/[?&]private=rte_open\.example\.com/);
  const asks = sheet.getByRole("checkbox", { name: "Ask for a password" });
  // Your address comes from your session, which says loopback here, so it turns to a password.
  await page.waitForFunction(
    () => document.querySelectorAll('[role="dialog"] .private-password-value').length > 0,
    undefined,
    { timeout: 10_000 },
  );
  if (!(await asks.isChecked()))
    fail("through loopback, with no address of yours to offer, it doesn't start with a password");
  const password = (await sheet.locator(".private-password-value").textContent()) ?? "";
  if (!/^[A-HJ-NP-Za-km-z2-9]{24}$/.test(password)) fail(`the password it makes is ${password}`);
  const review = sheet.getByRole("button", { name: /Review plan/ });
  if (await review.isEnabled()) fail("it goes on before you've said you kept the password");
  await shoot(page, "private-sheet");

  step("private: an address list too, and the plan, which shows neither the password nor its hash");
  await sheet.getByRole("checkbox", { name: "Only these addresses" }).check();
  const add = sheet.getByLabel("Add an address or a range");
  await add.fill("203.0.113.0/24");
  await add.press("Enter");
  await sheet.getByRole("list", { name: "Addresses let in" }).getByText("203.0.113.0/24").waitFor();
  await sheet.getByRole("checkbox", { name: "I've kept a copy of the password" }).check();
  await review.click();
  const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await plan.waitFor({ timeout: 30_000 });
  // In the plan itself, on top: the form under it names the domain too.
  await plan.getByText("open.example.com").first().waitFor({ timeout: 15_000 });
  // The form under the plan says the password until it has faded, so the page is read
  // once it's hidden.
  if ((await page.locator(".dw-sheet-view.is-away .private-password-value").count()) !== 1)
    fail("the plan doesn't cover the form that made the password, so this checks nothing");
  await coveredHidden(page);
  const said = await page.locator("body").innerText();
  if (said.includes(password)) fail("the plan shows the password");
  if (/\$2[aby]\$/.test(said)) fail("the plan shows the password's hash");
  await shoot(page, "private-plan");
  await plan.getByRole("button", { name: /Cancel/ }).click();
  await sheet.waitFor();
  if ((await sheet.locator(".private-password-value").textContent()) !== password)
    fail("cancelling the plan loses the password");
  await sheet.getByRole("button", { name: "Cancel" }).click();
  await sheet.waitFor({ state: "hidden" });

  step("private: a private route keeps its users' passwords, and fits a phone");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/domains?private=rte_locked.example.com`);
  const locked = page.getByRole("dialog", { name: "Who gets in to locked.example.com" });
  await locked.waitFor({ timeout: 15_000 });
  await locked.getByText("keeps their password").waitFor();
  await locked
    .getByRole("region", { name: "What a stranger gets" })
    .getByText("A stranger is asked for a password", { exact: true })
    .waitFor();
  const wider = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  if (wider > 0) fail(`on a phone, the sheet is ${wider}px wider than the page`);
  await shoot(page, "private-phone");
  await locked.getByRole("button", { name: "Cancel" }).click();
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.unroute(stubbed);
}

/** Where sign-in.ts runs this among the flows, right after Domains', on the fake engine (flows.ts). */
export const flow: Flow = { order: 61, run: (page, run) => privateFlow(page, run) };
