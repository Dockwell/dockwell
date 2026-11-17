// SPDX-License-Identifier: AGPL-3.0-only

// A catalog app whose owner Dockwell makes, set up through the panel when it can't
// (ADR 0105), on the fake Docker Engine of scripts/fakedocker: its install is held
// while its owner is made, the panel says Dockwell couldn't make it, with what the
// app said, the person sets it up themselves, and opens it to everyone with a
// passkey, as its plan says who reaches it first takes it. Activity says each.

import { writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Locator, Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";
import { onTop } from "./sheets.ts";

const DIGEST = `sha256:${"5".repeat(64)}`;

/**
 * An app whose owner is made by a command in its container, which the fake engine's
 * image has no shell for, so making it fails, as a real app's might.
 */
const entry = `id: e2e-wiki
name: E2E Wiki
summary: Something with an owner
category: storage
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
owner:
  service: web
  emailInput: OWNER_EMAIL
  passwordInput: OWNER_PASSWORD
  steps:
    - command:
        words: [sh, -c, wiki-admin create-owner]
  done:
    command:
      words: [sh, -c, wiki-admin has-owner]
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18410"
  - name: OWNER_EMAIL
    label: Owner's email
    type: INPUT_TYPE_EMAIL
  - name: OWNER_PASSWORD
    label: Owner's password
    type: INPUT_TYPE_SECRET
    generate: 24
    generateAs: GENERATE_AS_PASSWORD
compose: |
  services:
    web:
      image: wiki:1.0@${DIGEST}
      ports:
        - "127.0.0.1:\${PORT}:80"
`;

function fail(message: string): never {
  throw new Error(message);
}

/** Says where it runs once there's a choice: a connected server. */
async function chooseServer(sheet: Locator) {
  await sheet
    .getByText(/^Runs on/)
    .first()
    .waitFor();
  // One server is a line with nothing to choose; several are checkboxes.
  const boxes = sheet
    .getByRole("group", { name: "Runs on" })
    .locator("label", { hasNotText: "Offline" })
    .getByRole("checkbox");
  if ((await boxes.count()) === 0) return;
  await boxes.first().check();
}

async function heldAppFlow(page: Page, { origin, step, shoot, catalogDir }: FlowRun) {
  writeFileSync(join(catalogDir, "e2e-wiki.yaml"), entry);

  step("held app: installing an app whose owner Dockwell makes starts from your email");
  await page.goto(`${origin}/catalog/e2e-wiki/install`);
  const sheet = page.getByRole("dialog", { name: "Install E2E Wiki" });
  await sheet.waitFor();
  await chooseServer(sheet);
  await sheet.getByRole("button", { name: /Review plan/ }).click();
  const plan = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await plan.waitFor({ timeout: 30_000 });
  await plan
    .getByText(/Make the owner account in E2E Wiki|owner/i)
    .first()
    .waitFor();
  await plan.getByRole("button", { name: /Apply plan/ }).click();
  const open = page.getByRole("button", { name: "Open E2E Wiki", exact: true });
  await open.waitFor({ timeout: 120_000 });
  await open.click();

  step("held app: its page says Dockwell couldn't make its owner, with what the app said");
  const failed = page.getByText("Couldn't make E2E Wiki's owner", { exact: true });
  await failed.waitFor({ timeout: 120_000 });
  const callout = page.locator(".dw-callout", { has: failed });
  await callout.getByText(/not found|executable/).waitFor();
  await shoot(page, "held-app-failed");

  step("held app: set up by hand, it's held until a person opens it to everyone");
  await callout.getByRole("button", { name: "Set it up yourself" }).click();
  const held = page.locator(".dw-callout", { hasText: "Held until its owner account exists" });
  await held.waitFor();
  await shoot(page, "held-app-held");
  await held.getByRole("button", { name: "Open it to everyone" }).click();

  step("held app: opening it says who reaches it first takes it, and asks for a passkey");
  const opening = onTop(page.getByRole("dialog", { name: "Review plan" }));
  await opening.waitFor({ timeout: 30_000 });
  await opening.getByText(/first takes E2E Wiki/).waitFor();
  await opening.getByText("Applying this asks for your passkey").waitFor();
  await shoot(page, "held-app-open-plan");
  await opening.getByRole("button", { name: /Apply plan/ }).click();
  await held.waitFor({ state: "hidden", timeout: 120_000 });
  if ((await page.getByText("Couldn't make E2E Wiki's owner", { exact: true }).count()) !== 0)
    fail("the app still says its owner failed once it's open");

  step("held app: Activity says the owner couldn't be made, and that it was opened before it was");
  await page.goto(`${origin}/activity`);
  await page
    .getByText(/^Couldn't make E2E Wiki's owner/)
    .first()
    .waitFor({ timeout: 60_000 });
  for (let i = 0; i < 12; i++) {
    if ((await page.getByText("E2E Wiki opened to everyone before its owner was made").count()) > 0) break;
    await page.waitForTimeout(5_000);
    await page.reload();
  }
  await page.getByText("E2E Wiki opened to everyone before its owner was made").first().waitFor({ timeout: 30_000 });
  await shoot(page, "held-app-activity");
}

export const flow: Flow = { order: 46, run: (page, run) => heldAppFlow(page, run) };
