// SPDX-License-Identifier: AGPL-3.0-only

// An app learns what's newer than what it runs, through its server's agent and the
// fake engine's registry (ADR 0079): redis:7.2 is checked, and its page offers 7.4, a
// minor, as Redis's three-number tags say; the list marks it, and a person skips 7.4,
// which the page then says was skipped. The app goes again at the end, so the flows
// after it see what they saw before.

import { execFileSync } from "node:child_process";
import { join } from "node:path";

import type { Flow } from "./flows.ts";

function fail(message: string): never {
  throw new Error(message);
}

export const flow: Flow = {
  order: 97,
  run: async (page, { origin, step, shoot, bin, data, env }) => {
    if (!process.env["DOCKWELL_TEST_REGISTRY"]) {
      step("app updates: there's no registry stand-in on this engine, so nothing is checked");
      return;
    }
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

    step("app updates: an app on redis:7.2 is checked, and its page offers 7.4, a minor, as Redis numbers it");
    // On the control server, by its ID: earlier flows may have joined others, and a
    // name alone would then ask which.
    const fleet = JSON.parse(cli("servers", "list", "--json")) as {
      servers?: { id?: string; name?: string; controlServer?: boolean }[];
    };
    const control =
      fleet.servers?.find((s) => s.controlServer)?.id ?? fail(`no control server is listed: ${JSON.stringify(fleet)}`);
    cli(
      "services",
      "create",
      "updates-cache",
      "--project",
      "default",
      "--on",
      control,
      "--app-image",
      "redis:7.2",
      "-y",
    );
    const checked = cli("app-updates", "check", "updates-cache", "--project", "default");
    if (!checked.includes("7.4 minor")) fail(`the check found ${checked}`);
    const listed = JSON.parse(cli("services", "list", "--json")) as {
      services: { id: string; resource: { name: string } }[];
    };
    const id =
      listed.services.find((s) => s.resource.name === "updates-cache")?.id ?? fail("updates-cache isn't listed");
    await page.goto(`${origin}/apps/${id}`);
    const card = page.locator("section", { has: page.getByRole("heading", { name: "Update available" }) });
    await card.waitFor({ timeout: 20_000 });
    await card.getByText("7.2 → 7.4").waitFor();
    await card.getByText("minor", { exact: true }).waitFor();
    await card.getByRole("button", { name: "Update to 7.4" }).waitFor();
    await shoot(page, "app-updates-card");

    step("app updates: the list marks it, and its filter lists it");
    await page.goto(`${origin}/apps?show=updates`);
    const row = page.getByRole("row", { name: /updates-cache/ });
    await row.waitFor({ timeout: 20_000 });
    await row.getByTitle("Update available: 7.2 → 7.4, minor").waitFor();
    await shoot(page, "app-updates-list");

    step("app updates: skipping 7.4 sets it aside, and the page says it was skipped");
    await page.goto(`${origin}/apps/${id}`);
    await card.waitFor({ timeout: 20_000 });
    await card.getByText("Not now").click();
    await card.getByRole("button", { name: "Skip 7.4" }).click();
    const quiet = page.locator("section", { has: page.getByRole("heading", { name: "Updates", exact: true }) });
    await quiet.getByText("Up to date").waitFor({ timeout: 20_000 });
    await quiet.getByRole("button", { name: "Show 7.4 again" }).waitFor();

    step("app updates: the app goes again");
    cli("services", "delete", "updates-cache", "--project", "default", "-y");
  },
};
