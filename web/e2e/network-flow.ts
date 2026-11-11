// SPDX-License-Identifier: AGPL-3.0-only

// A server's Network section, on the page a person reads it on. What a real NAT
// does can't be shown here, as this runs on one machine: internal/control/fleet
// holds the sources to their order, and this walks what a person sees and does.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";

interface NetworkFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

export async function networkFlow(page: Page, { origin, step, shoot }: NetworkFlowOptions) {
  step("a server's Network section keeps its facts apart, and takes its public address from a person");
  const link = await ownServerPath(page, origin);
  await page.goto(`${origin}${link}`);
  const network = page.getByRole("region", { name: "Network" });
  await network.getByText("Reachable", { exact: true }).waitFor();
  await network.getByText("Not checked", { exact: true }).waitFor();
  await shoot(page, "network-before");

  await network
    .getByRole("button", { name: /^(Set|Change)$/ })
    .first()
    .click();
  const sheet = page.getByRole("dialog");
  const field = sheet.getByLabel("Public address");
  await field.fill("10.0.0.25");
  await sheet.getByRole("button", { name: "Set address" }).click();
  await sheet.getByText("10.0.0.25 is private, so the internet can't reach it there", { exact: false }).waitFor();
  await shoot(page, "network-refused");

  await field.fill("198.51.100.7");
  await sheet.getByRole("button", { name: "Set address" }).click();
  await sheet.waitFor({ state: "hidden" });
  await network.getByText("198.51.100.7", { exact: true }).waitFor();
  await network.getByText(/^Set by /).waitFor();
  // Setting an address says nothing of whether the internet can reach it.
  await network.getByText("Not checked", { exact: true }).waitFor();
  await shoot(page, "network-set");

  await page.goto(`${origin}/activity`);
  await page.getByText("Set the public address of", { exact: false }).first().waitFor();

  await page.goBack();
  await network.getByRole("button", { name: "Change" }).first().click();
  await page.getByRole("dialog").getByRole("button", { name: "Clear" }).click();
  await page.getByRole("dialog").waitFor({ state: "hidden" });
  await network.getByText("198.51.100.7", { exact: true }).waitFor({ state: "hidden" });

  await page.goto(`${origin}/activity`);
  await page.getByText("Cleared the public address of", { exact: false }).first().waitFor();
  await page.getByText("which was 198.51.100.7", { exact: false }).first().waitFor();

  step("a domain's record for a long IPv6 address fits the narrowest phone");
  const long = "2001:db8:1234:5678:9abc:def0:1234:5678";
  const call = async (method: string, body: unknown) =>
    page.evaluate(
      async ({ method, body }) => {
        const res = await fetch(`/dockwell.v1.ServerService/${method}`, {
          method: "POST",
          headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
          body: JSON.stringify(body),
        });
        if (!res.ok) throw new Error(`${method}: ${res.status} ${await res.text()}`);
        return (await res.json()) as { servers?: { id: string }[] };
      },
      { method, body },
    );
  // Whatever the machine this runs on is, an IPv6 address a person sets is a record to make.
  const { servers = [] } = await call("ListServers", {});
  for (const { id } of servers) await call("SetServerPublicAddress", { id, address: long });
  await page.goto(`${origin}/apps`);
  const app = await page.locator('a[href^="/apps/svc_"]').first().getAttribute("href");
  if (!app) throw new Error("there's no app to add a domain to");
  await page.setViewportSize({ width: 360, height: 780 });
  await page.goto(`${origin}${app}`);
  await page.getByRole("button", { name: "Add a domain" }).first().click();
  const form = page.getByRole("dialog", { name: /^Add a domain to / });
  await form.getByLabel("Domain").fill("app.example.com");
  const row = form.locator(".dw-dns-row", { hasText: long });
  await row.waitFor();
  const fits = await row.evaluate((el) => ({
    over: el.scrollWidth - el.clientWidth,
    page: document.documentElement.scrollWidth - window.innerWidth,
    name: el.querySelector(".dw-dns-name")?.getBoundingClientRect().width ?? 0,
  }));
  if (fits.over > 0) throw new Error(`at 360 px the record's row overflows by ${fits.over} px`);
  if (fits.page > 0) throw new Error(`at 360 px the domain form widens the page by ${fits.page} px`);
  if (fits.name < 100) throw new Error(`at 360 px the domain's name is ${fits.name} px wide, a letter or two a line`);
  await shoot(page, "network-domain-ipv6-phone");
  await page.keyboard.press("Escape");
  await form.waitFor({ state: "hidden" });
  await page.setViewportSize({ width: 1440, height: 900 });
  for (const { id } of servers) await call("SetServerPublicAddress", { id, address: "", ipv6: true });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 80, run: (page, run) => networkFlow(page, run) };
