// SPDX-License-Identifier: AGPL-3.0-only

// A server's processes, as Server-Processes.dc.html has them, on a real control plane
// whose own agent reads this machine's /proc: what runs, live, found by its PID, and
// a process this flow started stopped through the sheet, with the passkey it asks
// for, which the browser's virtual authenticator gives. Then Activity has it.

import { spawn } from "node:child_process";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

interface ProcessesFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function processesFlow(page: Page, { origin, step, shoot }: ProcessesFlowOptions) {
  step("a server's processes: what runs, live, sorted and found by PID");
  const sleeper = spawn("sleep", ["600"], { stdio: "ignore" });
  const ended = new Promise<NodeJS.Signals | null>((done) => sleeper.once("exit", (_code, signal) => done(signal)));
  try {
    const pid = sleeper.pid ?? fail("couldn't start a process to stop");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/`);
    const link = await page.locator('a[href^="/servers/srv_"]').first().getAttribute("href");
    if (!link) fail("the overview links to no server");
    await page.goto(`${origin}${link}`);
    await page
      .getByRole("navigation", { name: /^Server / })
      .getByRole("link", { name: "Processes" })
      .click();
    const table = page.getByRole("table", { name: /^Processes on / });
    await table.waitFor({ timeout: 15_000 });
    await page.getByText("Live", { exact: true }).waitFor();
    // CPU comes with the second list, a second after the first.
    await table
      .getByRole("row")
      .nth(1)
      .getByText(/\d+\.\d%/)
      .waitFor({ timeout: 10_000 });
    const cpu = page.getByRole("button", { name: "CPU" });
    if ((await cpu.locator("xpath=ancestor::th").getAttribute("aria-sort")) !== "descending") {
      fail("the list isn't sorted by CPU, busiest first");
    }
    await shoot(page, "server-processes");

    await page.getByRole("textbox", { name: "Filter processes" }).fill(String(pid));
    const row = table.getByRole("row", { name: new RegExp(`\\bsleep\\b.*\\b${pid}\\b`) });
    await row.waitFor();
    if ((await table.getByRole("row").count()) !== 2) fail(`filtering by PID ${pid} left more than its row`);

    step("stopping it says what that does, asks for the passkey, and it gets SIGTERM");
    await row.getByRole("button", { name: `Stop sleep (PID ${pid})` }).click();
    const sheet = page.getByRole("dialog", { name: "Stop sleep?" });
    await sheet.getByText("It gets SIGTERM", { exact: false }).waitFor();
    await shoot(page, "server-processes-stop");
    await sheet.getByRole("button", { name: "Stop process" }).click();
    await page.getByRole("dialog", { name: "Stopped sleep" }).waitFor({ timeout: 20_000 });
    const signal = await Promise.race([ended, new Promise<string>((done) => setTimeout(() => done("timeout"), 5_000))]);
    if (signal !== "SIGTERM") fail(`the process ended with ${signal}, not SIGTERM`);
    await page.getByRole("dialog").getByRole("button", { name: "Done" }).click();

    await page.goto(`${origin}/activity`);
    await page.getByText(`Stopped sleep (PID ${pid}) on`, { exact: false }).first().waitFor();

    step("the processes fit the narrowest phone");
    await page.goBack();
    await page.setViewportSize({ width: 360, height: 780 });
    await page.getByRole("list", { name: "Processes" }).waitFor({ timeout: 15_000 });
    const wide = await page.evaluate(() => document.documentElement.scrollWidth);
    if (wide > 360) fail(`the processes are ${wide} px wide on a 360 px phone`);
    await page.setViewportSize({ width: 1440, height: 900 });
  } finally {
    sleeper.kill();
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 95, run: (page, run) => processesFlow(page, run) };
