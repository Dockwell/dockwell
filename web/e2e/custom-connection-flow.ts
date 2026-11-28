// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { join } from "node:path";
import type { Locator } from "playwright-core";

import type { Flow } from "./flows.ts";

const connection = "e2e-custom-api";
const before = "e2e-custom-private-first";
const after = "e2e-custom-private-next";

function assertRedacted(text: string) {
  if (text.includes(before) || text.includes(after)) {
    throw new Error("a Custom connection secret reached a page or command output");
  }
}

async function applyPlan(plan: Locator, needs: string[]) {
  const apply = plan.getByRole("button", { name: /Apply plan/ });
  await apply.waitFor({ timeout: 20_000 });
  for (const name of needs) await plan.getByText(name, { exact: true }).first().waitFor();
  assertRedacted(await plan.innerText());
  await apply.click();
  const done = plan.getByRole("button", { name: "Done" });
  await done.waitFor({ timeout: 30_000 });
  assertRedacted(await plan.innerText());
  await done.click();
}

export const flow: Flow = {
  order: 99,
  run: async (page, { origin, bin, data, env, step }) => {
    const cli = (args: string[], input?: string) =>
      execFileSync(bin, [...args, "--data-dir", data, "--log-format", "json"], {
        encoding: "utf8",
        input,
        env: {
          ...process.env,
          ...env,
          USER: "mira",
          DOCKWELL_URL: "",
          DOCKWELL_TOKEN: "",
          XDG_CONFIG_HOME: join(data, "cli-config"),
        },
      });

    step("custom connections: create from a template and review a redacted plan");
    cli(["login", "--local"]);
    const fleet = JSON.parse(cli(["servers", "list", "--json"])) as {
      servers: { id: string; controlServer?: boolean }[];
    };
    const control = fleet.servers.find((s) => s.controlServer)?.id;
    if (!control) throw new Error("the control server is absent");
    for (const app of ["custom-reader", "custom-worker"]) {
      cli([
        "services",
        "create",
        app,
        "--project",
        "default",
        "--on",
        control,
        "--app-image",
        "example.com/sample:1",
        "-y",
      ]);
    }

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/connections`);
    const add = page.getByRole("button", { name: "Add connection", exact: true });
    const tile = page.getByRole("button", { name: "Custom", exact: true });
    await Promise.race([add.waitFor({ timeout: 20_000 }), tile.waitFor({ timeout: 20_000 })]);
    if (await add.isVisible()) await add.click();
    await tile.click();
    const editor = page.getByRole("dialog", { name: "Add a Custom connection" });
    await editor.getByLabel("Name", { exact: true }).fill("E2E Custom API");
    await editor.getByLabel("Start with").selectOption({ label: "API key" });
    await editor.getByLabel("Field 1 value").fill(before);
    await editor.getByRole("button", { name: "Advanced" }).click();
    await editor.getByRole("switch", { name: "Allow multiple apps" }).click();
    await editor.getByRole("button", { name: "Review plan" }).click();
    const plan = page.locator(".plan-sheet");
    await applyPlan(plan, []);
    await page.getByRole("heading", { level: 1, name: connection }).waitFor({ timeout: 20_000 });
    await page.getByText("Secret is set", { exact: true }).waitFor();

    step("custom connections: map the field from the app's Variables tab");
    const apps = JSON.parse(cli(["services", "list", "--json"])) as {
      services: { id: string; resource: { name: string } }[];
    };
    const reader = apps.services.find((s) => s.resource.name === "custom-reader");
    if (!reader) throw new Error("the Custom connection's app is absent");
    await page.goto(`${origin}/apps/${reader.id}/environment`);
    await page.getByRole("button", { name: "Add from a connection" }).click();
    const mapping = page.getByRole("dialog", { name: "Add from a connection" });
    await mapping.getByLabel("Connection", { exact: true }).selectOption(connection);
    const alias = mapping.getByLabel("api_key", { exact: true });
    if ((await alias.inputValue()) !== "E2E_CUSTOM_API_API_KEY")
      throw new Error("the field has no environment default");
    await alias.fill("APP_TOKEN");
    await mapping.getByRole("button", { name: "Review plan" }).click();
    await applyPlan(plan, ["APP_TOKEN"]);
    const envRow = page.getByRole("row", { name: /APP_TOKEN/ });
    await envRow.getByRole("link", { name: `From ${connection}` }).waitFor();
    assertRedacted(await envRow.innerText());

    assertRedacted(
      cli([
        "connections",
        "connect-custom",
        "--project",
        "default",
        "--app",
        "custom-worker",
        "--connection",
        connection,
        "--fields",
        "api_key=APP_TOKEN",
        "-y",
      ]),
    );

    step("custom connections: rotating one value plans a restart of both apps");
    await page.goto(`${origin}/connections/${connection}`);
    await page.getByRole("button", { name: "Change", exact: true }).click();
    const editing = page.getByRole("dialog", { name: `Connection ${connection}` });
    if ((await editing.getByLabel("Field 1 value").inputValue()) !== "")
      throw new Error("the editor loaded a saved secret");
    await editing.getByLabel("Field 1 value").fill(after);
    await editing.getByRole("button", { name: "Review plan" }).click();
    await applyPlan(plan, ["custom-reader", "custom-worker"]);
    assertRedacted(await page.locator("main").innerText());
    assertRedacted(cli(["connections", "list", "--json"]));

    step("custom connections: the CLI can save and list a Custom connection");
    const listed = cli(
      ["connections", "create", "e2e-custom-cli", "--kind", "custom", "--custom-fields", "-", "-y"],
      JSON.stringify({ name: "key", type: "CONNECTION_CUSTOM_TYPE_SECRET", secretValue: after }),
    );
    assertRedacted(listed);
    const current = cli(["connections", "list", "--json"]);
    assertRedacted(current);
    if (!current.includes("e2e-custom-cli")) throw new Error("the CLI's Custom connection is absent");
  },
};
