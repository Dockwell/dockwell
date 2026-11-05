// SPDX-License-Identifier: AGPL-3.0-only

// The plan sheet end to end, on the fake Docker Engine of scripts/fakedocker,
// which can fail each way a first deploy really does. Every signature flow
// runs through this sheet, so it is measured here in a real browser: the shell
// holds still from a form to its plan, what can't be taken back leads, each
// failure reads in plain words, the keyboard goes all the way, contrast holds
// in both themes, and axe finds nothing.

import { mkdtempSync, readFileSync, symlinkSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Browser, BrowserContext, Page } from "playwright-core";

import type { DockerProbe } from "./docker-probe.ts";

export interface PlanFlowOptions {
  origin: string;
  browser: Browser;
  context: BrowserContext;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
  /** What Docker is really running, on the fake engine. */
  docker: DockerProbe;
  /** Stops a container on the fake engine, as someone at the server would. */
  stopByHand: (id: string) => Promise<void>;
}

const axeSource = readFileSync(createRequire(import.meta.url).resolve("axe-core/axe.min.js"), "utf8");

/** Long enough for a view's fade to end: measuring mid-way measures it fading. */
const FADES_MS = 250;

function fail(message: string): never {
  throw new Error(message);
}

/** The text a person can see in the sheet now. */
async function sheetText(page: Page): Promise<string> {
  const text = await page.locator(".dw-sheet-view:not(.is-away)").last().innerText();
  return text.replace(/\s+/g, " ");
}

/**
 * Measures every piece of text in the sheet against what is behind it, as
 * composited, opacity included, and every failure of WCAG 2.2 AA: 4.5:1, or 3:1
 * for large text. A disabled control is exempt, as WCAG exempts it.
 */
async function expectContrast(page: Page, what: string, root = '[role="dialog"]') {
  await page.waitForTimeout(FADES_MS);
  const low = await page.evaluate((selector) => {
    const paint = document.createElement("canvas").getContext("2d", { willReadFrequently: true });
    if (!paint) return ["no canvas to measure with"];
    const rgba = (colour: string): [number, number, number, number] => {
      paint.clearRect(0, 0, 1, 1);
      paint.fillStyle = colour;
      paint.fillRect(0, 0, 1, 1);
      const [r = 0, g = 0, b = 0, a = 0] = paint.getImageData(0, 0, 1, 1).data;
      return [r, g, b, a / 255];
    };
    type RGB = [number, number, number];
    const over = (top: [number, number, number, number], under: RGB): RGB => [
      top[0] * top[3] + under[0] * (1 - top[3]),
      top[1] * top[3] + under[1] * (1 - top[3]),
      top[2] * top[3] + under[2] * (1 - top[3]),
    ];
    const channel = (c: number) => (c / 255 <= 0.03928 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4);
    const luminance = ([r, g, b]: RGB) => 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
    const contrast = (a: RGB, b: RGB) => {
      const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
      return (hi + 0.05) / (lo + 0.05);
    };
    const ground = (el: Element | null): RGB => {
      const layers: [number, number, number, number][] = [];
      for (let at = el; at; at = at.parentElement) {
        const c = rgba(getComputedStyle(at).backgroundColor);
        if (c[3] > 0) layers.push(c);
        if (c[3] >= 1) break;
      }
      return layers.reverse().reduce<RGB>((under, top) => over(top, under), [255, 255, 255]);
    };
    const opacity = (el: Element | null) => {
      let out = 1;
      for (let at = el; at; at = at.parentElement) out *= Number(getComputedStyle(at).opacity);
      return out;
    };
    const out: string[] = [];
    const scope = document.querySelector(selector) ?? document.body;
    const walker = document.createTreeWalker(scope, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const el = node.parentElement;
      const text = node.textContent?.trim();
      if (!text || !el || el.closest(".dw-sr, [disabled], [aria-disabled='true'], [inert]")) continue;
      const style = getComputedStyle(el);
      if (style.visibility === "hidden" || style.display === "none" || el.getBoundingClientRect().width <= 1) continue;
      const colour = rgba(style.color);
      const under = ground(el);
      const ratio = contrast(over([colour[0], colour[1], colour[2], colour[3] * opacity(el)], under), under);
      const size = Number.parseFloat(style.fontSize);
      const large = size >= 24 || (size >= 18.66 && Number.parseInt(style.fontWeight, 10) >= 700);
      if (ratio < (large ? 3 : 4.5)) out.push(`${ratio.toFixed(2)}:1 "${text.slice(0, 40)}" ${style.color}`);
    }
    return out;
  }, root);
  if (low.length > 0) fail(`${what}: under WCAG AA: ${low.join("; ")}`);
}

/** Runs axe over the page in the browser, where it can measure colours too. */
async function expectAxe(page: Page, what: string) {
  await page.waitForTimeout(FADES_MS);
  // Not a script tag, which the panel's content security policy rightly refuses.
  await page.evaluate(axeSource);
  const violations = await page.evaluate(async () => {
    const axe = (
      window as unknown as {
        axe: { run(root: Element): Promise<{ violations: { id: string; nodes: { html: string }[] }[] }> };
      }
    ).axe;
    const root = document.querySelector('[role="dialog"]') ?? document.body;
    return (await axe.run(root)).violations.map(
      (v) => `${v.id}: ${v.nodes.map((n) => n.html.slice(0, 120)).join(" | ")}`,
    );
  });
  if (violations.length > 0) fail(`${what}: axe found: ${violations.join("\n  ")}`);
}

/** Opens New app with these filled in, and asks for the plan. */
async function reviewNew(
  page: Page,
  origin: string,
  image: string,
  name: string,
  more?: (form: ReturnType<Page["getByRole"]>) => Promise<void>,
) {
  await page.goto(`${origin}/apps/new`);
  const form = page.getByRole("dialog", { name: "New app" });
  await form.waitFor();
  await form.getByLabel("Image", { exact: true }).fill(image);
  await form.getByRole("textbox", { name: /^Name/ }).first().fill(name);
  await more?.(form);
  await form.getByRole("button", { name: /Review plan/ }).click();
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  return plan;
}

const addPort = (port: number) => async (form: ReturnType<Page["getByRole"]>) => {
  await form.getByRole("button", { name: "Add port" }).click();
  await form.getByLabel("On the server").fill(String(port));
  await form.getByLabel("In the app").fill("80");
};

/**
 * What ListDomains answers, in the wire format the built panel reads: one domain, waiting
 * on its certificate. The fake engine can't run the ingress, so this stands in for a route.
 */
function aPendingDomain(name: string, service: string, message: string): Buffer {
  const length = (n: number) => {
    const out: number[] = [];
    let left = n;
    for (; left > 127; left >>>= 7) out.push((left & 127) | 128);
    out.push(left);
    return Buffer.from(out);
  };
  const bytes = (field: number, value: Buffer | string) => {
    const body = Buffer.from(value);
    return Buffer.concat([Buffer.from([(field << 3) | 2]), length(body.length), body]);
  };
  const number = (field: number, value: number) => Buffer.from([field << 3, value]);
  const route = Buffer.concat([bytes(1, "rte_stub"), bytes(2, "stub"), bytes(4, service), number(5, 80)]);
  const certificate = Buffer.concat([number(1, 1), bytes(3, message)]);
  const status = Buffer.concat([bytes(1, name), bytes(2, certificate)]);
  const server = Buffer.concat([bytes(1, "srv_stub"), bytes(2, "stub"), bytes(3, status)]);
  return bytes(1, Buffer.concat([bytes(1, name), bytes(3, `https://${name}`), bytes(4, route), bytes(5, server)]));
}

/** Where the plan's first row is, to see whether it holds its place as the plan runs. */
async function planRowTop(page: Page): Promise<number> {
  return (await page.locator(".dw-sheet-layer:not(.is-leaving) .dw-plan-res").first().boundingBox())?.y ?? Number.NaN;
}

/**
 * The opacity of the sheet as it leaves, a frame at a time from just before the
 * trigger: -1 for a frame with no copy of it on the way out.
 */
async function framesOfLeaving(page: Page, trigger: () => Promise<void>): Promise<number[]> {
  await page.evaluate(() => {
    const frames: number[] = [];
    (window as unknown as { leaving: number[] }).leaving = frames;
    const look = () => {
      const copy = document.querySelector(".dw-sheet-layer.is-leaving .dw-sheet");
      frames.push(copy ? Number(getComputedStyle(copy).opacity) : -1);
      if (frames.length < 90) requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  });
  await trigger();
  await page.waitForFunction(() => (window as unknown as { leaving: number[] }).leaving.length >= 90);
  return page.evaluate(() => (window as unknown as { leaving: number[] }).leaving);
}

/** Watches, a frame at a time, for the scrim or the shell going away. */
async function watchShell(page: Page) {
  await page.evaluate(() => {
    const seen = {
      sheets: new Set<number>(),
      scrims: new Set<string>(),
      shell: document.querySelector(".dw-sheet"),
      lost: 0,
      on: true,
    };
    (window as unknown as { shellSeen: typeof seen }).shellSeen = seen;
    const look = () => {
      if (!seen.on) return;
      seen.sheets.add(document.querySelectorAll(".dw-sheet").length);
      const scrim = document.querySelector(".dw-scrim");
      seen.scrims.add(scrim ? getComputedStyle(scrim).opacity : "gone");
      if (document.querySelector(".dw-sheet") !== seen.shell) seen.lost += 1;
      requestAnimationFrame(look);
    };
    look();
  });
  return () =>
    page.evaluate(() => {
      const seen = (
        window as unknown as { shellSeen: { sheets: Set<number>; scrims: Set<string>; lost: number; on: boolean } }
      ).shellSeen;
      seen.on = false;
      return { sheets: [...seen.sheets], scrims: [...seen.scrims], lost: seen.lost };
    });
}

export async function planFlow(
  page: Page,
  { origin, browser, context, step, shoot, docker, stopByHand }: PlanFlowOptions,
) {
  const light = () => page.emulateMedia({ colorScheme: "light", reducedMotion: "no-preference" });
  const dark = () => page.emulateMedia({ colorScheme: "dark" });

  step("plan: the top bar's search and its shortcut hold their contrast, in both themes");
  await page.goto(`${origin}/apps`);
  await page.locator(".topbar-search").waitFor();
  await expectContrast(page, "the top bar, light", ".topbar");
  await dark();
  await expectContrast(page, "the top bar, dark", ".topbar");
  await light();

  step("plan: one shell and one scrim from the form to its plan and back, with the form as you left it");
  await page.goto(`${origin}/apps/new`);
  const form = page.getByRole("dialog", { name: "New app" });
  await form.waitFor();
  await page.waitForTimeout(350);
  await form.getByLabel("Image", { exact: true }).fill("nginx:1.27-alpine");
  await form.getByRole("textbox", { name: /^Name/ }).first().fill("first");
  const shell = await watchShell(page);
  await page.keyboard.press("Control+Enter");
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.waitFor({ timeout: 30_000 });
  await page.waitForTimeout(200);
  await shoot(page, "plan-plain");
  await page.keyboard.press("Escape");
  await form.waitFor();
  await page.waitForTimeout(200);
  const held = await shell();
  if (held.lost > 0 || held.sheets.join() !== "1" || held.scrims.join() !== "1") {
    fail(`the shell moved across the handover: ${JSON.stringify(held)}`);
  }
  if ((await form.getByRole("textbox", { name: /^Name/ }).first().inputValue()) !== "first")
    fail("the form lost what was typed");
  const back = await page.evaluate(() => (document.activeElement as HTMLInputElement | null)?.value);
  if (back !== "first") fail(`focus came back to ${JSON.stringify(back)}, not the field it left`);

  step("plan: a plan is a review: fields in words, no steps with one server, Undo in the footer alone");
  await page.keyboard.press("Control+Enter");
  await plan.waitFor({ timeout: 30_000 });
  const review = await sheetText(page);
  for (const debug of ["restart_policy", "ports[", "mounts[", "Steps by server", "one at a time", "Undo is a plan"]) {
    if (review.includes(debug)) fail(`the plan shows ${JSON.stringify(debug)}`);
  }
  if (!review.includes("no downtime expected")) fail("the plan doesn't say what goes down");
  if (
    (await plan
      .locator(".plan-lead")
      .first()
      .evaluate((el) => getComputedStyle(el).textAlign)) === "right"
  ) {
    fail("the impact is right-aligned");
  }
  await expectAxe(page, "a plan");
  await expectContrast(page, "a plan, light");
  await dark();
  await expectAxe(page, "a plan, dark");
  await expectContrast(page, "a plan, dark");
  await light();

  step("plan: Ctrl+Enter in the command bar leaves the plan behind it as it is");
  const beneath = page.url();
  await page.keyboard.press("Control+k");
  const bar = page.getByRole("combobox");
  await bar.waitFor();
  await page.keyboard.press("Control+Enter");
  await page.waitForTimeout(FADES_MS);
  if (!(await bar.isVisible())) fail("Ctrl+Enter in the command bar ran what it highlighted");
  if (page.url() !== beneath) fail(`Ctrl+Enter in the command bar moved the page to ${page.url()}`);
  if ((await docker()).some((c) => c.name.includes("first"))) fail("Ctrl+Enter in the command bar applied the plan");
  if (!(await page.getByRole("heading", { name: "Review plan", includeHidden: true }).count())) {
    fail("the plan left from under the bar");
  }
  await page.keyboard.press("Escape");
  await bar.waitFor({ state: "hidden" });

  step("plan: the keyboard goes all the way: Ctrl+Enter applies, focus never drops, and it ends on the way on");
  await page.keyboard.press("Control+Enter");
  await page.getByRole("heading", { name: "first is live" }).waitFor({ timeout: 30_000 });
  await page.waitForFunction(() => document.activeElement?.textContent?.startsWith("Open first"));
  await shoot(page, "plan-arrived");
  const arrived = await sheetText(page);
  if (!arrived.includes("1 created") || arrived.includes("to create"))
    fail(`the arrival isn't in the past tense: ${arrived}`);
  if (arrived.includes("no downtime")) fail("the arrival still promises no downtime");
  if (!arrived.includes("Nothing outside")) fail(`the arrival doesn't say nothing can reach it: ${arrived}`);
  await page.getByRole("button", { name: "Add a domain" }).waitFor();
  await page.getByRole("button", { name: "Add a port" }).waitFor();
  await expectAxe(page, "an arrival");
  await expectContrast(page, "an arrival, light");
  await dark();
  await expectContrast(page, "an arrival, dark");
  await light();
  await page.keyboard.press("Control+Enter");
  await page.getByRole("heading", { level: 1, name: "first", exact: true }).waitFor();

  step("plan: an app with a port arrives with its address as the first line, and nothing moves as it lands");
  const planned = await reviewNew(page, origin, "nginx:slow", "holder", addPort(8080));
  await planned.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "Applying" }).waitFor();
  await page
    .locator(".plan-step.is-running")
    .getByText(/Pulling nginx:slow/)
    .waitFor({ timeout: 10_000 });
  const counting = page.locator(".plan-step.is-running .plan-step-time");
  const started = await counting.innerText();
  if (!/^\d+ s$/.test(started)) fail(`the elapsed counter reads ${JSON.stringify(started)}`);
  const topWhileRunning = await planRowTop(page);
  if ((await page.locator(".plan-step.is-running").innerText()).includes("Created"))
    fail("the step says done while it runs");
  await expectContrast(page, "a plan applying, light");
  await shoot(page, "plan-applying");
  await page.getByRole("heading", { name: "holder is live" }).waitFor({ timeout: 30_000 });
  const address = page.locator(".plan-reach a");
  await address.waitFor();
  const topOnArrival = await planRowTop(page);
  if (Math.abs(topOnArrival - topWhileRunning) > 1) {
    fail(`the plan moved ${topOnArrival - topWhileRunning} px on arrival`);
  }
  if ((await page.locator(".plan-steps").count()) !== 0) fail("the arrival repeats the steps on one server");
  if (!/:8080$/.test(await address.innerText())) fail(`the address reads ${await address.innerText()}`);
  const first = await page.locator(".plan-body").evaluate((el) => el.firstElementChild?.className);
  if (first !== "plan-reach") fail(`the address isn't the first line: ${first}`);
  await shoot(page, "plan-arrived-address");
  await page.getByRole("button", { name: /Open holder/ }).click();
  await page.getByRole("heading", { level: 1, name: "holder", exact: true }).waitFor();

  step("plan: Docker's socket leads the plan as a risk, with its confirmation beside Apply");
  const risky = await reviewNew(page, origin, "ghcr.io/acme/web:2.4.1", "web", async (f) => {
    await f.getByRole("button", { name: "Add volume" }).click();
    await f.getByLabel("Volume or server path").fill("/var/run/docker.sock");
    await f.getByLabel("Path in the app").fill("/var/run/docker.sock");
  });
  const riskFirst = await risky
    .locator(".plan-body")
    .evaluate((el) => el.firstElementChild?.classList.contains("dw-tone-critical"));
  if (!riskFirst) fail("the risk isn't at the top of the plan");
  await risky.locator(".plan-risk").getByText("web gets root on").waitFor();
  const row = risky.locator(".dw-plan-label", { hasText: "Docker's socket" });
  if ((await row.count()) !== 1 || !(await row.evaluate((el) => el.classList.contains("is-critical")))) {
    fail("Docker's socket isn't marked critical in the diff");
  }
  if ((await risky.locator(".dw-diff-to", { hasText: "docker.sock" }).count()) !== 0)
    fail("Docker's socket reads green");
  const field = risky.locator(".dw-sheet-foot .plan-gate input");
  const apply = risky.locator(".dw-sheet-foot").getByRole("button", { name: /Apply plan/ });
  if (!(await apply.isDisabled())) fail("Apply works before the confirmation");
  const fieldBox = await field.boundingBox();
  const applyBox = await apply.boundingBox();
  if (!fieldBox || !applyBox || Math.abs(fieldBox.y + fieldBox.height - (applyBox.y + applyBox.height)) > 12) {
    fail(`the confirmation isn't beside Apply: ${JSON.stringify({ fieldBox, applyBox })}`);
  }
  await page.waitForFunction(() => document.activeElement?.tagName === "INPUT");
  await shoot(page, "plan-socket");
  await expectAxe(page, "a risky plan");
  await expectContrast(page, "a risky plan, light");
  await dark();
  await expectContrast(page, "a risky plan, dark");
  await light();
  const fingerprint = await risky.locator(".plan-gate code").first().innerText();
  await page.keyboard.type(fingerprint);
  await page.waitForFunction(() => !document.querySelector<HTMLButtonElement>(".plan-apply")?.disabled);
  await risky.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("dialog", { name: "New app" }).waitFor();

  step(
    "plan: each failure a person hits first reads in plain words, the chain only behind Details, and Edit and try again goes back",
  );
  for (const [image, name, words, more] of [
    ["nginx:nosuchtag", "tagged", "nginx:nosuchtag isn't on its registry"],
    ["acme/app:broken", "exits", "exits stopped right after it started"],
    ["acme/app:unhealthy", "sick", "sick runs, but fails its health check"],
    [
      "nginx:1.27-alpine",
      "mounted",
      "mounted can't mount /readonly/uploads",
      async (f: ReturnType<Page["getByRole"]>) => {
        await f.getByRole("button", { name: "Add volume" }).click();
        await f.getByLabel("Volume or server path").fill("/readonly/uploads");
        await f.getByLabel("Path in the app").fill("/uploads");
      },
    ],
  ] as const) {
    const failing = await reviewNew(page, origin, image, name, more);
    const gate = await failing.locator(".plan-gate code").count();
    if (gate > 0) await page.keyboard.type(await failing.locator(".plan-gate code").first().innerText());
    await page.keyboard.press("Control+Enter");
    await page.getByRole("heading", { name: `Couldn't create ${name}` }).waitFor({ timeout: 40_000 });
    await page.waitForFunction(() => document.activeElement?.textContent?.startsWith("Edit and try again"));
    const notice = page.locator(".plan-failure");
    const said = await notice.innerText();
    if (!said.includes(words)) fail(`${name} reads: ${said}`);
    if (/env_|chg_|-1\b.*docker logs/.test(said)) fail(`${name} leaks an ID: ${said}`);
    const text = await sheetText(page);
    if (text.split(words).length !== 2) fail(`${name} says it more than once: ${text}`);
    if (text.includes("to create")) fail(`${name} still has a plan to come: ${text}`);
    const details = notice.locator("details");
    await details.locator("summary").click();
    const raw = await details.locator("pre").innerText();
    if (!raw.includes("env_") && !raw.includes("can't")) fail(`${name}'s details read: ${raw}`);
    if (name === "tagged") {
      await page.setViewportSize({ width: 390, height: 844 });
      const summary = await details.locator("summary").boundingBox();
      if (!summary || summary.height < 44 || summary.width < 44) {
        fail(`Details is ${summary?.width} by ${summary?.height} px on a phone, too small for a thumb`);
      }
      await page.setViewportSize({ width: 1440, height: 900 });
      await shoot(page, "plan-failed");
      await expectAxe(page, "a failure");
      await expectContrast(page, "a failure, light");
      await dark();
      await expectContrast(page, "a failure, dark");
      await light();
    }
    await page.getByRole("button", { name: /Edit and try again/ }).click();
    const again = page.getByRole("dialog", { name: "New app" });
    await again.waitFor();
    if ((await again.getByLabel("Image", { exact: true }).inputValue()) !== image)
      fail(`${name} lost its image on the way back`);
  }

  step("plan: an image its registry won't give is refused before there is a plan, in plain words, on the form");
  await page.goto(`${origin}/apps/new`);
  const typo = page.getByRole("dialog", { name: "New app" });
  await typo.waitFor();
  await typo.getByLabel("Image", { exact: true }).fill("acme/doesnotexist:1.0");
  await typo.getByRole("textbox", { name: /^Name/ }).first().fill("typo");
  await typo.getByRole("button", { name: /Review plan/ }).click();
  await typo.getByText(/its registry won't give it to a pull/).waitFor({ timeout: 30_000 });
  if ((await page.getByRole("dialog", { name: "Review plan" }).count()) !== 0) fail("a typo got as far as a plan");

  step("plan: a port something else holds is refused before there is a plan, in plain words, on the form");
  await page.goto(`${origin}/apps/new`);
  const holding = page.getByRole("dialog", { name: "New app" });
  await holding.waitFor();
  await holding.getByLabel("Image", { exact: true }).fill("nginx:1.27-alpine");
  await holding.getByRole("textbox", { name: /^Name/ }).first().fill("second");
  await addPort(8080)(holding);
  await holding.getByRole("button", { name: /Review plan/ }).click();
  await holding
    .getByText(/port 8080/i)
    .first()
    .waitFor({ timeout: 30_000 });
  if ((await page.getByRole("dialog", { name: "Review plan" }).count()) !== 0) fail("a held port got as far as a plan");

  step("plan: after a failure, fixing the image and trying again takes the same name");
  const fixed = await reviewNew(page, origin, "nginx:nosuchtag", "fixme");
  await fixed.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "Couldn't create fixme" }).waitFor({ timeout: 40_000 });
  await page.getByRole("button", { name: /Edit and try again/ }).click();
  const again = page.getByRole("dialog", { name: "New app" });
  await again.getByLabel("Image", { exact: true }).fill("nginx:1.27-alpine");
  await again.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "fixme is live" }).waitFor({ timeout: 30_000 });

  step("plan: an undo is a plan in the same sheet, Cancel goes back to the arrival, and applying it reads the same");
  await page.getByRole("button", { name: /Undo until/ }).click();
  const undo = page.getByRole("dialog", { name: "Review plan" });
  await undo.waitFor();
  await undo.getByText("Undo: Run nginx:1.27-alpine as fixme").waitFor();
  await undo.getByText(/stops, and you can undo this until/).waitFor();
  await undo.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("heading", { name: "fixme is live" }).waitFor();
  await page.getByRole("button", { name: /Open fixme/ }).click();
  await page.getByRole("heading", { level: 1, name: "fixme", exact: true }).waitFor();

  step("plan: a failed deploy of a running app says the old version is running again, and it is");
  const deployFixme = async (image: string) => {
    await page.getByRole("button", { name: "Deploy", exact: true }).click();
    const form = page.getByRole("dialog", { name: "Deploy fixme" });
    await form.getByLabel("Image").fill(image);
    await form.getByRole("button", { name: /Review plan/ }).click();
    await plan.waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: /Apply plan/ }).click();
    await page.getByRole("heading", { name: `Couldn't deploy ${image}` }).waitFor({ timeout: 40_000 });
    return form;
  };
  const fixmeRuns = async (image: string, when: string) => {
    const running = (await docker()).filter((c) => c.service === "fixme");
    if (running.length !== 1 || running[0]?.image !== image || !running[0].running) {
      fail(`${when}, Docker has ${JSON.stringify(running)}`);
    }
  };
  await fixmeRuns("nginx:1.27-alpine", "before the deploy");
  await deployFixme("acme/app:broken");
  const rollback = page.locator(".plan-rollback");
  await rollback.getByText("fixme runs nginx:1.27-alpine again").waitFor({ timeout: 40_000 });
  await fixmeRuns("nginx:1.27-alpine", "once it says the old version is back");
  const brokenText = await sheetText(page);
  if (!brokenText.includes("so it was down while that happened")) fail(`the outage goes unsaid: ${brokenText}`);
  if (!brokenText.includes("fixme stopped right after it started, with exit code 1")) fail(`it reads: ${brokenText}`);
  // In what the form in front of you has: it has an image, and no command or variables.
  if (!brokenText.includes("Check the image and tag, then deploy again.")) fail(`its fix reads: ${brokenText}`);
  if (brokenText.includes("Check its command")) fail(`its fix can't be done from here: ${brokenText}`);
  if (brokenText.includes("docker logs")) fail(`it sends you to a container that is gone: ${brokenText}`);
  if (/isn't running|can't take traffic|What it would have done|rolled back the steps/.test(brokenText)) {
    fail(`it says what the rollback made false: ${brokenText}`);
  }
  if (!brokenText.includes("What it set out to do")) fail(`the plan's heading reads: ${brokenText}`);
  await shoot(page, "plan-rolled-back");
  await expectAxe(page, "a failure, rolled back");
  await expectContrast(page, "a failure, rolled back, light");
  await dark();
  await expectContrast(page, "a failure, rolled back, dark");
  await light();
  await page.getByRole("button", { name: /Edit and try again/ }).click();
  await page.getByRole("dialog", { name: "Deploy fixme" }).getByRole("button", { name: "Cancel" }).click();

  step("plan: a pull that fails before anything changed says nothing moved, and not that it runs");
  await deployFixme("nginx:nosuchtag");
  await rollback.getByText("fixme stays on nginx:1.27-alpine").waitFor({ timeout: 40_000 });
  const pullText = await sheetText(page);
  if (!pullText.includes("This stopped before it changed anything on the server.")) fail(`it reads: ${pullText}`);
  if (pullText.includes("was down")) fail(`it says there was an outage: ${pullText}`);
  await fixmeRuns("nginx:1.27-alpine", "once it says nothing moved");
  await page.getByRole("button", { name: /Edit and try again/ }).click();
  await page.getByRole("dialog", { name: "Deploy fixme" }).getByRole("button", { name: "Cancel" }).click();

  step("plan: a failure can be left from the sheet: Done goes, sliding away, and focus goes back to Deploy");
  await deployFixme("nginx:nosuchtag");
  await rollback.waitFor({ timeout: 40_000 });
  await page.waitForFunction(() => !document.querySelector(".plan-rollback.dw-tone-cobalt"));
  const exit = await framesOfLeaving(page, () => page.getByRole("button", { name: /^Done/ }).click());
  const moving = exit.filter((opacity) => opacity > 0 && opacity < 1);
  if (moving.length < 3) fail(`the sheet cuts out instead of leaving: ${exit.join(" ")}`);
  if (moving.some((opacity, at) => at > 0 && opacity > (moving[at - 1] ?? 1) + 0.01)) {
    fail(`the sheet comes back as it leaves: ${exit.join(" ")}`);
  }
  if (exit.at(-1) !== -1) fail(`the sheet's copy stays: ${exit.join(" ")}`);
  await page.waitForFunction(() => document.activeElement?.textContent?.trim().startsWith("Deploy"));
  if ((await page.getByRole("dialog").count()) !== 0) fail("Done leaves a dialog open");

  step("plan: a deploy reads as a deploy, and nothing moves from the review to the arrival");
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const moved = page.getByRole("dialog", { name: "Deploy fixme" });
  await moved.getByLabel("Image").fill("nginx:1.28-alpine");
  await moved.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("to update").waitFor();
  const reviewText = await sheetText(page);
  if (/replace/i.test(reviewText)) fail(`a deploy reads as a replace: ${reviewText}`);
  if (!/ down|rolling, with nothing stopped/.test(reviewText)) fail(`a deploy leaves out its downtime: ${reviewText}`);
  if (!/Rolls back to r\d+ if it fails/.test(reviewText)) fail(`a deploy leaves out its safety net: ${reviewText}`);
  const atReview = await planRowTop(page);
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "Deploying nginx:1.28-alpine" }).waitFor({ timeout: 10_000 });
  const whileApplying = await planRowTop(page);
  await page.getByRole("heading", { name: "fixme is live" }).waitFor({ timeout: 30_000 });
  const atArrival = await planRowTop(page);
  if (Math.abs(whileApplying - atReview) > 0.5 || Math.abs(atArrival - atReview) > 0.5) {
    fail(`the plan moves as it runs: ${atReview}, then ${whileApplying}, then ${atArrival}`);
  }
  await page.getByRole("button", { name: /^Done/ }).click();
  await page.waitForFunction(() => document.activeElement?.textContent?.trim().startsWith("Deploy"));

  step("plan: a domain's row has its room from the review on, and nothing moves as it arrives");
  const stubbed = /DomainService\/ListDomains/;
  await page.route(stubbed, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/proto",
      body: aPendingDomain("held.example.com", "fixme", "Waiting for the certificate authority to answer."),
    }),
  );
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const heldForm = page.getByRole("dialog", { name: "Deploy fixme" });
  await heldForm.getByLabel("Image").fill("nginx:1.27-alpine");
  await heldForm.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.locator(".plan-room-held .is-domain .plan-reach-how").waitFor({ state: "attached" });
  const heldAtReview = await planRowTop(page);
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "fixme is live" }).waitFor({ timeout: 30_000 });
  await page.locator(".plan-reach-row.is-domain", { hasText: "held.example.com" }).waitFor();
  const heldOnArrival = await planRowTop(page);
  if (Math.abs(heldOnArrival - heldAtReview) > 0.5) {
    fail(`the plan moves as a domain arrives: ${heldAtReview}, then ${heldOnArrival}`);
  }
  await page.getByRole("button", { name: /^Done/ }).click();
  await page.unroute(stubbed);
  await page.reload();
  await page.getByRole("heading", { level: 1, name: "fixme", exact: true }).waitFor();

  step("plan: a port reads as a port, and focus finds the page's heading when what opened it has gone");
  await page.getByRole("button", { name: "Add a port" }).first().click();
  const portForm = page.getByRole("dialog", { name: "Add a port to fixme" });
  await portForm.getByLabel(/On the server/).fill("9411");
  await portForm.getByLabel(/In the app/).fill("80");
  await portForm.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByText("Publish port", { exact: true }).waitFor();
  if (/replace/i.test(await sheetText(page))) fail(`a new port reads as a replace: ${await sheetText(page)}`);
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "Published port 9411" }).waitFor({ timeout: 30_000 });
  await page.getByRole("button", { name: /^Done/ }).click();
  await page.waitForFunction(() => {
    const at = document.activeElement;
    return at?.tagName === "H1" || at?.textContent?.trim().startsWith("Add a port");
  });

  step("plan: an ordinary folder warns in amber with nothing to type, and Docker's socket stays the alarm");
  const folder = await reviewNew(page, origin, "nginx:1.27-alpine", "folder", async (f) => {
    await f.getByRole("button", { name: "Add volume" }).click();
    await f.getByLabel("Volume or server path").fill("/srv/site/html");
    await f.getByLabel("Path in the app").fill("/usr/share/nginx/html");
  });
  const amber = folder.locator(".plan-risk");
  await amber.waitFor();
  if (!(await amber.evaluate((el) => el.classList.contains("dw-tone-warning")))) fail("an ordinary folder isn't amber");
  if ((await folder.locator(".plan-gate").count()) !== 0) fail("an ordinary folder asks for typing");
  const folderApply = folder.getByRole("button", { name: /Apply plan/ });
  if (await folderApply.isDisabled()) fail("an ordinary folder stops Apply");
  if (!(await folderApply.evaluate((el) => el.classList.contains("dw-btn-primary")))) {
    fail("an ordinary folder gets the danger button");
  }
  await expectAxe(page, "an ordinary folder");
  await expectContrast(page, "an ordinary folder, light");
  await dark();
  await expectContrast(page, "an ordinary folder, dark");
  await light();
  await shoot(page, "plan-folder");
  await folder.getByRole("button", { name: "Cancel" }).click();

  step("plan: a symlink to /etc reads red, with its confirmation, for where it leads");
  const conf = join(mkdtempSync(join(tmpdir(), "dockwell-plan-")), "conf");
  symlinkSync("/etc", conf);
  const linked = await reviewNew(page, origin, "nginx:1.27-alpine", "linked", async (f) => {
    await f.getByRole("button", { name: "Add volume" }).click();
    await f.getByLabel("Volume or server path").fill(conf);
    await f.getByLabel("Path in the app").fill("/conf");
  });
  const linkRisk = linked.getByRole("alert").filter({ hasText: "through a symlink" });
  await linkRisk.waitFor();
  if (!(await linkRisk.evaluate((el) => el.classList.contains("dw-tone-critical"))))
    fail("a symlink to /etc isn't red");
  if (!(await linkRisk.innerText()).includes("It leads to /etc")) fail("it doesn't say where the symlink leads");
  if ((await linked.locator(".plan-gate").count()) !== 1) fail("a symlink to /etc asks for no typing");
  await shoot(page, "plan-symlink");
  await linked.getByRole("button", { name: "Cancel" }).click();

  step("plan: a Compose file leads with its apps, folds the rest, and arrives under the name you gave it");
  await page.goto(`${origin}/apps/new`);
  const composing = page.getByRole("dialog", { name: "New app" });
  await composing.waitFor();
  await composing.getByRole("radio", { name: "Compose" }).click();
  await composing.getByLabel("Compose file").fill(`services:
  db:
    image: mariadb:11.4
    volumes: ["db-data:/var/lib/mysql"]
  web:
    image: nginx:1.27-alpine
    ports: ["8090:80"]
    volumes: ["wp-data:/data"]
volumes:
  db-data: {}
  wp-data: {}
`);
  await composing.getByRole("textbox", { name: /^Project name/ }).fill("journal");
  const composeReview = composing.getByRole("button", { name: /Review plan/ });
  await composeReview.click();
  await plan.waitFor({ timeout: 30_000 });
  const also = plan.getByRole("button", { name: /Also creates project journal\s*, a network and 2 volumes/ });
  await also.waitFor({ timeout: 10_000 }).catch(async () => fail(`the Compose plan reads: ${await sheetText(page)}`));
  const leading = await plan.locator(".dw-plan > .dw-plan-list > .dw-plan-res .dw-plan-name").first().innerText();
  if (!leading.startsWith("web")) fail(`the Compose plan leads with: ${leading}`);
  if ((await also.getAttribute("aria-expanded")) !== "false") fail("what comes along starts open");
  const composeText = await sheetText(page);
  if (composeText.includes("production")) fail(`a first Compose file shows its environment: ${composeText}`);
  await expectAxe(page, "a Compose plan");
  await expectContrast(page, "a Compose plan, light");
  await dark();
  await expectContrast(page, "a Compose plan, dark");
  await light();
  await shoot(page, "plan-compose");
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "journal is live" }).waitFor({ timeout: 40_000 });
  await plan.waitFor({ state: "detached" }).catch(() => {});
  await shoot(page, "plan-compose-arrived");
  // Closing it goes to the page's heading: what opened it was the page before.
  await page.getByRole("button", { name: "Close" }).click();
  await page.waitForFunction(() => document.activeElement?.tagName === "H1");
  if ((await page.getByRole("dialog").count()) !== 0) fail("× on an arrival leaves a dialog open");

  step("plan: a rollback reads like any plan, and leads with what it leaves as it is");
  const keeper = await reviewNew(page, origin, "nginx:1.27-alpine", "keeper", async (f) => {
    await f.getByRole("button", { name: "Add volume" }).click();
    await f.getByLabel("Volume or server path").fill("keeper-data");
    await f.getByLabel("Path in the app").fill("/data");
  });
  await keeper.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "keeper is live" }).waitFor({ timeout: 30_000 });
  await page.getByRole("button", { name: /Open keeper/ }).click();
  await page.getByRole("heading", { level: 1, name: "keeper", exact: true }).waitFor();
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const deploy = page.getByRole("dialog", { name: "Deploy keeper" });
  await deploy.getByLabel("Image").fill("nginx:1.28-alpine");
  await deploy.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "keeper is live" }).waitFor({ timeout: 30_000 });
  await page.getByRole("button", { name: /Done/ }).click();
  await page.getByRole("link", { name: /Deployments/ }).click();
  await page.getByRole("button", { name: "Roll back to r1" }).click();
  await plan.waitFor({ timeout: 30_000 });
  const note = plan.locator(".plan-body > .dw-callout");
  await note.waitFor();
  if (!(await note.evaluate((el) => el.classList.contains("dw-tone-warning"))))
    fail("the rollback's note isn't a warning");
  if (!(await note.innerText()).includes("keeper keeps what is in keeper-data as it is")) {
    fail(`the rollback's note reads: ${await note.innerText()}`);
  }
  if (await plan.getByRole("button", { name: /Apply plan/ }).isDisabled())
    fail("a note stops someone applying the plan");
  await expectAxe(page, "a rollback");
  await expectContrast(page, "a rollback, light");
  await shoot(page, "plan-rollback");
  // It reads as what it is for, and arrives as the brand book has it, with its note in the right tense.
  await plan.getByText("Roll back", { exact: true }).waitFor();
  if (/replace/i.test(await sheetText(page))) fail(`a rollback reads as a replace: ${await sheetText(page)}`);
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "Rolled back to r1" }).waitFor({ timeout: 30_000 });
  const rolledText = await sheetText(page);
  if (/is live|replaced|before you apply/i.test(rolledText)) fail(`a rollback arrives as: ${rolledText}`);
  await page.getByRole("button", { name: /^Done/ }).click();

  step("plan: deleting says it stops for good, with Delete all the way and the danger button");
  await page.goto(`${origin}/apps`);
  await page.getByRole("link", { name: "fixme", exact: true }).click();
  await page.getByRole("heading", { level: 1, name: "fixme", exact: true }).waitFor();
  await page.getByRole("button", { name: "Delete this app" }).click();
  const deleting = page.getByRole("dialog", { name: "Review plan" });
  await deleting.waitFor();
  const button = deleting.getByRole("button", { name: /Delete fixme/ });
  await button.waitFor();
  if (!(await button.evaluate((el) => el.classList.contains("dw-btn-danger")))) fail("Delete isn't the danger button");
  const deleteText = await sheetText(page);
  if (/no downtime|destroy/i.test(deleteText)) fail(`the delete plan reads: ${deleteText}`);
  if (!/fixme stops, and you can undo this until \d+ \w+\./.test(deleteText)) {
    fail(`the delete plan reads: ${deleteText}`);
  }
  if (deleteText.includes("doesn't come back")) fail(`the delete plan contradicts its undo: ${deleteText}`);
  if (!deleteText.includes("Undo for")) fail(`the delete plan doesn't offer its undo: ${deleteText}`);
  await shoot(page, "plan-delete");
  await button.click();
  await page.getByRole("heading", { name: "fixme is deleted" }).waitFor({ timeout: 30_000 });
  await page.getByRole("button", { name: /Done/ }).click();
  await page.getByRole("heading", { level: 1, name: "Apps", exact: true }).waitFor();

  step(
    "plan: a rollout says how it goes: rolling, what a drain ends, why one stops first, data that stays, what it starts",
  );
  const live = async (name: string, more?: (form: ReturnType<Page["getByRole"]>) => Promise<void>) => {
    const first = await reviewNew(page, origin, `acme/${name}:1`, name, more);
    await first.getByRole("button", { name: /Apply plan/ }).click();
    await page.getByRole("heading", { name: `${name} is live` }).waitFor({ timeout: 30_000 });
    await page.getByRole("button", { name: new RegExp(`^Open ${name}`) }).click();
    await page.getByRole("heading", { level: 1, name }).waitFor();
  };
  const deployNext = async (name: string) => {
    await page.getByRole("button", { name: "Deploy", exact: true }).click();
    const form = page.getByRole("dialog", { name: /^Deploy / });
    await form.getByLabel("Image").fill(`acme/${name}:2`);
    await form.getByRole("button", { name: /Review plan/ }).click();
    await plan.waitFor({ timeout: 30_000 });
    await plan.getByText("to update").waitFor();
    return sheetText(page);
  };
  const says = (what: string, text: string, words: string[]) => {
    for (const w of words) if (!text.includes(w)) fail(`${what} doesn't say "${w}": ${text}`);
    if (/env_|chg_/.test(text)) fail(`${what} leaks an ID: ${text}`);
  };
  await live("rolls");
  const rollingText = await deployNext("rolls");
  says("a rolling deploy", rollingText, [
    "rolling, with nothing stopped",
    "A new container takes over once it has been healthy for",
    "Dockwell rolls this back",
    "What is in flight gets up to",
    "ends with the old container",
  ]);
  await shoot(page, "plan-rolling");
  await expectAxe(page, "a rolling plan");
  await expectContrast(page, "a rolling plan");
  await plan.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("dialog", { name: "Deploy rolls" }).getByRole("button", { name: "Cancel" }).click();

  const rolls = (await docker()).find((c) => c.service === "rolls" && c.running);
  if (!rolls) fail("rolls isn't running to be stopped");
  await stopByHand(rolls.id);
  const startsText = await deployNext("rolls");
  says("a deploy of what someone stopped", startsText, [
    "rolls is stopped, so this starts it",
    "It has to stay healthy for",
    "Dockwell rolls this back",
  ]);
  for (const untrue of ["rolling, with nothing stopped", "goes on serving", "in flight"]) {
    if (startsText.includes(untrue)) fail(`a deploy of what someone stopped says "${untrue}": ${startsText}`);
  }
  await shoot(page, "plan-starts-stopped");
  await plan.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("dialog", { name: "Deploy rolls" }).getByRole("button", { name: "Cancel" }).click();

  await live("keeps", async (form) => {
    await form.getByRole("button", { name: "Add volume" }).click();
    await form.getByLabel("Volume or server path").fill("keeps-data");
    await form.getByLabel("Path in the app").fill("/data");
  });
  const stoppingText = await deployNext("keeps");
  says("a deploy that stops first", stoppingText, [
    " down",
    "Two containers mustn't write to the volume keeps-data at once, so the old one stops first",
    "If keeps is rolled back, keeps-data stays as it is",
  ]);
  await shoot(page, "plan-stops-first");
  await expectAxe(page, "a plan that stops first");
  await expectContrast(page, "a plan that stops first");
  await plan.getByRole("button", { name: "Cancel" }).click();
  await page.getByRole("dialog", { name: "Deploy keeps" }).getByRole("button", { name: "Cancel" }).click();

  await page.goto(`${origin}/apps/new`);
  const writing = page.getByRole("dialog", { name: "New app" });
  await writing.waitFor();
  await writing.getByRole("radio", { name: "Compose" }).click();
  await writing.getByLabel("Compose file").fill(`services:
  both:
    image: acme/both:1
    volumes: ["both-data:/data"]
    deploy:
      update_config:
        order: start-first
volumes:
  both-data: {}
`);
  await writing.getByRole("textbox", { name: /^Project name/ }).fill("writers");
  await writing.getByRole("button", { name: /Review plan/ }).click();
  await plan.waitFor({ timeout: 30_000 });
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("heading", { name: "both is live" }).waitFor({ timeout: 30_000 });
  await page.goto(`${origin}/apps`);
  // A file of one service reads as its project in the list.
  await page.getByRole("link", { name: "writers", exact: true }).click();
  await page.getByRole("button", { name: "Deploy", exact: true }).waitFor();
  const writersText = await deployNext("both");
  says("two writers", writersText, ["Two containers may write to both-data", "which can corrupt a database"]);
  await plan.locator(".plan-risk").waitFor();
  await shoot(page, "plan-two-writers");
  await expectAxe(page, "two writers");
  await expectContrast(page, "two writers");
  await plan.getByRole("button", { name: "Cancel" }).click();
  await page
    .getByRole("dialog", { name: /^Deploy / })
    .getByRole("button", { name: "Cancel" })
    .click();

  step("plan: on a phone, the actions are in reach of a thumb, and a confirmation sits with them");
  await page.setViewportSize({ width: 390, height: 844 });
  const phonePlan = await reviewNew(page, origin, "acme/phone:1", "phone", async (f) => {
    await f.getByRole("button", { name: "Add volume" }).click();
    await f.getByLabel("Volume or server path").fill("/var/run/docker.sock");
    await f.getByLabel("Path in the app").fill("/s");
  });
  const phoneApply = phonePlan.getByRole("button", { name: /Apply plan/ });
  const box = await phoneApply.boundingBox();
  if (!box || box.height < 44) fail(`Apply is ${box?.height} px tall on a phone`);
  if ((await phonePlan.locator(".dw-kbd").count()) !== 0) fail("a phone shows a key to press");
  const input = phonePlan.locator(".plan-gate input");
  const size = await input.evaluate((el) => Number.parseFloat(getComputedStyle(el).fontSize));
  if (size < 16) fail(`the confirmation is ${size} px on a phone, which zooms in as it takes focus`);
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (overflow > 0) fail(`a phone scrolls sideways by ${overflow} px`);
  await shoot(page, "plan-phone");
  await phonePlan.getByRole("button", { name: "Cancel" }).click();
  await page.setViewportSize({ width: 1440, height: 900 });

  step("plan: reads in Chinese, and passes axe there too");
  const chinese = await browser.newContext({
    storageState: await context.storageState(),
    locale: "zh-CN",
    viewport: { width: 1440, height: 900 },
  });
  try {
    const zh = await chinese.newPage();
    await zh.goto(`${origin}/apps/new`);
    const zhForm = zh.getByRole("dialog", { name: "新建应用" });
    await zhForm.waitFor();
    await zhForm.getByRole("textbox", { name: "镜像", exact: true }).fill("nginx:1.27-alpine");
    await zhForm.getByRole("textbox", { name: /^名称/ }).first().fill("zhong");
    await zhForm.getByRole("button", { name: /审查计划/ }).click();
    const zhPlan = zh.getByRole("dialog", { name: "审查计划" });
    await zhPlan.waitFor({ timeout: 30_000 });
    await zhPlan.getByText("预计不会停机", { exact: false }).waitFor();
    await expectAxe(zh, "a plan in Chinese");
    await expectContrast(zh, "a plan in Chinese");
    await shoot(zh, "plan-zh");
    await zhPlan.getByRole("button", { name: /应用计划/ }).click();
    await zh.getByRole("heading", { name: "zhong 已上线" }).waitFor({ timeout: 30_000 });
    await expectAxe(zh, "an arrival in Chinese");
    await zh.close();
  } finally {
    await chinese.close();
  }

  step("plan: under reduced motion, the handover doesn't animate");
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(`${origin}/apps/new`);
  const still = page.getByRole("dialog", { name: "New app" });
  await still.waitFor();
  await still.getByLabel("Image", { exact: true }).fill("nginx:1.27-alpine");
  await still.getByRole("textbox", { name: /^Name/ }).first().fill("still");
  await still.getByRole("button", { name: /Review plan/ }).click();
  await page.getByRole("dialog", { name: "Review plan" }).waitFor({ timeout: 30_000 });
  const motion = await page.evaluate(() =>
    Array.from(document.querySelectorAll(".dw-sheet-view, .plan-body")).map((el) => {
      const style = getComputedStyle(el);
      return [style.animationName, style.animationDuration, style.transitionDuration];
    }),
  );
  for (const [name, animation, transition] of motion) {
    if (name !== "none" && Number.parseFloat(animation ?? "0") > 0.01) fail(`it still animates: ${motion.join(" / ")}`);
    if (Number.parseFloat(transition ?? "0") > 0.01) fail(`it still transitions: ${motion.join(" / ")}`);
  }
  await page.getByRole("dialog", { name: "Review plan" }).getByRole("button", { name: "Cancel" }).click();
  const leftStill = await framesOfLeaving(page, async () => {
    await still.getByRole("button", { name: "Cancel" }).click();
  });
  if (leftStill.some((opacity) => opacity !== -1))
    fail(`under reduced motion it still slides away: ${leftStill.join(" ")}`);
  await page.emulateMedia({ reducedMotion: "no-preference" });
}
