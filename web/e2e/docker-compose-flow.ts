// SPDX-License-Identifier: AGPL-3.0-only

import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Page } from "playwright-core";

import { dockerHealth, dockerRun, dockerStarted } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth, onTop, pastTheEdge } from "./sheets.ts";

async function assertStackFits(page: Page, view: string) {
  for (const colorScheme of ["dark", "light"] as const) {
    await page.emulateMedia({ colorScheme });
    for (const width of [1440, 1024, 390, 320]) {
      await atWidth(page, { width, height: 900 });
      await page.evaluate(async () => {
        await document.fonts.ready;
        await new Promise<void>((resolved) => requestAnimationFrame(() => requestAnimationFrame(() => resolved())));
      });
      const { wider, what } = await pastTheEdge(page);
      if (wider > 1) throw new Error(`${view} is wider than ${width}px in ${colorScheme}: ${what}`);
      const overflowingCell = await page.evaluate(
        () =>
          [...document.querySelectorAll(".compose-table th, .compose-table td")].find(
            (cell) => cell.scrollWidth > cell.clientWidth + 1,
          )?.textContent,
      );
      if (overflowingCell) throw new Error(`${view}, ${width}px ${colorScheme}: ${overflowingCell} escapes its column`);
    }
  }
  await atWidth(page, { width: 1440, height: 900 });
}

async function importFixture(page: Page, serverId: string) {
  const fixture = await page.evaluate(async (server) => {
    const response = await fetch("/v1/compose:import", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        compose: [
          "name: workspace-fixture",
          "services:",
          "  web:",
          "    image: nginx:1.27-alpine",
          "    environment:",
          `      PRIVATE_KEY: \${PRIVATE_KEY}`,
          "    ports:",
          '      - "127.0.0.1:18465:80"',
          "  db:",
          "    image: postgres:17",
          "    environment:",
          `      POSTGRES_PASSWORD: \${PRIVATE_KEY}`,
          "    volumes:",
          "      - database-data:/var/lib/postgresql/data",
          ...["edge", "jobs", "metrics", "search"].flatMap((service) => [
            `  ${service}:`,
            `    image: registry.example.test/example/teams/operations/release-images/workspace-fixture/${service}@sha256:${"a".repeat(64)}`,
          ]),
          "volumes:",
          "  database-data:",
        ].join("\n"),
        variables: { PRIVATE_KEY: "compose-demo-sealed" },
        servers: [server],
      }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(`Compose fixture could not plan: ${result.message}`);
    return { id: result.change.id, fingerprint: result.change.plan.fingerprint };
  }, serverId);
  await applyFixture(page, fixture);
}

async function applyFixture(page: Page, fixture: { id: string; fingerprint: string }) {
  await page.evaluate(async ({ id, fingerprint }) => {
    const response = await fetch(`/v1/changes/${id}:apply`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ fingerprint }),
    });
    if (!response.ok) throw new Error("Compose fixture could not apply");
  }, fixture);
  const until = Date.now() + 45_000;
  while (Date.now() < until) {
    const change = await page.evaluate(async (id) => {
      const response = await fetch(`/v1/changes/${id}`);
      return (await response.json()).change as { state?: string; error?: string } | undefined;
    }, fixture.id);
    if (change?.state === "CHANGE_STATE_APPLIED") return;
    if (change?.state === "CHANGE_STATE_FAILED") throw new Error(change.error);
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error("the Compose fixture did not finish applying");
}

export const flow: Flow = {
  order: 73,
  run: async (page, { origin, step, shoot, socket }) => {
    const server = await ownServerPath(page, origin);
    const list = `${origin}${server}/docker/composes`;
    await atWidth(page, { width: 1440, height: 900 });
    await importFixture(page, server.split("/").at(-1) ?? "");
    const ids = await Promise.all(
      ["web", "worker"].map((service) =>
        dockerRun(socket, `outside-fixture-${service}-1`, {
          image: "nginx:1.27-alpine",
          labels: {
            "com.docker.compose.project": "outside-fixture",
            "com.docker.compose.service": service,
            "com.docker.compose.project.working_dir": "/srv/compose-fixture",
          },
        }),
      ),
    );
    const before = await Promise.all(ids.map((id) => dockerStarted(socket, id)));

    step("docker compose: New Compose and the managed stack open from the server");
    await page.goto(list);
    await page.getByRole("button", { name: "New Compose", exact: true }).click();
    const newForm = page.getByRole("dialog", { name: "New app" });
    await newForm.getByRole("textbox", { name: "Compose file" }).waitFor();
    await newForm.getByRole("button", { name: "Cancel", exact: true }).click();
    await page.getByRole("link", { name: "workspace-fixture", exact: true }).waitFor();
    await page.getByRole("link", { name: "outside-fixture", exact: true }).waitFor();
    await page.getByRole("columnheader", { name: "Last change" }).waitFor();
    await assertStackFits(page, "Compose list");
    await shoot(page, "docker-compose-list");
    const folder = await mkdtemp(join(tmpdir(), "dockwell-compose-source-"));
    try {
      await writeFile(join(folder, "compose.yaml"), "services:\n  web:\n    image: nginx:1.27-alpine\n");
      await page.getByRole("button", { name: "Import", exact: true }).click();
      await page.getByRole("menuitem", { name: "Server folder" }).click();
      const source = page.getByRole("dialog", { name: "Import from a server folder" });
      await source.getByRole("textbox", { name: "Folder path" }).fill(folder);
      await source.getByRole("button", { name: "Open folder" }).click();
      await source.getByRole("button", { name: "compose.yaml" }).waitFor();
      await assertStackFits(page, "Compose source picker");
      await shoot(page, "docker-compose-source");
      await source.getByRole("button", { name: "Cancel", exact: true }).click();
    } finally {
      await rm(folder, { recursive: true, force: true });
    }
    const rowActions = page.getByRole("button", { name: "Actions for workspace-fixture" });
    await rowActions.scrollIntoViewIfNeeded();
    const trigger = await rowActions.boundingBox();
    if (!trigger) throw new Error("The stack's row actions are missing");
    // Leave less room below the button than the menu needs, even in the small fixture.
    const height = Math.min(900, Math.ceil(trigger.y + trigger.height + 48));
    await atWidth(page, { width: 1440, height });
    await rowActions.click();
    const menu = page.getByRole("menu", { name: "Actions for workspace-fixture" });
    const bounds = await menu.boundingBox();
    const anchor = await rowActions.boundingBox();
    if (!bounds || !anchor || bounds.y < 0 || bounds.y + bounds.height > anchor.y) {
      throw new Error("The stack's action menu did not fit above its button");
    }
    await menu.getByRole("menuitem", { name: "Open", exact: true }).click();
    await atWidth(page, { width: 1440, height: 900 });
    const webRow = page.getByRole("row").filter({ has: page.getByRole("link", { name: "web", exact: true }) });
    await webRow.locator(".dw-status.dw-tone-running").waitFor();
    if (
      !(await webRow.getByRole("link", { name: /web-1$/ }).getAttribute("href"))?.match(
        /\/containers\/[a-f0-9]{64}(?:\?|$)/,
      )
    ) {
      throw new Error("The service's container link doesn't pin an Engine ID");
    }
    await webRow.getByText(/18465.*80/).waitFor();
    if ((await page.getByRole("table", { name: "Services" }).getByRole("row").count()) !== 7) {
      throw new Error("Compose's six services did not fit in the table");
    }
    const stackURL = page.url();
    step("docker compose: engine events update the open page and backup previews wait for their tab");
    const previewRequest = (request: { url(): string }) =>
      new URL(request.url()).pathname.endsWith("/dockwell.v1.BackupService/PreviewBackup");
    const previews: string[] = [];
    const trackPreviews = (request: { url(): string }) => {
      if (previewRequest(request)) previews.push(request.url());
    };
    page.on("request", trackPreviews);
    const webID = (await webRow.getByRole("link", { name: /web-1$/ }).getAttribute("href"))?.match(/[a-f0-9]{64}/)?.[0];
    if (!webID) throw new Error("The web service has no Engine ID");
    const readStack = (request: { url(): string }) =>
      new URL(request.url()).pathname.endsWith("/dockwell.v1.DockerService/GetDockerObject");
    const healthRead = page.waitForRequest(readStack, { timeout: 15_000 });
    await dockerHealth(socket, webID, "unhealthy", "fixture diagnostic");
    await healthRead;
    await webRow.getByText("Unhealthy", { exact: true }).waitFor({ timeout: 5_000 });
    await dockerHealth(socket, webID, "healthy", "fixture recovered");
    await webRow.getByText("Healthy", { exact: true }).waitFor({ timeout: 15_000 });
    const idleRead = await page.waitForRequest(readStack, { timeout: 11_000 }).then(
      () => true,
      (error: unknown) => {
        if (error instanceof Error && error.name === "TimeoutError") return false;
        throw error;
      },
    );
    if (idleRead) throw new Error("The stack re-read its Docker membership with no Engine changes");
    await assertStackFits(page, "Compose services");
    await shoot(page, "docker-compose-services");
    await page.getByRole("link", { name: "Environment", exact: true }).click();
    await page.getByText(/environment\.PRIVATE_KEY/).waitFor();
    await assertStackFits(page, "Compose environment");
    await shoot(page, "docker-compose-environment");
    await page.getByRole("link", { name: "Logs", exact: true }).click();
    await page.getByRole("combobox", { name: "Service logs" }).waitFor();
    await assertStackFits(page, "Compose logs");
    await shoot(page, "docker-compose-logs");

    step("docker compose: review stack and service controls");
    const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
    for (const action of ["Stop stack", "Start stack", "Restart stack"]) {
      await page.getByRole("button", { name: "Actions for workspace-fixture" }).click();
      await page.getByRole("menuitem", { name: action, exact: true }).click();
      const apply = plan.getByRole("button", { name: /Apply plan/ });
      await apply.waitFor();
      if (action === "Restart stack") {
        await plan.getByRole("button", { name: "Cancel", exact: true }).click();
      } else {
        await apply.click();
        await onTop(page.getByRole("dialog")).getByRole("button", { name: /^Done/ }).click({ timeout: 45_000 });
      }
    }
    await page.getByRole("link", { name: "Services", exact: true }).click();
    await webRow.getByRole("button", { name: /Actions for .*web-1/ }).click();
    await page.getByRole("menuitem", { name: "Restart this container" }).click();
    await plan.getByRole("button", { name: "Cancel", exact: true }).click();

    step("docker compose: review an edit to one service and preserve its sealed value");
    await page.getByRole("link", { name: "Compose file", exact: true }).click();
    const edit = page.getByRole("textbox", { name: "Compose file", exact: true });
    const text = await edit.inputValue();
    if (
      text.includes("compose-demo-sealed") ||
      text.includes("dev.dockwell.set") ||
      !text.startsWith("name: workspace-fixture\n") ||
      !text.includes(`POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}`)
    )
      throw new Error("Expected a sealed placeholder");
    const kept = page
      .locator("details")
      .filter({ has: page.locator("summary").filter({ hasText: "Kept in Dockwell" }) });
    await kept.locator("summary").click();
    await kept.getByText(/environment\.POSTGRES_PASSWORD/).waitFor();
    if (await kept.getByText(/desired_run_state|run state/i).count())
      throw new Error("Run state appeared among kept values");
    await assertStackFits(page, "Compose editor");
    await shoot(page, "docker-compose-editor");
    const draft = text.replace("nginx:1.27-alpine", "nginx:1.28-alpine");
    await edit.fill(draft.replace("image: nginx:1.28-alpine", "image: nginx:1.28-alpine\n    env_file: [outside.env]"));
    await page.getByRole("button", { name: "Review plan", exact: true }).click();
    await page
      .getByText(/Line \d+:.*env_file/)
      .first()
      .waitFor();
    await edit.fill(draft);
    await page.getByRole("button", { name: "Review plan", exact: true }).click();
    await plan.getByRole("button", { name: /Apply plan/ }).click();
    await onTop(page.getByRole("dialog")).getByRole("button", { name: /^Done/ }).click({ timeout: 40_000 });
    await page.getByRole("button", { name: "Review plan", exact: true }).and(page.locator(":disabled")).waitFor();
    if ((await edit.inputValue()).includes("compose-demo-sealed")) throw new Error("A sealed value reached the editor");

    step("docker compose: a newer revision leaves the stale draft intact");
    const environment = new URL(page.url()).pathname.split("/").at(-1);
    const workspace = `${origin}/v1/compose/workspaces/${environment}`;
    const read = await page.request.get(workspace);
    if (!read.ok()) throw new Error("The edited Compose workspace could not be read");
    const current = await read.json();
    const replacement = current.workspace.yaml.replace("nginx:1.28-alpine", "nginx:1.27-alpine");
    if (replacement === current.workspace.yaml) throw new Error("The initial edit did not apply");
    const planned = await page.request.post(`${workspace}:plan`, {
      headers: { Origin: origin },
      data: { snapshot: current.snapshot, yaml: replacement },
    });
    const { change: incoming } = await planned.json();
    if (!planned.ok() || !incoming) throw new Error("The later Compose revision could not plan");
    await applyFixture(page, { id: incoming.id, fingerprint: incoming.plan.fingerprint });
    await edit.fill(`${draft.replace("nginx:1.28-alpine", "nginx:1.29-alpine")}\n# My later draft\n`);
    await page.getByRole("button", { name: "Review plan", exact: true }).click();
    const reload = page.getByRole("button", { name: "Reload and keep my edits", exact: true });
    await reload.waitFor();
    if (!(await edit.inputValue()).includes("# My later draft")) throw new Error("The stale draft was discarded");
    await shoot(page, "docker-compose-stale");
    await reload.click();
    if (
      !(await page.getByRole("textbox", { name: "Previous draft" }).inputValue()).includes("# My later draft") ||
      !(await page.getByRole("textbox", { name: "Latest file" }).inputValue()).includes("nginx:1.27-alpine")
    ) {
      throw new Error("The overlapping draft and latest file did not remain visible");
    }
    await page.getByRole("button", { name: "Copy", exact: true }).waitFor();
    await assertStackFits(page, "Overlapping Compose draft");
    await shoot(page, "docker-compose-stale-conflict");

    step("docker compose: database backup preview and deletion each stay on the stack");
    await page.goto(stackURL);
    if (previews.length) throw new Error("Backup previews loaded before the Backups tab opened");
    await Promise.all([
      page.waitForRequest(previewRequest),
      page
        .getByRole("navigation", { name: "Compose workspace" })
        .getByRole("link", { name: "Backups", exact: true })
        .click(),
    ]);
    page.off("request", trackPreviews);
    await page.getByText("database-data", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Preview backup", exact: true }).click();
    const backed = onTop(page.getByRole("dialog", { name: "Back up database-data" }));
    await backed
      .getByText(/PostgreSQL/)
      .first()
      .waitFor();
    await assertStackFits(page, "Compose backup");
    await shoot(page, "docker-compose-backup");
    await backed.getByRole("button", { name: "Cancel", exact: true }).click();
    await page.getByRole("button", { name: "Delete stack", exact: true }).click();
    const deletion = page.getByRole("dialog", { name: "Delete stack" });
    await deletion.getByText("Volumes and their data stay.").waitFor();
    await deletion.getByText("Service", { exact: true }).first().waitFor();
    await deletion.getByText("Network", { exact: true }).first().waitFor();
    await deletion.getByText("Volume", { exact: true }).first().waitFor();
    await shoot(page, "docker-compose-deletion-selection");
    await deletion.getByRole("button", { name: "Review deletion", exact: true }).click();
    await plan.getByText("Volumes and their data stay", { exact: true }).waitFor();
    const keptLogs = plan.getByText(/^Kept logs for .*expire in 7 days/);
    await keptLogs.waitFor();
    const logged = await keptLogs.textContent();
    if (!["db", "edge", "jobs", "metrics", "search", "web"].every((name) => logged?.includes(name))) {
      throw new Error("The stack deletion does not name every service in a single log note");
    }
    await shoot(page, "docker-compose-deletion");
    await plan.getByRole("button", { name: "Cancel", exact: true }).click();

    step("docker compose: take on a whole outside project without restarting it");
    await page.goto(list);
    await page.getByRole("button", { name: "Import", exact: true }).click();
    await page.getByRole("menuitem", { name: "Existing project" }).click();
    await page.getByRole("link", { name: "outside-fixture", exact: true }).click();
    await page.getByText("/srv/compose-fixture", { exact: true }).waitFor();
    await assertStackFits(page, "Outside Compose project");
    await shoot(page, "docker-compose-outside");
    await page.getByRole("button", { name: "Take on", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Take on outside-fixture" })
      .getByRole("button", { name: "Review plan", exact: true })
      .click();
    await plan.getByRole("button", { name: /Apply plan/ }).click();
    await onTop(page.getByRole("dialog"))
      .getByRole("button", { name: /^Done|^Open/ })
      .first()
      .click({ timeout: 40_000 });
    const after = await Promise.all(ids.map((id) => dockerStarted(socket, id)));
    if (after.some((item, index) => item !== before[index])) throw new Error("Take on restarted a Compose service");
    await page.goto(list);
    await page.getByRole("link", { name: "outside-fixture", exact: true }).click();
    for (const id of ids) {
      await page.locator(`a[href*="/containers/${id}"]`).waitFor();
    }
  },
};
