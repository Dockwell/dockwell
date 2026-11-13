// SPDX-License-Identifier: AGPL-3.0-only

// An app from a folder end to end (ADR 0058): New app takes a folder on the server on
// a language's version, reads who owns it and what it holds, and a person approves the
// plan, which reaches the folder. Docker runs the version's pinned image with the
// folder at /app, as its owner. The app's page says what it runs on, and its version
// changes through a plan; the Websites page's runtimes list the app on its language;
// and deleting the app leaves its folder. On the fake engine of scripts/fakedocker the
// image runs at 127.0.0.1, where this listens on the app's port, so its check passes; on
// real Docker the app in the folder answers it. scripts/docker-e2e.sh runs one app of
// each language on real Docker.

import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:net";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";

import type { Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";

export interface RuntimesFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  /** What Docker itself says runs. */
  docker: DockerProbe;
}

function fail(message: string): never {
  throw new Error(message);
}

/** Listens on a free port of 127.0.0.1, taking connections and saying nothing, as an app's port a check knocks at. */
function listening(): Promise<{ server: Server; port: number }> {
  return new Promise((done, failed) => {
    const server = createServer((socket) => socket.end());
    server.on("error", failed);
    server.listen(0, "127.0.0.1", () => {
      const at = server.address();
      if (at === null || typeof at === "string") failed(new Error("the listener has no port"));
      else done({ server, port: at.port });
    });
  });
}

/** Reads the plan sheet, applies it, and waits for it to end with the button that ends it. */
async function applyPlan(page: Page, done: string) {
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan|^Delete / }).click();
  const finish = page.getByRole("button", { name: done, exact: true });
  await finish.waitFor({ timeout: 180_000 });
  await finish.click();
}

/** The most entries an agent's listing carries (internal/agent/files MaxEntries). */
const LISTING_BOUND = 5000;

export async function runtimesFlow(page: Page, { origin, step, shoot, docker }: RuntimesFlowOptions) {
  // In a busy folder, as a shared /tmp or a media library is: more entries than a listing
  // carries, each sorting before the app's, so only a lookup by its own path finds it (#588).
  const busy = mkdtempSync(join(tmpdir(), "dw-busy-"));
  for (let i = 0; i <= LISTING_BOUND; i++) writeFileSync(join(busy, `aa-${String(i).padStart(5, "0")}`), "");
  const folder = mkdtempSync(join(busy, "shop-"));
  writeFileSync(
    join(folder, "package.json"),
    '{"name": "shop", "version": "1.0.0", "private": true, "scripts": {"start": "node server.js"}}\n',
  );
  writeFileSync(
    join(folder, "server.js"),
    'require("http").createServer((_, res) => res.end("shop")).listen(Number(process.env.PORT));\n',
  );
  const { server, port } = await listening();
  const owner = `${process.getuid?.() ?? 1000}:${process.getgid?.() ?? 1000}`;
  const name = "storefront";
  try {
    step("runtimes: New app takes a folder, and reads who owns it, what it holds and what it runs on");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/apps/new`);
    const sheet = page.getByRole("dialog", { name: "New app" });
    await sheet.waitFor();
    await sheet.getByRole("radio", { name: "Folder" }).click();
    const runsOn = sheet.getByRole("group", { name: "Runs on" });
    if ((await runsOn.count()) > 0) await runsOn.getByRole("checkbox").first().check();
    // The folder is picked: here by its path, typed into the picker, as someone who knows it would.
    await sheet.getByRole("button", { name: /^(Choose a folder|Change the folder)/ }).click();
    await sheet.getByRole("textbox", { name: "Path", exact: true }).fill(folder);
    await sheet.getByRole("button", { name: "Done", exact: true }).click();
    await sheet.getByText("Holds package.json and 1 more").waitFor({ timeout: 15_000 });
    // Browsing the busy folder says it shows only some of it, and a name finds the app's.
    await sheet.getByRole("button", { name: `Change the folder, ${folder}` }).click();
    await sheet
      .getByRole("navigation", { name: "Looking in" })
      .getByRole("button", { name: basename(busy), exact: true })
      .click();
    await sheet
      .getByText(`Showing 5,000 of ${(LISTING_BOUND + 2).toLocaleString("en")}. Type a name to find one.`)
      .waitFor();
    await sheet.getByRole("searchbox", { name: `Find in ${busy}` }).fill("shop-");
    await sheet
      .getByRole("list", { name: `Folders in ${busy}` })
      .getByRole("button", { name: basename(folder), exact: true })
      .click();
    await sheet.getByRole("button", { name: "Done", exact: true }).click();
    if ((await sheet.getByRole("button", { name: `Change the folder, ${folder}` }).count()) !== 1) {
      fail("finding the folder by name in a busy folder didn't choose it");
    }
    await sheet.getByText("Holds package.json and 1 more").waitFor({ timeout: 15_000 });
    // Its files say what it runs on, so nobody picks a language: the newest Node.js, as nothing names a version.
    const runtime = sheet.getByText(/^Node\.js \d+$/);
    await runtime.waitFor({ timeout: 15_000 });
    await sheet.getByText("from package.json").waitFor();
    const newest = ((await runtime.textContent()) ?? "").replace("Node.js ", "");
    if (!/^\d+$/.test(newest)) fail(`New app starts Node.js on '${newest}'`);
    if ((await sheet.getByRole("radio", { name: "Node.js" }).count()) > 0)
      fail("New app asks for a language its folder already says");
    // How it starts and who it runs as are the language's and the folder owner's, under Advanced.
    await sheet.getByRole("button", { name: "Advanced" }).click();
    const runsAs = sheet.getByRole("textbox", { name: /^Runs as/ });
    if ((await runsAs.inputValue()) !== owner) fail(`New app runs it as '${await runsAs.inputValue()}', want ${owner}`);
    await sheet.getByRole("textbox", { name: "Name", exact: true }).fill(name);
    if ((await sheet.getByRole("button", { name: `Change the folder, ${folder}` }).count()) !== 1) {
      fail("naming the app moved the folder someone chose");
    }
    await sheet.getByRole("textbox", { name: /^Port(?!s)/ }).fill(String(port));
    await shoot(page, "app-new-folder");

    step("runtimes: its plan names the folder it reaches, and a person applies it");
    await sheet.getByRole("button", { name: /Review plan/ }).click();
    const plan = page.getByRole("dialog", { name: "Review plan" });
    await plan.waitFor({ timeout: 30_000 });
    await plan.getByText(folder).filter({ visible: true }).first().waitFor();
    await shoot(page, "app-plan-folder");
    await applyPlan(page, `Open ${name}`);
    await page.waitForURL(/\/apps\/svc_/);

    step("runtimes: Docker runs the version's pinned image, with the folder at /app");
    const ran = (await docker()).filter((c) => c.service === name);
    if (ran.length !== 1 || !ran[0]?.image.startsWith(`node:${newest}.`) || !ran[0].image.includes("@sha256:")) {
      fail(`Docker runs ${JSON.stringify(ran)} for ${name}, want node:${newest} pinned by digest`);
    }

    step("runtimes: the environment editor overrides a runtime default while keeping its other values");
    // A new app lands on its logs, as it starts: its page is the address without the tab.
    const overviewURL = new URL(page.url()).href.replace(/(\/apps\/svc_[^/?#]+).*$/, "$1");
    await page.goto(`${overviewURL}/environment`);
    const cache = page.getByRole("row", { name: /^NPM_CONFIG_CACHE/ });
    await cache.getByText("/tmp/.npm", { exact: true }).waitFor();
    await cache.getByText("Set by its runtime", { exact: true }).waitFor();
    const portRow = page.getByRole("row", { name: /^PORT/ });
    if ((await portRow.getByRole("button").count()) !== 0) fail("the environment editor offers to override PORT");
    await cache.getByRole("button", { name: "Change NPM_CONFIG_CACHE", exact: true }).click();
    const edit = page.getByRole("form", { name: "Change NPM_CONFIG_CACHE" });
    await edit.getByRole("textbox", { name: "New value of NPM_CONFIG_CACHE" }).fill("/app/.cache");
    await edit.getByRole("button", { name: "Change", exact: true }).click();
    await page
      .getByRole("region", { name: "Unapplied changes" })
      .getByRole("button", { name: /Review plan/ })
      .click();
    await applyPlan(page, "Done");
    await cache.getByText("Set on this app", { exact: true }).waitFor();
    const overridden = (await docker()).filter((c) => c.service === name && c.running);
    if (overridden.length !== 1 || overridden[0]?.image !== ran[0]?.image)
      fail("the environment edit changed the runtime image");
    for (const value of ["NPM_CONFIG_CACHE=/app/.cache", "HOME=/tmp", `PORT=${port}`]) {
      if (!overridden[0]?.env.includes(value)) fail(`the environment edit lost ${value}`);
    }
    await page.goto(overviewURL);

    step("runtimes: the app's page says what it runs on and its folder, and its version changes through a plan");
    await page.getByText(`Node.js ${newest} from`).first().waitFor({ timeout: 15_000 });
    const config = page.getByRole("region", { name: "Configuration" });
    await config.getByText(folder).waitFor();
    await config.getByText("npm start").waitFor();
    await shoot(page, "app-runtime");
    await page.getByRole("button", { name: "Change version" }).first().click();
    const change = page.getByRole("dialog", { name: `Change ${name}'s version` });
    await change.waitFor();
    const options = await change
      .getByRole("combobox")
      .locator("option")
      .evaluateAll((all) => all.map((o) => (o as HTMLOptionElement).value));
    const older = options.find((v) => v !== newest) ?? fail(`Node.js offers ${options.join(", ")} alone`);
    await change.getByRole("combobox").selectOption(older);
    await shoot(page, "app-runtime-version");
    await change.getByRole("button", { name: /Review plan/ }).click();
    await applyPlan(page, "Done");
    await page.getByText(`Node.js ${older} from`).first().waitFor({ timeout: 30_000 });
    const moved = (await docker()).filter((c) => c.service === name && c.running);
    if (moved.length !== 1 || !moved[0]?.image.startsWith(`node:${older}.`)) {
      fail(`after the change Docker runs ${JSON.stringify(moved)} for ${name}, want node:${older}`);
    }
    const appURL = page.url();

    step("runtimes: the Websites page's runtimes list Node.js with the app on it");
    await page.goto(`${origin}/websites`);
    const runtimes = page.getByRole("region", { name: "Runtimes" });
    const card = runtimes.locator("article", { has: page.getByRole("heading", { name: "Node.js" }) });
    await card.getByText(`1 app · ${name} ${older}`).waitFor({ timeout: 15_000 });
    await shoot(page, "websites-runtimes");

    step("runtimes: on a phone, the app's page says what it runs on");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(appURL);
    const phoneConfig = page.getByRole("region", { name: "Configuration" });
    await phoneConfig.getByText(`Node.js ${older}`).waitFor({ timeout: 15_000 });
    await phoneConfig.getByText(folder, { exact: true }).waitFor();
    await shoot(page, "apps-runtime-phone");
    await page.setViewportSize({ width: 1440, height: 900 });

    step("runtimes: deleting the app leaves its folder as it is");
    await page.goto(`${origin}/apps`);
    await page
      .getByRole("link", { name: new RegExp(`^${name}`) })
      .first()
      .click();
    await page
      .getByRole("navigation", { name: "App sections" })
      .getByRole("link", { name: "Settings", exact: true })
      .click();
    await page
      .getByRole("region", { name: "Danger zone" })
      .getByRole("button", { name: `Delete ${name}`, exact: true })
      .click();
    await applyPlan(page, "Done");
    await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor({ timeout: 30_000 });
    if ((await docker()).some((c) => c.service === name)) fail(`${name} is still in Docker after it was deleted`);
    step("  runtimes: a folder on Node.js, planned, approved, run, moved to another version and deleted");
  } finally {
    server.close();
    rmSync(busy, { recursive: true, force: true });
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts): after Websites, whose runtimes it lists. */
export const flow: Flow = { order: 66, run: (page, run) => runtimesFlow(page, run) };
