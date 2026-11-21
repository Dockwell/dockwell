// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync, truncateSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Route } from "playwright-core";
import { dockerPull, dockerRemoveImage } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

const original = "fixture/archive-image-with-a-long-repository-name:original";
const reviewed = "fixture/archive-image-with-a-long-repository-name:original-imported";

export const flow: Flow = {
  order: 1.6,
  async run(page, { origin, shoot, step, socket }) {
    const scratch = mkdtempSync(join(tmpdir(), "dockwell-archive-e2e-"));
    try {
      await dockerPull(socket, original);
      const config = JSON.stringify({ architecture: "amd64", os: "linux", rootfs: { type: "layers", diff_ids: [] } });
      const cfg = `${createHash("sha256").update(config).digest("hex")}.json`;
      writeFileSync(join(scratch, cfg), config);
      writeFileSync(
        join(scratch, "manifest.json"),
        JSON.stringify([{ Config: cfg, RepoTags: [original], Layers: [] }]),
      );
      const file = join(scratch, "image.tar");
      const extra = join(scratch, "archive-metadata");
      writeFileSync(extra, "");
      truncateSync(extra, 72 << 20);
      execFileSync("tar", ["-cf", file, "-C", scratch, cfg, "manifest.json", "archive-metadata"]);
      const server = await ownServerPath(page, origin);
      await page.goto(`${origin}${server}/docker/images`);
      await page.getByRole("button", { name: "Image actions", exact: true }).click();
      await page.getByRole("menuitem", { name: "Import image" }).click();
      const sheet = onTop(page.getByRole("dialog", { name: "Import image" }));
      await sheet.getByRole("button", { name: "Choose file" }).waitFor();
      await shoot(page, "docker-slice6b-import");
      await sheet.getByLabel("Docker image archive").setInputFiles(file);
      const bytes = /\/v1\/docker\/artifacts\/[^/]+\/[^/]+\/bytes\?offset=/;
      let release = () => {};
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      const held = async (route: Route) => {
        if (new URL(route.request().url()).searchParams.get("offset") !== "0") await gate;
        await route.continue().catch(() => undefined);
      };
      await page.route(bytes, held);
      step("Docker archives: pause and resume the upload before choosing a new alias");
      try {
        await sheet.getByRole("button", { name: "Upload archive" }).click();
        await sheet.getByText("88%", { exact: true }).waitFor({ timeout: 60_000 });
        await shoot(page, "docker-slice6b-progress");
        await sheet.getByRole("button", { name: "Pause" }).click();
        await sheet.getByRole("button", { name: "Resume" }).waitFor();
      } finally {
        release();
        await page.unroute(bytes, held);
      }
      await sheet.getByRole("button", { name: "Close" }).click();
      await page.reload();
      const leases = page.getByRole("region", { name: "Image transfers" });
      await leases.getByText("image.tar", { exact: true }).waitFor();
      await shoot(page, "docker-slice6b-leases");
      await leases.getByRole("button", { name: "Resume" }).click();
      await sheet.getByLabel("Docker image archive").setInputFiles(file);
      await sheet.getByRole("button", { name: "Resume" }).click();
      await sheet.getByText("Choose another tag", { exact: true }).waitFor();
      await shoot(page, "docker-slice6b-conflict");
      await sheet.getByRole("button", { name: `Use ${reviewed}` }).click();
      for (const width of [1440, 1024, 390, 320]) {
        await atWidth(page, { width, height: 900 });
        if ((await pastTheEdge(page)).wider > 1) throw new Error(`Import sheet overflows at ${width}px`);
      }
      await atWidth(page, { width: 1440, height: 900 });
      await sheet.getByRole("button", { name: "Review plan" }).click();
      const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await plan.getByText(reviewed, { exact: true }).waitFor();
      await shoot(page, "docker-slice6b-import-plan");
      await plan.getByRole("button", { name: /^Apply plan/ }).click();
      const imported = onTop(page.getByRole("dialog", { name: "Images imported" }));
      await imported.getByRole("button", { name: "Done" }).click();

      step("Docker archives: review an exact export and download its sealed bytes");
      await page.getByRole("link", { name: reviewed, exact: true }).click();
      await page.getByRole("button", { name: `Actions for ${reviewed}` }).click();
      await page.getByRole("menuitem", { name: "Export image" }).click();
      const ex = onTop(page.getByRole("dialog", { name: "Export image" }));
      await shoot(page, "docker-slice6b-export");
      await ex.getByRole("button", { name: "Review plan" }).click();
      await plan.getByRole("button", { name: /^Apply plan/ }).click();
      const ready = onTop(page.getByRole("dialog", { name: "Image archive ready" }));
      const download = ready.getByRole("link", { name: "Download", exact: true });
      await download.waitFor();
      await shoot(page, "docker-slice6b-download");
      const downloading = page.waitForEvent("download");
      await download.click();
      const saved = await downloading;
      if (await saved.failure()) throw new Error("The sealed archive download failed");
      await ready.getByRole("button", { name: "Done" }).click();
    } finally {
      await dockerRemoveImage(socket, reviewed).catch(() => undefined);
      await dockerRemoveImage(socket, original).catch(() => undefined);
      rmSync(scratch, { recursive: true, force: true });
    }
  },
};
