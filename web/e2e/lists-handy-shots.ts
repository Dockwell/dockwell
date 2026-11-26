// SPDX-License-Identifier: AGPL-3.0-only

import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import type { Page } from "playwright-core";
import type { FlowRun } from "./flows.ts";
import { atWidth } from "./sheets.ts";
import { bytes, number } from "./wire.ts";

const suffix = (i: number) => String(i + 1).padStart(2, "0");

async function rpc<T>(page: Page, service: string, method: string, body: object): Promise<T> {
  return (await page.evaluate(
    async ({ service, method, body }) => {
      const res = await fetch(`/dockwell.v1.${service}/${method}`, {
        method: "POST",
        headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${method}: ${res.status} ${await res.text()}`);
      return res.json() as Promise<unknown>;
    },
    { service, method, body },
  )) as T;
}

interface Ref {
  id: string;
  resource: { name: string; scope: string };
}

async function seed(page: Page, data: string) {
  const apply = async (summary: string, edits: object[]) => {
    const { change } = await rpc<{ change: { id: string; plan: { fingerprint: string } } }>(
      page,
      "ChangeService",
      "ProposeChange",
      { summary, edits },
    );
    await rpc(page, "ChangeService", "ApplyChange", { id: change.id, fingerprint: change.plan.fingerprint });
  };
  await apply(
    "Sample projects",
    Array.from({ length: 60 }, (_, i) => ({
      resource: { kind: "project", name: `sample-project-${suffix(i)}` },
      spec: { project: {} },
    })),
  );
  const { resources: projects } = await rpc<{ resources: Ref[] }>(page, "ResourceService", "ListResources", {
    kind: "project",
  });
  const { resources: environments } = await rpc<{ resources: Ref[] }>(page, "ResourceService", "ListResources", {
    kind: "environment",
  });
  const projectScopes = projects.filter((p) => p.resource.name.startsWith("sample-project-")).map((p) => p.id);
  const scopes = environments.filter((e) => projectScopes.includes(e.resource.scope)).map((env) => env.id);
  if (scopes.length !== 60) throw new Error(`Expected 60 sample environments, got ${scopes.length}`);
  const scope = scopes[0];
  if (!scope) throw new Error("The stand has no sample environment");
  await apply(
    "Sample services",
    Array.from({ length: 60 }, (_, i) => ({
      resource: { kind: "service", name: `sample-app-${suffix(i)}`, scope: scopes[i % 10] },
      spec: { service: { app: { image: "nginx:1.27", desiredRunState: "DESIRED_RUN_STATE_STOPPED" } } },
    })),
  );
  await apply(
    "Sample variables",
    Array.from({ length: 60 }, (_, i) => ({
      resource: { kind: "variable", name: `SAMPLE_FLAG_${suffix(i)}`, scope },
      spec: { variable: { value: i % 2 === 0 ? "enabled" : "disabled" } },
    })),
  );

  // Runs are history, not revisioned resources. Insert historical receipts in
  // this disposable stand so the browser exercises real RPC cursors.
  const db = new DatabaseSync(join(data, "control", "state.db"));
  try {
    db.exec("PRAGMA busy_timeout = 5000; BEGIN IMMEDIATE");
    const insert = db.prepare(`INSERT INTO backups
      (id, environment_id, volume, volume_id, server_id, state, method, size_bytes, actor, error, started_at, finished_at)
      VALUES (?, ?, ?, ?, ?, ?, 'files', ?, 'system', ?, ?, ?)`);
    const server = String(db.prepare("SELECT id FROM servers LIMIT 1").get()?.["id"] ?? "");
    if (!server) throw new Error("The stand has no server for the backup receipts");
    for (let i = 0; i < 120; i++) {
      const failed = i % 11 === 0;
      const at = Date.now() - 600_000 - i * 86_400_000;
      insert.run(
        `bkp_sample_${i}`,
        scope,
        `sample-volume-${suffix(i % 10)}`,
        `vol_sample_${i % 10}`,
        server,
        failed ? "failed" : "done",
        failed ? 0 : (15 + i) * 1_000_000,
        failed ? "The disk is full." : "",
        at,
        at + 14000,
      );
    }
    db.exec("COMMIT");
  } finally {
    db.close();
  }
  return { scope, service: "sample-app-01" };
}

/** Isolated read fixtures for the other priority lists, which need no deploys or external accounts. */
async function addListFixtures(page: Page, scope: string, service: string) {
  const reply = async (method: string, row: (index: number) => Buffer[]) => {
    const body = Buffer.concat(Array.from({ length: 60 }, (_, i) => bytes(1, Buffer.concat(row(i)))));
    await page.route(new RegExp(`/${method}(?:\\?|$)`), (route) =>
      route.fulfill({ contentType: "application/proto", body }),
    );
  };
  const ref = (name: string) => Buffer.concat([bytes(1, "service"), bytes(2, scope), bytes(3, name)]);
  await reply("ListDatabases", (i) => [
    bytes(1, `svc_sample_db_${i}`),
    bytes(2, ref(`sample-db-${suffix(i)}`)),
    number(3, 1),
    bytes(4, "postgres:17"),
    bytes(5, "17"),
    number(9, 5432),
  ]);
  await reply("ListWebsites", (i) => [
    bytes(1, `prj_sample_site_${i}`),
    bytes(2, `sample-site-${suffix(i)}`),
    bytes(3, `sample-site-${suffix(i)}.example.test`),
    bytes(4, bytes(1, bytes(1, "https://upstream.example.test"))),
    bytes(5, `env_sample_site_${i}`),
  ]);
  await reply("ListDomains", (i) => {
    const name = `sample-domain-${suffix(i)}.example.test`;
    const url = `https://${name}`;
    const route = Buffer.concat([
      bytes(1, `rte_sample_${i}`),
      bytes(2, `sample-route-${suffix(i)}`),
      bytes(4, service),
      number(5, 80),
      bytes(6, url),
    ]);
    return [bytes(1, name), bytes(2, scope), bytes(3, url), bytes(4, route)];
  });
  await reply("ListConnections", (i) => [
    bytes(1, `con_sample_${i}`),
    bytes(2, `sample-bucket-${suffix(i)}`),
    number(4, 1),
    bytes(5, "https://storage.example.test"),
    bytes(7, `archive-${suffix(i)}`),
  ]);
}

export async function listScreenshots(page: Page, { data, origin, step }: FlowRun, directory: string) {
  step("lists: seed 60 apps across 10 projects, 120 backup runs, 60 projects and 60 variables");
  const { scope, service } = await seed(page, data);
  await mkdir(directory, { recursive: true });
  const probe = await page.context().newPage();
  try {
    await addListFixtures(probe, scope, service);
    for (const width of [1440, 390]) {
      await atWidth(probe, { width, height: width === 1440 ? 900 : 844 });
      for (const [path, query, filter, sort, phoneSort] of [
        ["projects", "sample", "Filter projects", "Project", "name"],
        ["apps", "sample", "Filter apps", "App", "app"],
        ["backups", "sample", "Filter backups", "Volume", "volume"],
        ["variables", "SAMPLE", "Filter by name", "Key", "name"],
        ["websites", "sample", "Filter websites", "Website", "website"],
        ["databases", "sample", "Filter databases", "Database", "name"],
        ["domains", "sample", "Filter domains", "Domain", "domain"],
        ["connections", "sample", "Filter connections", "Connection", "name"],
      ] as const) {
        step(`lists: capture ${path} at ${width}px`);
        const target = new URL(path, origin);
        if (path === "variables") target.searchParams.set("environment", scope);
        await probe.goto(target.toString());
        const search = probe.getByRole("searchbox", { name: filter });
        await search.fill(query);
        await probe.waitForURL((url) => url.searchParams.get("q") === query);
        if (width === 390) await probe.getByRole("combobox", { name: "Sort by" }).selectOption(phoneSort);
        else await probe.getByRole("button", { name: sort, exact: true }).click();
        await probe.waitForURL((url) => url.searchParams.get("sort") === phoneSort);
        if (path === "backups") {
          const all = probe.getByRole("button", { name: "Search all backups" });
          await all.click();
          await all.waitFor({ state: "hidden" });
        }
        const next = probe.getByRole("button", { name: "Next", exact: true });
        await next.click();
        await probe.waitForURL((url) => url.searchParams.get("page") === "2");
        const range = path === "backups" ? "51-100 of 120" : "51-60 of 60";
        await probe.locator(".dw-pager-summary").getByText(range, { exact: true }).waitFor();
        await probe.evaluate(() => window.scrollTo(0, 0));
        await probe.screenshot({ path: join(directory, `${path}-${width}-page-2.png`), fullPage: true });
        if (path === "apps" && width === 1440) {
          const row = probe.getByRole("row", { name: /sample-app-/ }).first();
          const cell = row.getByRole("cell").nth(1);
          await cell.scrollIntoViewIfNeeded();
          const box = await cell.boundingBox();
          if (!box) throw new Error("An app row has no cell to right-click");
          const point = { x: box.x + 12, y: box.y + 12 };
          await probe.mouse.click(point.x, point.y, { button: "right" });
          const menu = probe.getByRole("menu");
          await menu.waitFor();
          const placed = await menu.boundingBox();
          if (!placed || Math.abs(placed.x - point.x) > 2 || Math.abs(placed.y - point.y) > 2)
            throw new Error("The app menu did not open at the pointer");
          await probe.keyboard.press("Escape");
          if (!(await row.evaluate((node) => node === document.activeElement)))
            throw new Error("Closing the app menu did not return focus to the row");
        }
        if (path === "variables" && width === 1440) {
          await probe
            .getByRole("row", { name: /SAMPLE_FLAG_/ })
            .first()
            .press("Shift+F10");
        } else {
          await probe
            .getByRole("button", { name: /^Actions for / })
            .first()
            .click();
        }
        await probe.getByRole("menu").waitFor();
        await probe.screenshot({ path: join(directory, `${path}-${width}-menu.png`) });
      }
    }
  } finally {
    await probe.close();
  }
}
