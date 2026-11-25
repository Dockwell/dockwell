// SPDX-License-Identifier: AGPL-3.0-only

import { dockerInspect, dockerRemove, dockerRun, dockerStarted } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop } from "./sheets.ts";

export const flow: Flow = {
  order: 71,
  run: async (page, { origin, step, shoot, socket }) => {
    const server = await ownServerPath(page, origin);
    const inventory = `${origin}${server}/docker/containers`;
    const name = "outside-lifecycle-fixture";
    const engineId = await dockerRun(socket, name, {
      image: "acme/legacy:prints",
      network: "host",
      labels: { "com.docker.compose.project": "outside-lifecycle", "com.docker.compose.service": "web" },
    });
    const started = await dockerStarted(socket, engineId);
    const named = page.getByRole("link", { name, exact: true });
    const rowActions = page.getByRole("button", { name: `Actions for ${name}` });
    const review = onTop(page.getByRole("dialog", { name: "Review plan" }));
    const apply = async () => {
      await review.getByRole("button", { name: /^Apply plan/ }).click();
      await page.getByRole("dialog").getByRole("button", { name: /^Done/ }).click({ timeout: 35_000 });
    };

    try {
      step("docker unmanaged: review exact row actions and pause without taking on");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(inventory);
      await page.getByRole("textbox", { name: "Search containers" }).fill(name);
      await named.waitFor();
      await rowActions.click();
      await page.getByRole("menuitem", { name: "Pause", exact: true }).waitFor();
      await shoot(page, "docker-unmanaged-actions");
      if (!(await page.getByRole("menuitem", { name: "Pause", exact: true }).isVisible())) await rowActions.click();
      await page.getByRole("menuitem", { name: "Pause", exact: true }).click();
      await review.getByText(engineId.slice(0, 12), { exact: true }).waitFor();
      await review.getByText("Applying this asks for your passkey", { exact: true }).waitFor();
      await shoot(page, "docker-unmanaged-plan");
      await apply();
      await page.getByRole("row").filter({ has: named }).getByText("Paused", { exact: true }).waitFor();

      step("docker unmanaged: unpause from the batch bar");
      await page.getByRole("checkbox", { name: `Select ${name}` }).check();
      await page.getByRole("button", { name: "Unpause 1", exact: true }).click();
      await apply();
      if ((await dockerStarted(socket, engineId)) !== started)
        throw new Error("pausing replaced or restarted the container");

      step("docker unmanaged: review the same control from Compose and container detail");
      await page.goto(`${origin}${server}/docker/composes/${encodeURIComponent("outside:outside-lifecycle")}`);
      await page.getByRole("button", { name: "Actions for outside-lifecycle", exact: true }).click();
      await page.getByRole("menuitem", { name: "Kill", exact: true }).click();
      const kill = page.getByRole("dialog", { name: "Kill", exact: true });
      await kill.getByRole("textbox", { name: "Seconds before SIGKILL" }).waitFor();
      await shoot(page, "docker-unmanaged-signal");
      await kill.getByRole("button", { name: "Cancel", exact: true }).click();
      await page.goto(`${inventory}/${engineId}`);
      await page.getByRole("button", { name: `Actions for ${name}` }).click();
      await page.getByRole("menuitem", { name: "Rename", exact: true }).click();
      const rename = page.getByRole("dialog", { name: "Rename", exact: true });
      await rename.getByRole("textbox", { name: "New container name" }).fill(`${name}-renamed`);
      await rename.getByRole("button", { name: "Review plan", exact: true }).click();
      await apply();
      if ((await dockerInspect(socket, engineId)).Name !== `/${name}-renamed`)
        throw new Error("Docker name did not change");

      step("docker unmanaged: unsupported run flags block review and an edited command makes a managed plan");
      await page.goto(inventory);
      await page.getByRole("button", { name: "Import", exact: true }).click();
      await page.getByRole("menuitem", { name: "Paste docker run", exact: true }).click();
      const paste = page.getByRole("dialog", { name: "Paste docker run", exact: true });
      const command = paste.getByRole("textbox", { name: "Docker run command" });
      const reviewRun = paste.getByRole("button", { name: "Review plan", exact: true });
      await command.fill("docker run --rm --name pasted-fixture -e 'TITLE=two words' acme/legacy:prints");
      await paste.getByText("--rm", { exact: true }).waitFor();
      if (await reviewRun.isEnabled()) throw new Error("unsupported option was ignored");
      await command.fill("docker run --name pasted-fixture -e 'TITLE=two words' acme/legacy:prints");
      await shoot(page, "docker-unmanaged-import");
      await reviewRun.click();
      await review.getByRole("button", { name: /^Apply plan/ }).waitFor();
      await review.getByRole("button", { name: "Cancel", exact: true }).click();

      step("docker unmanaged: open a local image and search Docker Hub from Images");
      await page.goto(`${origin}${server}/docker/images`);
      const image = page.getByRole("row").filter({ hasText: "acme/legacy" }).first();
      await image.getByRole("button", { name: /Actions for/ }).click();
      await page.getByRole("menuitem", { name: "Run image", exact: true }).click();
      const run = page.getByRole("dialog", { name: "Run image", exact: true });
      if (
        !(await run.getByRole("textbox", { name: "Docker run command" }).inputValue()).startsWith("docker run sha256:")
      ) {
        throw new Error("Run image did not pin the local image ID");
      }
      await shoot(page, "docker-unmanaged-local-image");
      await run.getByRole("button", { name: "Cancel", exact: true }).click();
      await page.getByRole("button", { name: "Search registries", exact: true }).click();
      const search = page.getByRole("dialog", { name: "Search registries", exact: true });
      await search.getByRole("textbox", { name: "Search Docker Hub" }).fill("nginx");
      await search.getByRole("button", { name: "nginx", exact: true }).click();
      await search
        .getByRole("button", { name: "Review pull", exact: true })
        .or(search.getByRole("button", { name: "Check for updates", exact: true }))
        .waitFor();
      await shoot(page, "docker-unmanaged-search");
      await search.getByRole("button", { name: "Cancel", exact: true }).click();
    } finally {
      await dockerRemove(socket, engineId);
    }
  },
};
