// SPDX-License-Identifier: AGPL-3.0-only

import { mkdir } from "node:fs/promises";

import type { Page } from "playwright-core";
import { dockerRun } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { checkLoadingScreen, holdRead, shootLoadingScreen } from "./loading-flow.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

const networkName = "fixture-network-with-many-attached-containers";
const volumeName = "fixture-volume-with-a-long-mountpoint";

async function fits(page: Page) {
  const before = page.viewportSize();
  try {
    for (const colorScheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme });
      for (const width of [1440, 1024, 390, 320]) {
        await atWidth(page, { width, height: 900 });
        const overflow = await page
          .locator(".docker-workspace")
          .evaluate((root) =>
            [
              root,
              ...root.querySelectorAll("td, dd, h2, .docker-card, .docker-storage-pools li"),
              ...document.querySelectorAll(".docker-storage-page h1, .mobile-header:has(+ .docker-storage-page) h1"),
            ]
              .filter((node) => node.clientWidth > 0 && node.scrollWidth > node.clientWidth + 2)
              .map((node) => node.textContent?.slice(0, 80)),
          );
        if (overflow.length)
          throw new Error(`Docker storage overflow at ${width}px in ${colorScheme}: ${overflow.join(", ")}`);
      }
    }
  } finally {
    await page.emulateMedia({ colorScheme: "light" });
    if (before) await atWidth(page, before);
  }
}

async function applyPlan(page: Page) {
  const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await review.getByRole("button", { name: /Apply plan/ }).click({ timeout: 30_000 });
  await page.getByRole("dialog").getByRole("button", { name: "Done", exact: true }).click({ timeout: 40_000 });
}

async function chooseEnvironment(page: Page) {
  const field = page.getByRole("combobox", { name: "Environment", exact: true });
  const first = field.locator('option:not([value=""])').first();
  await first.waitFor({ state: "attached" });
  if (!(await field.inputValue())) await field.selectOption((await first.getAttribute("value")) ?? "");
}

async function storageLoading(
  page: Page,
  origin: string,
  server: string,
  paths: { network: string; volume: string },
  step: (what: string) => void,
) {
  const slow = "Docker is slow to answer on this server";
  const list = (kind: "networks" | "volumes", singular: string, name: string) => ({
    name: `docker-slice5-${kind}-loading`,
    rpc: "DockerService.ListDockerObjects",
    path: `${server}/docker/${kind}`,
    scope: ":is(.docker-storage-list-table.is-loading, .docker-workspace .dw-skeleton-rows)",
    atOnce: async (p: Page) => {
      await p.getByRole("heading", { name: new RegExp(`^${kind}$`, "i") }).waitFor();
      await p.getByRole("textbox", { name: `Find a ${singular}` }).waitFor();
      await p.locator(".docker-storage-list-table th", { hasText: "Driver" }).waitFor();
    },
    landed: (p: Page) => p.getByRole("link", { name, exact: true }).waitFor(),
    slow,
    loadedShot: true,
  });
  const detail = (kind: "network" | "volume", name: string) => ({
    name: `docker-slice5-${kind}-detail-loading`,
    rpc: "DockerService.GetDockerObject",
    path: new URL(paths[kind]).pathname,
    scope: ".docker-detail-loading",
    atOnce: async (p: Page) => {
      await p.locator("h1 .dw-skeleton").waitFor({ state: "visible" });
    },
    landed: (p: Page) => p.getByRole("heading", { name, exact: true }).waitFor(),
    slow,
    loadedShot: true,
  });
  const screens = [
    list("networks", "network", networkName),
    list("volumes", "volume", volumeName),
    detail("network", networkName),
    detail("volume", volumeName),
  ];
  for (const screen of screens) await checkLoadingScreen(page, origin, screen, step);
  const dir = process.env["DOCKWELL_E2E_LOADING_SHOTS"];
  if (dir) {
    await mkdir(dir, { recursive: true });
    for (const screen of screens) await shootLoadingScreen(page, origin, screen, dir);
  }
}

export const flow: Flow = {
  order: 1200,
  async run(page, { origin, step, shoot, socket }) {
    const server = await ownServerPath(page, origin);
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}${server}/docker/networks`);
    step("Docker storage: follow a network's stopped endpoints and a volume's mounts");
    const networkRow = page.getByRole("row").filter({
      has: page.getByRole("link", { name: networkName, exact: true }),
    });
    await networkRow.getByText("+7 more", { exact: true }).waitFor();
    if (await networkRow.getByText("10.104.27.0/24", { exact: true }).count()) {
      throw new Error("the inventory row expanded its full address-pool list");
    }
    await fits(page);
    await shoot(page, "docker-slice5-networks");
    await page.getByRole("link", { name: networkName, exact: true }).click();
    await page.getByRole("heading", { name: networkName, exact: true }).waitFor();
    await page.getByRole("link", { name: "fixture-storage-workload-12", exact: true }).waitFor();
    await page.getByText("10.104.27.0/24", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Network actions" }).click();
    const remove = page.getByRole("menuitem", { name: /Review cleanup/ });
    if ((await remove.getAttribute("aria-disabled")) !== "true") {
      throw new Error("a network with attached containers could be sent to cleanup");
    }
    await remove.getByText("Containers still use this network").waitFor();
    await page.getByRole("button", { name: "Network actions" }).click();
    await fits(page);
    await shoot(page, "docker-slice5-network-detail");
    const fixtureNetworkPage = page.url();
    await page.getByRole("link", { name: "fixture-storage-workload-02", exact: true }).click();
    await page.getByRole("link", { name: networkName, exact: true }).waitFor();
    await page.getByRole("link", { name: volumeName, exact: true }).click();
    await page.getByRole("heading", { name: volumeName, exact: true }).waitFor();
    await page.getByText("Read only", { exact: true }).waitFor();
    await page.getByText(/extraordinarily-long-folder-name-for-a-docker-volume-mountpoint/).waitFor();
    await fits(page);
    await shoot(page, "docker-slice5-volume-detail");
    const fixtureVolumePage = page.url();
    await page.getByRole("link", { name: "Open files", exact: true }).click();
    await page.waitForURL(
      (url) =>
        url.pathname.endsWith("/files") && url.searchParams.get("path")?.endsWith("/nested-volume-data") === true,
    );
    await page.goBack();
    await page.getByRole("link", { name: "Volumes", exact: true }).click();
    await page.getByRole("link", { name: volumeName, exact: true }).waitFor();
    await fits(page);
    await shoot(page, "docker-slice5-volumes");

    step("Docker storage: create a local volume and a checked bridge through reviewed plans");
    await page.getByRole("button", { name: "New volume", exact: true }).first().click();
    await page.getByRole("textbox", { name: "Volume name" }).fill("reviewed-docker-data");
    await chooseEnvironment(page);
    await page.getByRole("button", { name: "Review plan", exact: true }).click();
    await applyPlan(page);
    await page.getByRole("heading", { name: /_reviewed-docker-data$/ }).waitFor({ timeout: 30_000 });
    const preview = page.waitForResponse(
      (response) => response.url().includes("/dockwell.v1.BackupService/PreviewBackup") && response.ok(),
    );
    await page.getByRole("button", { name: "Back up", exact: true }).click();
    await preview;
    await page.goto(`${origin}${server}/docker/networks`);
    await page.getByRole("button", { name: "New network", exact: true }).first().click();
    const releaseCheck = await holdRead(page, "DockerService.CheckDockerNetwork");
    try {
      await page.getByRole("textbox", { name: "Network name" }).fill("reviewed-docker-network");
      await chooseEnvironment(page);
      const sheet = onTop(page.getByRole("dialog", { name: "New network" }));
      await sheet.locator(".dw-skeleton-field .dw-skeleton").first().waitFor({ state: "visible" });
    } finally {
      await releaseCheck();
    }
    await page.getByText("Bridge · Advanced", { exact: true }).click();
    const subnet = page.getByRole("textbox", { name: "IPv4 subnet" });
    await subnet.fill("10.104.20.0/24");
    await page.getByText(`Overlaps 10.104.20.0/24 on ${networkName}`).waitFor();
    await subnet.fill("");
    await page.getByText("Checked on the selected server").waitFor();
    await page.getByRole("button", { name: "Review plan", exact: true }).click();
    await applyPlan(page);
    const heading = page.getByRole("heading", { name: /_reviewed-docker-network$/ });
    await heading.waitFor({ timeout: 30_000 });
    const newNetworkName = (await heading.innerText()).trim();
    const newNetworkPage = page.url();

    step("Docker networks: protect the last network, then connect and disconnect through plans");
    const workload = "fixture-reviewed-attachment";
    await dockerRun(socket, workload, { image: "acme/storage:1", network: networkName });
    await page.goto(fixtureNetworkPage);
    const row = page.getByRole("row").filter({
      has: page.getByRole("link", { name: /fixture-reviewed-attachment(?:-\d+)?$/ }),
    });
    const menu = row.getByRole("button", { name: /Actions for/ });
    await menu.click();
    await row.getByRole("menuitem", { name: "Take on", exact: true }).click();
    await onTop(page.getByRole("dialog", { name: `Take on ${workload}` }))
      .getByRole("button", { name: "Review plan", exact: true })
      .click();
    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    await plan.getByRole("button", { name: /Apply plan/ }).click({ timeout: 40_000 });
    await page
      .getByRole("dialog")
      .getByRole("button", { name: /^Open fixture-reviewed-attachment|^Done$/ })
      .first()
      .click({ timeout: 40_000 });
    await menu.click();
    const disconnect = row.getByRole("menuitem", { name: /Disconnect/ });
    await row.getByText("This is the workload's last network").waitFor();
    if ((await disconnect.getAttribute("aria-disabled")) !== "true") {
      throw new Error("the workload's last network could be disconnected");
    }
    const releaseChoices = await holdRead(page, "DockerService.ListDockerObjects");
    const picker = page.getByRole("dialog", { name: "Connect another network" });
    try {
      await row.getByRole("menuitem", { name: "Connect another network", exact: true }).click();
      await picker.getByText(workload, { exact: true }).waitFor();
      await picker.locator(".dw-skeleton-field .dw-skeleton").first().waitFor({ state: "visible" });
    } finally {
      await releaseChoices();
    }
    await picker.getByRole("combobox", { name: "Network" }).selectOption({ label: newNetworkName });
    await picker.getByRole("button", { name: "Review plan", exact: true }).click();
    await applyPlan(page);
    await page.goto(newNetworkPage);
    await menu.click();
    await row.getByRole("menuitem", { name: "Disconnect", exact: true }).click();
    await applyPlan(page);
    await page.getByText("No containers attached", { exact: true }).waitFor();
    await storageLoading(page, origin, server, { network: fixtureNetworkPage, volume: fixtureVolumePage }, step);
  },
};
