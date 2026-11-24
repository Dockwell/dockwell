// SPDX-License-Identifier: AGPL-3.0-only

// Server history charts, live network connections tab, and process detail sheet
// (with secret redaction and no environment leak) on a real control plane.

import { spawn } from "node:child_process";

import type { Page } from "playwright-core";

import type { Flow } from "./flows.ts";
import { ownServerPath } from "./own-server.ts";
import { atWidth } from "./sheets.ts";

interface MetricsHistoryFlowOptions {
  origin: string;
  step: (message: string) => void;
  shoot: (page: Page, name: string) => Promise<void>;
}

function fail(message: string): never {
  throw new Error(message);
}

export async function metricsHistoryFlow(page: Page, { origin, step, shoot }: MetricsHistoryFlowOptions) {
  step("server overview shows history charts with range switcher");
  await atWidth(page, { width: 1440, height: 900 });
  const link = await ownServerPath(page, origin);
  await page.goto(`${origin}${link}`);
  const hist = page.getByRole("region", { name: "History" });
  await hist.waitFor({ timeout: 15_000 });
  await hist.getByRole("heading", { name: "CPU" }).waitFor();
  await hist.getByRole("heading", { name: "Memory" }).waitFor();
  await hist.getByRole("heading", { name: "Load" }).waitFor();
  await hist.getByRole("heading", { name: "Disk I/O" }).waitFor();
  await hist.getByRole("heading", { name: "Network" }).waitFor();
  await hist.getByRole("radio", { name: "7 d" }).click();
  await hist.getByRole("radio", { name: "24 h" }).click();
  await shoot(page, "server-overview-history");

  step("processes page opens detail sheet with secret-redacted full command and no env");
  // Spawn a child with a secret-looking argument via sh -c so we can inspect redaction.
  const secret = "e2e_super_secret_token_987654";
  const envSecret = "E2E_ENV_MUST_NOT_LEAK_123456";
  const child = spawn("sh", ["-c", `sleep 600 # --api-key ${secret}`], {
    stdio: "ignore",
    env: { ...process.env, E2E_PRIVATE_SECRET: envSecret },
  });
  try {
    const pid = child.pid ?? fail("couldn't start test process");
    await page.goto(`${origin}${link}/processes`);
    const table = page.getByRole("table", { name: /^Processes on / });
    await table.waitFor({ timeout: 15_000 });

    // Switch to Connections tab and confirm live sockets load and filter.
    step("connections tab lists live sockets and filters by protocol");
    await page.getByRole("radio", { name: "Connections" }).click();
    const connTable = page.getByRole("table", { name: /^Connections on / });
    await connTable.waitFor({ timeout: 15_000 });
    await page.getByRole("combobox", { name: "Filter by protocol" }).selectOption("tcp");
    await page.getByRole("combobox", { name: "Filter by protocol" }).selectOption("");
    await shoot(page, "server-connections");

    // Back to Processes tab, filter for our child PID, click it to open ProcessDetailSheet.
    await page.getByRole("radio", { name: /^Processes/ }).click();
    await table.waitFor({ timeout: 15_000 });
    await page.getByRole("searchbox", { name: "Filter processes" }).fill(String(pid));
    const row = table.locator("tbody tr", { hasText: String(pid) }).first();
    await row.waitFor({ timeout: 15_000 });
    await row.click();

    const dialog = page.getByRole("dialog");
    await dialog.waitFor({ timeout: 10_000 });
    await dialog.getByRole("heading", { name: "Command line" }).waitFor();
    const text = await dialog.innerText();
    if (text.includes(secret) || text.includes(envSecret)) {
      fail("process detail sheet leaked a secret or environment variable");
    }
    await shoot(page, "server-process-detail");
    await page.keyboard.press("Escape");
  } finally {
    child.kill();
  }
}

/** Runs right after processes-flow.ts (order 95). */
export const flow: Flow = { order: 96, run: (page, run) => metricsHistoryFlow(page, run) };
