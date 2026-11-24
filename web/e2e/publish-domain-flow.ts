// SPDX-License-Identifier: AGPL-3.0-only

// Adding a domain through Cloudflare (ADR 0104), on Cloudflare's stand-in beside the
// fake engine: a cloudflared run in the stand-in /proc, connected to web-1 as the
// stand-in's connector says, so the agent finds the server's tunnel and the control
// plane proves it. New website picks its name from a connected zone, checks it live,
// takes the tunnel by default with the others one step away, and its plan lists what
// it writes at Cloudflare. A record made in Cloudflare by hand is said with Replace it.
// The fake engine can't run the ingress, so the plan isn't applied here.

import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { atWidth, LONG_HOSTNAME, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

function fail(message: string): never {
  throw new Error(message);
}

// What the stand-in holds (scripts/fakedocker/cloudflare.go, internal/control/dnsproviders/cftest).
const TOKEN = "e2e-cloudflare-token";
const ACCOUNT = "acc-0123456789abcdef";
const TUNNEL = "3f2d1c0b-4a5b-4c6d-8e7f-9a0b1c2d3e4f";
const CONNECTOR = "e2e-connector-1";
const PID = "4242";

/**
 * Puts a cloudflared run of web-1 in the stand-in /proc, on the server's own network,
 * whose /ready names the stand-in's connector. Its token is made up, with a made-up secret.
 */
export function runCloudflared(proc: string): void {
  mkdirSync(join(proc, "self", "ns"), { recursive: true });
  mkdirSync(join(proc, PID, "ns"), { recursive: true });
  const net = "net:[4026531840]";
  symlinkSync(net, join(proc, "self", "ns", "net"));
  symlinkSync(net, join(proc, PID, "ns", "net"));
  // The fields after the name: its start time is the 20th.
  writeFileSync(
    join(proc, PID, "stat"),
    `${PID} (cloudflared) S 1 ${PID} ${PID} 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 12345 0 0\n`,
  );
  const token = Buffer.from(JSON.stringify({ a: ACCOUNT, t: TUNNEL, s: "bWFkZS11cA==" })).toString("base64");
  writeFileSync(
    join(proc, PID, "cmdline"),
    ["cloudflared", "tunnel", "--metrics", "127.0.0.1:20241", "run", "--token", token, ""].join("\0"),
  );
  writeFileSync(join(proc, PID, "ready"), `${CONNECTOR}\n`);
}

export const flow: Flow = {
  // After DNS providers connects Cloudflare.
  order: 99.5,
  run: async (page, { origin, step, shoot, bin, data }) => {
    const api = process.env["DOCKWELL_TEST_CLOUDFLARE"];
    const proc = process.env["DOCKWELL_TEST_PROC"];
    if (!api || !proc) {
      step("adding a domain through Cloudflare: there's no stand-in for Cloudflare here");
      return;
    }
    runCloudflared(proc);
    const read = async <T>(path: string): Promise<T> =>
      (await (await page.request.get(`${origin}${path}`)).json()) as T;
    const { servers } = await read<{ servers: { id: string; name: string }[] }>("/v1/servers");
    const server = servers.find((s) => s.name === LONG_HOSTNAME) ?? fail("the e2e server isn't listed");
    // What a server's tunnels were is kept a while, so the check asks past it, as Check
    // again does, until the run just started is seen (the review's B-M7).
    const tunnelSeen = async (body: Record<string, unknown>) => {
      let said = "";
      for (let i = 0; i < 60; i++) {
        const res = await page.request.post(`${origin}/dockwell.v1.DomainService/CheckDomain`, {
          data: { serverId: server.id, domain: "probe.example.com", ...body },
          headers: { "Content-Type": "application/json", "Cache-Control": "no-cache", Origin: origin },
        });
        said = await res.text();
        const answer = JSON.parse(said) as { cloudflare?: { ways?: { way?: string; off?: string }[] } };
        if (answer.cloudflare?.ways?.some((w) => w.way === "ROUTE_WAY_SERVER_TUNNEL" && !w.off)) return;
        await page.waitForTimeout(1000);
      }
      fail(`the check never saw the server's tunnel: ${said}`);
    };
    await tunnelSeen({ publish: {} });
    const cloudflare = async (path: string, init: RequestInit = {}) =>
      (await (
        await fetch(`${api}${path}`, {
          ...init,
          headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" },
        })
      ).json()) as { result: { id: string; name: string }[] | { id: string } };
    try {
      step("adding a domain through Cloudflare: New website picks its name from a connected zone");
      await atWidth(page, { width: 1440, height: 1000 });
      await page.goto(`${origin}/websites`);
      await page.getByRole("link", { name: "New website" }).click();
      const sheet = page.getByRole("dialog", { name: "New website" });
      await sheet.waitFor();
      const zone = sheet.getByRole("combobox", { name: "Zone" });
      await zone.waitFor({ timeout: 20_000 });
      if ((await zone.inputValue()) !== "example.com") fail(`the zone starts at ${await zone.inputValue()}`);
      const sub = sheet.getByRole("textbox", { name: "Subdomain" });
      await sub.fill("docs");
      await sheet.getByText("Free in Dockwell and at Cloudflare").waitFor({ timeout: 30_000 });

      step("adding a domain through Cloudflare: the server's tunnel, found and proved, is the way in");
      try {
        await sheet.getByText(/^Through the tunnel web-1 on /).waitFor({ timeout: 30_000 });
      } catch {
        // What the check said, so a failure here says why the tunnel wasn't the way in.
        const { servers } = (await (await page.request.get(`${origin}/v1/servers`)).json()) as {
          servers: { id: string; name: string }[];
        };
        const said = await Promise.all(
          servers.map(async (s) =>
            (
              await page.request.post(`${origin}/dockwell.v1.DomainService/CheckDomain`, {
                data: { serverId: s.id, domain: "docs.example.com" },
                headers: { "Content-Type": "application/json" },
              })
            ).text(),
          ),
        );
        fail(`the server's tunnel isn't the way in: ${said.join(" | ")}`);
      }
      await sheet.getByText("Adds *.example.com to web-1, once").waitFor();
      if ((await sheet.getByText(/Make this record|Point an A record/).count()) > 0) {
        fail("a name in a connected zone still asks for a record made by hand");
      }
      await shoot(page, "publish-domain-new-website");

      step("adding a domain through Cloudflare: the other ways are one step away");
      await sheet.getByRole("button", { name: "Change the way in" }).click();
      const ways = sheet.getByRole("radiogroup", { name: "Way in" });
      await ways.waitFor();
      if (!(await ways.getByRole("radio", { name: /Through the tunnel web-1/ }).isChecked())) {
        fail("the tunnel isn't the way picked");
      }
      await ways.getByRole("radio", { name: /Something else in front/ }).waitFor();
      await shoot(page, "publish-domain-ways");
      await ways.getByRole("radio", { name: /Through the tunnel web-1/ }).click();

      step("adding a domain through Cloudflare: the plan says what it writes at Cloudflare, before the server's steps");
      await sheet.getByRole("button", { name: /Review plan/ }).click();
      const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await plan.waitFor({ timeout: 30_000 });
      const atCloudflare = plan.getByRole("region", { name: "At Cloudflare" });
      await atCloudflare
        .getByText("Write the DNS record for docs.example.com at Cloudflare")
        .waitFor({ timeout: 15_000 });
      await atCloudflare.getByText("Add *.example.com to the tunnel web-1 at Cloudflare").waitFor();
      await atCloudflare.scrollIntoViewIfNeeded();
      await shoot(page, "publish-domain-plan");
      await plan.getByRole("button", { name: /Cancel/ }).click();
      await sheet.waitFor();
      if ((await sub.inputValue()) !== "docs") fail("cancelling the plan loses the name");

      step("adding a domain through Cloudflare: a record made in Cloudflare is said, with Replace it");
      const zones = (await cloudflare("/zones")).result as { id: string; name: string }[];
      const zoneId = zones.find((z) => z.name === "example.com")?.id ?? fail("the stand-in has no example.com");
      const made = (
        await cloudflare(`/zones/${zoneId}/dns_records`, {
          method: "POST",
          body: JSON.stringify({
            type: "A",
            name: "vpn.example.com",
            content: "203.0.113.9",
            ttl: 300,
            proxied: false,
          }),
        })
      ).result as { id: string };
      try {
        await sub.fill("vpn");
        await sheet.getByText("A record exists: A 203.0.113.9, made in Cloudflare").waitFor({ timeout: 30_000 });
        await sheet.getByRole("button", { name: "Replace it" }).waitFor();
        await shoot(page, "publish-domain-taken");
      } finally {
        await cloudflare(`/zones/${zoneId}/dns_records/${made.id}`, { method: "DELETE" });
      }

      step("adding a domain through Cloudflare: the sheet on a phone, the name over the zone");
      // (The app's page path follows, once its project is the provider's.)
      await sub.fill("docs");
      await sheet.getByText(/^Through the tunnel web-1 on /).waitFor({ timeout: 30_000 });
      await atWidth(page, { width: 390, height: 844 });
      await shoot(page, "publish-domain-phone");
      await atWidth(page, { width: 1440, height: 1000 });
      await sheet.getByRole("button", { name: "Cancel" }).click();

      step(
        "adding a domain through Cloudflare: the provider serves an app's project, and sets its zone to Full (strict), applied",
      );
      const cli = (...args: string[]) =>
        execFileSync(bin, [...args, "--data-dir", data, "--log-format", "json"], {
          encoding: "utf8",
          env: {
            ...process.env,
            USER: "mira",
            DOCKWELL_URL: "",
            DOCKWELL_TOKEN: "",
            XDG_CONFIG_HOME: join(data, "cli-publish"),
          },
        });
      cli("login", "--local");
      cli("services", "create", "wiki-app", "--project", "default", "--app-image", "nginx:1.27-alpine", "--yes");
      const listedApps = await read<{ services: { id: string; resource: { name: string; scope: string } }[] }>(
        "/v1/services",
      );
      const appEnv =
        listedApps.services.find((sv) => sv.resource.name === "wiki-app")?.resource.scope ??
        fail("wiki-app wasn't made");
      const envs = await read<{ resources: { id: string; resource: { scope: string } }[] }>(
        "/v1/resources?kind=environment",
      );
      const project =
        envs.resources.find((r) => r.id === appEnv)?.resource.scope ?? fail("wiki-app's project isn't listed");
      const planned = JSON.parse(
        cli(
          "dns-providers",
          "update",
          "cloudflare",
          "--provider",
          "cloudflare",
          "--zones",
          "example.com",
          "--zones",
          "example.org",
          "--every-project=false",
          "--projects",
          project,
          "--strict-zones",
          "example.com",
          "--plan-only",
          "--json",
        ),
      ) as { change?: { id?: string } };
      await page.goto(`${origin}/changes/${planned.change?.id ?? fail("the provider's plan has no change")}`);
      const providerPlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await providerPlan
        .getByRole("region", { name: "At Cloudflare" })
        .getByText("Set example.com to Full (strict) at Cloudflare")
        .waitFor({ timeout: 30_000 });
      await providerPlan.getByRole("button", { name: /^Apply plan/ }).click();
      await page
        .getByRole("dialog", { name: /^(Applied|cloudflare is changed)$/ })
        .getByRole("button", { name: "Done" })
        .click();
      const ssl = (await cloudflare(`/zones/${zoneId}/settings/ssl`)) as unknown as { result: { value: string } };
      if (ssl.result.value !== "strict") fail(`the zone's mode after the plan: ${ssl.result.value}`);

      step("adding a domain through Cloudflare: from an app's page, the tunnel by default, another way one step away");
      const { services } = await read<{ services: { id: string; resource: { name: string } }[] }>("/v1/services");
      const app = services.find((sv) => sv.resource.name === "wiki-app") ?? fail("wiki-app isn't there");
      await tunnelSeen({ projectId: project });
      await page.goto(`${origin}/apps/${app.id}`);
      await page.getByRole("button", { name: "Add a domain" }).first().click();
      const add = page.getByRole("dialog", { name: "Add a domain to wiki-app" });
      await add.getByRole("combobox", { name: "Zone" }).waitFor({ timeout: 20_000 });
      const addSub = add.getByRole("textbox", { name: "Subdomain" });
      await addSub.fill("blog");
      await add.getByText(/^Through the tunnel web-1 on /).waitFor({ timeout: 30_000 });
      await add
        .getByText(/^Reaches/)
        .first()
        .waitFor();
      await shoot(page, "publish-domain-app");
      await add.getByRole("button", { name: "Change the way in" }).click();
      await add
        .getByRole("radiogroup", { name: "Way in" })
        .getByRole("radio", { name: /Something else in front/ })
        .click();
      await add.getByRole("button", { name: /Review plan/ }).click();
      const frontPlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await frontPlan.waitFor({ timeout: 30_000 });
      if ((await frontPlan.getByRole("region", { name: "At Cloudflare" }).count()) > 0) {
        fail("Something else in front still writes at Cloudflare");
      }
      await frontPlan.getByRole("button", { name: /Cancel/ }).click();
      await add.waitFor();

      step("adding a domain through Cloudflare: Replace it, and the plan's replace step");
      const hand = (
        await cloudflare(`/zones/${zoneId}/dns_records`, {
          method: "POST",
          body: JSON.stringify({
            type: "A",
            name: "vpn.example.com",
            content: "203.0.113.9",
            ttl: 300,
            proxied: false,
          }),
        })
      ).result as { id: string };
      try {
        await add.getByRole("button", { name: "Change the way in" }).click();
        await add
          .getByRole("radiogroup", { name: "Way in" })
          .getByRole("radio", { name: /Through the tunnel web-1/ })
          .click();
        await addSub.fill("vpn");
        await add.getByText("A record exists: A 203.0.113.9, made in Cloudflare").waitFor({ timeout: 30_000 });
        await add.getByRole("button", { name: "Replace it" }).click();
        await add.getByRole("button", { name: /Review plan/ }).click();
        const replacePlan = onTop(page.getByRole("dialog", { name: "Review plan" }));
        await replacePlan
          .getByRole("region", { name: "At Cloudflare" })
          .getByText("Replace the DNS record for vpn.example.com at Cloudflare, keeping what was there")
          .waitFor({ timeout: 30_000 });
        await shoot(page, "publish-domain-replace-plan");
        await replacePlan.getByRole("button", { name: /Cancel/ }).click();
        await add.getByRole("button", { name: "Cancel" }).click();
      } finally {
        await cloudflare(`/zones/${zoneId}/dns_records/${hand.id}`, { method: "DELETE" });
      }

      step(
        "adding a domain through Cloudflare: Domains says where each name stands at Cloudflare, with the connection beside the list, and Sync now reads it",
      );
      // The fake engine can't run the ingress, so no route here is published: the list
      // is answered by hand for that, and the connection beside it, its Sync now and the
      // token's rights are the control plane's and the stand-in's (#820).
      const named = (name: string, way: number, state: number) => {
        const publish = Buffer.concat([
          number(1, way),
          way === 1 ? bytes(2, TUNNEL) : Buffer.alloc(0),
          bytes(3, "cloudflare"),
        ]);
        const route = Buffer.concat([
          bytes(1, `rte_${name}`),
          bytes(2, name.replaceAll(".", "-")),
          bytes(4, "wiki-app"),
          number(5, 80),
          bytes(7, "off"),
          bytes(81, "127.0.0.1"),
          bytes(113, publish),
        ]);
        const on = Buffer.concat([bytes(1, server.id), bytes(2, server.name), bytes(3, bytes(1, name))]);
        const said = Buffer.concat([
          bytes(1, name),
          number(2, way),
          way === 1 ? bytes(3, "web-1") : Buffer.alloc(0),
          number(5, state),
        ]);
        return bytes(
          1,
          Buffer.concat([
            bytes(1, name),
            bytes(2, appEnv),
            bytes(3, `https://${name}`),
            bytes(4, route),
            bytes(5, on),
            bytes(6, said),
          ]),
        );
      };
      // Ways: 1 the server's tunnel, 3 Cloudflare's proxy. States: 1 published, 3 its record gone.
      const listed = Buffer.concat([
        named("docs.example.com", 3, 1),
        named("help.example.com", 1, 3),
        named("wiki.example.com", 1, 1),
      ]);
      await page.route(/DomainService\/ListDomains/, (route) =>
        route.fulfill({ status: 200, contentType: "application/proto", body: listed }),
      );
      try {
        await page.goto(`${origin}/domains`);
        const table = page.getByRole("table", { name: "Domains" });
        await table.getByRole("columnheader", { name: "At Cloudflare" }).waitFor({ timeout: 20_000 });
        const help = table.getByRole("row", { name: /^help\.example\.com/ });
        await help.getByText("Its DNS record is gone at Cloudflare").waitFor();
        await help.getByRole("button", { name: "Put help.example.com back at Cloudflare" }).waitFor();
        await table
          .getByRole("row", { name: /^wiki\.example\.com/ })
          .getByText("by *.example.com")
          .waitFor();
        await table
          .getByRole("row", { name: /^docs\.example\.com/ })
          .getByText("proxied")
          .waitFor();
        const panel = page.getByRole("region", { name: "Cloudflare" });
        await panel.getByText("example.com, example.org").waitFor();
        await panel.getByRole("button", { name: "Sync now" }).click();
        // The read, and the token's rights as the stand-in grants them.
        await panel.getByText("Edits DNS, publishes through tunnels").waitFor({ timeout: 30_000 });
        await panel.getByText(/^(\d+ s ago|just now|now)$/).waitFor();
        await shoot(page, "publish-domain-domains");
      } finally {
        await page.unroute(/DomainService\/ListDomains/);
      }
    } finally {
      // The run goes, so nothing after reads a tunnel the machine doesn't run.
      rmSync(join(proc, PID), { recursive: true, force: true });
    }
  },
};
