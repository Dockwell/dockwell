// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { join } from "node:path";
import { setTimeout } from "node:timers/promises";

import type { Flow } from "./flows.ts";
import { atWidth } from "./sheets.ts";

export const flow: Flow = {
  order: 1210,
  async run(page, { data, bin, origin, shoot, step }) {
    step("control backup: Settings warns when no off-site destination is selected");
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/settings/backups`);
    const panel = page.getByRole("region", { name: "Control plane", exact: true });
    await panel.getByText("Off-site copy needed").waitFor();
    await shoot(page, "control-backup-no-destination");

    const objects = new Map<string, Buffer>();
    const s3 = createServer(async (req, res) => {
      const url = new URL(req.url ?? "/", "http://127.0.0.1");
      const key = decodeURIComponent(url.pathname);
      if (req.method === "PUT") {
        if (objects.has(key) && req.headers["if-none-match"] === "*") {
          res.writeHead(412).end();
          return;
        }
        const parts: Buffer[] = [];
        for await (const part of req) parts.push(Buffer.from(part));
        objects.set(key, Buffer.concat(parts));
        res.writeHead(200).end();
        return;
      }
      if (req.method === "DELETE") {
        objects.delete(key);
        res.writeHead(204).end();
        return;
      }
      if (url.searchParams.has("versioning")) {
        res.writeHead(200, { "Content-Type": "application/xml" }).end("<VersioningConfiguration/>");
        return;
      }
      if (url.searchParams.get("list-type") === "2") {
        const prefix = url.searchParams.get("prefix") ?? "";
        const entries = [...objects.keys()]
          .map((path) => path.slice("/control-archive/".length))
          .filter((name) => name.startsWith(prefix))
          .map((name) => `<Contents><Key>${name}</Key></Contents>`);
        res
          .writeHead(200, { "Content-Type": "application/xml" })
          .end(`<ListBucketResult>${entries.join("")}</ListBucketResult>`);
        return;
      }
      const body = objects.get(key);
      if (!body) {
        res.writeHead(404).end();
      } else {
        res.writeHead(200, { "Content-Length": body.length }).end(req.method === "HEAD" ? undefined : body);
      }
    });
    await new Promise<void>((resolve, reject) => {
      s3.once("error", reject);
      s3.listen(0, "127.0.0.1", resolve);
    });
    try {
      const address = s3.address();
      if (!address || typeof address === "string") throw new Error("The storage fixture has no address");
      const endpoint = `http://127.0.0.1:${address.port}`;
      const cli = (...args: string[]) =>
        execFileSync(bin, [...args, "--data-dir", data], {
          encoding: "utf8",
          env: { ...process.env, DOCKWELL_URL: "", DOCKWELL_TOKEN: "", XDG_CONFIG_HOME: join(data, "backup-cli") },
        });
      const org = cli("resources", "list", "--kind", "project", "--json").match(/org_[0-9A-Za-z]+/)?.[0];
      if (!org) throw new Error("No organization is listed");
      const edit = join(data, "control-backup-destination.json");
      writeFileSync(
        edit,
        JSON.stringify({
          resource: { kind: "backup.destination", scope: org, name: "control-archive" },
          spec: {
            backupDestination: {
              endpoint,
              bucket: "control-archive",
              region: "us-east-1",
              prefix: "backups",
              accessKey: "AKIDCONTROL",
              secretKey: "fixture-s3-secret",
              passphrase: "fixture-application-backup-passphrase",
            },
          },
        }),
        { mode: 0o600 },
      );
      cli("login", "--local");
      cli("changes", "propose", "--summary", "Add control backup storage", "--edits", `@${edit}`, "-y");

      step("control backup: the owner selects an existing destination using their passkey");
      await panel.getByRole("button", { name: "Pick a destination" }).click();
      const picker = page.getByRole("dialog", { name: "Off-site destination" });
      await picker.getByLabel("Destination").selectOption("control-archive");
      await picker.getByRole("button", { name: "Save destination" }).click();
      await picker.waitFor({ state: "hidden" });
      await panel.getByText("control-archive", { exact: true }).waitFor();
      await panel.getByRole("button", { name: "Take one now" }).click();
      // Earlier flows may have kept a bundle on a second server. Honor the
      // scheduler's reported start time when the next backup is rate-limited.
      const queued = JSON.parse(cli("control", "backup", "status", "--json")) as { nextAt?: string };
      const nextAt = Date.parse(queued.nextAt ?? "");
      const deadline = Math.max(Date.now(), Number.isFinite(nextAt) ? nextAt : 0) + 45_000;
      let sealed: Buffer | undefined;
      while (!sealed && Date.now() < deadline) {
        sealed = [...objects.entries()].find(([key]) => key.endsWith(".dwcb"))?.[1];
        if (!sealed) await setTimeout(200);
      }
      if (!sealed?.subarray(0, 26).toString().startsWith("dockwell-control-backup-1")) {
        throw new Error("The storage fixture did not receive a sealed control plane backup");
      }
      await panel.getByText("Stored", { exact: true }).waitFor();
      await shoot(page, "control-backup");
      const status = cli("control", "backup", "status", "--json");
      if (!status.includes("control-archive") || status.includes("fixture-s3-secret")) {
        throw new Error("The CLI did not show the storage status without credentials");
      }

      step("control backup: the passphrase is shown with a passkey and confirmed before opening the recovery sheet");
      await panel.getByRole("button", { name: "Show passphrase", exact: true }).click();
      const pass = page.getByRole("dialog", { name: "Recovery passphrase" });
      await pass.getByRole("button", { name: "Show passphrase", exact: true }).click();
      const phrase = await pass.locator(".control-backup-secret code").innerText();
      if (!/^(?:[0-9A-HJKMNP-TV-Z]{4}-){7}[0-9A-HJKMNP-TV-Z]{4}$/.test(phrase)) {
        throw new Error("The generated passphrase did not have eight Crockford groups");
      }
      await pass
        .getByLabel("Type the passphrase to confirm")
        .fill(phrase.toLowerCase().replaceAll("-", " ").replaceAll("0", "o").replaceAll("1", "l"));
      await pass.getByRole("button", { name: "Confirm and open recovery sheet" }).click();
      const sheet = page.getByRole("dialog", { name: "Recovery sheet" });
      await sheet.getByText("AKIDCONTROL").waitFor();
      if ((await sheet.innerText()).includes("fixture-s3-secret") || (await sheet.innerText()).includes(phrase)) {
        throw new Error("The recovery sheet showed a secret");
      }
      await sheet.getByText(/dockwell open-backup /).waitFor();
      await sheet.getByText("Fetch the newest off-site copy").waitFor();
      if ((await sheet.innerText()).match(/\d{19}-cbk_|control backup status/)) {
        throw new Error(
          "The recovery sheet left a command which depends on a specific bundle or on Dockwell being online",
        );
      }
      await shoot(page, "control-backup-recovery");
      await sheet.getByRole("button", { name: "Close", exact: true }).last().click();

      await panel.getByRole("button", { name: "Set passphrase", exact: true }).click();
      const setting = page.getByRole("dialog", { name: "Set recovery passphrase" });
      await setting.getByLabel("Passphrase", { exact: true }).selectOption("custom");
      await setting.getByLabel("New passphrase").fill("cQ8#nRsW4@kH9zE2tM7w");
      await setting.getByText("Strength: meets the requirement").waitFor();
      await shoot(page, "control-backup-strength");
      await setting.getByRole("button", { name: "Close", exact: true }).last().click();

      const file = join(data, "control-backup-e2e.dwcb");
      const passFile = join(data, "control-backup-passphrase");
      writeFileSync(file, sealed, { mode: 0o600 });
      writeFileSync(passFile, ` ${phrase.toLowerCase().replaceAll("-", "_")} \n`, { mode: 0o600 });
      cli("open-backup", file, "--output", join(data, "opened-control-backup"), "--passphrase-file", passFile);

      await atWidth(page, { width: 390, height: 844 });
      await panel.getByText("Stored", { exact: true }).waitFor();
      await panel.getByRole("button", { name: "Recovery sheet" }).click();
      await sheet.waitFor();
      await sheet.getByRole("button", { name: "Print or save as PDF" }).waitFor();

      step("control backup: Overview has the off-site age, and take and show appear on Activity");
      await atWidth(page, { width: 1440, height: 900 });
      await page.goto(origin);
      await page.getByRole("region", { name: "Control plane backup" }).getByText("Newest off-site copy").waitFor();
      await page.goto(`${origin}/activity?kind=backups`);
      await page
        .getByRole("row", { name: /Requested a control plane backup/ })
        .first()
        .waitFor();
      await page
        .getByRole("row", { name: /Showed the recovery passphrase/ })
        .first()
        .waitFor();
      await page
        .getByRole("row", { name: /Sent a control plane backup to control-archive/ })
        .first()
        .waitFor();
      step("control backup: an earlier destination remains visible and a passkey can dismiss it");
      await page.goto(`${origin}/settings/backups`);
      await panel.getByRole("button", { name: "Change destination" }).click();
      await picker.getByLabel("Destination").selectOption("");
      await picker.getByRole("button", { name: "Remove destination" }).click();
      await picker.waitFor({ state: "hidden" });
      const earlier = panel.getByRole("list", { name: "Earlier destinations" });
      await earlier.getByText("control-archive", { exact: true }).waitFor();
      await shoot(page, "control-backup-earlier-destination");
      await earlier.getByRole("button", { name: "Dismiss" }).click();
      await earlier.waitFor({ state: "hidden" });
    } finally {
      await new Promise<void>((resolve) => s3.close(() => resolve()));
    }
  },
};
