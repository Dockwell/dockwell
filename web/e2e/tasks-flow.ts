// SPDX-License-Identifier: AGPL-3.0-only

// Scheduled tasks end to end, on the fake Docker Engine of scripts/fakedocker: from
// the rail to none yet, a shell command made through a plan that takes a passkey as
// it's root on the server, run now by the agent, and what it printed. The agent here
// runs /bin/sh on this machine, so the command only prints. scripts/docker-e2e.sh
// runs each kind on real Docker.

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";

export interface TasksFlowOptions {
  origin: string;
  /** Reports each step. */
  step: (what: string) => void;
  /** Saves screenshots of a screen, if the run was asked to. */
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function tasksFlow(page: Page, { origin, step, shoot }: TasksFlowOptions) {
  step("tasks: the rail leads to them, and none yet says what one is");
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`${origin}/apps`);
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Scheduled tasks" }).click();
  await page.getByRole("heading", { level: 1, name: "Scheduled tasks" }).waitFor();
  await page.getByRole("heading", { level: 2, name: "No scheduled tasks yet" }).waitFor();
  await shoot(page, "tasks-empty");

  step("tasks: a shell command is a plan, which takes a passkey as it runs as root");
  await page.getByRole("button", { name: "New task" }).first().click();
  const sheet = page.getByRole("dialog", { name: "New task" });
  await sheet.getByLabel("Name").fill("say-hello");
  await sheet.getByLabel("What people call it").fill("Say hello");
  await sheet.getByLabel("Command").fill("echo hello-from-a-task; echo and-on-stderr >&2");
  // The schedule's words, under the cron that says it: the first is its hidden option.
  await sheet.getByText("Every day at 04:00").last().waitFor();
  await shoot(page, "tasks-new");
  await sheet.getByRole("button", { name: "Review plan" }).click();
  const plan = page.locator(".plan-sheet");
  await plan.locator(".plan-gate").getByText("Applying this asks for your passkey").waitFor({ timeout: 30_000 });
  await plan.locator(".plan-gate input").fill(await plan.locator(".plan-gate code").first().innerText());
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  await page.getByRole("button", { name: "Done", exact: true }).waitFor({ timeout: 30_000 });
  await page.getByRole("button", { name: "Done", exact: true }).click();

  step("tasks: it's listed with its schedule in words and as cron, and runs now on its server's agent");
  const table = page.getByRole("table", { name: "Scheduled tasks" });
  const row = table.getByRole("row", { name: /Say hello/ });
  await row.getByText("Every day at 04:00").waitFor({ timeout: 20_000 });
  await row.getByText("0 4 * * *").waitFor();
  const panel = page.getByRole("region", { name: "Say hello" });
  await panel.getByText("It hasn't run yet. Run it now to see what it does.").waitFor();
  await panel.getByRole("button", { name: "Run now" }).click();
  // The lines it printed, which the command above them names too.
  await panel.getByText("hello-from-a-task", { exact: true }).waitFor({ timeout: 30_000 });
  await panel.getByText("and-on-stderr", { exact: true }).waitFor();
  await row.getByText("Succeeded").waitFor({ timeout: 20_000 });
  await shoot(page, "tasks-list");

  step("tasks: on a phone, each is a row, and nothing widens the page");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: /Say hello/ }).waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  if (wide > 0) fail(`the page scrolls ${wide}px sideways on a phone`);
  await shoot(page, "tasks-phone");

  step("tasks: timeline labels stay on the track at the day's boundaries, on desktop and phones");
  try {
    for (const width of [1280, 390, 360]) {
      await page.setViewportSize({ width, height: 900 });
      for (const [hour, minute] of [
        [0, 0],
        [0, 3],
        [5, 59],
        [6, 0],
        [11, 59],
        [12, 0],
        [17, 59],
        [18, 0],
        [23, 59],
      ] as const) {
        const fixed = await page.evaluate(([h, m]) => new Date(2026, 10, 10, h, m).getTime(), [hour, minute]);
        await page.clock.setFixedTime(fixed);
        await page.reload();
        await page.getByRole("region", { name: "Next 24 hours" }).waitFor();
        await page.evaluate(() => document.fonts.ready);
        if ((await page.locator(".tasks-axis-label").count()) !== 4) fail("the timeline lost an axis label");
        const outside = await page.locator(".tasks-axis-label").evaluateAll((labels) =>
          labels
            .filter((label) => {
              const rect = label.getBoundingClientRect();
              const track = label.parentElement?.getBoundingClientRect();
              return !track || rect.left < track.left - 0.5 || rect.right > track.right + 0.5;
            })
            .map((label) => label.textContent),
        );
        if (outside.length)
          fail(`timeline labels leave their track at ${hour}:${minute}, ${width}px: ${outside.join(", ")}`);
        const overflow = await page.evaluate(
          () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
        );
        if (overflow > 0) fail(`the tasks page scrolls ${overflow}px sideways at ${hour}:${minute}, ${width}px`);
      }
    }
  } finally {
    await page.clock.setSystemTime(new Date());
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.reload();
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 62, run: (page, run) => tasksFlow(page, run) };
