// SPDX-License-Identifier: AGPL-3.0-only
// Server labels (ADR 0091): the control server's page adds them with a picker, its
// plan says it's labels alone, with no step on the server, and the Servers list then
// selects and groups by them. The CLI labels the second server, whose agent the health
// flow left stopped, so a change of labels applies offline too, and lists by a
// selector. Each takes its labels off again, so later flows see the fleet as it was.
import { execFileSync } from "node:child_process";
import { join } from "node:path";

import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";

const SEEN_WITHIN = 30_000;

function fail(message: string): never {
  throw new Error(message);
}

/** Applies the plan the sheet shows, which a change of labels alone needs no passkey for, and closes it. */
async function applyLabels(page: Page, server: string): Promise<void> {
  const plan = page.getByRole("dialog", { name: "Review plan" });
  await plan.getByText(`Labels alone: no step runs on ${server}, online or offline.`).waitFor({ timeout: SEEN_WITHIN });
  await plan.getByRole("button", { name: /^Apply plan/ }).click();
  await page
    .getByRole("dialog", { name: `Labels are changed on ${server}` })
    .getByRole("button", { name: "Done", exact: true })
    .click({ timeout: SEEN_WITHIN });
}

export async function serverLabelsFlow(
  page: Page,
  { origin, step, shoot, bin, data, env }: Pick<FlowRun, "origin" | "step" | "shoot" | "bin" | "data" | "env">,
): Promise<void> {
  step("server labels: the control server's page adds labels with a picker, and the plan says it's labels alone");
  await page.goto(`${origin}/servers`);
  await page.locator("h1").first().waitFor();
  const listed = (await page.evaluate(async () => (await fetch("/v1/servers")).json())) as {
    servers?: { id: string; name: string; controlServer?: boolean }[];
  };
  const control = listed.servers?.find((s) => s.controlServer) ?? fail(`no control server: ${JSON.stringify(listed)}`);
  const second =
    listed.servers?.find((s) => s.name === "second") ?? fail(`no second server: ${JSON.stringify(listed)}`);
  await page.goto(`${origin}/servers/${control.id}`);
  await page.getByRole("button", { name: "Add labels" }).click();
  const sheet = page.getByRole("dialog", { name: `Labels of ${control.name}` });
  const field = sheet.getByRole("combobox", { name: "Labels" });
  // A label it can't take says why as it's typed, in the server's words.
  await field.fill("re gion");
  await sheet.getByText("It has a space, and a label holds letters, digits, -, _ and . alone").waitFor();
  await field.fill("region:eu");
  await field.press("Enter");
  await field.fill("pool:web");
  await field.press("Enter");
  await shoot(page, "server-labels-edit");
  await sheet.getByRole("button", { name: "Review plan" }).click();
  await shoot(page, "server-labels-plan");
  await applyLabels(page, control.name);
  await page.getByText("region:", { exact: false }).first().waitFor({ timeout: SEEN_WITHIN });

  step("server labels: the CLI labels the second server while its agent is stopped, and lists by a selector");
  const cli = (...args: string[]) =>
    execFileSync(bin, [...args, "--data-dir", data, "--log-format", "json"], {
      encoding: "utf8",
      env: {
        ...process.env,
        ...env,
        USER: "mira",
        DOCKWELL_URL: "",
        DOCKWELL_TOKEN: "",
        XDG_CONFIG_HOME: join(data, "cli-config"),
      },
    });
  cli("login", "--local");
  cli("servers", "label", "--server-ids", second.id, "--set", "region:us", "--yes");
  const names = (selector: string) =>
    (
      (JSON.parse(cli("servers", "list", "--selector", selector, "--json")) as { servers?: { name?: string }[] })
        .servers ?? []
    )
      .map((s) => s.name)
      .sort();
  if (JSON.stringify(names("region:eu")) !== JSON.stringify([control.name]))
    fail(`region:eu lists ${names("region:eu")}`);
  if (JSON.stringify(names("!pool")) !== JSON.stringify(["second"])) fail(`!pool lists ${names("!pool")}`);

  step("server labels: Servers selects by them, says how many match, and groups the tiles by region");
  await page.goto(`${origin}/servers?closed=true`);
  await page.getByText("in 2 regions").waitFor({ timeout: SEEN_WITHIN });
  const map = page.getByRole("region", { name: "Servers at a glance" });
  await map.getByText("eu", { exact: true }).waitFor();
  await map.getByText("us", { exact: true }).waitFor();
  const selector = page.getByRole("combobox", { name: "Add a label to the selector" });
  await selector.fill("pool:web");
  await selector.press("Enter");
  await page.getByText("1 matches").waitFor();
  await page.waitForURL(/selector=pool%3Aweb/);
  const table = page.getByRole("table", { name: "Servers matching pool:web, sorted by name" });
  if ((await table.getByRole("row").count()) !== 2) fail("the selector lists more than the control server");
  await shoot(page, "server-labels-selector");

  step("server labels: each server's labels come off again");
  cli("servers", "label", "--server-ids", second.id, "--remove", "region", "--yes");
  await page.goto(`${origin}/servers/${control.id}`);
  await page.getByRole("button", { name: `Edit ${control.name}'s labels` }).click();
  const again = page.getByRole("dialog", { name: `Labels of ${control.name}` });
  await again.getByRole("button", { name: "Remove pool:web" }).click();
  await again.getByRole("button", { name: "Remove region:eu" }).click();
  await again.getByRole("button", { name: "Review plan" }).click();
  await applyLabels(page, control.name);
  await page.getByRole("button", { name: "Add labels" }).waitFor({ timeout: SEEN_WITHIN });
}

/** Where sign-in.ts runs this among the flows (flows.ts): just after Servers' own. */
export const flow: Flow = { order: 82.5, run: (page, run) => serverLabelsFlow(page, run) };
