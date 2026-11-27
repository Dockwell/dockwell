// SPDX-License-Identifier: AGPL-3.0-only

import { BinaryReader } from "@bufbuild/protobuf/wire";
import type { Locator, Page } from "playwright-core";

import { dockerCreateStorage, dockerHasImage, dockerPull, dockerStorageNames, dockerTag } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { captureRowVerbs, proposeFixture } from "./row-verbs-shots.ts";
import { atWidth, onTop } from "./sheets.ts";

function bytesFields(input: Uint8Array, want: number): Uint8Array[] {
  const reader = new BinaryReader(input);
  const values: Uint8Array[] = [];
  while (reader.pos < reader.len) {
    const [field, wire] = reader.tag();
    if (field === want && wire === 2) values.push(reader.bytes());
    else reader.skip(wire);
  }
  return values;
}

function stringField(input: Uint8Array, field: number): string {
  return Buffer.from(bytesFields(input, field)[0] ?? []).toString("utf8");
}

function named(name: string, scope: string) {
  return { resource: { kind: "service", name, scope }, spec: { service: { app: { image: "nginx:1.27" } } } };
}

async function review(page: Page) {
  const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await plan.waitFor({ timeout: 30_000 });
  return plan;
}

function proposed(page: Page) {
  return page.waitForRequest((request) => request.url().endsWith("/dockwell.v1.ChangeService/ProposeChange"));
}

function editedNames(body: Uint8Array) {
  // ProposeChangeRequest.edits (2), then Edit.resource (1) and ResourceRef.name (3).
  return bytesFields(body, 2).map((edit) => stringField(bytesFields(edit, 1)[0] ?? new Uint8Array(), 3));
}

async function filterDocker(page: Page, field: Locator, search: string) {
  const filtered = page.waitForResponse((response) => {
    const url = new URL(response.url());
    if (!url.pathname.endsWith("/dockwell.v1.DockerService/ListDockerObjects")) return false;
    const query = url.searchParams.get("message");
    const body = query
      ? Buffer.from(query, url.searchParams.get("base64") === "1" ? "base64url" : "utf8")
      : response.request().postDataBuffer();
    return body?.includes(search) ?? false;
  });
  await field.fill(search);
  await (await filtered).finished();
}

async function planCleanup(page: Page, count: number) {
  const button = page
    .getByRole("toolbar", { name: "Actions for the rows selected" })
    .getByRole("button", { name: `Delete ${count}`, exact: true });
  await button.waitFor();
  const [request] = await Promise.all([
    page.waitForRequest((request) => request.url().endsWith("/dockwell.v1.DockerService/PlanDockerMaintenance")),
    button.click(),
  ]);
  const body = request.postDataBuffer() ?? new Uint8Array();
  // PlanDockerMaintenanceRequest.cleanup (10) and DockerCleanupSelection.targets (2).
  const targets = bytesFields(bytesFields(body, 10)[0] ?? new Uint8Array(), 2);
  if (targets.length !== count || new Set(targets.map((target) => stringField(target, 3))).size !== count)
    throw new Error(`The maintenance plan did not target ${count} distinct artifacts`);
  return { plan: await review(page), names: targets.map((target) => stringField(target, 4)) };
}

async function applyCleanup(page: Page, plan: Awaited<ReturnType<typeof review>>) {
  // Cleanup has a passkey gate. The review never asks for a typed fingerprint.
  await plan.getByText("Applying this asks for your passkey", { exact: true }).waitFor();
  if ((await plan.locator(".plan-gate input").count()) !== 0) throw new Error("Cleanup asked for a typed fingerprint");
  await plan.getByRole("button", { name: /^Apply plan/ }).click();
  await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 45_000 });
}

export const flow: Flow = {
  order: 110,
  async run(page, run) {
    const { origin, step, socket, docker } = run;
    const serverPath = await ownServerPath(page, origin);
    const server = serverPath.split("/").at(-1) ?? "";
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/apps`);

    const { resources } = await page.request
      .get(`${origin}/v1/resources?kind=environment`)
      .then((response) => response.json() as Promise<{ resources: { id: string; resource: { name: string } }[] }>);
    const scope = resources.find((item) => item.resource.name === "production")?.id;
    if (!scope) throw new Error("There is no production environment for the row verbs fixture");
    const apps = ["row-verbs-a", "row-verbs-b", "row-verbs-c"];
    await proposeFixture(
      page,
      "Row verbs fixture apps",
      apps.map((name) => ({ ...named(name, scope), servers: [server] })),
    );

    step("row verbs: pick a range, clear with Escape, and stop three apps in one plan");
    await page.goto(`${origin}/apps?q=row-verbs&sort=app`);
    const table = page.getByRole("table", { name: "Apps" });
    const first = table.getByRole("checkbox", { name: `Select ${apps[0]}`, exact: true });
    const last = table.getByRole("checkbox", { name: `Select ${apps[2]}`, exact: true });
    const bar = page.getByRole("toolbar", { name: "Actions for the rows selected" });
    await first.click();
    await last.click({ modifiers: ["Shift"] });
    await bar.getByText("3 selected", { exact: true }).waitFor();
    await last.press("Escape");
    await bar.waitFor({ state: "hidden" });
    await first.press("Control+a");
    await bar.getByText("3 selected", { exact: true }).waitFor();

    const asked = proposed(page);
    await bar.getByRole("button", { name: "Stop 3", exact: true }).click();
    const names = editedNames((await asked).postDataBuffer() ?? new Uint8Array());
    if (names.length !== apps.length || names.some((name, index) => name !== apps[index])) {
      throw new Error(`The batch planned different edits: ${names.join(", ")}`);
    }
    const plan = await review(page);
    for (const name of apps) await plan.getByText(name, { exact: true }).first().waitFor();
    const applied = page.waitForRequest((request) => request.url().endsWith("/dockwell.v1.ChangeService/ApplyChange"));
    await plan.getByRole("button", { name: /^Apply plan/ }).click();
    await applied;
    await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 45_000 });
    await bar.waitFor({ state: "hidden" });
    await page.waitForFunction(() => document.activeElement?.getAttribute("aria-label") === "Apps");
    if ((await docker()).some((item) => apps.includes(item.service) && item.running))
      throw new Error("The selected apps are still running on Docker after the batch stop");

    step("row verbs: open a row menu on a phone, then start one app and review Pause from its header");
    await atWidth(page, { width: 390, height: 844 });
    await page.getByRole("button", { name: `Actions for ${apps[0]}` }).click();
    await page.getByRole("menuitem", { name: "Start", exact: true }).click();
    await (await review(page)).getByRole("button", { name: /^Apply plan/ }).click();
    await page.getByRole("button", { name: "Done", exact: true }).click({ timeout: 45_000 });
    await atWidth(page, { width: 1440, height: 900 });
    await first.click();
    await last.click({ modifiers: ["Shift"] });
    await bar.getByText("2 skipped (Already stopped)", { exact: false }).waitFor();
    await bar.getByRole("button", { name: "Start 2", exact: true }).waitFor();
    const mixed = proposed(page);
    await bar.getByRole("button", { name: "Stop 1", exact: true }).click();
    const mixedNames = editedNames((await mixed).postDataBuffer() ?? new Uint8Array());
    if (mixedNames.join() !== apps[0]) throw new Error(`The mixed selection planned to stop ${mixedNames.join(", ")}`);
    await (await review(page)).getByRole("button", { name: "Cancel", exact: true }).click();
    const appFilter = page.getByRole("searchbox", { name: "Filter apps" });
    await appFilter.fill(apps[1] ?? "");
    await bar.getByText("1 selected", { exact: true }).waitFor();
    await appFilter.fill("row-verbs");
    await first.waitFor();
    if (await first.isChecked()) throw new Error("A row hidden by the app filter stayed selected");
    await bar.getByRole("button", { name: "Clear the selection" }).click();
    const row = table.getByRole("row", { name: new RegExp(apps[0] ?? "") });
    const context = page.getByRole("menu", { name: `Actions for ${apps[0]}` });
    await row.press("Shift+F10");
    await context.getByRole("menuitem", { name: "Open", exact: true }).click();
    await page.getByRole("button", { name: `Actions for ${apps[0]}` }).click();
    const headerMenu = page.getByRole("menu", { name: `Actions for ${apps[0]}` });
    await headerMenu.getByRole("menuitem", { name: "Restart", exact: true }).waitFor();
    await headerMenu.getByRole("menuitem", { name: "Pause", exact: true }).click();
    await (await review(page))
      .getByText(apps[0] ?? "", { exact: true })
      .first()
      .waitFor();
    await page.keyboard.press("Escape");

    step("row verbs: delete two specific images with one reviewed Docker plan");
    const images = ["row-verbs-image-a:1", "row-verbs-image-b:1", "row-verbs-image-keep:1"];
    await Promise.all(images.map((name) => dockerPull(socket, name)));
    await dockerTag(socket, images[2] ?? "", "row-verbs-image-keep", "2");
    await page.goto(`${origin}${serverPath}/docker/images`);
    const filter = page.getByRole("textbox", { name: "Search images" });
    await filterDocker(page, filter, "row-verbs-image");
    for (const image of images)
      await page.getByRole("checkbox", { name: `Select ${image}`, exact: true }).click({ timeout: 45_000 });
    await bar.getByText("3 selected", { exact: true }).waitFor();
    await bar.getByText("1 skipped (Has more than one tag)", { exact: true }).waitFor();
    const imageCleanup = await planCleanup(page, 2);
    await imageCleanup.plan.getByText("1 skipped (Has more than one tag)", { exact: true }).waitFor();
    await applyCleanup(page, imageCleanup.plan);
    const remaining = await Promise.all(images.map((name) => dockerHasImage(socket, name)));
    if (remaining.join() !== "false,false,true")
      throw new Error(`Unexpected images after cleanup: ${remaining.join()}`);

    for (const kind of ["volumes", "networks"] as const) {
      step(`row verbs: delete two selected ${kind} and keep a third`);
      const prefix = `row-verbs-${kind}`;
      const targets = [`${prefix}-a`, `${prefix}-b`];
      const keep = `${prefix}-keep`;
      await Promise.all([...targets, keep].map((name) => dockerCreateStorage(socket, kind, name)));
      await page.goto(`${origin}${serverPath}/docker/${kind}`);
      const search = page.getByRole("textbox", { name: kind === "volumes" ? "Find a volume" : "Find a network" });
      await filterDocker(page, search, prefix);
      for (const name of targets)
        await page.getByRole("checkbox", { name: `Select ${name}`, exact: true }).click({ timeout: 45_000 });
      const cleanup = await planCleanup(page, targets.length);
      if (cleanup.names.toSorted().join() !== targets.join())
        throw new Error(`Unexpected ${kind} in plan: ${cleanup.names.join(", ")}`);
      await applyCleanup(page, cleanup.plan);
      const left = await dockerStorageNames(socket, kind);
      if (!left.includes(keep) || targets.some((name) => left.includes(name)))
        throw new Error(
          `Unexpected ${kind} after cleanup: ${left.filter((name) => name.startsWith(prefix)).join(", ")}`,
        );
    }

    const directory = process.env["DOCKWELL_E2E_ROW_VERBS_SHOTS"];
    if (directory) await captureRowVerbs(page, run, { directory, scope, server, serverPath });
  },
};
