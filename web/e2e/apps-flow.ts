// SPDX-License-Identifier: AGPL-3.0-only

// The app screens end to end, in the panel: an app from an image goes through
// its plan to the list and its own page, gets a port, deploys another image,
// shows its revisions, rolls back as a plan, survives a deploy that fails, and
// exports. A Compose file is refused at the line; two files with a service of
// one name then run side by side, each in a project of its own. Every step
// that says what runs asks Docker too, rather than taking the API's word. make
// e2e runs it on the fake Docker Engine of scripts/fakedocker, and make
// docker-panel-e2e on real Docker, where publishing a port and reaching it
// means something.

import type { Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";

export interface AppsFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  /** Says what Docker itself runs. */
  docker: DockerProbe;
  /** The server port to publish; on real Docker it is reached too, and tells the images apart. */
  port?: number;
  /** Whether what is published can really be reached, as on real Docker. */
  reachable?: boolean;
  /** An image that starts and exits at once, for a deploy that fails. */
  failing: string;
}

const FIRST = "nginx:1.27-alpine";
const SECOND = "nginx:1.28-alpine";

function fail(message: string): never {
  throw new Error(message);
}

/** Reads the plan sheet's counts, applies it, and waits for it to end with the button that ends it. */
async function applyPlan(page: Page, done: string) {
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan|^Delete / }).click();
  const finish = page.getByRole("button", { name: done, exact: true });
  await finish.waitFor({ timeout: 120_000 });
  await finish.click();
}

/** Where the list's Status column starts, which nothing a row says should move. */
async function statusColumn(page: Page): Promise<number> {
  const head = await page.getByRole("columnheader", { name: "Status" }).boundingBox();
  return head?.x ?? fail("the list has no Status column");
}

export async function appsFlow(page: Page, { origin, step, shoot, docker, port, reachable, failing }: AppsFlowOptions) {
  /** The one container of a service, as Docker has it. */
  const running = async (service: string) => {
    const all = (await docker()).filter((c) => c.service === service);
    if (all.length !== 1) fail(`Docker has ${all.length} containers of ${service}: ${JSON.stringify(all)}`);
    return all[0] ?? fail(`Docker has no ${service}`);
  };
  /** Docker runs image for service, and it is running. */
  const expectDocker = async (service: string, image: string) => {
    const seen = await running(service);
    // A rollback pins the digest it goes back to, beside the tag.
    if (seen.image.split("@")[0] !== image || !seen.running) {
      fail(`Docker runs ${seen.image}, running ${seen.running}, for ${service}; want ${image} running`);
    }
  };
  /** nginx answers on the published port with the version it should have, which tells two images apart. */
  const expectServes = async (version: string) => {
    if (!reachable || !port) return;
    let got = "";
    for (let i = 0; i < 50; i++) {
      try {
        const res = await fetch(`http://127.0.0.1:${port}/`);
        got = res.headers.get("server") ?? "";
        if (res.ok && got.startsWith(version)) return;
      } catch (err) {
        got = String(err);
      }
      await new Promise((r) => setTimeout(r, 200));
    }
    fail(`port ${port} answers as "${got}", want ${version}`);
  };

  step("apps: the list invites the first app, and New app asks only for the image");
  await page.goto(`${origin}/apps`);
  await page.getByRole("heading", { name: "No apps yet" }).waitFor();
  await page.getByRole("link", { name: "New app" }).click();
  const sheet = page.getByRole("dialog", { name: "New app" });
  await sheet.waitFor();
  if ((await sheet.getByRole("group", { name: "Runs on" }).count()) > 0)
    fail("New app asks where to run with one server");
  await shoot(page, "apps-new");

  step(
    `apps: an image that won't run (${failing}) fails, Dockwell takes it away again, and the form comes back as you left it`,
  );
  await sheet.getByLabel("Image", { exact: true }).fill(failing);
  await sheet.getByLabel("Name").fill("doomed");
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  await page
    .getByRole("dialog", { name: "Review plan" })
    .getByRole("button", { name: /Apply plan/ })
    .click();
  const refused = page.getByRole("dialog", { name: "Couldn't create doomed" });
  await refused.waitFor({ timeout: 120_000 });
  await shoot(page, "apps-first-failed");
  await refused.getByRole("button", { name: /Edit and try again/ }).click();
  await sheet.waitFor();
  if ((await sheet.getByLabel("Image", { exact: true }).inputValue()) !== failing) fail("the form lost what you typed");
  // What went wrong is at the field, and what to change is ready to change.
  const refusedField = sheet.getByLabel("Image", { exact: true });
  await page.waitForFunction(() => document.activeElement?.getAttribute("aria-invalid") === "true");
  const selected = await refusedField.evaluate((el: HTMLInputElement) =>
    el.value.slice(el.selectionStart ?? 0, el.selectionEnd ?? 0),
  );
  if (selected !== failing) fail(`back on the form, "${selected}" is selected, want the whole image`);
  for (let i = 0; i < 100 && (await docker()).some((c) => c.running); i++) await page.waitForTimeout(100);
  if ((await docker()).some((c) => c.running)) fail(`Docker still runs what failed: ${JSON.stringify(await docker())}`);

  step(`apps: run ${FIRST}, review its plan, apply it with its progress in the sheet, and land on the app`);
  await sheet.getByLabel("Image", { exact: true }).fill(FIRST);
  await sheet.getByLabel("Name").fill("");
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await plan
    .locator(".dw-plan-summary")
    .getByText(/1 .*create/i)
    .waitFor();
  await shoot(page, "apps-plan");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("dialog", { name: /Applying|nginx is live/ }).waitFor();
  await page.getByRole("button", { name: "Open nginx" }).click({ timeout: 120_000 });
  await page.getByRole("heading", { level: 1, name: "nginx", exact: true }).waitFor();
  const panel = page.getByRole("region", { name: "Running" });
  await panel.getByText(FIRST).waitFor();
  await expectDocker("nginx", FIRST);
  await shoot(page, "apps-overview");

  step("apps: nothing reaches it yet, and adding a port is a plan that needs no secret read back");
  await panel.getByText(/Nothing outside the server reaches it yet/).waitFor();
  await panel.getByRole("button", { name: "Add a port" }).click();
  const adding = page.getByRole("dialog", { name: "Add a port to nginx" });
  await adding.getByLabel("On the server").fill(String(port ?? 18081));
  await adding.getByLabel("In the app").fill("80");
  await adding.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page, "Done");
  await panel.getByRole("link", { name: new RegExp(`Open .*:${port ?? 18081}`) }).waitFor({ timeout: 30_000 });
  await expectDocker("nginx", FIRST);
  await expectServes("nginx/1.27");
  await shoot(page, "apps-port");

  step("apps: it is on the list, with its image, revision and state");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Apps" }).click();
  const row = page.getByRole("row", { name: /nginx/ });
  await row.waitFor();
  for (const text of [FIRST, "Live", "r2"]) await row.getByText(text).first().waitFor();
  if ((await page.getByRole("columnheader", { name: "Runs on" }).count()) > 0)
    fail("the list has a server column with one server");
  const columnAt = await statusColumn(page);
  await shoot(page, "apps-list");

  step(`apps: deploy ${SECOND} from the app's page, as a plan`);
  await row.getByRole("link", { name: "nginx" }).click();
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const deploy = page.getByRole("dialog", { name: "Deploy nginx" });
  await deploy.getByLabel("Image").fill(SECOND);
  await deploy.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page, "Done");
  await expectDocker("nginx", SECOND);
  await expectServes("nginx/1.28");

  step(`apps: deploying ${SECOND} again pulls it, finds nothing newer, and says so`);
  const runningBefore = (await running("nginx")).id;
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  await deploy.getByText("again, and replaces nginx only if that's a newer image").waitFor();
  await deploy.getByRole("button", { name: /Review plan/ }).click();
  await page
    .getByRole("dialog", { name: "Review plan" })
    .getByRole("button", { name: /Apply plan/ })
    .click();
  // It doesn't read as a deploy: it says nothing changed, and why, with nothing to undo.
  await page.getByRole("heading", { name: "nginx stays as it is" }).waitFor({ timeout: 120_000 });
  await page.getByText(`${SECOND} is already the newest image, so nginx stays as it is`).waitFor();
  if ((await page.getByRole("button", { name: /Undo until/ }).count()) !== 0)
    fail("it offers to undo what didn't happen");
  if ((await running("nginx")).id !== runningBefore) fail("with nothing newer, the container was replaced");
  await page.getByRole("button", { name: /^Done/ }).click();

  step("apps: Deployments lists each revision, with what changed and who did it, in the past tense");
  await page.getByRole("link", { name: /Deployments/ }).click();
  const r3 = page.getByRole("row", { name: /r3/ });
  await r3.waitFor();
  await r3.getByText("Live").waitFor();
  await r3.getByText("Deployed").waitFor();
  await r3.getByText("Mira Chen").waitFor();
  await r3.getByText("From the panel").waitFor();
  await page.getByRole("row", { name: /r2/ }).getByText("Replaced").waitFor();
  await shoot(page, "apps-deployments");

  step("apps: one click rolls back to r2, which is a plan too");
  await page.getByRole("button", { name: "Roll back to r2" }).click();
  await applyPlan(page, "Done");
  const r4 = page.getByRole("row", { name: /r4/ });
  await r4.getByText("Live").waitFor({ timeout: 15_000 });
  await r4.getByText("Rolled back to r2").waitFor();
  await page.getByRole("row", { name: /r3/ }).getByText("Rolled back").waitFor();
  await page.getByRole("link", { name: "Overview", exact: true }).last().click();
  await panel.getByText(FIRST).waitFor();
  await expectDocker("nginx", FIRST);
  await expectServes("nginx/1.27");

  step(`apps: a deploy of ${failing} fails, Dockwell rolls it back on its own, and the page says so`);
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  await deploy.getByLabel("Image").fill(failing);
  await deploy.getByRole("button", { name: /Review plan/ }).click();
  await page
    .getByRole("dialog", { name: "Review plan" })
    .getByRole("button", { name: /Apply plan/ })
    .click();
  const broke = page.getByRole("dialog", { name: `Couldn't deploy ${failing}` });
  await broke.waitFor({ timeout: 120_000 });
  await shoot(page, "apps-deploy-failed");
  // The way back goes to the form it came from, with what went wrong at the field, ready to change.
  await broke.getByRole("button", { name: /Edit and try again/ }).click();
  const again = page.getByRole("dialog", { name: "Deploy nginx" });
  await again.waitFor();
  await page.waitForFunction(() => document.activeElement?.getAttribute("aria-invalid") === "true");
  await again.getByRole("button", { name: "Cancel" }).click();
  await page.getByText("The last deploy failed, so Dockwell rolled it back").waitFor({ timeout: 120_000 });
  await panel.getByText(FIRST).waitFor();
  await expectDocker("nginx", FIRST);
  await expectServes("nginx/1.27");
  await shoot(page, "apps-rolled-back");
  // What differs is what's offered: r4 and r2 are what it already runs.
  await page.getByRole("button", { name: "Roll back to r3" }).waitFor();
  for (const same of ["r2", "r4"]) {
    if ((await page.getByRole("button", { name: `Roll back to ${same}` }).count()) > 0)
      fail(`it offers to roll back to ${same}, which matches what runs`);
  }

  step("apps: on the narrowest phone nothing widens the page, the tabs fit, and the address reads whole");
  const wideAt = page.url();
  await page.setViewportSize({ width: 360, height: 780 });
  await page.goto(wideAt);
  await page.getByText("The last deploy failed, so Dockwell rolled it back").waitFor();
  const fits = await page.evaluate(() => {
    const at = document.querySelector(".app-reach-at");
    const tabs = document.querySelector(".dw-tabs");
    const callout = document.querySelector(".dw-callout code");
    const actions = document.querySelector(".dw-callout-actions");
    const body = document.querySelector(".dw-callout-body");
    return {
      page: document.documentElement.scrollWidth - window.innerWidth,
      address: at ? at.scrollWidth - at.clientWidth : -1,
      tabs: tabs ? tabs.scrollWidth - tabs.clientWidth : -1,
      // What the flow's short image can't show by itself: a long one would wrap,
      // and the actions sit under the text, so it keeps the width.
      callout: {
        wraps: callout ? getComputedStyle(callout).overflowWrap : "",
        stacked: (actions?.getBoundingClientRect().top ?? 0) >= (body?.getBoundingClientRect().bottom ?? 1),
      },
      offers: [...document.querySelectorAll(".app-actionbar .dw-btn")].map((b) => b.textContent),
    };
  });
  if (fits.page > 0) fail(`at 360 px the page is ${fits.page} px too wide`);
  if (fits.address !== 0) fail(`at 360 px the address is clipped by ${fits.address} px`);
  if (fits.tabs > 0) fail(`at 360 px the tabs overflow by ${fits.tabs} px`);
  if (fits.offers.join() !== "Deploy") fail(`the thumb bar offers ${fits.offers.join()}`);
  if (fits.callout.wraps !== "anywhere") fail(`a callout's image wraps as ${fits.callout.wraps}`);
  if (!fits.callout.stacked) fail("at 360 px a callout's actions don't sit under its text");
  await shoot(page, "apps-phone");

  step("apps: a domain behind a tunnel is one switch away, and fits the narrowest phone");
  await page.getByRole("button", { name: "Add a domain" }).first().click();
  const addDomain = page.getByRole("dialog", { name: /^Add a domain to / });
  await addDomain.getByLabel("Domain").fill("app.example.com");
  await addDomain.getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }).click();
  await addDomain.getByLabel("What's in front").fill("cloudflare");
  await addDomain.getByText("Point its DNS at your tunnel or proxy.").waitFor();
  const widens = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
  if (widens) fail("at 360 px the domain form widens the page");
  await shoot(page, "apps-domain-tunnel");
  // The server takes what the form asks, and the plan says what that means.
  await addDomain.getByRole("button", { name: /Review plan/ }).click();
  const tunnelPlan = page.getByRole("dialog", { name: "Review plan" });
  await tunnelPlan.waitFor({ timeout: 30_000 });
  await tunnelPlan.getByText(/reaches .+ unencrypted, as TLS ends before it/).waitFor();
  await shoot(page, "apps-domain-tunnel-plan");
  await tunnelPlan.getByRole("button", { name: "Cancel" }).click();
  // Cancelling comes back to what was typed.
  await addDomain.waitFor();
  if ((await addDomain.getByLabel("What's in front").inputValue()) !== "cloudflare")
    fail("cancelling the plan lost what's in front");
  await addDomain.getByRole("button", { name: "Cancel" }).click();
  // A long file's refusal is still on screen, above the sheet's footer.
  await page.goto(`${origin}/apps/new?source=compose`);
  const longFile = Array.from({ length: 40 }, (_, i) => `  svc${i}:\n    image: ${FIRST}`).join("\n");
  await page
    .getByRole("dialog", { name: "New app" })
    .getByLabel("Compose file")
    .fill(`services:\n  front:\n    build: .\n${longFile}\n`);
  await page.locator(".dw-code-field .dw-field-error").waitFor();
  const seen = await page.evaluate(() => {
    const error = document.querySelector(".dw-code-field .dw-field-error")?.getBoundingClientRect();
    const foot = document.querySelector(".dw-sheet-foot")?.getBoundingClientRect();
    return { error: error?.bottom ?? 0, foot: foot?.top ?? 0 };
  });
  if (seen.error > seen.foot) fail(`at 360 px a Compose error ends at ${seen.error}, under the footer at ${seen.foot}`);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(wideAt);

  await page.getByRole("link", { name: /Deployments/ }).click();
  await page.getByRole("row", { name: /r5/ }).getByText("Failed").waitFor();
  await page.getByRole("row", { name: /r6/ }).getByText("Rolled back after a failed deploy").waitFor();
  await page.getByRole("row", { name: /r4/ }).click();
  await page.getByText(/^r4 matches the latest revision\./).waitFor();

  step("apps: on a touch screen, a field takes a finger and iOS has no reason to zoom");
  const touch = await page
    .context()
    .browser()
    ?.newContext({
      hasTouch: true,
      isMobile: true,
      viewport: { width: 390, height: 844 },
      storageState: await page.context().storageState(),
    });
  if (touch) {
    const finger = await touch.newPage();
    await finger.goto(`${origin}/apps/new`);
    const image = finger.getByRole("dialog", { name: "New app" }).getByLabel("Image", { exact: true });
    await image.waitFor();
    const fieldSize = await image.evaluate((el) => ({
      height: (el.closest(".dw-input") as HTMLElement).getBoundingClientRect().height,
      font: Number.parseFloat(getComputedStyle(el).fontSize),
    }));
    if (fieldSize.height < 44 || fieldSize.font < 16)
      fail(`on a touch screen, a field is ${JSON.stringify(fieldSize)}`);
    await touch.close();
  }

  step("apps: nothing a row says moves the list's columns");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Apps" }).click();
  // The page it came from has rows that say this too.
  await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor();
  await page.getByRole("row", { name: /nginx/ }).getByText("Rolled back").waitFor();
  const columnNow = await statusColumn(page);
  if (columnNow !== columnAt) fail(`"Rolled back" moved the Status column from ${columnAt} to ${columnNow}`);
  // A word that isn't as short as these would still leave them where they are.
  const layout = await page.locator(".apps-table table").evaluate((table) => getComputedStyle(table).tableLayout);
  if (layout !== "fixed") fail(`the list's columns follow their content: table-layout is ${layout}`);
  // A status word is ink, and its shape carries the tone.
  const colours = await page
    .getByRole("row", { name: /nginx/ })
    .locator(".dw-status")
    .evaluate((badge) => {
      const glyph = badge.querySelector(".dw-glyph");
      const ink = getComputedStyle(document.documentElement).getPropertyValue("--ink").trim();
      const probe = document.createElement("span");
      probe.style.color = ink;
      document.body.append(probe);
      const inkColour = getComputedStyle(probe).color;
      probe.remove();
      return { word: getComputedStyle(badge).color, glyph: glyph ? getComputedStyle(glyph).color : "", ink: inkColour };
    });
  if (colours.word !== colours.ink || colours.glyph === colours.ink) {
    fail(`a status badge's word is ${colours.word} and its shape ${colours.glyph}, with ink at ${colours.ink}`);
  }
  await page.getByRole("row", { name: /nginx/ }).getByRole("link", { name: "nginx" }).click();

  step("apps: export it as a Compose file, with no secrets in it");
  await page.getByRole("button", { name: "Export as Compose" }).click();
  const exported = page.getByRole("dialog", { name: "Export as Compose" });
  await exported.getByRole("region", { name: "Compose file" }).getByText(`image: ${FIRST}`).waitFor();
  await shoot(page, "apps-export");
  await page.keyboard.press("Escape");

  step("apps: a Compose file is refused at the line, and one with no name can't change what is there");
  await page.goto(`${origin}/apps/new?source=compose`);
  const compose = page.getByRole("dialog", { name: "New app" });
  const field = compose.getByLabel("Compose file");
  await field.fill(`services:\n  front:\n    image: ${FIRST}\n    sysctls: [a=b]\n  back:\n    image: ${SECOND}\n`);
  await compose.locator(".dw-field-error", { hasText: "Line 4: the service front uses sysctls" }).waitFor();
  await shoot(page, "apps-compose-refused");
  await field.fill(`services:\n  front:\n    image: ${FIRST}\n  back:\n    image: ${SECOND}\n`);
  await compose.getByRole("region", { name: "What this file makes" }).getByText("This makes 2 apps").waitFor();
  await compose.getByText("Name it to review the plan").waitFor();
  if (await compose.getByRole("button", { name: /Review plan/ }).isEnabled())
    fail("a file with no name can be planned into a project that has apps");

  step("apps: two files with a service of one name run side by side, each in a project of its own");
  for (const [name, image] of [
    ["blog", FIRST],
    ["wiki", SECOND],
  ] as const) {
    await page.goto(`${origin}/apps/new?source=compose`);
    const one = page.getByRole("dialog", { name: "New app" });
    await one
      .getByLabel("Compose file")
      .fill(`name: ${name}\nservices:\n  front:\n    image: ${image}\n  back:\n    image: ${image}\n`);
    await one.getByRole("region", { name: "What this file makes" }).getByText("This makes 2 apps").waitFor();
    await one.getByRole("button", { name: /Review plan/ }).click();
    await applyPlan(page, "Show the apps");
    await page
      .getByRole("row", { name: new RegExp(name) })
      .getByText("2 apps")
      .waitFor();
  }
  await shoot(page, "apps-projects");
  const fronts = (await docker()).filter((c) => c.service === "front");
  if (fronts.length !== 2 || fronts[0]?.project === fronts[1]?.project) {
    fail(`Docker has these fronts: ${JSON.stringify(fronts)}; want one in each of two projects`);
  }
  if (new Set(fronts.map((c) => c.image)).size !== 2 || fronts.some((c) => !c.running)) {
    fail(`the fronts should run side by side, one of each image: ${JSON.stringify(fronts)}`);
  }
  const [one, other] = fronts;
  if (one?.networks.some((n) => other?.networks.includes(n))) {
    fail(`the two files' fronts share a network, so they can reach each other: ${JSON.stringify(fronts)}`);
  }

  step("apps: deleting goes through a plan too, and the list follows");
  // A project left with one app is that app's row, under the project's name.
  for (const name of ["nginx", "front", "front", "blog", "wiki"]) {
    await page.goto(`${origin}/apps`);
    const links = page.getByRole("link", { name, exact: true });
    await links.first().waitFor();
    const before = await links.count();
    await links.first().click();
    await page.getByRole("button", { name: "Delete this app" }).click();
    await applyPlan(page, "Done");
    await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor();
    for (let i = 0; i < 100 && (await links.count()) >= before; i++) await page.waitForTimeout(100);
    if ((await links.count()) >= before) fail(`deleting ${name} left it on the list`);
  }
  const left = await docker();
  if (left.some((c) => c.running)) fail(`Docker still runs what was deleted: ${JSON.stringify(left)}`);
}
