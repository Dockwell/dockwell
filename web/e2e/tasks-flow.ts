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
  await panel.getByText("hello-from-a-task").waitFor({ timeout: 30_000 });
  await panel.getByText("and-on-stderr").waitFor();
  await row.getByText("Succeeded").waitFor({ timeout: 20_000 });
  await shoot(page, "tasks-list");

  step("tasks: on a phone, each is a row, and nothing widens the page");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: /Say hello/ }).waitFor();
  const wide = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  if (wide > 0) fail(`the page scrolls ${wide}px sideways on a phone`);
  await shoot(page, "tasks-phone");
  await page.setViewportSize({ width: 1280, height: 900 });
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts). */
export const flow: Flow = { order: 62, run: (page, run) => tasksFlow(page, run) };
