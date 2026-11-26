// SPDX-License-Identifier: AGPL-3.0-only

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

/**
 * Verifies Track A polish across the live panel:
 * 1. Command bar concise page labels and global operational verbs (Terminal, Files, Updates, Logs, Restart -> plan).
 * 2. Open critical Alerts badge on desktop rail and phone More tab with accessible names.
 * 3. Shared PaginationBar navigation and announcements.
 */
export const flow: Flow = {
  order: 98,
  run: async (page, run) => {
    const { origin, step } = run;

    step("polish-a: command bar concise page labels and server operational verbs");
    await atWidth(page, { width: 1440, height: 900 });
    const serverPath = await ownServerPath(page, origin);
    const serversRes = await page.request.get(`${origin}/v1/servers`);
    const { servers } = (await serversRes.json()) as {
      servers?: { id: string; name: string; controlServer?: boolean }[];
    };
    const own = servers?.find((s) => s.controlServer);
    if (!own) throw new Error("polish-a: control server not found");

    await page.goto(`${origin}/`);
    await page.getByRole("heading", { level: 1 }).waitFor();
    await page.keyboard.press("Control+k");
    const bar = page.getByRole("dialog", { name: "Command bar" });
    await bar.waitFor();
    await bar.getByRole("option", { name: "Overview", exact: true }).waitFor();
    await bar.getByRole("option", { name: "Alerts", exact: true }).waitFor();
    await bar.getByRole("option", { name: `Terminal on ${own.name}`, exact: true }).waitFor();
    await bar.getByRole("option", { name: `Updates on ${own.name}`, exact: true }).waitFor();
    await bar.getByRole("option", { name: `Files on ${own.name}`, exact: true }).click();
    await page.waitForURL(`**${serverPath}/files**`);

    step("polish-a: command bar app Restart verb opens plan, alerts badge on desktop & phone, PaginationBar");
    const probe = await page.context().newPage();
    try {
      // Two firing alerts (one critical) for the desktop rail and phone More tab.
      const firingAlerts = Buffer.concat([
        bytes(1, Buffer.concat([bytes(1, "alt_p1"), bytes(3, "check-down"), number(5, 2), number(6, 1)])),
        bytes(1, Buffer.concat([bytes(1, "alt_p2"), bytes(3, "disk-high"), number(5, 1), number(6, 1)])),
        number(3, 2),
      ]);
      await probe.route(/\/ListAlerts(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: firingAlerts }),
      );

      // 55 websites to exercise the shared PaginationBar.
      const siteBody = Buffer.concat(
        Array.from({ length: 55 }, (_, i) => {
          const n = String(i + 1).padStart(2, "0");
          return bytes(
            1,
            Buffer.concat([
              bytes(1, `prj_pa_site_${i}`),
              bytes(2, `pa-site-${n}`),
              bytes(3, `pa-site-${n}.example.com`),
              bytes(4, bytes(1, bytes(1, "https://upstream.example.com"))),
              bytes(5, `env_pa_site_${i}`),
            ]),
          );
        }),
      );
      await probe.route(/\/ListWebsites(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: siteBody }),
      );

      // One running, placed app + restart plan response so Command Bar Restart opens Review plan.
      const appSpec = bytes(1, bytes(1, "ghcr.io/example/api:1.0.0"));
      const appRef = Buffer.concat([bytes(1, "service"), bytes(2, "env_pa"), bytes(3, "billing-api")]);
      const appDeployment = Buffer.concat([bytes(1, own.id), bytes(2, own.name), number(3, 2)]); // DEPLOYMENT_STATE_LIVE
      const serviceInfo = Buffer.concat([
        bytes(1, "svc_pa_api"),
        bytes(2, appRef),
        number(3, 1),
        bytes(5, appSpec),
        bytes(6, appDeployment),
      ]);
      await probe.route(/\/ListServices(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: bytes(1, serviceInfo) }),
      );
      let restartRequested = false;
      const plannedChange = Buffer.concat([
        bytes(1, "chg_pa_restart"),
        bytes(2, "Restart billing-api"),
        number(3, 1), // PLANNED
      ]);
      await probe.route(/\/RestartService(?:\?|$)/, (route) => {
        restartRequested = true;
        return route.fulfill({ contentType: "application/proto", body: bytes(1, plannedChange) });
      });

      await atWidth(probe, { width: 1440, height: 900 });
      await probe.goto(`${origin}/websites`);

      // Desktop rail Alerts badge with critical styling and accessible name.
      const desktopRail = probe.getByRole("navigation", { name: "Main" });
      const alertsLink = desktopRail.getByRole("link", { name: "Alerts 2 alerts open, including critical" });
      await alertsLink.waitFor();
      await alertsLink.locator(".rail-count.is-critical").getByText("2", { exact: true }).waitFor();

      // Shared PaginationBar on Websites list.
      await probe.locator(".dw-pager-summary").getByText("1-50 of 55", { exact: true }).waitFor();
      await probe.getByRole("button", { name: "Next", exact: true }).click();
      await probe.waitForURL((url) => url.searchParams.get("page") === "2");
      await probe.locator(".dw-pager-summary").getByText("51-55 of 55", { exact: true }).waitFor();
      await probe.getByRole("button", { name: "Previous", exact: true }).click();
      await probe.locator(".dw-pager-summary").getByText("1-50 of 55", { exact: true }).waitFor();

      // Command Bar app verbs: Logs for & Restart -> opens Review plan.
      await probe.keyboard.press("Control+k");
      const probeBar = probe.getByRole("dialog", { name: "Command bar" });
      await probeBar.waitFor();
      await probeBar.getByRole("option", { name: "Logs for billing-api", exact: true }).waitFor();
      await probeBar.getByRole("option", { name: "Restart billing-api Opens a plan", exact: true }).click();
      await probe.getByRole("dialog", { name: "Review plan" }).waitFor();
      if (!restartRequested) throw new Error("polish-a: Restart command did not call RestartService");
      await probe.keyboard.press("Escape");

      // Phone More tab badge and accessible name.
      await atWidth(probe, { width: 390, height: 844 });
      const phoneNav = probe.getByRole("navigation", { name: "Main" });
      const moreTab = phoneNav.getByRole("link", { name: "More 2 alerts open, including critical" });
      await moreTab.waitFor();
      await moreTab.locator(".tabbar-dot.is-critical").waitFor();
    } finally {
      await probe.close();
    }
  },
};
