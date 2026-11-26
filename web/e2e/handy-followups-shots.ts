// SPDX-License-Identifier: AGPL-3.0-only

import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import type { Page } from "playwright-core";
import { dockerRemove, dockerRun } from "./docker-probe.ts";
import type { FlowRun } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { proposeFixture } from "./row-verbs-shots.ts";
import { atWidth, onTop } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const pad = (n: number) => String(n + 1).padStart(2, "0");

/** Captures before/after screenshots at 1440 and 390 for the handy follow-ups track. */
export async function captureHandyFollowups(page: Page, { origin, step, socket }: FlowRun, directory: string) {
  await mkdir(directory, { recursive: true });
  const serverPath = await ownServerPath(page, origin);
  const server = serverPath.split("/").at(-1) ?? "";
  const { resources: projects } = await page.request
    .get(`${origin}/v1/resources?kind=project`)
    .then((r) => r.json() as Promise<{ resources: { id: string; resource: { name: string } }[] }>);
  const { resources: envs } = await page.request
    .get(`${origin}/v1/resources?kind=environment`)
    .then((r) => r.json() as Promise<{ resources: { id: string; resource: { name: string; scope: string } }[] }>);

  // Create a stack project with 35 apps + 20 default-project apps so collapsing the stack shows #914 N2 clearly.
  const stackName = "shot-stack";
  if (!projects.some((p) => p.resource.name === stackName)) {
    await proposeFixture(page, "Shot stack project", [
      { resource: { kind: "project", name: stackName }, spec: { project: {} } },
    ]);
  }
  const freshProjects = await page.request
    .get(`${origin}/v1/resources?kind=project`)
    .then((r) => r.json() as Promise<{ resources: { id: string; resource: { name: string } }[] }>);
  const freshEnvs = await page.request
    .get(`${origin}/v1/resources?kind=environment`)
    .then((r) => r.json() as Promise<{ resources: { id: string; resource: { name: string; scope: string } }[] }>);
  const stackProj = freshProjects.resources.find((p) => p.resource.name === stackName);
  const stackEnv = freshEnvs.resources.find((e) => e.resource.scope === stackProj?.id)?.id;
  const defaultEnv = envs.find((e) => e.resource.name === "production")?.id;
  if (!stackEnv || !defaultEnv) throw new Error("Missing environments for handy-followups shots");

  await proposeFixture(page, "Shot stack apps", [
    ...Array.from({ length: 35 }, (_, i) => ({
      resource: { kind: "service", name: `stack-svc-${pad(i)}`, scope: stackEnv },
      spec: { service: { app: { image: "nginx:1.27", desiredRunState: "DESIRED_RUN_STATE_STOPPED" } } },
      servers: [server],
    })),
    ...Array.from({ length: 20 }, (_, i) => ({
      resource: { kind: "service", name: `solo-app-${pad(i)}`, scope: defaultEnv },
      spec: { service: { app: { image: "nginx:1.27", desiredRunState: "DESIRED_RUN_STATE_STOPPED" } } },
      servers: [server],
    })),
  ]);

  const pausedName = "shot-paused-ext";
  const engineId = await dockerRun(socket, pausedName, { image: "acme/legacy:prints" });
  try {
    // Pause the external container via the UI once (on `page`, which has the virtual passkey) so we can open its Kill sheet.
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}${serverPath}/docker/containers`);
    await page.getByRole("textbox", { name: "Search containers" }).fill(pausedName);
    await page.getByRole("link", { name: pausedName, exact: true }).waitFor();
    await page.getByRole("button", { name: `Actions for ${pausedName}` }).click();
    await page.getByRole("menuitem", { name: "Pause", exact: true }).click();
    const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await page.getByRole("dialog").getByRole("button", { name: /^Done/ }).click({ timeout: 35_000 });
    await page
      .getByRole("row", { name: new RegExp(pausedName) })
      .getByText("Paused", { exact: true })
      .waitFor();

    // Capture Kill sheet on paused container at 1440 and 390.
    for (const width of [1440, 390]) {
      step(`handy-followups shots: paused kill sheet at ${width}px`);
      await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
      await page.goto(`${origin}${serverPath}/docker/containers`);
      await page.getByRole("textbox", { name: "Search containers" }).fill(pausedName);
      await page.getByRole("link", { name: pausedName, exact: true }).waitFor();
      await page.getByRole("button", { name: `Actions for ${pausedName}` }).click();
      await page.getByRole("menuitem", { name: "Kill", exact: true }).click();
      await page.getByRole("dialog", { name: "Kill", exact: true }).waitFor();
      await page.screenshot({ path: join(directory, `containers-kill-paused-${width}.png`) });
      await page.getByRole("dialog", { name: "Kill", exact: true }).getByRole("button", { name: "Cancel" }).click();
    }

    // Capture Apps with collapsed stack of 35 + 20 solo apps at 1440 and 390.
    for (const width of [1440, 390]) {
      step(`handy-followups shots: collapsed apps paging at ${width}px`);
      await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
      await page.goto(`${origin}/apps`);
      if (width === 1440) {
        const fold = page.getByRole("button", { name: `Fold ${stackName}` });
        const unfold = page.getByRole("button", { name: `Unfold ${stackName}` });
        await fold.or(unfold).waitFor();
        if (await fold.isVisible()) await fold.click();
        await unfold.waitFor();
      } else {
        await page.getByText(stackName, { exact: true }).waitFor();
      }
      await page.screenshot({ path: join(directory, `apps-collapsed-${width}.png`), fullPage: true });
    }

    // Mocked list fixtures on a fresh tab for Backups (20 destinations + loaded backups banner),
    // Connections (8 apps in Used by), and Websites (mixed active + paused batch bar).
    const mockPage = await page.context().newPage();
    try {
      const destBody = Buffer.concat(
        Array.from({ length: 20 }, (_, i) =>
          bytes(
            1,
            Buffer.concat([
              bytes(1, `dst_shot_${i}`),
              bytes(2, `archive-${pad(i)}`),
              bytes(3, "https://s3.example.com"),
              bytes(4, `bucket-${pad(i)}`),
              number(6, 4 + i),
              number(7, (10 + i) * 1_000_000),
            ]),
          ),
        ),
      );
      await mockPage.route(/\/ListBackupDestinations(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: destBody }),
      );
      const bkpBody = Buffer.concat([
        ...Array.from({ length: 50 }, (_, i) =>
          bytes(
            1,
            Buffer.concat([
              bytes(1, `bkp_shot_${i}`),
              bytes(2, defaultEnv),
              bytes(3, `orders-vol-${pad(i)}`),
              bytes(4, `vol_shot_${i}`),
              bytes(5, server),
              bytes(6, "hz-1"),
              number(7, 2),
              number(8, 1),
              number(10, (12 + i) * 1_000_000),
            ]),
          ),
        ),
        bytes(2, "older-cursor"),
      ]);
      await mockPage.route(/\/ListBackups(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: bkpBody }),
      );

      const useMsg = Buffer.concat([
        bytes(1, "lnk_shot_1"),
        bytes(2, "media"),
        bytes(3, defaultEnv),
        bytes(4, "prj_default"),
        ...Array.from({ length: 8 }, (_, i) => bytes(7, `svc_u_${i}`)),
        ...Array.from({ length: 8 }, (_, i) => bytes(8, `app-${pad(i)}`)),
      ]);
      const connBody = bytes(
        1,
        Buffer.concat([
          bytes(1, "con_shot_1"),
          bytes(2, "shared-media"),
          bytes(3, "Shared media"),
          number(4, 1),
          bytes(5, "https://s3.example.com"),
          bytes(7, "media-bucket"),
          number(10, 1),
          bytes(12, useMsg),
        ]),
      );
      await mockPage.route(/\/ListConnections(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: connBody }),
      );

      const siteActive = Buffer.concat([
        bytes(1, "prj_site_1"),
        bytes(2, "shop-site"),
        bytes(3, "shop.example.com"),
        bytes(4, bytes(1, bytes(1, "https://backend.example.com"))),
        bytes(5, "env_site_1"),
      ]);
      const sitePaused = Buffer.concat([
        bytes(1, "prj_site_2"),
        bytes(2, "docs-site"),
        bytes(3, "docs.example.com"),
        bytes(4, Buffer.concat([bytes(1, bytes(1, "https://docs-backend.example.com")), number(5, 1), number(6, 1)])),
        bytes(5, "env_site_2"),
      ]);
      const siteBody = Buffer.concat([bytes(1, siteActive), bytes(1, sitePaused)]);
      await mockPage.route(/\/ListWebsites(?:\?|$)/, (route) =>
        route.fulfill({ contentType: "application/proto", body: siteBody }),
      );

      for (const width of [1440, 390]) {
        await atWidth(mockPage, { width, height: width === 1440 ? 900 : 844 });

        step(`handy-followups shots: backups destinations + search banner at ${width}px`);
        await mockPage.goto(`${origin}/backups?q=orders`);
        await mockPage.getByRole("button", { name: "Search all backups" }).waitFor();
        await mockPage.screenshot({ path: join(directory, `backups-destinations-${width}.png`), fullPage: true });

        step(`handy-followups shots: connections used-by at ${width}px`);
        await mockPage.goto(`${origin}/connections`);
        await mockPage.getByText("shared-media", { exact: true }).waitFor();
        await mockPage.screenshot({ path: join(directory, `connections-used-by-${width}.png`) });

        step(`handy-followups shots: websites batch bar at ${width}px`);
        await mockPage.goto(`${origin}/websites`);
        await mockPage.getByText("shop.example.com", { exact: true }).waitFor();
        await mockPage.getByRole("checkbox", { name: "Select shop.example.com" }).click();
        await mockPage.getByRole("checkbox", { name: "Select docs.example.com" }).click();
        await mockPage.getByRole("toolbar", { name: "Actions for the rows selected" }).waitFor();
        await mockPage.screenshot({ path: join(directory, `websites-batch-${width}.png`) });
      }
    } finally {
      await mockPage.close();
    }
  } finally {
    await dockerRemove(socket, engineId);
  }
}
