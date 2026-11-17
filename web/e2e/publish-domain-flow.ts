// SPDX-License-Identifier: AGPL-3.0-only

// Adding a domain through Cloudflare (ADR 0104), on Cloudflare's stand-in beside the
// fake engine: a cloudflared run in the stand-in /proc, connected to web-1 as the
// stand-in's connector says, so the agent finds the server's tunnel and the control
// plane proves it. New website picks its name from a connected zone, checks it live,
// takes the tunnel by default with the others one step away, and its plan lists what
// it writes at Cloudflare. A record made in Cloudflare by hand is said with Replace it.
// The fake engine can't run the ingress, so the plan isn't applied here.

import { mkdirSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { atWidth, onTop } from "./sheets.ts";

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
function runCloudflared(proc: string): void {
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
  run: async (page, { origin, step, shoot }) => {
    const api = process.env["DOCKWELL_TEST_CLOUDFLARE"];
    const proc = process.env["DOCKWELL_TEST_PROC"];
    if (!api || !proc) {
      step("adding a domain through Cloudflare: there's no stand-in for Cloudflare here");
      return;
    }
    runCloudflared(proc);
    // The control plane keeps what a server's tunnels were for 30 s, so a check another
    // flow made a moment ago without this run doesn't stand in for one with it.
    await page.waitForTimeout(31_000);
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
      await sub.fill("docs");
      await sheet.getByText(/^Through the tunnel web-1 on /).waitFor({ timeout: 30_000 });
      await atWidth(page, { width: 390, height: 844 });
      await shoot(page, "publish-domain-phone");
      await atWidth(page, { width: 1440, height: 1000 });
      await sheet.getByRole("button", { name: "Cancel" }).click();
    } finally {
      // The run goes, so nothing after reads a tunnel the machine doesn't run.
      rmSync(join(proc, PID), { recursive: true, force: true });
    }
  },
};
