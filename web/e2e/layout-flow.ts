// SPDX-License-Identifier: AGPL-3.0-only

// How wide each kind of page grows, as Layout in design/system/README.md sets
// out. On a 2560 px screen every page sits centred right of the rail, with
// gutters the same on both sides; a list grows past a detail page's 1360 px; a
// form stops at its narrower width; a log view takes the whole column; and the
// top bar's contents line up with the page's edges. At 1440 px and below every
// kind fills the column, as the mocks draw it.

import type { Page } from "playwright-core";

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
  page: Box;
  /** The page's padding on one side, which the top bar's contents line up with. */
  pad: number;
  /** The top bar's scope, and its last control, where the page has a top bar. */
  scope?: Box;
  end?: Box;
}

function fail(message: string): never {
  throw new Error(message);
}

/** Where the page sits in the column right of the rail, and where the top bar's contents sit above it. */
async function measure(page: Page): Promise<Measure> {
  return page.evaluate(() => {
    const box = (el: Element | null): Box | undefined => {
      if (!el) return undefined;
      const r = el.getBoundingClientRect();
      return { left: r.left, right: r.right, width: r.width };
    };
    const main = box(document.querySelector("#main"));
    const content = document.querySelector("#main .page");
    const page = box(content);
    if (!main || !content || !page) throw new Error("no page in the shell's main column");
    const scope = box(document.querySelector(".topbar-scope .topbar-scope-icon"));
    const end = box(document.querySelector(".topbar > :last-child"));
    return {
      main,
      page,
      pad: Number.parseFloat(getComputedStyle(content).paddingLeft),
      ...(scope ? { scope } : {}),
      ...(end ? { end } : {}),
    };
  });
}

/** Opens a path straight, as a link or a reload would, and waits for its page. */
async function open(page: Page, origin: string, path: string) {
  await page.goto(`${origin}${path}`);
  await page.locator("#main .page h1").first().waitFor();
  // The top bar names the server once the fleet has loaded.
  await page.locator(".topbar-scope").waitFor();
}

export async function layoutFlow(page: Page, { origin, step }: LayoutFlowOptions) {
  step("layout: each kind of page, centred at 2560 px, and filling the column at 1440 px and below");
  const services = await page.evaluate(
    async () => ((await (await fetch("/v1/services")).json()) as { services?: { id: string }[] }).services ?? [],
  );
  const app = services[0]?.id ?? fail("no app to measure a detail page and a log view on");
  const kinds = [
    { kind: "wide", path: "/apps", max: 1680 },
    { kind: "standard", path: `/apps/${app}`, max: 1360 },
    { kind: "narrow", path: "/profile", max: 1040 },
    { kind: "full", path: `/apps/${app}/logs`, max: Number.POSITIVE_INFINITY },
  ] as const;

  await page.setViewportSize({ width: 2560, height: 1000 });
  for (const { kind, path, max } of kinds) {
    await open(page, origin, path);
    const m = await measure(page);
    const left = m.page.left - m.main.left;
    const right = m.main.right - m.page.right;
    // Centred: the gutters either side of the page are the same, to a pixel or two of rounding.
    if (Math.abs(left - right) > 2) {
      fail(
        `${kind} page ${path} at 2560 px isn't centred: ${left.toFixed(1)} px on its left, ${right.toFixed(1)} on its right`,
      );
    }
    const wants = Math.min(max, m.main.width);
    if (Math.abs(m.page.width - wants) > 1) {
      fail(`${kind} page ${path} at 2560 px is ${m.page.width.toFixed(1)} px wide, not ${wants.toFixed(1)}`);
    }
    // The top bar's scope sits above the page's first column, and its last control ends at the page's right edge.
    if (m.scope && Math.abs(m.scope.left - (m.page.left + m.pad)) > 2) {
      fail(
        `the top bar's scope starts at ${m.scope.left.toFixed(1)} px over a ${kind} page that starts at ${(m.page.left + m.pad).toFixed(1)}`,
      );
    }
    if (m.end && Math.abs(m.end.right - (m.page.right - m.pad)) > 2) {
      fail(
        `the top bar's last control ends at ${m.end.right.toFixed(1)} px over a ${kind} page that ends at ${(m.page.right - m.pad).toFixed(1)}`,
      );
    }
    step(`  ${kind} ${path.replace(app, "<app>")}: ${Math.round(m.page.width)} px, ${Math.round(left)} px either side`);
  }

  // A wide list uses more than a detail page's width; a detail page doesn't.
  await open(page, origin, "/apps");
  const list = await measure(page);
  if (list.page.width <= 1360)
    fail(`the Apps list at 2560 px is ${list.page.width} px wide, no wider than a detail page`);

  for (const width of [1440, 1280]) {
    await page.setViewportSize({ width, height: 900 });
    for (const { kind, path } of kinds) {
      await open(page, origin, path);
      const m = await measure(page);
      if (Math.abs(m.page.width - m.main.width) > 1) {
        fail(
          `${kind} page ${path} at ${width} px is ${m.page.width.toFixed(1)} px wide in a ${m.main.width.toFixed(1)} px column`,
        );
      }
    }
    step(`  at ${width} px every kind fills the ${Math.round((await measure(page)).main.width)} px column`);
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}
