// SPDX-License-Identifier: AGPL-3.0-only
// Servers, as Servers-List lays them out: a reader down the list who chooses a
// server stays where they are, and the server's inspector is in view beside the
// list, held under the top bar as the page scrolls.
import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { bytes, number } from "./wire.ts";

/** How far the page is scrolled, as the window scrolls it under the shell's sticky rail and top bar. */
const scrolled = (page: Page) => page.evaluate(() => (document.scrollingElement as HTMLElement).scrollTop);

/** Two frames, so what a choice changes has been laid out and drawn. */
const settled = (page: Page) =>
  page.evaluate(() => new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done()))));

export async function serversFlow(page: Page, { origin, step }: Pick<FlowRun, "origin" | "step">): Promise<void> {
  step("servers: choosing a server down the page leaves it where it is, with the server's inspector in view");
  const was = page.viewportSize();
  // Short enough that even one server's page scrolls, wide enough for the inspector beside the list.
  const height = 320;
  await page.setViewportSize({ width: 1280, height });
  try {
    await page.goto(`${origin}/servers?closed=true`);
    const row = page.getByRole("table", { name: "Servers, sorted by name" }).getByRole("row").nth(1);
    await row.waitFor();
    // Down the page, as a reader of a long list is, with the row on the screen, as a click needs it.
    await page.evaluate(() => {
      const el = document.scrollingElement as HTMLElement;
      el.scrollTop = el.scrollHeight;
    });
    await row.scrollIntoViewIfNeeded();
    const before = await scrolled(page);
    if (before < 40) throw new Error(`at ${height} px the page scrolls ${before} px, so this measures nothing`);
    await row.click();
    const inspector = page.getByRole("complementary");
    await inspector.waitFor();
    await settled(page);
    const after = await scrolled(page);
    if (Math.abs(after - before) > 2) throw new Error(`choosing a server moved the page from ${before} to ${after} px`);
    const box = await inspector.boundingBox();
    // Under the top bar and on the screen, as it holds there however far the page scrolls.
    const bar = await page.evaluate(() =>
      Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--scopebar-height")),
    );
    if (!box || box.y < bar - 1 || box.y >= height) {
      throw new Error(`the inspector isn't in view under the top bar: ${JSON.stringify(box)}, top bar ${bar} px`);
    }
    step(
      `  chosen at ${before} px down, the page stayed at ${after} px, with the inspector at ${Math.round(box.y)} px`,
    );
  } finally {
    if (was) await page.setViewportSize(was);
  }
  await manyServersFlow(page, { origin, step });
}

/** A Connect envelope for the layout fixture's server snapshot. */
function envelope(payload: Buffer, flags = 0): Buffer {
  const header = Buffer.alloc(5);
  header[0] = flags;
  header.writeUInt32BE(payload.length, 1);
  return Buffer.concat([header, payload]);
}

/** Long-list focus in the built panel, on an isolated tab with 60 pending servers. */
async function manyServersFlow(page: Page, { origin, step }: Pick<FlowRun, "origin" | "step">) {
  const probe = await page.context().newPage();
  // A layout-only snapshot, using Server's id, name and connection fields. The
  // separate tab keeps it out of the real-agent flow and the other panel flows.
  const snapshot = Buffer.concat([
    ...Array.from({ length: 60 }, (_, i) =>
      envelope(
        bytes(1, Buffer.concat([bytes(1, `focus_${i}`), bytes(2, `node-${String(i).padStart(2, "0")}`), number(4, 3)])),
      ),
    ),
    envelope(number(3, 1)),
    envelope(Buffer.from("{}"), 2),
  ]);
  try {
    await probe.route("**/dockwell.v1.ServerService/WatchServers", (route) =>
      route.fulfill({ contentType: "application/connect+proto", body: snapshot }),
    );
    for (const width of [1440, 1180]) {
      await probe.setViewportSize({ width, height: 900 });
      await probe.goto(`${origin}/servers?closed=true`);
      const tile = probe.getByRole("button", { name: /^node-53, / });
      await tile.click();
      const inspector = probe.getByRole("complementary", { name: "node-53" });
      await inspector.waitFor();
      const row = probe.getByRole("row", { name: /^node-53/ });
      await row.scrollIntoViewIfNeeded();
      await inspector.getByRole("button", { name: "Close node-53" }).click();
      await inspector.waitFor({ state: "detached" });
      await settled(probe);
      if ((await probe.locator(".servers-table tbody tr").count()) !== 60) {
        throw new Error(`closing a server past row 20 collapsed the list at ${width} px`);
      }
      if (!(await row.evaluate((el) => el === document.activeElement))) {
        throw new Error(`closing a server past row 20 lost keyboard focus at ${width} px`);
      }
    }
    step("servers: closing a server past row 20 keeps its row and returns keyboard focus, wide and wrapped");

    await probe.setViewportSize({ width: 390, height: 844 });
    await probe.goto(`${origin}/servers?closed=true`);
    const rows = probe.locator(".servers-phone-row");
    const chosen = rows.nth(40);
    await chosen.scrollIntoViewIfNeeded();
    await chosen.focus();
    await settled(probe);
    const before = await scrolled(probe);
    await probe.keyboard.press("Enter");
    await probe.getByRole("region", { name: "Chosen server" }).waitFor();
    await settled(probe);
    if (Math.abs((await scrolled(probe)) - before) > 2) throw new Error("a phone keyboard choice moved the page");
    for (let i = 0; i < 19; i++) {
      await probe.keyboard.press("Tab");
      await settled(probe);
      const box = await probe.evaluate(() => {
        const el = document.activeElement;
        const row = el?.getBoundingClientRect();
        const card = document.querySelector(".servers-phone-chosen")?.getBoundingClientRect();
        const tab = document.querySelector(".tabbar")?.getBoundingClientRect();
        const header = document.querySelector(".mobile-header")?.getBoundingClientRect();
        return {
          isRow: el?.classList.contains("servers-phone-row"),
          top: row?.top ?? -1,
          bottom: row?.bottom ?? -1,
          above: header?.bottom ?? 0,
          below: Math.min(card?.top ?? innerHeight, tab?.top ?? innerHeight),
        };
      });
      if (!box.isRow || box.top < box.above + 3 || box.bottom > box.below - 3) {
        throw new Error(`phone keyboard focus is obscured: ${JSON.stringify(box)}`);
      }
    }
    step("servers: phone keyboard focus stays above the chosen card and tab bar through the last row");
  } finally {
    await probe.close();
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts): after network, before settings. */
export const flow: Flow = { order: 82, run: (page, run) => serversFlow(page, run) };
