// SPDX-License-Identifier: AGPL-3.0-only

// The containers Dockwell didn't make, as someone moving a server in meets them: one
// run by hand is on the server's page, its logs read like an app's, and looking at
// it touches nothing. The same steps run on scripts/fakedocker in make e2e, and on
// this machine's Docker in make docker-panel-e2e.

import type { Page } from "playwright-core";

import { type DockerProbe, dockerRemove, dockerRun, dockerStarted } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";

export interface ContainersFlowOptions {
  origin: string;
  step: (what: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
  socket: string;
  docker: DockerProbe;
  /** What a container runs that prints "legacy says hello": scripts/fakedocker's image, or a command on Docker's. */
  image: string;
}

const NAME = "legacy-api";
const SECRET = "not-for-the-panel";
const SAYS = "legacy says hello";
const PANEL = "Containers Dockwell didn't make";

function fail(message: string): never {
  throw new Error(message);
}

export async function containersFlow(
  page: Page,
  { origin, step, shoot, socket, docker, image }: ContainersFlowOptions,
) {
  step("containers: one run by hand is on the server's page, and nothing of Dockwell's is");
  const id = await dockerRun(socket, NAME, {
    image,
    command: ["sh", "-c", `echo ${SAYS}; exec sleep 3600`],
    env: [`API_KEY=${SECRET}`],
    port: 18_093,
    mount: "legacy-data:/data",
  });
  try {
    const before = await dockerStarted(socket, id);
    await page.setViewportSize({ width: 1280, height: 900 });
    const link = await ownServerPath(page, origin);
    await page.goto(`${origin}${link}`);
    const panel = page.getByRole("region", { name: PANEL });
    const row = panel.getByRole("row", { name: new RegExp(`\\b${NAME}\\b`) });
    await row.waitFor({ timeout: 20_000 });
    const text = (await row.innerText()).replace(/\s+/g, " ");
    for (const want of ["Running", image, "18093→80", "legacy-data"]) {
      if (!text.includes(want)) fail(`the row reads "${text}", without ${want}`);
    }
    const listed = await panel.innerText();
    if (!listed.includes("looking here changes nothing")) fail(`the panel doesn't say what it is: ${listed}`);
    const own = await docker();
    if (own.length === 0) fail("the flow ran with nothing of Dockwell's on the server, so this shows nothing about it");
    if (!listed.includes("more container")) fail(`the panel doesn't say what it left out: ${listed}`);
    for (const c of own) {
      if (listed.includes(c.name)) fail(`Dockwell's own ${c.name} is in the list of what it didn't make`);
    }
    await shoot(page, "containers");

    step("containers: its logs read like an app's, and no value of its environment shows");
    await row.getByRole("link", { name: NAME }).click();
    await page.getByRole("heading", { level: 1, name: NAME, exact: true }).waitFor();
    await page.getByRole("region", { name: "What Docker says of it" }).getByText("legacy-data→/data").waitFor();
    await page.getByRole("log").getByText(SAYS).waitFor({ timeout: 20_000 });
    await shoot(page, "container");
    if ((await page.content()).includes(SECRET)) fail("a value of the container's environment reached the page");

    step("containers: on a phone it stacks, and nothing runs off the screen");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${origin}${link}`);
    const stacked = page.getByRole("link", { name: new RegExp(NAME) }).first();
    await stacked.waitFor({ timeout: 20_000 });
    const wide = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    if (wide > 0) fail(`the page runs ${wide} px off a phone's screen`);
    await shoot(page, "containers-phone");
    await stacked.click();
    await page.getByRole("log").getByText(SAYS).waitFor({ timeout: 20_000 });
    await shoot(page, "container-phone");
    await page.setViewportSize({ width: 1280, height: 900 });

    step("containers: nothing was touched by being looked at");
    const after = await dockerStarted(socket, id);
    if (after !== before) fail(`looking at it changed it: ${before}, then ${after}`);

    step("containers: taking it on is a plan, it doesn't restart, and it leaves the list for Apps");
    await page.goto(`${origin}${link}/containers/${NAME}`);
    const take = page.getByRole("region", { name: "Take it on" });
    await take.getByRole("textbox", { name: "Name as a service" }).fill("legacy");
    await take.getByRole("button", { name: "Plan taking it on" }).click();
    const sheet = page.getByRole("dialog");
    await sheet.getByText("takes on the container").first().waitFor({ timeout: 20_000 });
    await shoot(page, "take-on-plan");
    if ((await page.content()).includes(SECRET)) fail("a value of the container's environment reached the plan");
    await sheet.getByRole("button", { name: /Apply plan/ }).click();
    await sheet.locator(".plan-title-words", { hasText: "is taken on" }).waitFor({ timeout: 30_000 });
    await sheet
      .getByRole("button", { name: /^Open legacy|^Done/ })
      .first()
      .click();
    await page.getByRole("heading", { level: 1, name: "legacy", exact: true }).waitFor({ timeout: 30_000 });
    const taken = await dockerStarted(socket, id);
    if (taken !== before) fail(`taking it on changed it: ${before}, then ${taken}`);
    await page.goto(`${origin}${link}`);
    await page.getByRole("region", { name: PANEL }).getByText("Dockwell runs").waitFor({ timeout: 20_000 });
    if (await page.getByRole("region", { name: PANEL }).getByRole("link", { name: NAME }).count()) {
      fail(`${NAME} is still among what Dockwell didn't make once it was taken on`);
    }
  } finally {
    await dockerRemove(socket, id, "legacy-data");
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = {
  order: 70,
  run: (page, run) => containersFlow(page, { ...run, image: "acme/legacy:prints" }),
};
