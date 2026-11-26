// SPDX-License-Identifier: AGPL-3.0-only

import { dockerRemove, dockerRun } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { captureHandyFollowups } from "./handy-followups-shots.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const pad = (n: number) => String(n + 1).padStart(2, "0");

export const flow: Flow = {
  order: 115,
  run: async (page, run) => {
    const { origin, step, socket } = run;
    const serverPath = await ownServerPath(page, origin);
    const server = serverPath.split("/").at(-1) ?? "";

    step("handy-followups: search keeps container selection and paused Kill restricts signals (#919 N3, #925 N1)");
    const pausedName = "handy-paused-ext";
    const engineId = await dockerRun(socket, pausedName, { image: "acme/legacy:prints" });
    try {
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}${serverPath}/docker/containers`);
      const search = page.getByRole("textbox", { name: "Search containers" });
      await search.fill("handy-paused");
      const box = page.getByRole("checkbox", { name: `Select ${pausedName}` });
      await box.waitFor();
      await box.click();
      await search.fill(pausedName);
      await page.getByRole("link", { name: pausedName, exact: true }).waitFor();
      if (!(await box.isChecked())) {
        throw new Error("Container selection was cleared while typing a matching search");
      }
      await box.click();
      await page.getByRole("button", { name: `Actions for ${pausedName}` }).click();
      await page.getByRole("menuitem", { name: "Pause", exact: true }).click();
      const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await review.getByRole("button", { name: /^Apply plan/ }).click();
      await page.getByRole("dialog").getByRole("button", { name: /^Done/ }).click({ timeout: 35_000 });
      await page
        .getByRole("row", { name: new RegExp(pausedName) })
        .getByText("Paused", { exact: true })
        .waitFor();

      await page.getByRole("button", { name: `Actions for ${pausedName}` }).click();
      await page.getByRole("menuitem", { name: "Kill", exact: true }).click();
      const killDialog = page.getByRole("dialog", { name: "Kill", exact: true });
      await killDialog.waitFor();
      const options = await killDialog.getByRole("combobox", { name: "Signal" }).locator("option").allTextContents();
      if (options.length !== 2 || options[0] !== "SIGTERM, then SIGKILL" || options[1] !== "SIGKILL") {
        throw new Error(`Paused Kill offered unexpected signals: ${options.join(", ")}`);
      }
      await killDialog.getByText(/A paused container takes only SIGKILL, or SIGTERM then SIGKILL/).waitFor();
      await killDialog.getByRole("button", { name: "Cancel" }).click();
      await killDialog.waitFor({ state: "hidden" });
    } finally {
      await dockerRemove(socket, engineId);
    }

    step(
      "handy-followups: destinations search/paging, backups scope wording, connections Used by cap, websites batch bar",
    );
    const probe = await page.context().newPage();
    try {
      await atWidth(probe, { width: 1440, height: 900 });
      const destBody = Buffer.concat(
        Array.from({ length: 12 }, (_, i) =>
          bytes(
            1,
            Buffer.concat([
              bytes(1, `dst_flow_${i}`),
              bytes(2, `archive-${pad(i)}`),
              bytes(3, "https://s3.example.com"),
              bytes(4, `bucket-${pad(i)}`),
              number(6, 2 + i),
              number(7, (5 + i) * 1_000_000),
            ]),
          ),
        ),
      );
      await probe.route(/\/ListBackupDestinations(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: destBody }),
      );
      const bkpBody = Buffer.concat([
        ...Array.from({ length: 50 }, (_, i) =>
          bytes(
            1,
            Buffer.concat([
              bytes(1, `bkp_flow_${i}`),
              bytes(2, "env_flow"),
              bytes(3, `orders-vol-${pad(i)}`),
              bytes(4, `vol_flow_${i}`),
              bytes(5, server),
              bytes(6, "hz-1"),
              number(7, 2),
              number(8, 1),
              number(10, (10 + i) * 1_000_000),
            ]),
          ),
        ),
        bytes(2, "older-cursor"),
      ]);
      await probe.route(/\/ListBackups(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: bkpBody }),
      );
      const useMsg = Buffer.concat([
        bytes(1, "lnk_flow_1"),
        bytes(2, "media"),
        bytes(3, "env_flow"),
        bytes(4, "prj_flow"),
        ...Array.from({ length: 6 }, (_, i) => bytes(7, `svc_f_${i}`)),
        ...Array.from({ length: 6 }, (_, i) => bytes(8, `app-${pad(i)}`)),
      ]);
      const connBody = bytes(
        1,
        Buffer.concat([
          bytes(1, "con_flow_1"),
          bytes(2, "shared-media"),
          bytes(3, "Shared media"),
          number(4, 1),
          bytes(5, "https://s3.example.com"),
          bytes(7, "media-bucket"),
          number(10, 1),
          bytes(12, useMsg),
        ]),
      );
      await probe.route(/\/ListConnections(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: connBody }),
      );
      await probe.goto(`${origin}/backups?q=orders`);
      await probe.getByText("Matches from the newest 50 backups.", { exact: true }).waitFor();
      const destRegion = probe.getByRole("region", { name: "Destinations" });
      await destRegion.getByText("1-8 of 12", { exact: true }).waitFor();
      await destRegion.getByRole("button", { name: "Next", exact: true }).click();
      await destRegion.getByText("archive-12", { exact: true }).waitFor();
      await destRegion.getByRole("searchbox", { name: "Filter destinations" }).fill("archive-04");
      await destRegion.getByText("archive-04", { exact: true }).waitFor();

      await probe.goto(`${origin}/connections`);
      const connRow = probe.getByRole("row", { name: /shared-media/ });
      await connRow.getByRole("button", { name: "and 3 more", exact: true }).click();
      await connRow.getByText(/app-06/).waitFor();
      await connRow.getByRole("button", { name: "Show fewer", exact: true }).click();

      const siteActive = Buffer.concat([
        bytes(1, "prj_flow_site_1"),
        bytes(2, "shop-site"),
        bytes(3, "shop.example.com"),
        bytes(4, bytes(1, bytes(1, "https://backend.example.com"))),
        bytes(5, "env_flow_site_1"),
      ]);
      const sitePaused = Buffer.concat([
        bytes(1, "prj_flow_site_2"),
        bytes(2, "docs-site"),
        bytes(3, "docs.example.com"),
        bytes(4, Buffer.concat([bytes(1, bytes(1, "https://docs-backend.example.com")), number(5, 1), number(6, 1)])),
        bytes(5, "env_flow_site_2"),
      ]);
      const siteBody = Buffer.concat([bytes(1, siteActive), bytes(1, sitePaused)]);
      await probe.route(/\/ListWebsites(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: siteBody }),
      );
      await probe.goto(`${origin}/websites`);
      await probe.getByRole("checkbox", { name: "Select shop.example.com" }).click();
      await probe.getByRole("checkbox", { name: "Select docs.example.com" }).click();
      const bar = probe.getByRole("toolbar", { name: "Actions for the rows selected" });
      await bar.getByRole("button", { name: "Pause 1", exact: true }).waitFor();
      await bar.getByRole("button", { name: "Resume 1", exact: true }).waitFor();
      await bar.getByRole("button", { name: "Set HTTPS 2", exact: true }).waitFor();
      await bar.getByText(/Already paused/).waitFor();
    } finally {
      await probe.close();
    }

    const directory = process.env["DOCKWELL_HANDY_FOLLOWUPS_SHOTS"];
    if (directory) await captureHandyFollowups(page, run, directory);
  },
};
