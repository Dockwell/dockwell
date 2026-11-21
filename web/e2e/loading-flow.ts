// SPDX-License-Identifier: AGPL-3.0-only

// While a read is slow, each screen shows the shape of what is coming, in place:
// rows under a table's real header, a page's head and tabs, a sheet's choices, and,
// past a few seconds, one line that says what the wait is. Never a lone sentence
// (interaction principle 7 in design/system/README.md). Each read here is held
// on purpose, the way a slow Docker or a far server holds it, then let go.
//
// With DOCKWELL_E2E_LOADING_SHOTS=<dir> it also saves each screen while it waits,
// at 1440 and 390 px, light and dark, and once more with its slow line.

import { mkdir } from "node:fs/promises";
import { join } from "node:path";

import type { Page, Route } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

/** Holds every call to the RPC until the returned function lets them go. */
async function hold(page: Page, rpc: string): Promise<() => Promise<void>> {
  // A read the panel may send as a GET, with its message in the query.
  const pattern = new RegExp(`/dockwell\\.v1\\.${rpc.replace(".", "\\/")}(\\?|$)`);
  const held: Route[] = [];
  let open = false;
  const handler = (route: Route) => {
    if (open) return route.continue();
    held.push(route);
    return undefined;
  };
  await page.route(pattern, handler);
  return async () => {
    open = true;
    await page.unroute(pattern, handler);
    await Promise.all(held.splice(0).map((route) => route.continue().catch(() => undefined)));
  };
}

/** Fails if the page shows a wait as a sentence on its own, anywhere a person can see, sheets too. */
async function noBareLine(page: Page, where: string) {
  const said = await page.locator("body").evaluate((body) =>
    Array.from(body.querySelectorAll("p, span, div, li, h1, h2, h3"))
      .filter((node) => {
        if (!(node instanceof HTMLElement) || node.closest(".dw-sr")) return false;
        if (node.offsetParent === null || node.children.length > 1) return false;
        return getComputedStyle(node).visibility !== "hidden";
      })
      .map((node) => {
        // What a person sees: words only assistive technology hears are left out.
        const seen = node.cloneNode(true) as HTMLElement;
        for (const unseen of Array.from(seen.querySelectorAll(".dw-sr"))) unseen.remove();
        return seen.textContent?.trim() ?? "";
      })
      .filter(Boolean),
  );
  const bare = said.filter((text) => /^(Reading|Loading|Checking|Fetching|Asking)\b/.test(text));
  if (bare.length > 0) fail(`${where} says its wait in words, not shapes: ${bare.join(" | ")}`);
}

/** Waits for the shapes in scope to show, after their delay, and checks the page says nothing bare. */
async function expectShapes(page: Page, scope: string, where: string) {
  const shapes = page.locator(`${scope} .dw-skeleton`);
  await shapes.first().waitFor({ state: "visible", timeout: 10_000 });
  if ((await shapes.count()) < 3)
    fail(`${where} draws ${await shapes.count()} shapes, too few to stand for its content`);
  const busy = await page.locator('[aria-busy="true"]').count();
  if (busy === 0) fail(`${where} draws shapes, but nothing says it is busy`);
  await noBareLine(page, where);
}

interface Screen {
  /** The shot's name, and the step's. */
  name: string;
  /** What is held while it waits. */
  rpc: string;
  path: string;
  /** Where its shapes are. */
  scope: string;
  /** What shows at once, before anything lands: the page's head, its tabs, its toolbar. */
  atOnce?: (page: Page) => Promise<void>;
  /** The line a long wait gets. */
  slow?: string;
  /** What shows once the read lands, so the shapes are known to go. */
  landed: (page: Page) => Promise<void>;
  /** Shoot it once it has landed too, to show the screen with its data. */
  loadedShot?: boolean;
}

async function check(page: Page, origin: string, screen: Screen, step: FlowRun["step"]) {
  step(`loading: ${screen.name} draws what is coming while ${screen.rpc} waits`);
  const release = await hold(page, screen.rpc);
  try {
    await page.goto(`${origin}${screen.path}`);
    await screen.atOnce?.(page);
    await expectShapes(page, screen.scope, screen.name);
    if (screen.slow) {
      await page.getByText(screen.slow, { exact: true }).waitFor({ timeout: 8_000 });
      await noBareLine(page, `${screen.name}, after a long wait`);
    }
  } finally {
    await release();
  }
  // Once the read lands, its shapes go, all of them, and what it read takes their place.
  await page
    .locator(`${screen.scope} .dw-skeleton`)
    .first()
    .waitFor({ state: "detached", timeout: 15_000 })
    .catch(() => fail(`${screen.name} keeps its shapes after its read landed`));
  await screen.landed(page);
}

async function shoot(page: Page, origin: string, screen: Screen, dir: string) {
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    for (const width of [1440, 390]) {
      const release = await hold(page, screen.rpc);
      try {
        await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
        await page.goto(`${origin}${screen.path}`);
        // A phone may draw rows where a wide screen draws a table, so any of its shapes will do.
        await page.locator(".dw-skeleton").first().waitFor({ state: "visible", timeout: 10_000 });
        await page.waitForTimeout(250);
        await page.screenshot({ path: join(dir, `${screen.name}-${width}-${scheme}.png`), animations: "disabled" });
        if (screen.slow) {
          await page.getByText(screen.slow, { exact: true }).waitFor({ timeout: 8_000 });
          await page.screenshot({
            path: join(dir, `${screen.name}-slow-${width}-${scheme}.png`),
            animations: "disabled",
          });
        }
      } finally {
        await release();
      }
      if (screen.loadedShot) {
        // The same screen once its read has landed, to show it with its data.
        await screen.landed(page);
        await page.waitForTimeout(250);
        await page.screenshot({
          path: join(dir, `${screen.name}-loaded-${width}-${scheme}.png`),
          animations: "disabled",
        });
      }
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await atWidth(page, { width: 1440, height: 900 });
}

/** Every screen that waits on a read, for the tour that shoots them all mid-wait. Fake IDs do: each read is held. */
function tourOf(server: string): { name: string; path: string; holdSession?: boolean }[] {
  const hex = "4f".repeat(32);
  return [
    { name: "session", path: "/apps", holdSession: true },
    { name: "status", path: "/status" },
    { name: "overview", path: "/" },
    { name: "apps", path: "/apps" },
    { name: "app", path: "/apps/svc_on_its_way" },
    { name: "app-logs", path: "/apps/svc_on_its_way/logs" },
    { name: "new-app", path: "/apps/new" },
    { name: "websites", path: "/websites" },
    { name: "website", path: "/websites/web_on_its_way" },
    { name: "databases", path: "/databases" },
    { name: "database", path: "/databases/svc_on_its_way" },
    { name: "connections", path: "/connections" },
    { name: "connection", path: "/connections/on-its-way" },
    { name: "domains", path: "/domains" },
    { name: "backups", path: "/backups" },
    { name: "backup", path: "/backups/bkp_on_its_way" },
    { name: "volume", path: "/volumes/vol_on_its_way" },
    { name: "projects", path: "/projects" },
    { name: "project", path: "/projects/prj_on_its_way" },
    { name: "variables", path: "/variables" },
    { name: "catalog", path: "/catalog" },
    { name: "catalog-app", path: "/catalog/postgres" },
    { name: "catalog-install", path: "/catalog/postgres/install" },
    { name: "catalog-sources", path: "/catalog/sources" },
    { name: "activity", path: "/activity" },
    { name: "alerts", path: "/alerts" },
    { name: "alert-checks", path: "/alerts/checks" },
    { name: "alert-rules", path: "/alerts/rules" },
    { name: "alert-channels", path: "/alerts/channels" },
    { name: "scheduled-tasks", path: "/scheduled-tasks" },
    { name: "servers", path: "/servers" },
    { name: "server", path: server },
    { name: "docker", path: `${server}/docker` },
    { name: "docker-containers", path: `${server}/docker/containers` },
    { name: "docker-container", path: `${server}/docker/containers/${hex}` },
    { name: "docker-images", path: `${server}/docker/images` },
    { name: "docker-image", path: `${server}/docker/images/sha256:${hex}` },
    { name: "docker-composes", path: `${server}/docker/composes` },
    { name: "docker-cleanup", path: `${server}/docker/cleanup` },
    { name: "files", path: `${server}/files` },
    { name: "updates", path: `${server}/updates` },
    { name: "firewall", path: `${server}/firewall` },
    { name: "processes", path: `${server}/processes` },
    { name: "system-services", path: `${server}/services` },
    { name: "system-log", path: `${server}/log` },
    { name: "import", path: `${server}/import` },
    { name: "settings-members", path: "/settings/members" },
    { name: "settings-tokens", path: "/settings/api-tokens" },
    { name: "settings-integrations", path: "/settings/integrations" },
    { name: "settings-audit-log", path: "/settings/audit-log" },
    { name: "settings-control-plane", path: "/settings/control-plane" },
    { name: "settings-general", path: "/settings/general" },
    { name: "profile", path: "/profile" },
  ];
}

/** Holds every read but the session and the server list, or the session too, so each section shows its wait. */
async function holdAll(page: Page, session: boolean): Promise<() => Promise<void>> {
  const pattern = /\/dockwell\.v1\.[A-Za-z]+\/[A-Za-z]+/;
  const held: Route[] = [];
  const handler = (route: Route) => {
    const url = route.request().url();
    const lets =
      /AuthService\/|ServerService\/WatchServers/.test(url) && !(session && /AuthService\/GetAuthState/.test(url));
    if (lets) return route.continue();
    held.push(route);
    return undefined;
  };
  await page.route(pattern, handler);
  return async () => {
    await page.unroute(pattern, handler);
    await Promise.all(held.splice(0).map((route) => route.abort().catch(() => undefined)));
  };
}

/** Shoots every screen while it waits, at 1440 and 390, light and dark, and lists any that drew no shapes. */
async function tour(page: Page, origin: string, server: string, dir: string, step: FlowRun["step"]) {
  await mkdir(join(dir, "tour"), { recursive: true });
  const bare: string[] = [];
  for (const screen of tourOf(server)) {
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      for (const width of [1440, 390]) {
        const release = await holdAll(page, screen.holdSession ?? false);
        try {
          await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
          await page.goto(`${origin}${screen.path}`);
          const drew = await page
            .locator(".dw-skeleton")
            .first()
            .waitFor({ state: "visible", timeout: 6_000 })
            .then(() => true)
            .catch(() => false);
          if (!drew && !bare.includes(screen.name)) bare.push(screen.name);
          await page.waitForTimeout(250);
          await page.screenshot({
            path: join(dir, "tour", `${screen.name}-${width}-${scheme}.png`),
            animations: "disabled",
          });
        } finally {
          await release();
        }
      }
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await atWidth(page, { width: 1440, height: 900 });
  step(
    bare.length
      ? `loading: these drew no shapes while they waited: ${bare.join(", ")}`
      : "loading: every screen drew its shapes",
  );
}

export async function loadingFlow(page: Page, { origin, step }: FlowRun) {
  await atWidth(page, { width: 1440, height: 900 });
  const server = await ownServerPath(page, origin);
  const screens: Screen[] = [
    {
      name: "loading-images",
      rpc: "DockerService.ListDockerObjects",
      path: `${server}/docker/images`,
      scope: ".docker-image-table",
      atOnce: async (p) => {
        // The toolbar draws at once and works: the heading, the state filter and the search.
        await p.getByRole("heading", { name: "Images", exact: true }).waitFor();
        await p.getByRole("radiogroup", { name: "Image state" }).waitFor();
        if (await p.getByLabel("Search images").isDisabled()) fail("Images' search waits for the list");
        await p.locator(".docker-image-table th", { hasText: "Platform" }).waitFor();
      },
      slow: "Docker is slow to list images on this server",
      loadedShot: true,
      landed: async (p) => {
        // The table on a wide screen, the list on a phone, or none at all.
        await p
          .getByRole("table", { name: "Local images" })
          .or(p.getByRole("list", { name: "Local images" }))
          .or(p.locator(".empty-state"))
          .first()
          .waitFor({ timeout: 15_000 });
      },
    },
    {
      name: "loading-containers",
      rpc: "DockerService.ListDockerObjects",
      path: `${server}/docker/containers`,
      scope: ".docker-table",
      atOnce: async (p) => {
        await p.getByRole("heading", { name: "Containers" }).waitFor();
        await p.locator(".docker-table th", { hasText: "Name" }).waitFor();
      },
      slow: "Docker is slow to answer on this server",
      landed: async (p) => {
        await p
          .getByRole("table", { name: "Container inventory" })
          .or(p.locator(".empty-state"))
          .first()
          .waitFor({ timeout: 15_000 });
      },
    },
    {
      name: "loading-apps",
      rpc: "ServiceService.ListServices",
      path: "/apps",
      scope: ".apps-table",
      atOnce: async (p) => {
        await p.getByRole("heading", { level: 1, name: "Apps" }).waitFor();
        await p.locator(".apps-table th", { hasText: "Status" }).waitFor();
      },
      landed: async (p) => {
        await p
          .getByRole("table", { name: /apps/i })
          .or(p.locator(".empty-state"))
          .first()
          .waitFor({ timeout: 15_000 });
      },
    },
    {
      name: "loading-app",
      rpc: "ServiceService.ListServices",
      path: "/apps/svc_still_on_its_way",
      scope: "main",
      atOnce: async (p) => {
        // The page's tabs draw at once and work; its title stands as a shape until the app is known.
        await p.getByRole("navigation", { name: "App sections" }).getByRole("link", { name: "Logs" }).waitFor();
        await p.locator("h1 .dw-skeleton").waitFor({ state: "visible" });
      },
      landed: async (p) => {
        await p.getByText("No app with this ID").waitFor({ timeout: 15_000 });
      },
    },
    {
      name: "loading-new-app",
      rpc: "CatalogService.ListCatalogEntries",
      path: "/apps/new",
      scope: ".new-app-entries",
      atOnce: async (p) => {
        // The sheet and its search draw at once; only the catalog's entries wait.
        await p.getByRole("dialog", { name: "New app" }).getByLabel("Search the catalog").waitFor();
      },
      landed: async (p) => {
        await p
          .getByRole("dialog", { name: "New app" })
          .getByRole("button")
          .filter({ has: p.locator(".new-app-entry-name") })
          .first()
          .waitFor({ timeout: 15_000 });
      },
    },
  ];
  for (const screen of screens) await check(page, origin, screen, step);

  step("loading: a phone draws a list's rows while Apps waits");
  await atWidth(page, { width: 390, height: 844 });
  const release = await hold(page, "ServiceService.ListServices");
  try {
    await page.goto(`${origin}/apps`);
    await expectShapes(page, ".dw-skeleton-rows", "Apps on a phone");
  } finally {
    await release();
  }
  await atWidth(page, { width: 1440, height: 900 });

  const dir = process.env["DOCKWELL_E2E_LOADING_SHOTS"];
  if (dir) {
    step(`loading: each screen while it waits, at 1440 and 390, light and dark, into ${dir}`);
    await mkdir(dir, { recursive: true });
    for (const screen of screens) await shoot(page, origin, screen, dir);
    await tour(page, origin, server, dir, step);
  }
  // Nothing here makes anything; New app closes with the page it opened on.
  await page.goto(`${origin}/apps`);
}

export const flow: Flow = { order: 97, run: (page, run) => loadingFlow(page, run) };

export { check as checkLoadingScreen, hold as holdRead, shoot as shootLoadingScreen };
