// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Page, Route } from "playwright-core";
import { dockerHasImage, dockerRemoveImage } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

function buildSheet(page: Page) {
  return onTop(page.getByRole("dialog", { name: "Build image", exact: true }));
}

async function openBuild(page: Page) {
  await page.getByRole("button", { name: "Image actions", exact: true }).click();
  await page.getByRole("menuitem", { name: "Build image", exact: true }).click();
  const sheet = buildSheet(page);
  await sheet.getByLabel("Local image name").waitFor();
  return sheet;
}

async function chooseFolder(sheet: ReturnType<typeof buildSheet>, dir: string) {
  await sheet.getByRole("button", { name: "Choose a folder", exact: true }).click();
  await sheet.getByLabel("Path", { exact: true }).fill(dir);
  await sheet.getByRole("button", { name: "Done", exact: true }).click();
}

export const flow: Flow = {
  order: 1.65,
  async run(page, { origin, shoot, step, socket }) {
    const dir = mkdtempSync(join(tmpdir(), "dockwell-build-e2e-"));
    const published: string[] = [];
    try {
      writeFileSync(join(dir, "Dockerfile"), "FROM scratch\nRUN echo fixture-docker-build; sleep 40\n");
      writeFileSync(join(dir, "payload"), "fixture context");
      const archive = join(dir, "context.tar");
      execFileSync("tar", ["-cf", archive, "-C", dir, "Dockerfile", "payload"]);
      const server = await ownServerPath(page, origin);
      await page.goto(`${origin}${server}/docker/images`);
      const starting = await openBuild(page);
      await chooseFolder(starting, dir);
      await starting.getByLabel("Local image name").fill("fixture/folder");
      for (const width of [1440, 1024, 390, 320]) {
        await atWidth(page, { width, height: 900 });
        if ((await pastTheEdge(page)).wider > 1) throw new Error(`Build sheet overflows at ${width}px`);
      }
      await atWidth(page, { width: 1440, height: 900 });
      await shoot(page, "docker-slice6c-folder");
      await starting.getByLabel("Source", { exact: true }).selectOption("git");
      await starting.getByLabel("Repository", { exact: true }).fill("https://github.com/example/fixture");
      await starting.getByLabel("Branch, tag or commit").fill("main");
      await shoot(page, "docker-slice6c-git");
      await starting.getByLabel("Source", { exact: true }).selectOption("upload");
      await starting.getByLabel("Build context archive").setInputFiles(archive);
      await starting.getByLabel("Local image name").fill("fixture/upload");
      await shoot(page, "docker-slice6c-upload");
      await starting.getByRole("button", { name: "Review plan" }).click();
      const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      const out = await plan
        .locator("code")
        .filter({ hasText: /^fixture\/upload:dockwell-/ })
        .textContent();
      if (!out) throw new Error("The reviewed image should have a unique local tag");
      published.push(out);
      step("Docker build opens its log on demand and cancels before publication");
      await plan.getByRole("button", { name: /^Apply plan/ }).click();
      const running = onTop(page.getByRole("dialog", { name: "Building image" }));
      await running.getByRole("button", { name: "Build log", exact: true }).click();
      const log = running.getByRole("log", { name: "Build log" });
      await log.getByText("fixture-docker-build", { exact: true }).waitFor();
      await shoot(page, "docker-slice6c-running-log");
      let release = () => {};
      const gate = new Promise<void>((accept) => {
        release = accept;
      });
      const endpoint = "**/dockwell.v1.DockerService/CancelDockerImageBuild";
      let pending: Promise<void> | undefined;
      const waitForShot = (route: Route) => {
        pending = gate.then(() => route.continue());
        return pending;
      };
      await page.route(endpoint, waitForShot);
      try {
        await running.getByRole("button", { name: "Cancel build" }).click();
        await running.getByText("Cancelling", { exact: true }).waitFor();
        await shoot(page, "docker-slice6c-cancelling");
      } finally {
        release();
        await pending;
        await page.unroute(endpoint, waitForShot);
      }
      const ended = onTop(page.getByRole("dialog", { name: "Image build ended" }));
      await ended.getByText("Cancelled", { exact: true }).waitFor();
      if (await dockerHasImage(socket, out)) throw new Error("a cancelled build published its output tag");
      await ended.getByRole("button", { name: "Close" }).click();
      const failed = await openBuild(page);
      await chooseFolder(failed, dir);
      await failed.getByRole("button", { name: "Override Dockerfile" }).click();
      await failed.getByLabel("Dockerfile override").fill("FROM scratch\nRUN echo fixture-failure; exit 7\n");
      await failed.getByLabel("Local image name").fill("fixture/failure");
      await failed.getByRole("button", { name: "Review plan" }).click();
      await plan.getByRole("button", { name: /^Apply plan/ }).click();
      await ended.getByRole("button", { name: "Build log", exact: true }).click();
      const cause = ended.locator(".plan-failure");
      await cause.getByText("Step 2 of 2 failed", { exact: true }).waitFor();
      await cause.filter({ hasText: "RUN echo fixture-failure; exit 7" }).waitFor();
      await ended.getByRole("log", { name: "Build log" }).getByText("fixture-failure", { exact: true }).waitFor();
      await shoot(page, "docker-slice6c-failed");
      await ended.getByRole("button", { name: "Close" }).click();
    } finally {
      for (const image of published) await dockerRemoveImage(socket, image).catch(() => undefined);
      rmSync(dir, { force: true, recursive: true });
    }
  },
};
