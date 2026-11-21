// SPDX-License-Identifier: AGPL-3.0-only

import type { Page } from "playwright-core";

import { dockerInspect, dockerRemove, dockerRestartLoop, dockerRun, dockerStarted } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

async function layouts(page: Page, label: string) {
  for (const colorScheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme });
    for (const width of [1440, 1024, 390, 320]) {
      await atWidth(page, { width, height: 900 });
      await page.waitForFunction(() =>
        [...document.querySelectorAll(".dw-sheet")].every((sheet) =>
          sheet.getAnimations().every((animation) => animation.playState === "finished"),
        ),
      );
      const fits = await page.evaluate(() => {
        const viewport = document.documentElement.clientWidth;
        const controls = [
          ...document.querySelectorAll(
            ".docker-heading-actions > *, .docker-card-foot button, .docker-take-on input, .docker-take-on select",
          ),
        ]
          .map((element) => element.getBoundingClientRect())
          .filter((box) => box.width > 0);
        return (
          document.documentElement.scrollWidth <= viewport + 1 &&
          controls.every((box) => box.left >= -1 && box.right <= viewport + 1)
        );
      });
      if (!fits) {
        const { what } = await pastTheEdge(page);
        throw new Error(`${label} overflows the ${width}px ${colorScheme} layout${what ? ` (${what})` : ""}`);
      }
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await atWidth(page, { width: 1440, height: 900 });
}

export const flow: Flow = {
  order: 72,
  run: async (page, { origin, step, shoot, socket }) => {
    const name = "workload-fixture";
    const secret = "unread-workload-fixture";
    const volume = "workload-fixture-data";
    const server = await ownServerPath(page, origin);
    const inventory = `${origin}${server}/docker/containers`;
    const id = await dockerRun(socket, name, {
      image: "acme/legacy:prints",
      env: [`WORKLOAD_KEY=${secret}`],
      mount: `${volume}:/data`,
    });
    const initial = await dockerStarted(socket, id);
    try {
      step("docker workload: New container opens Advanced on the selected server");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(inventory);
      await page.getByRole("button", { name: "Import", exact: true }).waitFor();
      await layouts(page, "Docker workload header");
      await shoot(page, "docker-workload-inventory");
      await page.getByRole("link", { name: "New container" }).click();
      const create = page.getByRole("dialog", { name: "New app" });
      const restart = create.getByRole("combobox", { name: "Restarts", exact: true });
      await restart.selectOption({ label: "When it exits with an error" });
      await create.getByRole("textbox", { name: "Restart retries" }).fill("4");
      if (!page.url().includes(server.split("/").at(-1) ?? "")) throw new Error("New app lost its selected server");
      await layouts(page, "New container Advanced");
      await shoot(page, "docker-workload-new");
      await create.getByRole("button", { name: "Cancel", exact: true }).click();
      await create.getByRole("button", { name: "Discard", exact: true }).click();
      await page.getByRole("button", { name: "Import", exact: true }).click();
      await page.getByRole("menuitem", { name: "Compose file" }).waitFor();
      await page.getByRole("menuitem", { name: "Move in" }).waitFor();
      await shoot(page, "docker-workload-import");
      await page.getByRole("menuitem", { name: "Take on a container" }).click();

      step("docker workload: take on the exact container with its settings intact");
      const row = page.getByRole("row").filter({ has: page.getByRole("link", { name, exact: true }) });
      await row.getByRole("button", { name: "Take on", exact: true }).click();
      const adopt = page.getByRole("dialog", { name: `Take on ${name}` });
      await adopt.getByText("Keeps the current container", { exact: false }).waitFor();
      await layouts(page, "Take on");
      await shoot(page, "docker-workload-take-on");
      await adopt.getByRole("button", { name: "Review plan", exact: true }).click();
      const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
      await plan.getByRole("button", { name: /Apply plan/ }).waitFor();
      if ((await plan.innerText()).includes(secret)) throw new Error("a secret reached the plan");
      await plan.getByRole("button", { name: /Apply plan/ }).click();
      await page
        .getByRole("dialog")
        .getByRole("button", { name: /^Open workload-fixture|^Done/ })
        .first()
        .click({ timeout: 40_000 });
      await page.getByRole("heading", { name, exact: true, level: 1 }).waitFor();
      if ((await dockerStarted(socket, id)) !== initial) throw new Error("Take on restarted the container");

      const review = async () => {
        await plan.getByRole("button", { name: /Apply plan/ }).click();
        await page.getByRole("dialog").getByRole("button", { name: /^Done/ }).click({ timeout: 40_000 });
      };
      const filterToWorkload = async () => {
        const filtered = page.waitForResponse((response) => {
          const url = new URL(response.url());
          if (!url.pathname.endsWith("/dockwell.v1.DockerService/ListDockerObjects")) return false;
          const query = url.searchParams.get("message");
          const body = query
            ? Buffer.from(query, url.searchParams.get("base64") === "1" ? "base64url" : "utf8")
            : response.request().postDataBuffer();
          return body?.includes(name) ?? false;
        });
        await page.getByRole("textbox", { name: "Search containers" }).fill(name);
        await filtered;
        await row.getByRole("link", { name: "Managed by Dockwell" }).waitFor({ timeout: 30_000 });
      };
      const actions = page.getByRole("button", { name: `Actions for ${name}` });

      step("docker workload: stop through a revision and start from the app");
      await page.goto(inventory);
      await filterToWorkload();
      await actions.click();
      await shoot(page, "docker-workload-managed");
      await page.getByRole("menuitem", { name: "Stop workload" }).click();
      await plan
        .getByText(/health checks/i)
        .first()
        .waitFor();
      await shoot(page, "docker-workload-stop-plan");
      await review();
      await row.getByText("Stopped", { exact: true }).waitFor({ timeout: 30_000 });
      await row.getByRole("link", { name: "Managed by Dockwell" }).click();
      const start = page.getByRole("button", { name: "Start", exact: true }).last();
      await start.waitFor();
      await page.getByRole("region", { name: "What runs" }).getByText("Stopped", { exact: true }).waitFor();
      await page.getByRole("region", { name: "Recent deployments" }).getByText("Stopped", { exact: true }).waitFor();
      await layouts(page, "Stopped app");
      await shoot(page, "docker-workload-stopped");
      await start.click();
      await review();
      const started = await dockerInspect(socket, id);
      if (
        !started.State.Running ||
        !started.Config.Env?.includes(`WORKLOAD_KEY=${secret}`) ||
        (await dockerStarted(socket, id)) === initial
      ) {
        throw new Error("Start failed to preserve the adopted container and its environment");
      }

      step("docker workload: restart just the reviewed container");
      await dockerRestartLoop(socket, id);
      const beforeRestart = await dockerStarted(socket, id);
      await page.goto(inventory);
      await filterToWorkload();
      await row.getByText("Restarting", { exact: true }).waitFor();
      await actions.click();
      await shoot(page, "docker-workload-restarting");
      await page.getByRole("menuitem", { name: "Restart this container" }).click();
      await plan.getByText(id.slice(0, 12), { exact: false }).first().waitFor();
      await review();
      if ((await dockerStarted(socket, id)) === beforeRestart) throw new Error("Docker didn't restart the reviewed ID");
    } finally {
      await dockerRemove(socket, id, volume);
    }
  },
};
