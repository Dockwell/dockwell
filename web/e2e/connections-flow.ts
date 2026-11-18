// SPDX-License-Identifier: AGPL-3.0-only

// Connections (ADR 0099): Connections adds one to a bucket from a store's tile, the
// bucket checked from the server as its keys are typed. An address Dockwell keeps
// every check and app from, as this server's own is, can't be planned; a bucket the
// check can't reach from a network of its own still plans, as an app may reach it
// from its own; the plan never shows the key. The connection's page checks it again. An app
// linked to it reads its names from it on its Environment tab, and Disconnect lets go
// of the link with each value kept, as a plan. Then an app's
// Connect storage checks it from the app's network, which plans nothing here and says
// why, as nothing on this machine is a bucket the app's network reaches. Then it's
// removed, as nothing reads it.

import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";

function fail(message: string): never {
  throw new Error(message);
}

// Made-up keys. The first endpoint is this machine's loopback, which Dockwell sends
// nothing to; the second an IPv6 documentation address, which the guard allows and
// nothing answers at: a machine with no IPv6 route says so at once, one with a route
// waits out the check.
const KEY_ID = "AKIDE2EEXAMPLE";
const SECRET = "e2e-connection-secret-key";
const OWN = "http://127.0.0.1:9";
const ENDPOINT = "http://[2001:db8::10]:9000";
const UNREACHED = /Can't reach it|No answer/;

export const flow: Flow = {
  order: 98,
  run: async (page, { origin, step, shoot, bin, data, env }) => {
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
    cli(["login", "--local"]);
    step("connections: an app that reads S3 names, made from the CLI");
    const fleet = JSON.parse(cli(["servers", "list", "--json"])) as {
      servers?: { id?: string; controlServer?: boolean }[];
    };
    const control =
      fleet.servers?.find((s) => s.controlServer)?.id ?? fail(`no control server is listed: ${JSON.stringify(fleet)}`);
    cli(["variables", "create", "NOTES_S3_BUCKET", "--project", "default", "--value", "old", "-y"]);
    cli(["secrets", "create", "NOTES_S3_SECRET_ACCESS_KEY", "--project", "default", "--value-file", "-", "-y"], "old");
    cli([
      "services",
      "create",
      "notes",
      "--project",
      "default",
      "--on",
      control,
      "--app-image",
      "ghcr.io/example/notes:1",
      "--variables",
      "NOTES_S3_BUCKET=",
      "--secrets",
      "NOTES_S3_SECRET_ACCESS_KEY=",
      "-y",
    ]);
    const listed = JSON.parse(cli(["services", "list", "--json"])) as {
      services: { id: string; resource: { name: string; scope: string } }[];
    };
    const notesInfo = listed.services.find((s) => s.resource.name === "notes") ?? fail("notes isn't listed");
    const notes = notesInfo.id;
    const notesEnv = notesInfo.resource.scope;

    step("connections: one added from a store's tile, checked as its keys are typed");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(`${origin}/connections`);
    await page.getByRole("heading", { level: 1, name: "Connections" }).waitFor({ timeout: 20_000 });
    await page.getByRole("heading", { name: "No connections yet" }).waitFor({ timeout: 20_000 });
    await shoot(page, "connections-empty");
    await page.getByRole("button", { name: "Another S3-compatible store" }).click();
    const sheet = page.getByRole("dialog", { name: "Add a connection" });
    await sheet.waitFor();
    await sheet.getByRole("textbox", { name: "Endpoint" }).fill(OWN);
    await sheet.getByRole("textbox", { name: "Bucket" }).fill("e2e-files");
    await sheet.getByRole("textbox", { name: "Access key ID" }).fill(KEY_ID);
    await sheet.getByLabel("Secret access key").fill(SECRET);
    // The server's own address is one no check and no app reaches, from any network.
    await sheet.getByText("Address refused").waitFor({ timeout: 30_000 });
    await sheet.getByText(/No app can use it either/).waitFor();
    const review = sheet.getByRole("button", { name: "Review plan" });
    if (!(await review.isDisabled())) fail("an address Dockwell sends nothing to can be planned");
    await shoot(page, "connections-add-refused");
    await sheet.getByRole("textbox", { name: "Endpoint" }).fill(ENDPOINT);
    await sheet.getByText(UNREACHED).waitFor({ timeout: 90_000 });
    await sheet.getByText(/It saves unchecked/).waitFor();
    await shoot(page, "connections-add");
    if (await review.isDisabled()) fail("a bucket the server can't reach from here can't be planned");
    await review.click();
    const plan = page.locator(".plan-sheet");
    const apply = plan.getByRole("button", { name: /Apply plan/ });
    await apply.waitFor({ timeout: 15_000 });
    if ((await plan.innerText()).includes(SECRET)) fail("the connection's plan shows its secret key");
    await apply.click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();

    step("connections: its page says nothing reads it, and checks it again");
    await page.getByRole("heading", { level: 1, name: "e2e-files" }).waitFor({ timeout: 20_000 });
    await page.getByText("No app reads it yet.").waitFor();
    await page.getByRole("button", { name: "Check", exact: true }).click();
    await page.getByText(UNREACHED).first().waitFor({ timeout: 90_000 });
    if ((await page.locator("main").innerText()).includes(SECRET)) fail("the connection's page shows its key");
    await shoot(page, "connections-page");

    step("connections: the list says its last check, which Check kept (#799)");
    await page.goto(`${origin}/connections`);
    await page.getByRole("columnheader", { name: "Last check" }).waitFor({ timeout: 20_000 });
    const listedRow = page.getByRole("row", { name: /e2e-files/ });
    await listedRow.getByText(/^Fails from /).waitFor({ timeout: 20_000 });
    await shoot(page, "connections-last-check");
    await page.goto(`${origin}/connections/e2e-files`);
    await page.getByRole("heading", { level: 1, name: "e2e-files" }).waitFor({ timeout: 20_000 });

    step("connections: an app linked to it reads its names from it, and Disconnect keeps their values");
    // The link Connect storage would plan, made from the CLI, as nothing here is a
    // bucket the app's network reaches.
    const org =
      cli(["resources", "list", "--kind", "project", "--json"]).match(/org_[0-9A-Za-z]+/)?.[0] ??
      fail("no organization is listed");
    const from = (part: string) => ({ connection: { scope: org, name: "e2e-files", part, link: "storage" } });
    const edits = [
      {
        resource: { kind: "link", scope: notesEnv, name: "storage" },
        spec: {
          link: {
            label: "Storage",
            connection: "e2e-files",
            kind: "LINK_KIND_S3",
            prefix: "notes/",
            sets: [
              { name: "NOTES_S3_BUCKET", part: "LINK_PART_BUCKET" },
              { name: "NOTES_S3_SECRET_ACCESS_KEY", part: "LINK_PART_SECRET_KEY" },
            ],
          },
        },
      },
      {
        resource: { kind: "variable", scope: notesEnv, name: "NOTES_S3_BUCKET" },
        spec: { variable: { from: from("LINK_PART_BUCKET") } },
      },
      {
        resource: { kind: "secret", scope: notesEnv, name: "NOTES_S3_SECRET_ACCESS_KEY" },
        spec: { secret: { from: from("LINK_PART_SECRET_KEY") } },
      },
    ];
    const args = ["changes", "propose", "--summary", "Connect notes to e2e-files", "-y"];
    edits.forEach((edit, i) => {
      const file = join(data, `connections-edit-${i}.json`);
      writeFileSync(file, JSON.stringify(edit));
      args.push("--edits", `@${file}`);
    });
    cli(args);
    await page.goto(`${origin}/apps/${notes}/environment`);
    const fromConnection = page.getByRole("link", { name: "From e2e-files" });
    await fromConnection.first().waitFor({ timeout: 20_000 });
    if ((await fromConnection.count()) !== 2)
      fail(`${await fromConnection.count()} names read as from e2e-files, want 2`);
    if (await page.getByRole("button", { name: "Change NOTES_S3_BUCKET" }).count()) {
      fail("a name the link sets offers its own change");
    }
    await shoot(page, "connections-env-linked");
    await page.goto(`${origin}/connections/e2e-files`);
    const usedBy = page.getByRole("region", { name: "Used by" });
    await usedBy.getByRole("link", { name: "notes" }).waitFor({ timeout: 20_000 });
    await shoot(page, "connections-used");
    await usedBy.getByRole("button", { name: "Disconnect storage" }).click();
    const detach = page.locator(".plan-sheet");
    const applyDetach = detach.getByRole("button", { name: /Apply plan/ });
    await applyDetach.waitFor({ timeout: 30_000 });
    if ((await detach.innerText()).includes(SECRET)) fail("disconnecting shows the connection's key");
    await applyDetach.click();
    await detach.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await detach.getByRole("button", { name: "Done" }).click();
    await page.getByText("No app reads it yet.").waitFor({ timeout: 20_000 });
    const kept = cli(["variables", "list", "--environment", notesEnv, "--json"]);
    if (!kept.includes("e2e-files")) fail(`after disconnecting, NOTES_S3_BUCKET isn't kept: ${kept}`);

    step("connections: Connect storage checks from the app's network, and plans nothing it can't reach");
    await page.goto(`${origin}/apps/${notes}/environment`);
    await page.getByRole("button", { name: "Connect storage" }).click();
    const connect = page.getByRole("dialog", { name: "Connect notes to storage" });
    await connect.waitFor();
    const picked = connect.getByRole("combobox", { name: "Connection" });
    await picked.waitFor({ timeout: 15_000 });
    if ((await picked.inputValue()) !== "e2e-files") fail(`Connect storage picked ${await picked.inputValue()}`);
    const bucket = connect.getByRole("combobox", { name: "NOTES_S3_BUCKET" });
    if ((await bucket.inputValue()) === "0") fail("Connect storage suggested nothing for NOTES_S3_BUCKET");
    await shoot(page, "connections-connect-storage");
    await connect.getByRole("button", { name: /Check and review plan/ }).click();
    await connect.getByText("Nothing was planned", { exact: true }).waitFor({ timeout: 90_000 });
    await shoot(page, "connections-connect-refused");
    await connect.getByRole("button", { name: "Cancel" }).click();

    step("connections: one nothing reads is removed, as a plan");
    await page.goto(`${origin}/connections/e2e-files`);
    await page.getByRole("button", { name: "Change" }).click();
    const change = page.getByRole("dialog", { name: "Connection e2e-files" });
    await change.waitFor();
    await change.getByRole("button", { name: "Remove" }).click();
    const removal = page.locator(".plan-sheet");
    // A plan that only deletes names what it deletes on its button.
    await removal.getByRole("button", { name: /Apply plan|Delete e2e-files/ }).click();
    await removal.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await removal.getByRole("button", { name: "Done" }).click();
    await page.goto(`${origin}/connections`);
    await page.getByRole("heading", { name: "No connections yet" }).waitFor({ timeout: 20_000 });
  },
};
