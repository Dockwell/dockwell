// SPDX-License-Identifier: AGPL-3.0-only

import { writeFile } from "node:fs/promises";
import { join } from "node:path";

import type { Page } from "playwright-core";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

const image = "fixture/cleanup-image-with-a-long-repository-name:review";
const idle = "fixtureidlebuildcache0001";
const busy = "fixturebusybuildcache0002";
const network = "fixture-idle-network";
const anonVolume = "f".repeat(64);
const namedVolume = "fixture-unused-volume";

async function fits(page: Page, selector: string) {
  const viewport = page.viewportSize();
  try {
    for (const colorScheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme });
      for (const width of [1440, 1024, 390, 320]) {
        await atWidth(page, { width, height: 900 });
        const overflow = await page
          .locator(selector)
          .evaluate((root) =>
            [root, ...root.querySelectorAll("code, h2, th, td, dt, dd, .docker-cleanup-item, .docker-image-row")]
              .filter(
                (node) =>
                  node.getBoundingClientRect().width > 0 &&
                  node.scrollWidth > node.clientWidth + 2 &&
                  getComputedStyle(node).display !== "inline",
              )
              .map((node) => node.textContent?.trim().slice(0, 70)),
          );
        if (overflow.length)
          throw new Error(`Docker layout overflow at ${width}px in ${colorScheme}: ${overflow.join(", ")}`);
      }
    }
  } finally {
    await page.emulateMedia({ colorScheme: "light" });
    if (viewport) await atWidth(page, viewport);
  }
}

export const flow: Flow = {
  order: 1,
  async run(page, { data, origin, step, shoot }) {
    const server = await ownServerPath(page, origin);
    step("Docker cleanup: disk table, local image history and reviewed removals");
    await page.goto(`${origin}${server}/docker`);
    const nav = page.getByRole("navigation", { name: "Docker views" });
    const fullTabRule = await nav.evaluate((node) => {
      const parent = node.parentElement;
      return parent && node.getBoundingClientRect().right >= parent.getBoundingClientRect().right - 1;
    });
    if (!fullTabRule) throw new Error("the Docker tab rule stops before the edge of the workspace");
    const disk = page.getByRole("table", { name: "Docker disk usage" });
    await disk
      .getByRole("row", { name: /Build cache/ })
      .getByText("48 MB")
      .waitFor({ timeout: 30_000 });
    await disk
      .getByRole("row", { name: /Images/ })
      .getByText("10 MB", { exact: true })
      .waitFor();
    await fits(page, ".docker-disk-table");
    const viewport = page.viewportSize();
    try {
      for (const width of [1440, 1024]) {
        await atWidth(page, { width, height: 900 });
        const machine = page.locator(".rail-item", { has: page.locator(".rail-count.is-machine") });
        const layout = await machine.evaluate((node) => {
          const label = node.querySelector<HTMLElement>(".rail-label");
          const count = node.querySelector<HTMLElement>(".rail-count");
          if (!label || !count) return false;
          const previous = count.textContent;
          // Enrollment shortens hostnames to 56 characters. Exercise the
          // shared shell with a 60-character value without renaming the server.
          count.textContent = "fixture".padEnd(60, "a");
          try {
            return (
              label.textContent === "Server" &&
              label.scrollWidth <= label.clientWidth &&
              count.scrollWidth > count.clientWidth &&
              getComputedStyle(count).textOverflow === "ellipsis"
            );
          } finally {
            count.textContent = previous;
          }
        });
        if (!layout) throw new Error(`Server rail label or 60-character value did not fit at ${width}`);
      }
    } finally {
      if (viewport) await atWidth(page, viewport);
    }
    await shoot(page, "docker-slice4-overview");
    await disk.getByRole("link", { name: "Images" }).click();
    await page.getByRole("link", { name: image }).waitFor();
    await page.getByText("linux/amd64", { exact: true }).first().waitFor();
    // A row of the table on a wide screen, an item of the list on a narrow one.
    const dangling = page
      .getByRole("row")
      .or(page.getByRole("listitem"))
      .filter({ has: page.getByText("Dangling", { exact: true }) });
    const shortId = (await dangling.getByRole("link").innerText()).trim();
    if (!/^[a-f0-9]{12}$/.test(shortId)) throw new Error(`Dangling image name was ${shortId}`);
    await fits(page, ".docker-image-table, .docker-artifact-list");
    await shoot(page, "docker-slice4-images");
    await dangling.getByRole("link").click();
    await page.getByRole("heading", { name: shortId, exact: true }).waitFor();
    await page.getByRole("button", { name: "Copy full image ID" }).waitFor();
    await page.getByRole("link", { name: "All images", exact: true }).click();
    await page.getByRole("link", { name: image }).click();
    await page.getByRole("heading", { name: image, exact: true }).waitFor();
    await page.getByText("Build instructions are redacted").waitFor();
    await page.locator(".docker-layer-list").getByText("52 MB").waitFor();
    if ((await page.content()).includes("fixture-history-secret"))
      throw new Error("raw image history reached the panel");
    await fits(page, ".docker-detail-body");
    await shoot(page, "docker-slice4-image-detail");

    await page.goto(`${origin}${server}/docker/cleanup`);
    await page.getByRole("checkbox", { name: new RegExp(idle) }).waitFor();
    if (!(await page.getByRole("checkbox", { name: new RegExp(idle) }).isChecked()))
      throw new Error("idle cache wasn't selected by default");
    const target = (name: string) => page.getByRole("checkbox", { name: new RegExp(name) });
    const anonShort = anonVolume.slice(0, 12);
    if (
      !(await target(shortId).isChecked()) ||
      !(await target(network).isChecked()) ||
      !(await target(anonShort).isChecked()) ||
      (await target(image).isChecked()) ||
      (await target(namedVolume).isChecked())
    )
      throw new Error(
        "dangling images, networks, anonymous volumes, tagged images or named volumes have the wrong defaults",
      );
    await page.getByRole("checkbox", { name: /Include unused tagged image/ }).check();
    await page.getByRole("checkbox", { name: /Include unused named volume/ }).check();
    if (!(await target(image).isChecked()) || !(await target(namedVolume).isChecked()))
      throw new Error("opt-in checkboxes did not select tagged images and named volumes");
    await page.getByText("Protected artifacts", { exact: true }).click();
    await page.getByText(busy, { exact: true }).waitFor();
    await page.getByText("In use by builds", { exact: true }).waitFor();
    await fits(page, ".docker-workspace");
    await shoot(page, "docker-slice4-cleanup");
    await page.getByRole("button", { name: "Review plan", exact: true }).click();
    const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await review.getByText(idle, { exact: true }).waitFor();
    for (const name of [shortId, image, network, anonShort, namedVolume]) {
      await review.getByText(name, { exact: true }).waitFor();
    }
    if ((await review.innerText()).includes(busy)) throw new Error("active cache was in the cleanup plan");
    await fits(page, ".dw-sheet-view:not(.is-away) .plan-body");
    await shoot(page, "docker-slice4-plan");
    const rejected = page.waitForResponse(
      (response) => response.url().endsWith("/dockwell.v1.ChangeService/ApplyChange") && response.status() === 403,
    );
    await review.getByRole("button", { name: /^Apply plan/ }).click();
    await rejected;
    const finished = onTop(page.getByRole("dialog"));
    await finished.getByText("Freed 158 MB", { exact: true }).waitFor({ timeout: 30_000 });
    if ((await finished.getByText("Removed", { exact: true }).count()) !== 6)
      throw new Error("cleanup did not receipt each reviewed object");
    await fits(page, ".dw-sheet-view:not(.is-away) .plan-body");
    await shoot(page, "docker-slice4-receipt");
    await onTop(page.getByRole("dialog")).getByRole("button", { name: "Done", exact: true }).click();
    await page.getByText("No eligible artifacts", { exact: true }).waitFor();

    step("Docker cleanup: an install admin can clear a pause with a passkey");
    // Seed the marker left by the previous agent version on this test install.
    await writeFile(join(data, "agent", "docker-cleanup", "disabled"), "build cache verification failed");
    await page.reload();
    await page.getByText("The build cache removal could not be verified", { exact: true }).waitFor();
    await fits(page, ".docker-workspace");
    await shoot(page, "docker-slice4-paused");
    await page.getByRole("button", { name: "Re-enable cleanup", exact: true }).click();
    const recovery = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await recovery.getByText("Re-enable Docker cleanup on", { exact: false }).waitFor();
    if (await recovery.getByText("Docker cleanup permanently removes", { exact: false }).count())
      throw new Error("recovery was presented as a removal");
    const needsPasskey = page.waitForResponse(
      (response) => response.url().endsWith("/dockwell.v1.ChangeService/ApplyChange") && response.status() === 403,
    );
    await recovery.getByRole("button", { name: /^Apply plan/ }).click();
    await needsPasskey;
    const resumed = onTop(page.getByRole("dialog", { name: "Docker cleanup re-enabled" }));
    await resumed.getByRole("button", { name: "Done", exact: true }).click();
    await page.getByRole("button", { name: "Re-enable cleanup", exact: true }).waitFor({ state: "hidden" });
    await page.getByText("No eligible artifacts", { exact: true }).waitFor();
  },
};
