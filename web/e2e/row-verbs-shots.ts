// SPDX-License-Identifier: AGPL-3.0-only

import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import type { Page } from "playwright-core";

import { dockerCreateStorage, dockerPull, dockerRun } from "./docker-probe.ts";
import type { FlowRun } from "./flows.ts";
import { atWidth } from "./sheets.ts";

interface Planned {
  id: string;
  state?: string;
  error?: string;
  plan: { fingerprint: string };
}

export async function proposeFixture(page: Page, summary: string, edits: object[]) {
  await page.evaluate(
    async ({ summary, edits }) => {
      async function change(method: string, body: object) {
        const response = await fetch(`/dockwell.v1.ChangeService/${method}`, {
          method: "POST",
          headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
          body: JSON.stringify(body),
        });
        if (!response.ok) throw new Error(`${summary}: ${method} ${response.status} ${await response.text()}`);
        return ((await response.json()) as { change: Planned }).change;
      }
      const plan = await change("ProposeChange", { summary, edits });
      await change("ApplyChange", { id: plan.id, fingerprint: plan.plan.fingerprint });
      const end = Date.now() + 45_000;
      while (Date.now() < end) {
        const status = await change("GetChange", { id: plan.id });
        if (status.state === "CHANGE_STATE_APPLIED") return;
        if (status.state === "CHANGE_STATE_FAILED") throw new Error(status.error ?? summary);
        await new Promise((resolve) => setTimeout(resolve, 250));
      }
      throw new Error(`${summary} did not apply`);
    },
    { summary, edits },
  );
}

/** Put screenshots for both layouts on a disposable test stand with realistic inventory. */
export async function captureRowVerbs(
  page: Page,
  { origin, socket, step }: FlowRun,
  { directory, scope, server, serverPath }: { directory: string; scope: string; server: string; serverPath: string },
) {
  const series = (n: number) => Array.from({ length: n }, (_, i) => String(i + 1).padStart(2, "0"));
  step(
    "row verbs: seed 30 apps, 40 containers, 20 images, 10 volumes, 10 networks and 10 scheduled tasks for screenshots",
  );
  await proposeFixture(
    page,
    "Screenshot apps",
    series(30).map((i) => ({
      resource: { kind: "service", scope, name: `sample-web-${i}` },
      spec: { service: { app: { image: "nginx:1.27", desiredRunState: "DESIRED_RUN_STATE_STOPPED" } } },
      servers: [server],
    })),
  );
  await proposeFixture(
    page,
    "Screenshot tasks",
    series(10).map((i) => ({
      resource: { kind: "task", scope, name: `sample-task-${i}` },
      spec: {
        task: {
          displayName: `Sample task ${i}`,
          schedule: "0 4 1 1 *",
          timeZone: "UTC",
          paused: Number(i) > 5,
          request: { url: "https://example.com/", method: "GET" },
        },
      },
      servers: [server],
    })),
  );
  for (const i of series(40))
    await dockerRun(socket, `sample-container-${i}`, { image: "nginx:1.27", port: 8000 + Number(i) });
  for (const i of series(20)) await dockerPull(socket, `sample-image-${i}:1`);
  for (const i of series(10)) {
    await dockerCreateStorage(socket, "volumes", `sample-volume-${i}`);
    await dockerCreateStorage(socket, "networks", `sample-network-${i}`);
  }
  await mkdir(directory, { recursive: true });
  for (const width of [1440, 390]) {
    await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
    for (const [name, path, target] of [
      ["apps", "/apps?q=sample-web", "sample-web-01"],
      ["containers", `${serverPath}/docker/containers`, "sample-container-01"],
      ["images", `${serverPath}/docker/images`, "sample-image-01:1"],
      ["volumes", `${serverPath}/docker/volumes`, "sample-volume-01"],
      ["networks", `${serverPath}/docker/networks`, "sample-network-01"],
      ["tasks", "/scheduled-tasks?q=sample", "Sample task 01"],
    ] as const) {
      step(`row verbs: capture ${name} at ${width}px`);
      await page.goto(`${origin}${path}`);
      if (name === "containers" || name === "images" || name === "volumes" || name === "networks") {
        const search = `sample-${name.slice(0, -1)}`;
        const filtered = page.waitForResponse((response) => {
          const url = new URL(response.url());
          if (!url.pathname.endsWith("/dockwell.v1.DockerService/ListDockerObjects") || !response.ok()) return false;
          const message = url.searchParams.get("message");
          const body = message
            ? Buffer.from(message, url.searchParams.get("base64") === "1" ? "base64url" : "utf8")
            : response.request().postDataBuffer();
          return body?.includes(search) ?? false;
        });
        await page.getByRole("textbox", { name: /^(Find|Search)/ }).fill(search);
        await filtered;
      }
      // Docker's inventory is cursor-paged by Engine identity, independent of the sample names.
      const picks = page.getByRole("checkbox", { name: new RegExp(`^Select ${target.replace("01", "[0-9]{2}")}$`) });
      const second = picks.nth(1);
      await second.waitFor({ timeout: 30_000 });
      const first = picks.first();
      const label = await first.getAttribute("aria-label");
      if (!label) throw new Error(`No sample row label for ${name}`);
      const menu = `Actions for ${label.slice("Select ".length)}`;
      await first.click();
      await second.click();
      await page.getByRole("toolbar", { name: "Actions for the rows selected" }).getByText("2 selected").waitFor();
      await page.evaluate(async () => {
        await document.fonts.ready;
        window.scrollTo(0, 0);
      });
      await page.screenshot({ path: join(directory, `${name}-${width}.png`) });
      const button = page.getByRole("button", { name: menu, exact: true });
      const popup = page.getByRole("menu", { name: menu, exact: true });
      await button.click();
      try {
        await popup.waitFor({ timeout: 1500 });
      } catch {
        // An inventory event can move the row during a pointer click. Reopen on its settled position.
        if ((await button.getAttribute("aria-expanded")) === "true") await button.click();
        await button.click();
        await popup.waitFor();
      }
      await page.screenshot({ path: join(directory, `${name}-${width}-menu.png`) });
    }
  }
  await atWidth(page, { width: 1440, height: 900 });
}
