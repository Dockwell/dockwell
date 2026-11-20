// SPDX-License-Identifier: AGPL-3.0-only

// Backup storage as S3 connections (#831, ADR 0099):
// - Unified deduplicated storage counts across Backups, Integrations, and Connections.
// - Schedule sheet lists S3 connections first, disables app-serving connections with why
//   and offers New connection from it (endpoint/region prefilled, bucket/keys empty), and
//   seals an unused S3 connection inline in one two-edit plan.
// - Connections shows backup destinations under Used by and disables Use in an app, Share,
//   and Remove with why while backups use the connection.
// - Integrations offers Use as a connection on standalone legacy destinations, turning one
//   into a connection without double-counting.

import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { atWidth, pastTheEdge } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

export const flow: Flow = {
  order: 98.2,
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

    const proposeEdits = (summary: string, edits: unknown[]) => {
      const args = ["changes", "propose", "--summary", summary, "-y"];
      edits.forEach((edit, i) => {
        const file = join(data, `bkp-conn-edit-${Date.now()}-${i}.json`);
        writeFileSync(file, JSON.stringify(edit));
        args.push("--edits", `@${file}`);
      });
      cli(args);
    };

    step("backup-connections: seed an app-serving connection, an unused S3 connection, and a legacy destination");
    const org =
      cli(["resources", "list", "--kind", "project", "--json"]).match(/org_[0-9A-Za-z]+/)?.[0] ??
      fail("no organization is listed");
    const services = JSON.parse(cli(["services", "list", "--json"])) as {
      services: { id: string; resource: { name: string; scope: string } }[];
    };
    const notesInfo = services.services.find((s) => s.resource.name === "notes") ?? fail("notes service not found");
    const notesEnv = notesInfo.resource.scope;

    proposeEdits("Add uploads connection for notes", [
      {
        resource: { kind: "connection", scope: org, name: "uploads" },
        spec: {
          connection: {
            displayName: "Uploads",
            kind: "LINK_KIND_S3",
            s3: {
              endpoint: "https://s3.eu-central-1.amazonaws.com",
              region: "eu-central-1",
              bucket: "site-uploads",
              accessKeyId: "AKIDUPLOADS",
              secretKey: "uploads-secret-key",
            },
            reach: "CONNECTION_REACH_EVERY",
          },
        },
      },
      {
        resource: { kind: "link", scope: notesEnv, name: "uploads" },
        spec: {
          link: {
            label: "Uploads",
            connection: "uploads",
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
        spec: {
          variable: {
            from: { connection: { scope: org, name: "uploads", part: "LINK_PART_BUCKET", link: "uploads" } },
          },
        },
      },
      {
        resource: { kind: "secret", scope: notesEnv, name: "NOTES_S3_SECRET_ACCESS_KEY" },
        spec: {
          secret: {
            from: { connection: { scope: org, name: "uploads", part: "LINK_PART_SECRET_KEY", link: "uploads" } },
          },
        },
      },
    ]);

    proposeEdits("Add offsite S3 connection and legacy backup destination", [
      {
        resource: { kind: "connection", scope: org, name: "offsite" },
        spec: {
          connection: {
            displayName: "Offsite",
            kind: "LINK_KIND_S3",
            s3: {
              endpoint: "https://s3.us-east-1.amazonaws.com",
              region: "us-east-1",
              bucket: "acme-offsite",
              accessKeyId: "AKIDOFFSITE",
              secretKey: "offsite-secret-key",
            },
            reach: "CONNECTION_REACH_EVERY",
          },
        },
      },
      {
        resource: { kind: "backup.destination", scope: org, name: "legacy-r2" },
        spec: {
          backupDestination: {
            endpoint: "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com",
            region: "auto",
            bucket: "legacy-backups",
            prefix: "old",
            accessKey: "AKIDLEGACY",
            secretKey: "legacy-secret-key",
            passphrase: "legacy-passphrase-for-e2e-test",
          },
        },
      },
    ]);

    step("backup-connections: Backups leaves out app-only connections while Integrations counts all buckets");
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/backups`);
    const destPanel = page.getByRole("region", { name: "Destinations" });
    await destPanel.getByRole("link", { name: "offsite" }).waitFor({ timeout: 20_000 });
    await destPanel.getByText("legacy-r2").waitFor();
    if ((await destPanel.getByRole("link", { name: "uploads" }).count()) !== 0) {
      fail("Backups Destinations listed app-serving connection uploads");
    }

    await page.goto(`${origin}/settings/integrations`);
    const storageSection = page.getByRole("region", { name: "Object storage" });
    await storageSection.getByText("3 buckets").waitFor({ timeout: 20_000 });
    await storageSection.getByRole("article", { name: "legacy-r2" }).waitFor();
    await shoot(page, "backup-connections-integrations");

    step("backup-connections: schedule sheet lists S3 connections first, disables app-serving ones, and seals inline");
    await page.goto(`${origin}/backups`);
    const schedules = page.getByRole("region", { name: "Schedules" });
    await schedules.getByRole("button", { name: "Edit this schedule" }).first().click();
    const schedule = page.getByRole("dialog", { name: "Change the schedule" });
    const uploadsRadio = schedule.getByRole("radio", { name: /Use Uploads \(s3\.eu-central-1\.amazonaws\.com\)/ });
    await uploadsRadio.waitFor({ timeout: 15_000 });
    if (!(await uploadsRadio.isDisabled())) fail("app-serving connection is not disabled in schedule picker");
    await schedule.getByText("Serves notes, whose key could delete the backups").waitFor();
    await schedule
      .getByRole("radio", {
        name: /Use legacy-r2 \(0123456789abcdef0123456789abcdef\.r2\.cloudflarestorage\.com\)/,
      })
      .waitFor();
    await schedule.getByText("Bucket legacy-backups · encrypted under old/").waitFor();

    // New connection from it prefills endpoint/region with empty bucket and keys.
    await schedule
      .locator(".backup-storage-option", { hasText: "Use Uploads" })
      .getByRole("button", { name: "New connection from it" })
      .click();
    const newConn = page.getByRole("dialog", { name: "Add a connection" });
    await newConn.waitFor();
    if ((await newConn.getByRole("combobox", { name: "Region" }).inputValue()) !== "eu-central-1") {
      fail("New connection from it did not prefill region");
    }
    if ((await newConn.getByRole("textbox", { name: "Bucket" }).inputValue()) !== "") {
      fail("New connection from it did not leave bucket empty");
    }
    await newConn.getByRole("button", { name: "Cancel" }).click();

    // Pick unused S3 connection offsite -> shows inline passphrase fields.
    await schedule.getByRole("radio", { name: /Use Offsite \(s3\.us-east-1\.amazonaws\.com\)/ }).check();
    await schedule.getByText("Keep this passphrase somewhere else").waitFor();
    await shoot(page, "backup-connections-schedule");

    // Apply the two-edit schedule plan (creating backup.destination + updating backup.policy).
    await schedule.getByLabel("I've kept a copy of the passphrase").check();
    await schedule.getByRole("button", { name: "Review plan" }).click();
    const plan = page.locator(".plan-sheet");
    await plan.getByRole("button", { name: /Apply plan/ }).click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();

    step("backup-connections: Backups and Connections count connections and legacy destinations accurately");
    await page.goto(`${origin}/backups`);
    await page.getByRole("region", { name: "Destinations" }).getByRole("link", { name: "offsite" }).waitFor({
      timeout: 20_000,
    });
    if (
      (await page.getByRole("region", { name: "Destinations" }).getByRole("link", { name: "uploads" }).count()) !== 0
    ) {
      fail("Backups Destinations listed app-serving connection uploads after scheduling");
    }
    await shoot(page, "backup-connections-backups");

    await page.goto(`${origin}/connections`);
    await page.getByText("2 connections · 1 backup destination · used by 1 app").waitFor({ timeout: 20_000 });
    await page
      .getByRole("row", { name: /offsite/ })
      .getByText("Backups")
      .waitFor();
    await page
      .getByRole("row", { name: /legacy-r2/ })
      .getByRole("button", { name: "Use as a connection" })
      .waitFor();
    await shoot(page, "backup-connections-list");

    // Verify 390px phone fit while legacy-r2 row (with Use as a connection) is present.
    await atWidth(page, { width: 390, height: 844 });
    for (const path of ["/backups", "/settings/integrations", "/connections"]) {
      await page.goto(`${origin}${path}`);
      await page.locator("main").first().waitFor();
      await page
        .locator('[aria-busy="true"]')
        .first()
        .waitFor({ state: "detached", timeout: 15_000 })
        .catch(() => {});
      const edge = await pastTheEdge(page);
      if (edge.wider > 0) fail(`on a 390px phone, ${path} is ${edge.wider}px wider than the page: ${edge.what}`);
    }
    await atWidth(page, { width: 1440, height: 900 });

    step("backup-connections: Connections shows backups under Used by and disables Share and Remove with why");
    await page.goto(`${origin}/connections/offsite`);
    const usedBy = page.getByRole("region", { name: "Used by" });
    await usedBy.getByRole("link", { name: "offsite" }).waitFor({ timeout: 20_000 });
    const shareSwitch = page.getByRole("switch", { name: /More than one app may use it/ });
    if (!(await shareSwitch.isDisabled())) fail("Share switch is enabled on a connection used by backups");
    await page.getByRole("button", { name: "Replace keys" }).click();
    const editConn = page.getByRole("dialog", { name: "Connection offsite" });
    const removeBtn = editConn.getByRole("button", { name: /Remove/ });
    if ((await removeBtn.getAttribute("aria-disabled")) !== "true") {
      fail("Remove is enabled on a connection used by backups");
    }
    await removeBtn.hover();
    await editConn.getByText(/Backup destination offsite uses it/).waitFor();
    await editConn.getByRole("button", { name: "Cancel" }).click();

    step("backup-connections: Use as a connection converts legacy-r2 without double-counting");
    await page.goto(`${origin}/settings/integrations`);
    const legacyCard = page.getByRole("article", { name: "legacy-r2" });
    await legacyCard.getByRole("button", { name: "Use as a connection" }).click();
    const convertPlan = page.locator(".plan-sheet");
    await convertPlan.getByRole("button", { name: /Apply plan/ }).click();
    await convertPlan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await convertPlan.getByRole("button", { name: "Done" }).click();
    await page.getByRole("region", { name: "Object storage" }).getByText("3 buckets").waitFor();
    if (
      (await page
        .getByRole("article", { name: "legacy-r2" })
        .getByRole("button", { name: "Use as a connection" })
        .count()) !== 0
    ) {
      fail("legacy-r2 still offers Use as a connection after conversion");
    }

    await page.goto(`${origin}/connections`);
    await page.getByText("3 connections · used by 1 app · 2 backup destinations").waitFor({ timeout: 20_000 });
    await page.getByRole("row", { name: /legacy-r2/ }).waitFor();
  },
};
