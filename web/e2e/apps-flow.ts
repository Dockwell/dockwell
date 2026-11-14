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

import { request } from "node:http";

import type { Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";
import type { Flow } from "./flows.ts";

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
  /** Whether a certificate authority is there to ask, for a domain over HTTPS on a real engine. */
  certificates?: boolean;
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

/** What the ingress on this machine answers for a name, as something on its own network asks it. */
function throughIngress(name: string): Promise<string> {
  return new Promise((done) => {
    const req = request({ host: "127.0.0.1", port: 80, path: "/", headers: { Host: name }, timeout: 2000 }, (res) => {
      let body = "";
      res.on("data", (chunk: Buffer) => {
        body += chunk.toString();
      });
      res.on("end", () => done(`${res.headers["server"] ?? ""} ${body}`));
    });
    req.on("timeout", () => req.destroy());
    req.on("error", (err) => done(String(err)));
    req.end();
  });
}

/** Where the list's Status column starts, which nothing a row says should move. */
async function statusColumn(page: Page): Promise<number> {
  const head = await page.getByRole("columnheader", { name: "Status" }).boundingBox();
  return head?.x ?? fail("the list has no Status column");
}

export async function appsFlow(
  page: Page,
  { origin, step, shoot, docker, port, reachable, failing, certificates }: AppsFlowOptions,
) {
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

  step("apps: the list offers the ways to a first app, and a Docker image asks only for the image");
  await page.goto(`${origin}/apps`);
  await page.getByRole("heading", { name: "No apps yet" }).waitFor();
  await page
    .getByRole("list", { name: "Ways to run your first app" })
    .getByRole("link", { name: "A Docker image" })
    .click();
  const sheet = page.getByRole("dialog", { name: "New app" });
  await sheet.waitFor();
  // A Docker image opens New app on an image, where New app on its own opens on the catalog (#617).
  if (!(await sheet.getByRole("radio", { name: "Image" }).isChecked()))
    fail("A Docker image opens New app on something other than an image");
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
  await page.getByRole("dialog", { name: /Applying|nginx is (deployed|running)/ }).waitFor();
  await page.getByRole("button", { name: "Open nginx" }).waitFor({ timeout: 120_000 });
  await shoot(page, "apps-arrival-nothing-reaches-it");

  step("apps: nothing reaches it yet, so its arrival offers a port, and that opens the form on the app's page");
  await page
    .getByRole("dialog", { name: /^nginx is (deployed|running)$/ })
    .getByRole("button", { name: "Add a port" })
    .click();
  await page.getByRole("heading", { level: 1, name: "nginx", exact: true }).waitFor();
  const offered = page.getByRole("dialog", { name: "Add a port to nginx" });
  await offered.waitFor();
  await offered.getByRole("button", { name: "Cancel" }).click();
  await offered.waitFor({ state: "hidden" });
  const panel = page.getByRole("region", { name: "What runs" });
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
  // One line at this width, tagged or not: this machine's address decides which.
  const portCells = await page
    .locator(".app-reach-row", { has: page.getByRole("link", { name: new RegExp(`Open .*:${port ?? 18081}`) }) })
    .evaluate((row) => {
      const edge = row.getBoundingClientRect();
      return [...row.children].map((cell) => {
        const box = cell.getBoundingClientRect();
        return { top: box.top - edge.top, bottom: edge.bottom - box.bottom, right: edge.right - box.right };
      });
    });
  const inRow = portCells.every((cell) => cell.top >= 0 && cell.bottom >= 0 && cell.right >= 0);
  const sameLine = portCells.every((cell) => Math.abs(cell.top - (portCells[0]?.top ?? 0)) <= 8);
  if (portCells.length !== 4 || !inRow || !sameLine) fail(`a port's row breaks: ${JSON.stringify(portCells)}`);
  await expectDocker("nginx", FIRST);
  await expectServes("nginx/1.27");
  await shoot(page, "apps-port");

  step("apps: it is on the list, with its image, revision and state");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Apps" }).click();
  const row = page.getByRole("row", { name: /nginx/ });
  await row.waitFor();
  for (const text of [FIRST, "Running", "r2"]) await row.getByText(text).first().waitFor({ timeout: 40_000 });
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
  await r3.getByText("Deployed", { exact: true }).waitFor();
  if ((await r3.innerText()).includes("Live"))
    fail(`a revision says it's live, which its record can't: ${await r3.innerText()}`);
  await r3.getByText("Mira Chen").waitFor();
  await r3.getByText("From the panel").waitFor();
  await page.getByRole("row", { name: /r2/ }).getByText("Replaced").waitFor();
  await shoot(page, "apps-deployments");

  step("apps: one click rolls back to r2, which is a plan too");
  await page.getByRole("button", { name: "Roll back to r2" }).click();
  await applyPlan(page, "Done");
  const r4 = page.getByRole("row", { name: /r4/ });
  await r4.getByText("Deployed", { exact: true }).waitFor({ timeout: 15_000 });
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
      // Tabs that don't fit scroll within themselves, fading where more are, with the page's own in sight.
      tabs: tabs
        ? {
            over: tabs.scrollWidth - tabs.clientWidth,
            more: tabs.getAttribute("data-more") ?? "",
            current: (() => {
              const on = tabs.querySelector('[aria-current="page"]')?.getBoundingClientRect();
              const edge = tabs.getBoundingClientRect();
              return on !== undefined && on.left >= edge.left - 1 && on.right <= edge.right + 1;
            })(),
          }
        : undefined,
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
  if (!fits.tabs) fail("at 360 px there are no tabs");
  if (fits.tabs.over > 0 && fits.tabs.more === "")
    fail(`at 360 px the tabs overflow by ${fits.tabs.over} px, and nothing says so`);
  if (!fits.tabs.current) fail("at 360 px the tab of the page you're on is out of sight");
  if (fits.offers.join() !== "Logs,Restart,Deploy") fail(`the thumb bar offers ${fits.offers.join()}`);
  if (fits.callout.wraps !== "anywhere") fail(`a callout's image wraps as ${fits.callout.wraps}`);
  if (!fits.callout.stacked) fail("at 360 px a callout's actions don't sit under its text");
  await shoot(page, "apps-phone");

  step("apps: a domain behind a tunnel is one switch away, and fits the narrowest phone");
  // A screenshot above ends at another width, when it takes one.
  await page.setViewportSize({ width: 360, height: 780 });
  await page.getByRole("button", { name: "Add a domain" }).first().click();
  const addDomain = page.getByRole("dialog", { name: /^Add a domain to / });
  // What it says to do goes by the machine this runs on, and it always says something.
  await addDomain.locator(".domain-record").waitFor();
  await addDomain.getByLabel("Domain").fill("app.example.com");
  // HTTPS is on, said in a line: what's in front is one step away.
  await addDomain.getByRole("button", { name: "HTTPS", exact: true }).click();
  await addDomain.getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }).click();
  await addDomain.getByRole("radio", { name: /Cloudflare's proxy/ }).check();
  await addDomain.getByText(/crosses the open internet unencrypted/).waitFor();
  await addDomain.getByText("Point its DNS at your tunnel or proxy.").waitFor();
  // Big enough for a thumb, and its words turn it on and off too.
  const thumb = await addDomain.locator(".app-front-switch").boundingBox();
  if ((thumb?.height ?? 0) < 44) fail(`at 360 px the switch is ${thumb?.height} px tall, beside fields 44 tall`);
  await addDomain.getByText("This server takes no certificate", { exact: false }).click();
  await addDomain.getByRole("group", { name: "What's in front" }).waitFor({ state: "detached" });
  await addDomain.getByText("This server takes no certificate", { exact: false }).click();
  await addDomain.getByRole("group", { name: "What's in front" }).waitFor();
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
  if (!(await addDomain.getByRole("radio", { name: /Cloudflare's proxy/ }).isChecked()))
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

  // Only a real engine can run the ingress. It holds port 80 for as long as it runs.
  if (reachable) {
    step("apps: a domain behind a tunnel, applied, serves the app through Caddy, and the arrival says what it is");
    // Long enough to wrap on a phone, where it has to keep its icon and its copy button.
    const tunnelled = "a-tunnel-with-a-name-long-enough-to-wrap.e2e-dockwell.example.net";
    await page.getByRole("button", { name: "Add a domain" }).first().click();
    const form = page.getByRole("dialog", { name: /^Add a domain to / });
    await form.getByLabel("Domain").fill(tunnelled);
    await form.getByRole("button", { name: "HTTPS", exact: true }).click();
    await form.getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }).click();
    await form.getByRole("button", { name: /Review plan/ }).click();
    const review = page.getByRole("dialog", { name: "Review plan" });
    await review.waitFor({ timeout: 30_000 });
    // Nothing moves from the review until the row has settled, in any frame. The sheet slides in first.
    await page.waitForTimeout(400);
    const moved = page.evaluate(
      ({ settled, frames }) =>
        new Promise<number[]>((done) => {
          const tops = new Set<number>();
          let left = frames;
          const look = () => {
            const layer = document.querySelector(".dw-sheet-layer:not(.is-leaving)");
            const row = layer?.querySelector(".dw-plan-res");
            if (row) tops.add(Math.round(row.getBoundingClientRect().y));
            left -= 1;
            if (left === 0 || layer?.querySelector(".plan-reach")?.textContent?.includes(settled)) done([...tops]);
            else requestAnimationFrame(look);
          };
          look();
        }),
      { settled: "TLS ends in front", frames: 60 * 300 },
    );
    await review.getByRole("button", { name: /Apply plan/ }).click();
    const row = page.locator(".plan-reach-row", { hasText: tunnelled });
    await row.waitFor({ timeout: 180_000 });
    await row.getByText("TLS ends in front").waitFor({ timeout: 120_000 });
    await row.getByText(/point your tunnel's service at http:\/\/127\.0\.0\.1:80/).waitFor();
    const tops = await moved;
    if (Math.max(...tops) - Math.min(...tops) > 1) fail(`the plan moves as a domain arrives: ${tops.join(", ")}`);
    // Someone visiting comes in through what ends TLS, so that's where its link goes.
    const visit = await row.getByRole("link", { name: tunnelled }).getAttribute("href");
    if (visit !== `https://${tunnelled}`) fail(`a tunnel's link goes to ${visit}`);
    await page.setViewportSize({ width: 360, height: 844 });
    const nameBox = await row.locator(".plan-reach-at").boundingBox();
    for (const [what, part] of [
      ["icon", row.locator(":scope > .dw-icon")],
      ["copy button", row.getByRole("button")],
    ] as const) {
      const box = await part.boundingBox();
      if (!nameBox || !box || box.y >= nameBox.y + nameBox.height || nameBox.y >= box.y + box.height)
        fail(`on a phone, a long domain's ${what} isn't on a line of its name: ${JSON.stringify({ nameBox, box })}`);
    }
    if (await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth))
      fail("on a phone, an arrival with a long domain widens the page");
    await page.setViewportSize({ width: 1440, height: 900 });
    // The app's page behind it has read it too, and doesn't wait for its cache to run out.
    const onPage = page.locator(".app-reach-domain", { hasText: tunnelled });
    await onPage.waitFor({ timeout: 10_000 });
    // The real thing: what a tunnel on this server's own network would ask.
    let answered = "";
    for (let i = 0; i < 40 && !answered.includes("nginx"); i++) {
      answered = await throughIngress(tunnelled);
      if (!answered.includes("nginx")) await page.waitForTimeout(500);
    }
    if (!answered.includes("nginx")) fail(`Caddy doesn't send ${tunnelled} to the app: ${answered.slice(0, 200)}`);
    // Undoing it takes the route, and the ingress with it, so what follows has port 80 free.
    await page.getByRole("button", { name: /Undo until/ }).click();
    await applyPlan(page, "Done");
    await form.waitFor({ state: "hidden" });
    await onPage.waitFor({ state: "detached", timeout: 10_000 });
    await expectServes("nginx/1.27");

    if (certificates) {
      step("apps: a domain over HTTPS goes from pending to issued through the panel, and its row keeps its name");
      const secure = "secure.e2e-dockwell.example.net";
      await page.getByRole("button", { name: "Add a domain" }).first().click();
      await form.getByLabel("Domain").fill(secure);
      await form.getByRole("button", { name: /Review plan/ }).click();
      await review.waitFor({ timeout: 30_000 });
      await review.getByRole("button", { name: /Apply plan/ }).click();
      const secureRow = page.locator(".plan-reach-row", { hasText: secure });
      await secureRow.getByText(/Valid for \d+ days/).waitFor({ timeout: 180_000 });
      const opens = await secureRow.getByRole("link", { name: secure }).getAttribute("href");
      if (opens !== `https://${secure}`) fail(`a domain's link goes to ${opens}`);
      await page.getByRole("button", { name: "Done", exact: true }).click();
      // At desktop width, as the name was squeezed out of this row before.
      const secureOnPage = page.locator(".app-reach-domain", { hasText: secure });
      await secureOnPage.getByText(/Valid for \d+ days/).waitFor({ timeout: 10_000 });
      const named = await secureOnPage.locator(".app-reach-at").boundingBox();
      if ((named?.width ?? 0) < 200) fail(`at 1440 px a domain's name is ${named?.width} px wide`);
      step("apps: removing a live domain is a plan that says what a visitor finds, and it goes");
      await secureOnPage.getByRole("button", { name: `Remove ${secure}` }).click();
      await review.waitFor({ timeout: 30_000 });
      await review
        .getByText(new RegExp(`Anyone visiting ${secure.replaceAll(".", "\\.")} gets a refused connection`))
        .waitFor();
      if ((await review.innerText()).includes("no downtime expected"))
        fail("removing a live domain promises no downtime");
      await applyPlan(page, "Done");
      await secureOnPage.waitFor({ state: "detached", timeout: 10_000 });
      await expectServes("nginx/1.27");
    }

    step(
      "apps: an app on the server's own network is reached at its domain, on its page too, and deleting it takes both",
    );
    const hostPort = (port ?? 18081) + 2;
    const bare = "onhost.e2e-dockwell.example.net";
    const call = async (method: string, body: unknown) =>
      page.evaluate(
        async ({ method, body }) => {
          const res = await fetch(`/dockwell.v1.ChangeService/${method}`, {
            method: "POST",
            headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
            body: JSON.stringify(body),
          });
          return (await res.json()) as { change?: { id: string; state?: string; plan?: { fingerprint?: string } } };
        },
        { method, body },
      );
    const { change } = await call("ProposeChange", {
      summary: "Run onhost",
      edits: [
        {
          resource: { kind: "service", name: "onhost" },
          spec: {
            service: {
              app: {
                image: FIRST,
                hostNetwork: true,
                // nginx's image listens on port 80, which Caddy holds on this network.
                command: [
                  "sh",
                  "-c",
                  `sed -i -E '/\\[::\\]:80;/d; s/listen +80;/listen 127.0.0.1:${hostPort};/' /etc/nginx/conf.d/default.conf && exec nginx -g 'daemon off;'`,
                ],
              },
            },
          },
        },
      ],
    });
    if (!change?.id) fail("the server planned no change for an app on its own network");
    // The server's own network is as good as root, so this goes the way a person's
    // does, in the panel: the plan says why it asks, and a passkey confirms it.
    await page.goto(`${origin}/changes/${change?.id}`);
    const onHostPlan = page.getByRole("dialog", { name: "Review plan" });
    await onHostPlan.waitFor({ timeout: 30_000 });
    await onHostPlan
      .locator(".plan-risk")
      .getByText(/^onhost shares .+'s own network/)
      .waitFor();
    await onHostPlan.locator(".plan-gate").getByText("Applying this asks for your passkey").waitFor();
    await onHostPlan.locator(".plan-gate input").fill(await onHostPlan.locator(".plan-gate code").first().innerText());
    await onHostPlan.getByRole("button", { name: /Apply plan/ }).click();
    const onHostDone = page.getByRole("button", { name: "Done", exact: true });
    await onHostDone.waitFor({ timeout: 120_000 });
    await onHostDone.click();
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Apps" }).click();
    await page
      .getByRole("link", { name: /^onhost/ })
      .first()
      .click();
    await page.getByRole("heading", { level: 1, name: "onhost", exact: true }).waitFor();
    await page.getByRole("button", { name: "Add a domain" }).first().click();
    await form.getByLabel("Domain").fill(bare);
    const portOnServer = form.getByLabel("Port on the server");
    if ((await portOnServer.inputValue()) !== "")
      fail("the form offers a port on the server's own network that may not work");
    await portOnServer.fill(String(hostPort));
    await form.getByRole("button", { name: "HTTPS", exact: true }).click();
    await form.getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }).click();
    await form.getByRole("button", { name: /Review plan/ }).click();
    await applyPlan(page, "Done");
    await form.waitFor({ state: "hidden" });
    const hostRow = page.locator(".app-reach-domain", { hasText: bare });
    await hostRow.waitFor({ timeout: 30_000 });
    if ((await page.getByRole("region", { name: "What runs" }).innerText()).includes("Nothing outside"))
      fail("the page says nothing reaches an app whose domain it lists");
    let onHost = "";
    for (let i = 0; i < 40 && !onHost.includes("nginx"); i++) {
      onHost = await throughIngress(bare);
      if (!onHost.includes("nginx")) await page.waitForTimeout(500);
    }
    if (!onHost.includes("nginx")) fail(`Caddy doesn't send ${bare} to the app's port: ${onHost.slice(0, 200)}`);
    await page.reload();
    await hostRow.waitFor({ timeout: 30_000 });
    // Deleting is in Settings' danger zone, as the mock has it.
    await page
      .getByRole("navigation", { name: "App sections" })
      .getByRole("link", { name: "Settings", exact: true })
      .click();
    await page
      .getByRole("region", { name: "Danger zone" })
      .getByRole("button", { name: /^Delete / })
      .first()
      .click();
    await review.waitFor({ timeout: 30_000 });
    for (const goes of [bare, "onhost"]) await review.locator(".dw-plan-res", { hasText: goes }).first().waitFor();
    await applyPlan(page, "Done");
    await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor({ timeout: 30_000 });
    await page
      .getByRole("link", { name: /^nginx/ })
      .first()
      .click();
    await page.getByRole("heading", { level: 1, name: "nginx", exact: true }).waitFor();
  }

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
    await finger.goto(`${origin}/apps/new?source=image`);
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
    .locator(".apps-record-state")
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

  step("apps: its environment takes a variable and a secret as one plan, and its container gets both");
  await page.goto(`${origin}/apps`);
  await page.getByRole("link", { name: "nginx", exact: true }).first().click();
  await page.getByRole("heading", { level: 1, name: "nginx", exact: true }).waitFor();
  await page.getByRole("link", { name: "Environment", exact: true }).click();
  const envForm = page.getByRole("form", { name: "Add a variable" });
  const addValue = async (key: string, value: string, secret: boolean) => {
    await envForm.getByRole("textbox", { name: "Key" }).fill(key);
    await envForm.getByRole("textbox", { name: "Value" }).fill(value);
    if (secret) await envForm.getByRole("switch", { name: "Secret" }).click();
    await envForm.getByRole("button", { name: "Add" }).click();
  };
  await addValue("GREETING", "hello", false);
  await addValue("SIGNING_KEY", "e2e-signing-key", true);
  const unapplied = page.getByRole("region", { name: "Unapplied changes" });
  await unapplied.getByText("2 unapplied changes").waitFor();
  // A long value wraps in its cell: the table never widens, and no key breaks for it.
  await addValue("LONG_NOTE", "x".repeat(90).concat("-", "y".repeat(90), "/", "z".repeat(90)), false);
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: width > 600 ? 900 : 844 });
    // Wait for the responsive columns before measuring their layout.
    await page
      .getByRole("columnheader", { name: "Value", exact: true })
      .waitFor({ state: width < 640 ? "hidden" : "visible" });
    const fits = await page.evaluate(() => {
      const wrap = document.querySelector(".app-environment .dw-table-wrap");
      const keys = [...document.querySelectorAll(".app-environment .app-env-name")];
      return {
        table: wrap ? wrap.scrollWidth - wrap.clientWidth : -1,
        page: document.documentElement.scrollWidth - window.innerWidth,
        broken: keys.filter((k) => k.getBoundingClientRect().height > 24).map((k) => k.textContent),
      };
    });
    if (fits.table !== 0 || fits.page > 0)
      fail(`at ${width} px a long value widens the table by ${fits.table} px, the page by ${fits.page}`);
    if (fits.broken.length > 0) fail(`at ${width} px a long value breaks the keys ${fits.broken.join(", ")}`);
  }
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.getByRole("button", { name: "Revert LONG_NOTE" }).click();
  if ((await docker()).some((c) => c.env.some((v) => v.startsWith("GREETING="))))
    fail("an edit reached Docker before its plan");
  await shoot(page, "apps-environment-pending");
  await unapplied.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page, "Done");
  await unapplied.waitFor({ state: "detached" });
  await page
    .getByRole("row", { name: /GREETING/ })
    .getByText("hello")
    .waitFor();
  await page
    .getByRole("row", { name: /SIGNING_KEY/ })
    .getByRole("img", { name: "Hidden" })
    .waitFor();
  const given = (await docker()).find((c) => c.service === "nginx" && c.running)?.env ?? [];
  if (!given.includes("GREETING=hello") || !given.includes("SIGNING_KEY=e2e-signing-key")) {
    fail(`the container runs with ${JSON.stringify(given)}, want GREETING and SIGNING_KEY`);
  }
  await shoot(page, "apps-environment");

  step("apps: Variables and secrets lists what the app sets on itself beside what it shares, as its tab does");
  await page.getByRole("link", { name: "Variables and secrets", exact: true }).click();
  const ownRow = page.getByRole("row", { name: /^GREETING/ });
  await ownRow.getByText("hello").waitFor();
  await ownRow.getByText("Set on the app").waitFor();
  if (!(await ownRow.getByRole("link", { name: "nginx", exact: true }).isVisible()))
    fail("Variables and secrets doesn't link GREETING to nginx");
  await page
    .getByRole("row", { name: /^SIGNING_KEY/ })
    .getByRole("img", { name: "Hidden" })
    .waitFor();
  await shoot(page, "apps-variables-own");

  step("apps: Settings changes a limit as a plan, and its tab is in sight on a phone");
  await page.goto(`${origin}/apps`);
  await page.getByRole("link", { name: "nginx", exact: true }).first().click();
  await page.getByRole("heading", { level: 1, name: "nginx", exact: true }).waitFor();
  await page
    .getByRole("navigation", { name: "App sections" })
    .getByRole("link", { name: "Settings", exact: true })
    .click();
  await page.getByRole("button", { name: "Edit Resources" }).click();
  const limits = page.getByRole("form", { name: "Edit Resources" });
  // A limit is picked from what fits the server; any other amount is typed under Other, and checked.
  await limits.getByRole("combobox", { name: "Memory limit" }).selectOption("other");
  await limits.getByRole("textbox", { name: "Memory limit, typed" }).fill("lots");
  await limits.getByRole("button", { name: /Review plan/ }).click();
  await limits.getByText("Use an amount from 6M, such as 512M or 2G.").waitFor();
  await limits.getByRole("combobox", { name: "Memory limit" }).selectOption("256M");
  await shoot(page, "apps-settings-resources");
  await limits.getByRole("button", { name: /Review plan/ }).click();
  await applyPlan(page, "Done");
  await page.getByRole("region", { name: "Resources" }).getByText("256M").waitFor();
  await page.setViewportSize({ width: 360, height: 780 });
  await page.reload();
  await page.getByRole("region", { name: "Resources" }).waitFor();
  const settingsTab = await page.evaluate(() => {
    const tabs = document.querySelector(".dw-tabs")?.getBoundingClientRect();
    const on = document.querySelector('.dw-tabs [aria-current="page"]')?.getBoundingClientRect();
    return tabs && on
      ? {
          inSight: on.left >= tabs.left - 1 && on.right <= tabs.right + 1,
          wide: document.documentElement.scrollWidth - window.innerWidth,
        }
      : undefined;
  });
  if (!settingsTab?.inSight) fail("on a phone the Settings tab is out of sight on Settings");
  if ((settingsTab?.wide ?? 1) > 0) fail(`on a phone Settings is ${settingsTab?.wide} px too wide`);
  await shoot(page, "apps-settings-phone");
  await page.setViewportSize({ width: 1440, height: 900 });

  step("apps: deleting goes through a plan too, and the list follows");
  // A project left with one app is that app's row, under the project's name.
  for (const name of ["nginx", "front", "front", "blog", "wiki"]) {
    await page.goto(`${origin}/apps`);
    const links = page.getByRole("link", { name, exact: true });
    await links.first().waitFor();
    const before = await links.count();
    await links.first().click();
    // Deleting is in Settings' danger zone, as the mock has it.
    await page
      .getByRole("navigation", { name: "App sections" })
      .getByRole("link", { name: "Settings", exact: true })
      .click();
    // The app's own Delete comes first; one of several in a project offers the whole project after it (ADR 0074).
    await page
      .getByRole("region", { name: "Danger zone" })
      .getByRole("button", { name: /^Delete / })
      .first()
      .click();
    await applyPlan(page, "Done");
    await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor();
    for (let i = 0; i < 100 && (await links.count()) >= before; i++) await page.waitForTimeout(100);
    if ((await links.count()) >= before) fail(`deleting ${name} left it on the list`);
  }
  const left = await docker();
  if (left.some((c) => c.running)) fail(`Docker still runs what was deleted: ${JSON.stringify(left)}`);
  // The last app in each Compose project took its project with it (ADR 0074).
  const projects = await page.evaluate(async () => {
    const res = await fetch("/dockwell.v1.ResourceService/ListResources", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
      body: JSON.stringify({ kind: "project" }),
    });
    const body = (await res.json()) as { resources?: { resource: { name: string } }[] };
    return (body.resources ?? []).map((r) => r.resource.name);
  });
  for (const gone of ["blog", "wiki"]) {
    if (projects.includes(gone)) fail(`deleting the last app in ${gone} left the project: ${projects.join(", ")}`);
  }
  step("  the last app in each project took its project with it");
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 10, run: (page, run) => appsFlow(page, { ...run, failing: "nginx:broken" }) };
