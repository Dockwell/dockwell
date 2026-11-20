// SPDX-License-Identifier: AGPL-3.0-only

// Email alert channels through an install mail connection (#833, ADR 0099):
// - Add a channel offers Use the mail connection first when a usable SMTP connection exists,
//   hides inline SMTP settings, prefills From, and keeps Another server one step away.
// - Mail connections serving an app, shared, or whose password an app received show disabled
//   with why and their way out (New connection from it, or Paste a password made for alerts).
// - A connection-backed channel shows the connection by name with a link on its row and sheet.
// - Connections lists alert channels under Used by, keeps the one header count shape,
//   suppresses WantsMail, and disables Share and Remove with why.

import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { Flow } from "./flows.ts";
import { atWidth, pastTheEdge } from "./sheets.ts";

function fail(message: string): never {
  throw new Error(message);
}

const APP_SECRET = "re_e2e_app_mail_secret";
const OLD_SECRET = "brevo_e2e_old_mail_secret";
const ALERTS_SECRET = "pm_e2e_alerts_mail_token";

export const flow: Flow = {
  order: 98.3,
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

    const shotDir = process.env["DOCKWELL_ALERT_MAIL_SHOTS"];
    if (shotDir) mkdirSync(shotDir, { recursive: true });
    const capture = async (name: string) => {
      await shoot(page, name);
      if (!shotDir) return;
      const before = page.viewportSize();
      try {
        for (const width of [1440, 390] as const) {
          await atWidth(page, { width, height: width === 1440 ? 900 : 844 });
          await page.evaluate(() => window.scrollTo(0, 0));
          await page.waitForTimeout(100);
          await page.screenshot({ path: join(shotDir, `${name}-${width}.png`), fullPage: true });
        }
      } finally {
        if (before) await atWidth(page, before);
      }
    };

    const proposeEdits = (summary: string, edits: unknown[]) => {
      const args = ["changes", "propose", "--summary", summary, "-y"];
      edits.forEach((edit, i) => {
        const file = join(data, `alert-mail-edit-${Date.now()}-${i}.json`);
        writeFileSync(file, JSON.stringify(edit));
        args.push("--edits", `@${file}`);
      });
      cli(args);
    };

    // Flows before this one leave connections and backup destinations of their own,
    // so every count checked against the UI is read from the API.
    const counts = () => {
      const conns = (JSON.parse(cli(["connections", "list", "--json"])).connections ?? []) as { kind?: string }[];
      const dests = (JSON.parse(cli(["backups", "destinations", "--json"])).destinations ?? []) as {
        connection?: string;
      }[];
      const ownKeys = dests.filter((d) => !d.connection).length;
      return { connections: conns.length, ownKeys };
    };
    const base = counts();

    step("alert-mail: seed an app-serving, a password-held, and a usable mail connection");
    const org =
      cli(["resources", "list", "--kind", "project", "--json"]).match(/org_[0-9A-Za-z]+/)?.[0] ??
      fail("no organization is listed");
    const services = JSON.parse(cli(["services", "list", "--json"])) as {
      services: { id: string; resource: { name: string; scope: string } }[];
    };
    const notesInfo = services.services.find((s) => s.resource.name === "notes") ?? fail("notes service not found");
    const notesEnv = notesInfo.resource.scope;

    proposeEdits("Add mail connections for alert-mail e2e", [
      {
        resource: { kind: "connection", scope: org, name: "app-mail" },
        spec: {
          connection: {
            displayName: "App mail",
            kind: "LINK_KIND_SMTP",
            smtp: {
              host: "smtp.resend.com",
              port: 587,
              security: "SMTP_SECURITY_STARTTLS",
              user: "resend",
              password: APP_SECRET,
              sender: "notes@example.com",
              senderName: "Notes",
            },
            reach: "CONNECTION_REACH_EVERY",
          },
        },
      },
      {
        resource: { kind: "link", scope: notesEnv, name: "app-mail" },
        spec: {
          link: {
            label: "Mail",
            connection: "app-mail",
            kind: "LINK_KIND_SMTP",
            sets: [
              { name: "NOTES_SMTP_HOST", part: "LINK_PART_HOST" },
              { name: "NOTES_SMTP_PASSWORD", part: "LINK_PART_PASSWORD" },
            ],
          },
        },
      },
      {
        resource: { kind: "variable", scope: notesEnv, name: "NOTES_SMTP_HOST" },
        spec: {
          variable: {
            from: { connection: { scope: org, name: "app-mail", part: "LINK_PART_HOST", link: "app-mail" } },
          },
        },
      },
      {
        resource: { kind: "secret", scope: notesEnv, name: "NOTES_SMTP_PASSWORD" },
        spec: {
          secret: {
            from: { connection: { scope: org, name: "app-mail", part: "LINK_PART_PASSWORD", link: "app-mail" } },
          },
        },
      },
      {
        resource: { kind: "connection", scope: org, name: "old-mail" },
        spec: {
          connection: {
            displayName: "Old mail",
            kind: "LINK_KIND_SMTP",
            smtp: {
              host: "smtp-relay.brevo.com",
              port: 587,
              security: "SMTP_SECURITY_STARTTLS",
              user: "old-user",
              password: OLD_SECRET,
              sender: "old@example.com",
            },
            reach: "CONNECTION_REACH_EVERY",
          },
        },
      },
      {
        resource: { kind: "link", scope: notesEnv, name: "old-mail" },
        spec: {
          link: {
            label: "Old mail",
            connection: "old-mail",
            kind: "LINK_KIND_SMTP",
            sets: [{ name: "NOTES_OLD_SMTP_PASSWORD", part: "LINK_PART_PASSWORD" }],
          },
        },
      },
      {
        resource: { kind: "secret", scope: notesEnv, name: "NOTES_OLD_SMTP_PASSWORD" },
        spec: {
          secret: {
            from: { connection: { scope: org, name: "old-mail", part: "LINK_PART_PASSWORD", link: "old-mail" } },
          },
        },
      },
      {
        resource: { kind: "connection", scope: org, name: "alerts-mail" },
        spec: {
          connection: {
            displayName: "Alerts mail",
            kind: "LINK_KIND_SMTP",
            smtp: {
              host: "smtp.postmarkapp.com",
              port: 587,
              security: "SMTP_SECURITY_STARTTLS",
              user: ALERTS_SECRET,
              password: ALERTS_SECRET,
              sender: "alerts@example.com",
              senderName: "Dockwell",
            },
            reach: "CONNECTION_REACH_EVERY",
          },
        },
      },
    ]);

    // Disconnect old-mail from notes as DisconnectStorage does (keeping the password
    // as a plain secret on notes), so old-mail has no live link and held=true.
    proposeEdits("Disconnect old-mail so its password remains held", [
      {
        resource: { kind: "secret", scope: notesEnv, name: "NOTES_OLD_SMTP_PASSWORD" },
        spec: { secret: { value: OLD_SECRET } },
      },
      { resource: { kind: "link", scope: notesEnv, name: "old-mail" }, delete: true },
    ]);

    step(
      "alert-mail: Add a channel offers Use the mail connection first, disables unusable ones, and keeps Another server one step away",
    );
    await atWidth(page, { width: 1440, height: 900 });
    await page.goto(`${origin}/alerts/channels`);
    await page.getByRole("button", { name: "Add channel" }).click();
    const sheet = page.getByRole("dialog", { name: "Add a channel" });
    const useMail = sheet.getByRole("radio", { name: /Use the mail connection/ });
    await useMail.waitFor({ timeout: 20_000 });
    if (!(await useMail.isChecked())) fail("Use the mail connection is not pre-selected");
    await sheet.getByText("Alerts mail · smtp.postmarkapp.com:587 · STARTTLS").waitFor();
    if (
      (await sheet.getByRole("textbox", { name: "From", exact: true }).inputValue()) !== "Dockwell <alerts@example.com>"
    ) {
      fail(
        `From was not prefilled from the mail connection: ${await sheet.getByRole("textbox", { name: "From", exact: true }).inputValue()}`,
      );
    }
    if ((await sheet.getByLabel("Mail service").count()) !== 0) {
      fail("Mail service field showed while a mail connection was picked");
    }

    const appRadio = sheet.getByRole("radio", { name: /Use App mail \(smtp\.resend\.com\)/ });
    if (!(await appRadio.isDisabled())) fail("app-serving mail connection is not disabled");
    await sheet.getByText(/Serves .+, which could send mail as the alert sender/).waitFor();

    const oldRadio = sheet.getByRole("radio", { name: /Use Old mail \(smtp-relay\.brevo\.com\)/ });
    if (!(await oldRadio.isDisabled())) fail("password-held mail connection is not disabled");
    await sheet.getByText("An app received this connection's password").waitFor();

    await capture("alert-mail-add-connection");

    // Way out 1: New connection from it opens MailConnectionSheet prefilled with empty credentials.
    await sheet
      .locator(".alerts-mail-option", { hasText: "Use App mail" })
      .getByRole("button", { name: "New connection from it" })
      .click();
    const newConn = page.getByRole("dialog", { name: "Add a connection" });
    await newConn.waitFor();
    if ((await newConn.getByLabel("API key").inputValue()) !== "") {
      fail("New connection from it did not leave API key empty");
    }
    await newConn.getByRole("button", { name: "Cancel" }).click();
    await newConn.waitFor({ state: "detached" });

    // Way out 2: Paste a password made for alerts opens MailConnectionSheet in rotation mode.
    await sheet
      .locator(".alerts-mail-option", { hasText: "Use Old mail" })
      .getByRole("button", { name: "Paste a password made for alerts" })
      .click();
    const rotateConn = page.getByRole("dialog", { name: "Connection old-mail" });
    await rotateConn.waitFor();
    await rotateConn.getByText(/Paste a password made for alerts alone/).waitFor();
    await rotateConn.getByRole("button", { name: "Cancel" }).click();
    await rotateConn.waitFor({ state: "detached" });

    // Another server one step away reveals inline SMTP fields.
    await sheet.getByRole("radio", { name: /Another server/ }).check();
    await sheet.getByLabel("Mail service").waitFor();
    await capture("alert-mail-add-another");

    // Switch back to Use the mail connection, fill To, and plan the channel.
    await useMail.check();
    await sheet.getByRole("textbox", { name: "To", exact: true }).fill("ops@example.com");
    await sheet.getByRole("button", { name: "Review plan" }).click();
    const plan = page.locator(".plan-sheet");
    const apply = plan.getByRole("button", { name: /Apply plan/ });
    await apply.waitFor({ timeout: 20_000 });
    const planText = await plan.innerText();
    if (planText.includes(ALERTS_SECRET) || planText.includes(APP_SECRET) || planText.includes(OLD_SECRET)) {
      fail("alert channel plan leaked an SMTP password");
    }
    await apply.click();
    await plan.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 });
    await plan.getByRole("button", { name: "Done" }).click();

    step("alert-mail: channel row and edit sheet show the mail connection by name with a link");
    const table = page.getByRole("table", { name: "Alert channels" });
    const row = table.getByRole("row", { name: /alerts-mail/ });
    await row.waitFor({ timeout: 20_000 });
    const rowLink = row.getByRole("link", { name: "alerts-mail" });
    if ((await rowLink.getAttribute("href")) !== "/connections/alerts-mail") {
      fail(`channel row link href = ${await rowLink.getAttribute("href")}, want /connections/alerts-mail`);
    }
    await capture("alert-mail-channels-row");

    // Open the channel sheet by clicking the row outside the link.
    await row.getByText("ops@example.com").click();
    const editSheet = page.getByRole("dialog", { name: /^Change / });
    await editSheet.waitFor();
    const sheetLink = editSheet.getByRole("link", { name: "alerts-mail" });
    if ((await sheetLink.getAttribute("href")) !== "/connections/alerts-mail") {
      fail(`channel sheet link href = ${await sheetLink.getAttribute("href")}, want /connections/alerts-mail`);
    }
    await editSheet.getByRole("button", { name: "Cancel" }).click();

    step(
      "alert-mail: Connections lists alert channels under Used by, keeps one count shape, and locks Share and Remove",
    );
    const after = counts();
    if (after.connections !== base.connections + 3 || after.ownKeys !== base.ownKeys) {
      fail(`unexpected connection counts: got ${JSON.stringify(after)}, base was ${JSON.stringify(base)}`);
    }
    await page.goto(`${origin}/connections`);
    const connsText = after.connections === 1 ? "1 connection" : `${after.connections} connections`;
    const ownText =
      after.ownKeys === 0
        ? ""
        : after.ownKeys === 1
          ? " · 1 backup destination with its own keys"
          : ` · ${after.ownKeys} backup destinations with their own keys`;
    const leadPattern = new RegExp(`^${connsText}${ownText} · used by .*alert channel`);
    await page.getByText(leadPattern).first().waitFor({ timeout: 20_000 });
    await page
      .getByRole("row", { name: /alerts-mail/ })
      .getByText("Alerts", { exact: true })
      .waitFor();
    await capture("alert-mail-connections-list");

    await page.goto(`${origin}/connections/alerts-mail`);
    const usedBy = page.getByRole("region", { name: "Used by" });
    await usedBy.getByText("ops@example.com").waitFor({ timeout: 20_000 });
    await usedBy.getByRole("link", { name: "Manage in Alerts" }).waitFor();
    const shareSwitch = page.getByRole("switch", { name: /More than one app may use it/ });
    if (!(await shareSwitch.isDisabled())) fail("Share switch is enabled on a mail connection used by alerts");
    await page.getByText("Alerts use it, so it can't be shared with apps").waitFor();
    await capture("alert-mail-connection-detail");

    await page.getByRole("button", { name: "Replace password" }).click();
    const editConn = page.getByRole("dialog", { name: "Connection alerts-mail" });
    const removeBtn = editConn.getByRole("button", { name: /Remove/ });
    if ((await removeBtn.getAttribute("aria-disabled")) !== "true") {
      fail("Remove is enabled on a mail connection used by alerts");
    }
    await removeBtn.hover();
    await editConn.getByText(/Alert channel .+ uses it\. Remove it on Alerts first\./).waitFor();
    await editConn.getByRole("button", { name: "Cancel" }).click();

    step("alert-mail: verify 0px horizontal overflow at 390px phone width");
    await atWidth(page, { width: 390, height: 844 });
    for (const path of ["/alerts/channels", "/connections", "/connections/alerts-mail"]) {
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
    await page.goto(`${origin}/alerts/channels`);
    await page.getByRole("button", { name: "Add channel" }).click();
    await page.getByRole("dialog", { name: "Add a channel" }).waitFor();
    const sheetEdge = await pastTheEdge(page);
    if (sheetEdge.wider > 0) {
      fail(`on a 390px phone, Add a channel is ${sheetEdge.wider}px wider than the page: ${sheetEdge.what}`);
    }
    await atWidth(page, { width: 1440, height: 900 });
  },
};
