// SPDX-License-Identifier: AGPL-3.0-only

// Each person's own time zone on their profile (#834):
// - Profile's picker puts "This browser's" first with the resolved browser zone beside it.
// - Searching matches both city and UTC offset (e.g. "+09:00" and "tokyo").
// - Saving persists without asking for a passkey, updates times across screens (checked
//   against the exact ISO instant on Activity in both Asia/Tokyo and Pacific/Honolulu),
//   and starts new schedules in the chosen zone (checked in Scheduled Tasks).
// - Clearing back to "This browser's" saves cleanly and leaves the session tidy.
// - Captures Profile screenshots at 1440px and 390px with 0px horizontal overflow.

import { mkdirSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { atWidth, pastTheEdge } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

function clockInZone(iso: string, timeZone: string): string {
  return new Intl.DateTimeFormat("en-GB", {
    hour: "2-digit",
    minute: "2-digit",
    timeZone,
  }).format(new Date(iso));
}

export const flow: Flow = {
  order: 98.4,
  run: async (page, { origin, step, shoot }) => {
    const shotDir = process.env["DOCKWELL_PROFILE_TZ_SHOTS"];
    if (shotDir) mkdirSync(shotDir, { recursive: true });

    const capture = async (name: string) => {
      await shoot(page, name);
      if (!shotDir) return;
      const before = page.viewportSize();
      try {
        for (const width of [1440, 390] as const) {
          await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
          await page.evaluate(() => window.scrollTo(0, 0));
          await page.waitForTimeout(100);
          if (width === 390) {
            const overflow = await pastTheEdge(page);
            if (overflow.wider > 0) {
              fail(`profile ${name} overflows 390px viewport by ${overflow.wider}px: ${overflow.what}`);
            }
          }
          await page.screenshot({ path: join(shotDir, `${name}-${width}.png`), fullPage: true });
        }
      } finally {
        if (before) await atWidth(page, before);
      }
    };

    step("profile-timezone: open Profile and check This browser's default");
    await page.goto(`${origin}/profile`, { waitUntil: "domcontentloaded" });
    const account = page.getByRole("region", { name: "Account" });
    const trigger = account.getByRole("button", { name: /Time zone/i });
    await trigger.waitFor({ state: "visible" });

    const browserZone = await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC");
    const browserLabel = browserZone.replaceAll("_", " ");
    const initialText = (await trigger.textContent()) ?? "";
    if (!initialText.includes("This browser's") || !initialText.includes(browserLabel)) {
      fail(`expected Time zone trigger to show "This browser's" and "${browserLabel}", got "${initialText}"`);
    }
    await capture("profile-timezone-default");

    step("profile-timezone: open picker, verify order, search by offset and city, pick Asia/Tokyo");
    await trigger.click();
    const listbox = account.getByRole("listbox", { name: "Time zones" });
    await listbox.waitFor({ state: "visible" });
    const firstOption = (await listbox.getByRole("option").first().textContent()) ?? "";
    if (!firstOption.includes("This browser's") || !firstOption.includes(browserLabel)) {
      fail(`expected first option to be "This browser's" with "${browserLabel}", got "${firstOption}"`);
    }
    await capture("profile-timezone-picker");

    const search = account.getByRole("combobox", { name: "Search time zones by city or offset" });
    await search.fill("+09:00");
    await listbox.getByRole("option", { name: /Asia\/Tokyo/ }).waitFor({ state: "visible" });

    await search.fill("tokyo");
    await listbox.getByRole("option", { name: /Asia\/Tokyo/ }).click();
    const afterPick = (await trigger.textContent()) ?? "";
    if (!afterPick.includes("Asia/Tokyo") || !afterPick.includes("UTC+09:00")) {
      fail(`expected trigger to read Asia/Tokyo UTC+09:00, got "${afterPick}"`);
    }

    await account.getByRole("button", { name: "Save" }).click();
    await account.getByText("Saved").waitFor({ state: "visible" });

    // Reload to verify persistence through GetAuthState.
    await page.reload({ waitUntil: "domcontentloaded" });
    await trigger.waitFor({ state: "visible" });
    const persisted = (await trigger.textContent()) ?? "";
    if (!persisted.includes("Asia/Tokyo")) {
      fail(`expected Asia/Tokyo after reload, got "${persisted}"`);
    }
    await capture("profile-timezone-saved");

    step("profile-timezone: verify Activity renders times in Asia/Tokyo");
    await page.goto(`${origin}/activity`, { waitUntil: "domcontentloaded" });
    const firstWhen = page.locator(".activity-when").first();
    await firstWhen.waitFor({ state: "visible" });
    const tokyoIso = (await firstWhen.locator("time").getAttribute("datetime")) ?? fail("missing activity datetime");
    const tokyoClock = ((await firstWhen.locator("span").textContent()) ?? "").trim();
    const wantTokyo = clockInZone(tokyoIso, "Asia/Tokyo");
    if (tokyoClock !== wantTokyo) {
      fail(`Activity clock in Asia/Tokyo was "${tokyoClock}", want "${wantTokyo}" for ${tokyoIso}`);
    }

    step("profile-timezone: verify new scheduled task starts in Asia/Tokyo");
    await page.goto(`${origin}/scheduled-tasks`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "New task" }).click();
    const sheet = page.getByRole("dialog", { name: "New task" });
    await sheet.waitFor({ state: "visible" });
    await sheet.getByRole("button", { name: /Advanced/i }).click();
    const taskZone = await sheet.getByLabel("Time zone").inputValue();
    if (taskZone !== "Asia/Tokyo") {
      fail(`expected new task Time zone to start at "Asia/Tokyo", got "${taskZone}"`);
    }
    await sheet.getByRole("button", { name: "Cancel" }).click();
    await sheet.waitFor({ state: "hidden" });

    step("profile-timezone: switch to Pacific/Honolulu and verify Activity time updates");
    await page.goto(`${origin}/profile`, { waitUntil: "domcontentloaded" });
    await trigger.waitFor({ state: "visible" });
    await trigger.click();
    await search.fill("-10:00");
    await listbox.getByRole("option", { name: /Pacific\/Honolulu/ }).click();
    await account.getByRole("button", { name: "Save" }).click();
    await account.getByText("Saved").waitFor({ state: "visible" });

    await page.goto(`${origin}/activity`, { waitUntil: "domcontentloaded" });
    await firstWhen.waitFor({ state: "visible" });
    const honoluluIso =
      (await firstWhen.locator("time").getAttribute("datetime")) ?? fail("missing activity datetime in Honolulu");
    const honoluluClock = ((await firstWhen.locator("span").textContent()) ?? "").trim();
    const wantHonolulu = clockInZone(honoluluIso, "Pacific/Honolulu");
    if (honoluluClock !== wantHonolulu) {
      fail(`Activity clock in Pacific/Honolulu was "${honoluluClock}", want "${wantHonolulu}" for ${honoluluIso}`);
    }
    if (honoluluClock === clockInZone(honoluluIso, "Asia/Tokyo")) {
      fail(`Activity clock "${honoluluClock}" should differ between Asia/Tokyo and Pacific/Honolulu`);
    }

    step("profile-timezone: clear back to This browser's");
    await page.goto(`${origin}/profile`, { waitUntil: "domcontentloaded" });
    await trigger.waitFor({ state: "visible" });
    await trigger.click();
    await listbox.getByRole("option").first().click();
    await account.getByRole("button", { name: "Save" }).click();
    await account.getByText("Saved").waitFor({ state: "visible" });
  },
};
