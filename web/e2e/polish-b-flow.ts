// SPDX-License-Identifier: AGPL-3.0-only

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

/**
 * Verifies Track B polish across the live panel:
 * 1. Docker Containers unified batch bar (one Stop/Pause verb across managed + external rows,
 *    Rename hidden on multi-select, clean system container skip note, managed plan opens via Review plan,
 *    and unverified Running status is not green).
 * 2. Offline server history collapses to one compact panel instead of five empty charts.
 * 3. ServerFrame secondary host tabs group under System on narrow viewports with zero horizontal overflow.
 */
export const flow: Flow = {
  order: 99,
  run: async (page, run) => {
    const { origin, step } = run;
    const serverPath = await ownServerPath(page, origin);
    const serversRes = await page.request.get(`${origin}/v1/servers`);
    const { servers } = (await serversRes.json()) as {
      servers?: { id: string; name: string; controlServer?: boolean }[];
    };
    const own = servers?.find((s) => s.controlServer);
    if (!own) throw new Error("polish-b: control server not found");

    const probe = await page.context().newPage();
    try {
      step("polish-b: Docker Containers unified batch bar and non-green unverified Running badge");
      const appSpec = bytes(1, bytes(1, "nginx:1.27"));
      const appRef = Buffer.concat([bytes(1, "service"), bytes(2, "env_pb"), bytes(3, "shop")]);
      const appDeployment = Buffer.concat([bytes(1, own.id), bytes(2, own.name), number(3, 2)]);
      const serviceInfo = Buffer.concat([
        bytes(1, "svc_pb_shop"),
        bytes(2, appRef),
        number(3, 1),
        bytes(5, appSpec),
        bytes(6, appDeployment),
      ]);
      await probe.route(/\/ListServices(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: bytes(1, serviceInfo) }),
      );

      const containerObj = (engineId: string, name: string, ownership: number, image: string, managed?: boolean) =>
        bytes(
          1,
          Buffer.concat([
            bytes(1, Buffer.concat([bytes(1, own.id), number(2, 1), bytes(3, engineId)])),
            bytes(2, name),
            number(3, ownership),
            ...(managed ? [bytes(5, appRef)] : []),
            number(6, 2), // CONTAINER_STATE_RUNNING
            ...(managed ? [bytes(7, "svc_pb_shop"), number(8, 1)] : []),
            bytes(10, bytes(1, image)),
          ]),
        );

      const objectsBody = Buffer.concat([
        containerObj("ctr_pb_shop", "shop-1", 1, "nginx:1.27", true), // MANAGED
        containerObj("ctr_pb_redis", "redis-cache", 2, "redis:7"), // EXTERNAL
        containerObj("ctr_pb_sys", "dockwell-proxy", 3, "caddy:2"), // SYSTEM
      ]);
      await probe.route(/\/ListDockerObjects(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: objectsBody }),
      );

      let proposeCalled = false;
      const plannedChange = Buffer.concat([
        bytes(1, "chg_pb_stop"),
        bytes(2, "Stop shop"),
        number(3, 1), // PLANNED
      ]);
      await probe.route(/\/ProposeChange(?:\?|$)/, (route) => {
        proposeCalled = true;
        return route.fulfill({ contentType: "application/proto", body: bytes(1, plannedChange) });
      });

      await atWidth(probe, { width: 1440, height: 900 });
      await probe.goto(`${origin}${serverPath}/docker/containers`);
      await probe.getByRole("checkbox", { name: "Select shop-1" }).waitFor({ timeout: 20_000 });

      // Unverified Running containers must use neutral running tone, never green healthy.
      await probe
        .locator(".dw-status.dw-tone-running")
        .filter({ hasText: /^Running$/ })
        .first()
        .waitFor();
      const healthyRunning = await probe
        .locator(".dw-status.dw-tone-healthy, .dw-badge.dw-tone-healthy")
        .filter({ hasText: /^Running$/ })
        .count();
      if (healthyRunning > 0) {
        throw new Error(`polish-b: found ${healthyRunning} green healthy badge(s) for unverified Running state`);
      }

      // Mixed managed + external selection: one Stop button, one Pause button, no Rename.
      await probe.getByRole("checkbox", { name: "Select shop-1" }).check();
      await probe.getByRole("checkbox", { name: "Select redis-cache" }).check();
      const toolbar = probe.getByRole("toolbar");
      await toolbar.waitFor();
      if ((await toolbar.getByRole("button", { name: /^Stop / }).count()) !== 1) {
        throw new Error("polish-b: expected exactly one Stop button for mixed managed+external selection");
      }
      if ((await toolbar.getByRole("button", { name: /^Pause / }).count()) !== 1) {
        throw new Error("polish-b: expected exactly one Pause button for mixed managed+external selection");
      }
      if ((await toolbar.getByRole("button", { name: /Rename/ }).count()) !== 0) {
        throw new Error("polish-b: Rename button should not appear on multi-select");
      }

      // Adding a system container produces a clean skip note without colliding verbs.
      await probe.getByRole("checkbox", { name: "Select dockwell-proxy" }).check();
      await toolbar.getByText(/System container/).waitFor();

      // Clicking Stop 2 opens Review plan (never acts behind a plan).
      await toolbar.getByRole("button", { name: "Stop 2" }).click();
      await probe.getByRole("dialog", { name: "Review plan" }).waitFor();
      if (!proposeCalled) throw new Error("polish-b: Stop 2 did not propose a managed change plan");
      await probe.keyboard.press("Escape");

      step("polish-b: offline server history collapses to one panel and phone ServerFrame groups secondary tabs");
      const nowSec = Math.floor(Date.now() / 1000);
      const offlinePoint = (offsetSec: number) =>
        bytes(
          7,
          Buffer.concat([
            bytes(1, number(1, nowSec - offsetSec)),
            number(2, 2), // HISTORY_POINT_STATE_OFFLINE
          ]),
        );
      const offlineHistory = Buffer.concat([
        bytes(1, number(1, nowSec - 3600)),
        bytes(2, number(1, nowSec)),
        offlinePoint(3600),
        offlinePoint(1800),
        offlinePoint(0),
      ]);
      await probe.route(/\/GetServerHistory(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: offlineHistory }),
      );

      await probe.goto(`${origin}${serverPath}`);
      const historySection = probe.getByRole("region", { name: "History" });
      await historySection.waitFor({ timeout: 20_000 });
      await historySection.getByText("The agent was offline").waitFor();
      const offlineCount = await historySection.getByText("The agent was offline").count();
      if (offlineCount !== 1) {
        throw new Error(`polish-b: expected 1 offline history message, found ${offlineCount}`);
      }
      if ((await historySection.getByRole("img").count()) !== 0) {
        throw new Error("polish-b: offline history should collapse instead of rendering 5 empty charts");
      }

      // Narrow phone viewport: secondary host tabs collapse into System menu with zero horizontal overflow.
      await atWidth(probe, { width: 390, height: 844 });
      const nav = probe.getByRole("navigation", { name: /^Server / });
      const systemMenu = nav.getByRole("button", { name: /System/ });
      await systemMenu.waitFor();
      const scrollWidth = await probe.evaluate(() => document.documentElement.scrollWidth);
      if (scrollWidth > 390) {
        throw new Error(`polish-b: server overview overflows 390px viewport (${scrollWidth}px)`);
      }
      await systemMenu.click();
      await probe.getByRole("menuitem", { name: "Firewall" }).waitFor();
      await probe.keyboard.press("Escape");
    } finally {
      await probe.close();
    }
  },
};
