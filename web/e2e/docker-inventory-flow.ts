// SPDX-License-Identifier: AGPL-3.0-only

import type { Page } from "playwright-core";

import { dockerHealth, dockerRemove, dockerRun } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";

const name = "inventory-example";
const secret = "sensitive-inventory-fixture";
const longImage = `registry.example.test/platform/production/operations/container-diagnosis/worker@sha256:${"ab".repeat(32)}`;

async function assertFits(page: Page, text: string) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (overflow > 1) throw new Error(`${text} overflows by ${overflow} px`);
}

async function assertLayouts(page: Page, title: string, check?: () => Promise<void>) {
  for (const colorScheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme });
    for (const width of [1440, 1024, 390, 320]) {
      await atWidth(page, { width, height: 900 });
      await assertFits(page, `${title} at ${width} ${colorScheme}`);
      await check?.();
    }
  }
  await page.emulateMedia({ colorScheme: "light" });
  await atWidth(page, { width: 1440, height: 900 });
}

async function assertActiveTab(page: Page, name: "Overview" | "Containers") {
  const nav = page.getByRole("navigation", { name: "Docker views" });
  const tab = nav.getByRole("link", { name, exact: true });
  await tab.waitFor();
  const hasIndicator = await tab.evaluate((link) => {
    const current = link.parentElement?.querySelectorAll('[aria-current="page"]');
    return current?.length === 1 && current[0] === link && getComputedStyle(link, "::after").height === "2px";
  });
  if (!hasIndicator) throw new Error(`${name} has no active tab indicator`);
}

async function assertImageClips(page: Page) {
  const image = page.getByTitle(longImage, { exact: true });
  await image.waitFor();
  const clips = await image.evaluate((element) => {
    const style = getComputedStyle(element);
    const cell = element.closest("td, li");
    return (
      cell &&
      style.textOverflow === "ellipsis" &&
      style.overflowX === "hidden" &&
      element.scrollWidth > element.clientWidth &&
      element.getBoundingClientRect().right <= cell.getBoundingClientRect().right
    );
  });
  if (!clips)
    throw new Error(`the long image does not ellipsize within its column or card at ${page.viewportSize()?.width}`);
  const escaped = await page.locator(".docker-table tbody td > *").evaluateAll((children) =>
    children.some((child) => {
      const cell = child.parentElement?.getBoundingClientRect();
      const box = child.getBoundingClientRect();
      return cell && (box.right > cell.right + 1 || box.left < cell.left - 1);
    }),
  );
  if (escaped) throw new Error("a Docker table cell crosses into another column");
}

export const flow: Flow = {
  order: 71,
  run: async (page, { origin, step, shoot, socket }) => {
    step("docker inventory: a server tab counts and opens all containers");
    const server = await ownServerPath(page, origin);
    const inventory = `${origin}${server}/docker/containers`;
    const add = () =>
      dockerRun(socket, name, {
        image: "acme/legacy:prints",
        command: ["sh", "-c", "echo fixture log; exec sleep 3600"],
        env: [`SECRET=${secret}`],
        port: 18093,
        mount: "inventory-data:/data",
      });
    let id = await add();
    const attentionName = "inventory-attention";
    const sick = await dockerRun(socket, attentionName, {
      image: longImage,
      mount: "attention-data:/data",
    });
    try {
      await dockerHealth(socket, sick, "unhealthy", "sensitive health output");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(`${origin}${server}`);
      await page.getByRole("link", { name: "Docker", exact: true }).click();
      await page.getByRole("heading", { name: "Docker overview" }).waitFor();
      const engine = page.getByRole("region", { name: "Engine inventory" });
      await engine.getByText("Docker root directory").waitFor();
      await engine.getByText("Logging driver").waitFor();
      await engine.getByText("Registry mirrors").waitFor();
      const attention = page.getByRole("link", { name: "Needs attention", exact: false });
      await page.getByRole("region", { name: "Needs attention" }).getByRole("link", { name: attentionName }).waitFor();
      await assertLayouts(page, "docker overview", () => assertActiveTab(page, "Overview"));
      await shoot(page, "docker-depth-overview");
      await attention.click();
      const all = page.getByRole("radio", { name: "All", exact: true });
      await page.getByRole("link", { name: attentionName, exact: true }).waitFor();
      step("docker inventory: long image references fit their cells and filtered tabs stay active");
      await assertLayouts(page, "container inventory with a long image", async () => {
        await assertActiveTab(page, "Containers");
        await assertImageClips(page);
      });
      await all.click();
      const search = page.getByRole("textbox", { name: "Search containers" });
      await search.fill(name);
      const row = page.getByRole("link", { name, exact: true });
      await row.waitFor();
      await page.getByRole("radio", { name: "Stopped", exact: true }).click();
      await page.getByRole("heading", { name: "No containers match" }).waitFor();
      await page.getByRole("radio", { name: "Running", exact: true }).click();
      await row.waitFor();
      await page.getByRole("row").filter({ has: row }).getByText("5 MB", { exact: false }).waitFor({ timeout: 20_000 });

      await assertLayouts(page, "container inventory");
      await search.fill("");
      await page.getByRole("radio", { name: "All", exact: true }).click();
      await row.waitFor();
      await shoot(page, "docker-depth-containers");
      const actions = page.getByRole("button", { name: `Actions for ${name}` });
      await actions.click();
      await page.getByRole("menuitem", { name: "Take on" }).waitFor();
      await page.keyboard.press("Escape");
      await row.click();
      await page.getByRole("region", { name: "Lifecycle" }).waitFor();
      if (!page.url().includes(id)) throw new Error("container URL doesn't contain its full ID");
      await page.getByText("5 MB", { exact: true }).waitFor({ timeout: 20_000 });
      await assertLayouts(page, "docker summary");
      await shoot(page, "docker-depth-summary");
      await page.getByRole("link", { name: "Inspect", exact: true }).click();
      await page.getByText("Show redacted JSON", { exact: true }).click();
      const json = await page.getByRole("region", { name: "Redacted inspection data", exact: true }).innerText();
      if (!json.includes(id) || json.includes(secret) || json.includes("sleep 3600"))
        throw new Error("inspection was not redacted");
      await assertLayouts(page, "container inspect");
      await shoot(page, "docker-depth-inspect");
      await page.getByRole("link", { name: "Logs", exact: true }).click();
      await page.getByRole("log").getByText("legacy says hello").waitFor({ timeout: 20_000 });
      await assertLayouts(page, "docker logs");
      await shoot(page, "docker-depth-logs");
      await page.getByRole("link", { name: `Terminal for ${name}` }).click();
      await page.getByRole("button", { name: "Open a shell" }).waitFor();
      if (!page.url().includes(id)) throw new Error("terminal opened against a mutable name");
      await page.getByText("Run a command", { exact: true }).click();
      await page.getByRole("textbox", { name: "Executable" }).fill("/bin/sh");
      await page.getByRole("button", { name: "Add argument" }).click();
      await page.getByRole("textbox", { name: "Argument 1" }).fill("-i");
      await assertLayouts(page, "docker terminal");
      await shoot(page, "docker-depth-terminal");

      step("docker inventory: a same-name replacement has a different identity");
      const old = id;
      await page.goto(`${inventory}/${old}?name=${name}`);
      await page.getByRole("region", { name: "Lifecycle" }).waitFor();
      await dockerRemove(socket, old, "inventory-data");
      id = await add();
      const replacement = page.getByRole("link", { name: "Open replacement" });
      await replacement.waitFor({ timeout: 20_000 });
      await replacement.click();
      await page.getByRole("region", { name: "Lifecycle" }).waitFor();
      if (!page.url().includes(id) || id === old) throw new Error("the replacement did not open");

      await page.goto(`${origin}/servers/docker`);
      await assertActiveTab(page, "Overview");
      await page.goto(`${origin}/servers/docker?view=containers&attention=true`);
      await assertActiveTab(page, "Containers");
    } finally {
      await dockerRemove(socket, id, "inventory-data");
      await dockerRemove(socket, sick, "attention-data");
    }
  },
};
