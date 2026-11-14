// SPDX-License-Identifier: AGPL-3.0-only

// How wide each kind of page grows, as Layout in design/system/README.md sets
// out. On a 2560 px screen every page sits centred right of the rail, with
// gutters the same on both sides: the catalog's grid grows past a detail
// page's 1360 px, Apps, a table of few columns, stays at 1360 px with its head
// and table sharing their edges, Settings' form stops at its narrower width,
// and an app's log view takes the whole column under a head and tabs that stay
// where its other tabs have them. The top bar is the same on every page. At
// 1440 px and below every kind fills the column, as the mocks draw it.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

export interface LayoutFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
}

interface Box {
  left: number;
  right: number;
  width: number;
}

interface Measure {
  main: Box;
  /** What the shell's main column holds: the page, or Settings with its menu beside the page. */
  frame: Box;
  page: Box;
  heading: Box;
  tabs?: Box;
  /** The page's actions beside its title, its table, and a log view, where it has them. */
  actions?: Box;
  table?: Box;
  logs?: Box;
  /** The top bar's scope, and its last control. */
  scope: Box;
  end: Box;
}

function fail(message: string): never {
  throw new Error(message);
}

/** Where the page sits in the column right of the rail, its parts, and where the top bar's contents sit. */
async function measure(page: Page): Promise<Measure> {
  return page.evaluate(() => {
    const box = (el: Element | null | undefined): Box | undefined => {
      if (!el) return undefined;
      const r = el.getBoundingClientRect();
      return { left: r.left, right: r.right, width: r.width };
    };
    const need = (b: Box | undefined, what: string): Box => {
      if (!b) throw new Error(`no ${what} on ${location.pathname}`);
      return b;
    };
    const main = document.querySelector("#main");
    const content = main?.querySelector(".page");
    const tabs = box(content?.querySelector(":scope > .dw-tabs"));
    const actions = box(content?.querySelector(".page-actions"));
    const table = box(content?.querySelector(".dw-table-wrap"));
    const logs = box(content?.querySelector("section.logs"));
    return {
      main: need(box(main), "main column"),
      frame: need(box(main?.firstElementChild), "page frame"),
      page: need(box(content), "page"),
      heading: need(box(content?.querySelector("h1")), "heading"),
      ...(tabs ? { tabs } : {}),
      ...(actions ? { actions } : {}),
      ...(table ? { table } : {}),
      ...(logs ? { logs } : {}),
      scope: need(box(document.querySelector(".topbar-scope .topbar-scope-icon")), "top bar scope"),
      end: need(box(document.querySelector(".topbar > :last-child")), "top bar control"),
    };
  });
}

/** Opens a path straight, as a link or a reload would, and waits for its page. */
async function open(page: Page, origin: string, path: string, waitFor?: string) {
  await page.goto(`${origin}${path}`);
  await page.locator("#main .page h1").first().waitFor();
  // The top bar names the server once the fleet has loaded.
  await page.locator(".topbar-scope").waitFor();
  if (waitFor) await page.locator(waitFor).first().waitFor();
  // Measured once fonts have loaded and two frames have laid the page out, not mid-layout.
  await page.evaluate(() =>
    document.fonts.ready.then(
      () => new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done()))),
    ),
  );
}

const near = (a: number, b: number, by = 2) => Math.abs(a - b) <= by;
const px = (n: number) => `${n.toFixed(1)} px`;

export async function layoutFlow(page: Page, { origin, step }: LayoutFlowOptions) {
  step("layout: each kind of page, centred at 2560 px, and filling the column at 1440 px and below");
  const services = await page.evaluate(
    async () => ((await (await fetch("/v1/services")).json()) as { services?: { id: string }[] }).services ?? [],
  );
  const app = services[0]?.id ?? fail("no app to measure a detail page and a log view on");
  const kinds: { kind: string; path: string; max: number; waitFor?: string }[] = [
    { kind: "wide", path: "/catalog", max: 1680, waitFor: ".catalog-grid" },
    { kind: "standard, a table of few columns", path: "/apps", max: 1360, waitFor: ".apps-section table" },
    { kind: "standard", path: `/apps/${app}`, max: 1360 },
    { kind: "narrow", path: "/settings/general", max: 1040 },
    { kind: "standard, with a log view", path: `/apps/${app}/logs`, max: 1360, waitFor: "section.logs" },
  ];
  const name = (path: string) => path.replace(app, "<app>");

  await page.setViewportSize({ width: 2560, height: 1000 });
  const seen = new Map<string, Measure>();
  for (const { kind, path, max, waitFor } of kinds) {
    await open(page, origin, path, waitFor);
    const m = await measure(page);
    seen.set(path, m);
    const left = m.frame.left - m.main.left;
    const right = m.main.right - m.frame.right;
    // Centred: the gutters either side are the same, to a pixel or two of rounding.
    if (!near(left, right))
      fail(`${kind} ${name(path)} at 2560 px isn't centred: ${px(left)} left, ${px(right)} right`);
    if (!m.logs && !near(m.page.width, Math.min(max, m.main.width), 1)) {
      fail(`${kind} ${name(path)} at 2560 px is ${px(m.page.width)} wide, not ${max}`);
    }
    // The top bar keeps one gutter on every page, whatever the page's kind.
    if (!near(m.scope.left - m.main.left, 24) || !near(m.main.right - m.end.right, 24)) {
      fail(
        `the top bar over ${name(path)} moved: its scope ${px(m.scope.left - m.main.left)} from the rail, its end ${px(m.main.right - m.end.right)} from the edge`,
      );
    }
    step(`  ${kind} ${name(path)}: ${Math.round(m.frame.width)} px, ${Math.round(left)} px either side`);
  }

  // A grid grows past a detail page's width. A table of few columns doesn't: its page's head and the table share
  // their two edges, New app over the table's right edge.
  const catalog = seen.get("/catalog") ?? fail("the catalog wasn't measured");
  if (catalog.page.width <= 1360)
    fail(`the catalog at 2560 px is ${px(catalog.page.width)}, no wider than a detail page`);
  const apps = seen.get("/apps") ?? fail("Apps wasn't measured");
  const [table, actions] = [apps.table, apps.actions];
  if (!table || !near(table.left, apps.page.left + 24, 1) || !near(table.right, apps.page.right - 24, 1)) {
    fail(`Apps' table at 2560 px doesn't span its page: ${px(table?.left ?? 0)} to ${px(table?.right ?? 0)}`);
  }
  if (!actions || !near(actions.right, table.right, 1)) {
    fail(`New app ends at ${px(actions?.right ?? 0)}, not over the table's right edge at ${px(table.right)}`);
  }
  step(`  Apps' head and table share their edges, ${Math.round(table.width)} px apart`);

  // An app's head and tabs stay put when its Logs tab opens; only the log view takes the whole column.
  const overview = seen.get(`/apps/${app}`) ?? fail("the app wasn't measured");
  const logs = seen.get(`/apps/${app}/logs`) ?? fail("the app's logs weren't measured");
  if (!near(overview.heading.left, logs.heading.left, 1)) {
    fail(`an app's title moves from ${px(overview.heading.left)} to ${px(logs.heading.left)} on its Logs tab`);
  }
  const [was, is] = [overview.tabs, logs.tabs];
  if (!was || !is || !near(was.left, is.left, 1) || !near(was.right, is.right, 1)) {
    const edges = (b: Box | undefined) => (b ? `${px(b.left)} to ${px(b.right)}` : "nowhere");
    fail(`an app's tabs move on its Logs tab: ${edges(was)} on its overview, ${edges(is)} on Logs`);
  }
  if (!logs.logs || !near(logs.logs.width, logs.main.width - 48, 2)) {
    fail(`the log view is ${px(logs.logs?.width ?? 0)} wide in a ${px(logs.main.width)} column, not the whole of it`);
  }
  step(`  an app's head and tabs stay put on Logs, and its log view takes ${Math.round(logs.logs.width)} px`);

  for (const width of [1440, 1280]) {
    await page.setViewportSize({ width, height: 900 });
    for (const { kind, path, waitFor } of kinds) {
      await open(page, origin, path, waitFor);
      const m = await measure(page);
      if (!near(m.frame.width, m.main.width, 1)) {
        fail(`${kind} ${name(path)} at ${width} px is ${px(m.frame.width)} wide in a ${px(m.main.width)} column`);
      }
      if (m.logs && !near(m.heading.left, m.page.left + 24, 1)) fail(`an app's title on Logs at ${width} px moved`);
    }
    step(`  at ${width} px every kind fills the ${Math.round((await measure(page)).main.width)} px column`);
  }

  // On a phone, a callout with two long actions keeps to the screen, each action on a row of its own where both
  // don't fit (#624). It is the UI kit's Callout as it renders, with no class of a screen's own to help it.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${origin}/`);
  await page.locator("#main .page").first().waitFor();
  const callout = await page.evaluate(() => {
    const at = document.querySelector("#main .page") ?? document.body;
    const make = (tag: string, className: string, text?: string) => {
      const el = document.createElement(tag);
      el.className = className;
      if (text) el.textContent = text;
      return el;
    };
    const button = (label: string) => {
      const b = make("button", "dw-btn dw-btn-secondary dw-btn-md");
      b.append(make("span", "dw-btn-label", label));
      return b;
    };
    const box = make("div", "dw-callout dw-tone-warning");
    const body = make("div", "dw-callout-body");
    body.append(make("div", "dw-callout-title", "3 servers can't read Docker"));
    const actions = make("div", "dw-callout-actions");
    actions.append(button("Copy the command that restarts Docker"), button("Inspect"));
    box.append(make("span", "dw-callout-glyph"), body, actions);
    at.prepend(box);
    const edge = box.getBoundingClientRect();
    const out = [...actions.children]
      .map((b) => b.getBoundingClientRect())
      .filter((r) => r.right > edge.right + 0.5 || r.left < edge.left - 0.5).length;
    const wide = document.documentElement.scrollWidth;
    box.remove();
    return { out, wide, screen: window.innerWidth };
  });
  if (callout.wide > callout.screen) {
    fail(`a callout with two long actions makes the page ${px(callout.wide)} wide on a ${px(callout.screen)} screen`);
  }
  if (callout.out > 0) fail(`${callout.out} of a callout's actions run past its edge at 390 px`);
  step("  at 390 px a callout with two long actions keeps to the screen");
  await page.setViewportSize({ width: 1440, height: 900 });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 85, run: (page, run) => layoutFlow(page, run) };
