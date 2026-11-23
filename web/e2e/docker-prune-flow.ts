// SPDX-License-Identifier: AGPL-3.0-only

import { request } from "node:http";

import type { Page } from "playwright-core";
import type { Flow } from "./flows.ts";
import { holdRead } from "./loading-flow.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

function enginePost(socket: string, path: string, body: unknown): Promise<void> {
  return new Promise((done, failed) => {
    const payload = JSON.stringify(body);
    const req = request(
      {
        socketPath: socket,
        path,
        method: "POST",
        headers: { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) },
      },
      (res) => {
        let out = "";
        res.on("data", (chunk: Buffer) => {
          out += chunk.toString();
        });
        res.on("end", () => {
          if ((res.statusCode ?? 500) >= 300) failed(new Error(`Docker POST ${path}: ${res.statusCode} ${out}`));
          else done();
        });
      },
    );
    req.on("error", failed);
    req.end(payload);
  });
}

async function fits(page: Page, selector: string) {
  const viewport = page.viewportSize();
  try {
    for (const colorScheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme });
      for (const width of [1440, 390]) {
        await atWidth(page, { width, height: 900 });
        const overflow = await page
          .locator(selector)
          .evaluate((root) =>
            [root, ...root.querySelectorAll("code, h2, th, td, .docker-cleanup-item, .docker-filters")]
              .filter(
                (node) =>
                  node.getBoundingClientRect().width > 0 &&
                  node.scrollWidth > node.clientWidth + 2 &&
                  getComputedStyle(node).display !== "inline",
              )
              .map((node) => node.textContent?.trim().slice(0, 70)),
          );
        if (overflow.length)
          throw new Error(`Docker prune layout overflow at ${width}px in ${colorScheme}: ${overflow.join(", ")}`);
      }
    }
  } finally {
    await page.emulateMedia({ colorScheme: "light" });
    if (viewport) await atWidth(page, viewport);
  }
}

export const flow: Flow = {
  order: 2,
  async run(page, { origin, socket, step, shoot }) {
    const server = await ownServerPath(page, origin);
    const anonName = "e".repeat(64);
    const anonShort = anonName.slice(0, 12);
    const namedName = "prune-flow-named-volume";
    const composeVolName = "shop-db";
    const netName = "prune-flow-unused-network";
    const detailVolName = "prune-flow-detail-volume";

    step("Docker prune: seed unused volumes, Compose volume and network");
    await enginePost(socket, "/volumes/create", {
      Name: anonName,
      Driver: "local",
      Labels: { "com.docker.volume.anonymous": "" },
    });
    await enginePost(socket, "/volumes/create", { Name: namedName, Driver: "local" });
    await enginePost(socket, "/volumes/create", {
      Name: composeVolName,
      Driver: "local",
      Labels: { "com.docker.compose.project": "shop", "com.docker.compose.volume": "db" },
    });
    await enginePost(socket, "/networks/create", { Name: netName, Driver: "bridge" });

    step("Docker prune: Cleanup tab in Docker tab bar (server and fleet)");
    await page.goto(`${origin}${server}/docker/images`);
    const nav = page.getByRole("navigation", { name: "Docker views" });
    await nav.getByRole("link", { name: "Cleanup", exact: true }).waitFor();
    await page.getByRole("button", { name: "Remove unused", exact: true }).waitFor();

    // Capture the prune sheet in its loading state while PreviewDockerCleanup is held.
    step("Docker prune: sheet loading state");
    const held = await holdRead(page, "DockerService.PreviewDockerCleanup");
    try {
      await page.getByRole("button", { name: "Remove unused", exact: true }).click();
      await page.getByRole("dialog", { name: "Remove unused images" }).waitFor();
      await page.locator(".docker-prune-sheet .dw-skeleton-rows").waitFor();
      await page.waitForTimeout(220);
      await shoot(page, "docker-prune-sheet-loading");
    } finally {
      await held();
    }
    await onTop(page.getByRole("dialog")).locator(".dw-skeleton-rows").waitFor({ state: "hidden", timeout: 15_000 });
    await onTop(page.getByRole("dialog"))
      .getByRole("button", { name: /^(Cancel|Close)$/ })
      .last()
      .click();

    await page.getByRole("button", { name: "Refresh images", exact: true }).click();
    await page
      .locator(".docker-image-table, .docker-artifact-list")
      .getByText(/Unused|Dangling/)
      .first()
      .waitFor({ timeout: 15_000 });
    await fits(page, ".docker-workspace");
    await shoot(page, "docker-prune-images");

    step("Docker prune: Cleanup tab shows all kinds with safe defaults");
    await nav.getByRole("link", { name: "Cleanup", exact: true }).click();
    await page.getByRole("heading", { name: "Cleanup", exact: true }).waitFor();
    const anonBox = page.getByRole("checkbox", { name: new RegExp(anonShort) });
    const namedBox = page.getByRole("checkbox", { name: new RegExp(namedName) });
    const netBox = page.getByRole("checkbox", { name: new RegExp(netName) });
    await anonBox.waitFor({ timeout: 30_000 });
    if (!(await anonBox.isChecked()) || !(await netBox.isChecked()) || (await namedBox.isChecked())) {
      throw new Error("Cleanup tab did not apply safe defaults for anonymous volume, network and named volume");
    }
    await page.getByText("Protected artifacts", { exact: true }).click();
    await page.getByText("Belongs to Compose project shop", { exact: true }).waitFor();
    await fits(page, ".docker-workspace");
    await shoot(page, "docker-prune-cleanup");
    await page.locator(".docker-cleanup-footer").scrollIntoViewIfNeeded();
    await shoot(page, "docker-prune-cleanup-footer");

    step("Docker prune: Volumes tab Remove unused sheet (filled, done, empty)");
    await nav.getByRole("link", { name: "Volumes", exact: true }).click();
    await page.waitForURL(/\/docker\/volumes$/);
    // The Cleanup tab's "Volumes" sections can linger for a frame; wait on the Volumes table itself.
    await page.getByRole("link", { name: namedName, exact: true }).waitFor({ timeout: 30_000 });
    await fits(page, ".docker-workspace");
    await shoot(page, "docker-prune-volumes");

    await page.getByRole("button", { name: "Remove unused", exact: true }).click();
    const volSheet = onTop(page.getByRole("dialog", { name: "Remove unused volumes" }));
    await volSheet.getByRole("checkbox", { name: new RegExp(anonShort) }).waitFor({ timeout: 30_000 });
    if (!(await volSheet.getByRole("checkbox", { name: new RegExp(anonShort) }).isChecked())) {
      throw new Error("anonymous volume wasn't selected by default in Volumes sheet");
    }
    if (await volSheet.getByRole("checkbox", { name: new RegExp(namedName) }).isChecked()) {
      throw new Error("named volume was selected by default in Volumes sheet");
    }
    await volSheet.getByText("Data is deleted", { exact: true }).first().waitFor();
    await shoot(page, "docker-prune-sheet-filled");

    // Select all named volumes too so the subsequent sheet check shows the empty state cleanly.
    const includeAllNamed = volSheet.getByRole("checkbox", { name: /Include all/ });
    if ((await includeAllNamed.count()) > 0 && !(await includeAllNamed.isChecked())) {
      await includeAllNamed.check();
    }
    await volSheet.getByRole("button", { name: "Review plan", exact: true }).click();
    const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await review.getByText("Volume data is deleted permanently and cannot be undone.", { exact: true }).waitFor();
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    const done = onTop(page.getByRole("dialog"));
    await done.getByRole("button", { name: "Done", exact: true }).waitFor({ timeout: 30_000 });
    await done.getByText("Removed", { exact: true }).first().waitFor();
    await shoot(page, "docker-prune-sheet-done");
    await done.getByRole("button", { name: "Done", exact: true }).click();

    // Reopen Volumes prune sheet: no unused volumes remain, protected stopped-container & Compose volumes are shown.
    await page.getByRole("button", { name: "Remove unused", exact: true }).click();
    const emptySheet = onTop(page.getByRole("dialog", { name: "Remove unused volumes" }));
    await emptySheet.getByText("No unused volumes to remove", { exact: true }).waitFor({ timeout: 30_000 });
    await emptySheet.getByText("Mounted by a container", { exact: true }).first().waitFor();
    await emptySheet.getByText("Belongs to Compose project shop", { exact: true }).waitFor();
    await shoot(page, "docker-prune-sheet-empty");
    await emptySheet.getByRole("button", { name: "Close", exact: true }).last().click();

    step("Docker prune: Networks tab Remove unused sheet");
    await nav.getByRole("link", { name: "Networks", exact: true }).click();
    await page.getByRole("button", { name: "Remove unused", exact: true }).click();
    const netSheet = onTop(page.getByRole("dialog", { name: "Remove unused networks" }));
    await netSheet.getByRole("checkbox", { name: new RegExp(netName) }).waitFor({ timeout: 30_000 });
    await netSheet.getByRole("button", { name: "Review plan", exact: true }).click();
    const netReview = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await netReview.getByRole("button", { name: /^Apply plan/ }).click();
    await onTop(page.getByRole("dialog")).getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });

    step("Docker prune: Volume detail Remove volume action");
    await enginePost(socket, "/volumes/create", { Name: detailVolName, Driver: "local" });
    await nav.getByRole("link", { name: "Volumes", exact: true }).click();
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await page.getByRole("link", { name: detailVolName, exact: true }).click({ timeout: 30_000 });
    await page.getByRole("button", { name: "Remove volume", exact: true }).click();
    const singleSheet = onTop(page.getByRole("dialog", { name: "Remove unused volume" }));
    await singleSheet.getByRole("checkbox", { name: new RegExp(detailVolName) }).waitFor({ timeout: 30_000 });
    if (!(await singleSheet.getByRole("checkbox", { name: new RegExp(detailVolName) }).isChecked())) {
      throw new Error("single volume detail remove sheet did not preselect the focused volume");
    }
    await singleSheet.getByRole("button", { name: "Review plan", exact: true }).click();
    await onTop(page.getByRole("dialog", { name: "Review plan" }))
      .getByRole("button", { name: /^Apply plan/ })
      .click();
    await onTop(page.getByRole("dialog")).getByRole("button", { name: "Done", exact: true }).click({ timeout: 30_000 });

    step("Docker prune: Build cache Remove unused button in Overview disk table");
    await page.goto(`${origin}${server}/docker`);
    const disk = page.getByRole("table", { name: "Docker disk usage" });
    await disk.getByRole("button", { name: "Remove unused build cache", exact: true }).click({ timeout: 30_000 });
    await onTop(page.getByRole("dialog")).waitFor();
    await onTop(page.getByRole("dialog")).locator(".dw-skeleton-rows").waitFor({ state: "hidden", timeout: 15_000 });
    await onTop(page.getByRole("dialog"))
      .getByRole("button", { name: /^(Cancel|Close)$/ })
      .last()
      .click();
  },
};
