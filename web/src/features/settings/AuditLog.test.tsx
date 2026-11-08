// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  AuditActor,
  type AuditEvent,
  AuditEventSchema,
  ChangeState,
  EventKind,
  type ListAuditEventsRequest,
  PlanAction,
  Role,
  Surface,
  TokenScope,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { fileDay } from "../../lib/format";
import { expectAccessible } from "../../test/a11y";
import { onPhone, renderAt } from "../../test/render";
import { auditCSV, csvLine, MOST_EXPORTED } from "./AuditLogPage";

// Midday, so that the days these events fall on hold at any hour: near midnight UTC,
// 30 hours ago would be the day before yesterday.
vi.setSystemTime(new Date(2026, 10, 7, 12, 0));

const MINUTE = 60_000;
const ago = (minutes: number) => timestampFromDate(new Date(Date.now() - minutes * MINUTE));
const mira = { kind: ActorKind.USER, id: "usr_1", name: "Mira Chen" };
// The CLI's own token on the control server, which says so by what made it, not by its name.
const localToken = {
  kind: ActorKind.TOKEN,
  id: "tok_9",
  name: "Local access on web-1, read-write",
  localAccess: { account: "mira@web-1", scope: TokenScope.READ_WRITE },
};

function anEvent(init: MessageInitShape<typeof AuditEventSchema>): AuditEvent {
  return create(AuditEventSchema, { surface: Surface.PANEL, time: ago(1), ...init });
}

const deployWeb = {
  id: "chg_1",
  summary: "Deploy web",
  state: ChangeState.APPLIED,
  plan: { resources: [{ action: PlanAction.UPDATE, resource: { kind: "service", name: "web", scope: "production" } }] },
};

const events = [
  anEvent({
    id: "evt_4",
    action: "token.create",
    actor: mira,
    target: { id: "tok_1", name: "GitHub Actions deploy" },
    subject: "usr_1",
    detail: 'token tok_1 "GitHub Actions deploy", read_write, expires 5 Feb 2027',
    event: { kind: EventKind.ACCESS, action: "token.create", subject: { id: "tok_1", name: "GitHub Actions deploy" } },
  }),
  anEvent({
    id: "evt_3",
    action: "auth.sign_in_failed",
    time: ago(5),
    address: "203.0.113.9",
    subject: "usr_1",
    detail: "authenticator code on localhost from 203.0.113.9: wrong code",
    event: { kind: EventKind.ACCESS, action: "auth.sign_in_failed" },
  }),
  anEvent({
    id: "evt_2",
    action: "change.applied",
    time: ago(10),
    actor: localToken,
    surface: Surface.CLI,
    subject: "chg_1",
    detail: "Deploy web",
    event: { kind: EventKind.ACCESS, action: "change.applied", change: deployWeb },
  }),
  anEvent({
    id: "evt_1",
    action: "install.create",
    time: ago(60 * 30),
    actor: { kind: ActorKind.DOCKWELL, name: "Dockwell" },
    surface: Surface.AUTOMATIC,
    event: { kind: EventKind.ACCESS, action: "install.create" },
  }),
];

afterEach(async () => {
  await activateLocale("en");
});

describe("Audit log", () => {
  it("lists every event by day: who, what, on what, from where, and what it said", async () => {
    const listAuditEvents = vi.fn((req: ListAuditEventsRequest) => {
      void req;
      return { events };
    });
    const { container } = renderAt("/settings/audit-log", { audit: { listAuditEvents } });
    expect(await screen.findByRole("heading", { level: 1, name: "Audit log" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Audit log" }),
    ).toHaveAttribute("aria-current", "page");
    const table = await screen.findByRole("table", { name: "The audit log, newest first" });
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent(/^Today · /);
    expect(rows[2]).toHaveTextContent("Mira Chen");
    expect(rows[2]).toHaveTextContent("token.create");
    expect(rows[2]).toHaveTextContent("Panel");
    // Its detail is a sentence, as Activity says it.
    expect(rows[2]).toHaveTextContent("Created the API token GitHub Actions deploy");
    expect(rows[3]).toHaveTextContent("Not signed in");
    expect(rows[3]).toHaveTextContent("Couldn't sign in · 203.0.113.9");
    // The CLI's token reads as where it ran, and a change as where it landed.
    expect(within(rows[4] as HTMLElement).getByText("CLI on web-1")).toHaveAttribute(
      "title",
      "Local access token, read-write, tok_9, made by mira@web-1",
    );
    expect(rows[4]).toHaveTextContent("web · production");
    expect(rows[4]).toHaveTextContent("Applied Deploy web");
    expect(rows[4]).not.toHaveTextContent("chg_1");
    expect(rows[5]).toHaveTextContent(/^Yesterday · /);
    expect(rows[6]).toHaveTextContent("Set up this install");
    expect(screen.getByText("4 events in the last 24 hours. Pick a longer range to see more.")).toBeVisible();
    // The last 24 hours, from when the page opened.
    const since = listAuditEvents.mock.calls[0]?.[0].since;
    expect(since && Date.now() - timestampDate(since).getTime()).toBeGreaterThan(23 * 60 * MINUTE);
    await expectAccessible(container);
  });

  it("opens an event to all it recorded, and a change to who proposed and applied it, its state and its plan", async () => {
    const user = userEvent.setup();
    const getChange = vi.fn(() => ({
      change: {
        ...deployWeb,
        proposedBy: mira,
        proposedVia: Surface.PANEL,
        appliedBy: localToken,
        appliedVia: Surface.CLI,
        plan: { ...deployWeb.plan, servers: [{ serverId: "srv_1", serverName: "web-1" }] },
      },
    }));
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events }) }, changes: { getChange } });
    const table = await screen.findByRole("table", { name: "The audit log, newest first" });
    const toggle = within(table).getByRole("button", { name: /^Show details for change\.applied at / });
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const opened = table.querySelector(".audit-open") as HTMLElement;
    expect(within(opened).getByText("Applied Deploy web", { selector: "dd" })).toBeVisible();
    // To the second, with its zone named.
    expect(opened).toHaveTextContent(/When.*\d{2}:\d{2}:\d{2}/);
    expect(within(opened).getByText("chg_1", { selector: "dd code" })).toBeVisible();
    expect(await within(opened).findByText("Mira Chen · From the panel")).toBeVisible();
    expect(
      within(opened).getByText("Local access token, read-write, tok_9, made by mira@web-1 · From the CLI"),
    ).toBeVisible();
    expect(within(opened).getByText("web-1", { selector: "dd" })).toBeVisible();
    expect(within(opened).getByText("Applied", { selector: "dd" })).toBeVisible();
    expect(getChange).toHaveBeenCalledTimes(1);
    expect(within(opened).getByRole("link", { name: "Open in Activity" })).toHaveAttribute("href", "/changes/chg_1");
    await user.click(within(table).getByRole("button", { name: /^Show details for auth\.sign_in_failed/ }));
    const refused = table.querySelector(".audit-open") as HTMLElement;
    expect(within(refused).getByText("203.0.113.9", { selector: "dd code" })).toBeVisible();
    expect(within(refused).getByText("authenticator code on localhost from 203.0.113.9: wrong code")).toBeVisible();
    expect(within(table).queryByRole("link", { name: "Open in Activity" })).toBeNull();
  });

  it("names a token only by its name when nothing but its name says it's local access", async () => {
    const lookalike = anEvent({
      id: "evt_l",
      action: "token.use",
      actor: { kind: ActorKind.TOKEN, id: "tok_8", name: "Local access on prod-1, read-write" },
      surface: Surface.API,
      event: { kind: EventKind.ACCESS, action: "token.use" },
    });
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events: [lookalike] }) } });
    const table = await screen.findByRole("table", { name: "The audit log, newest first" });
    expect(within(table).getByText("Local access on prod-1, read-write")).toBeVisible();
    expect(within(table).queryByText(/CLI on/)).toBeNull();
    expect(table).toHaveTextContent("Used from a new address");
  });

  it("narrows the log by words, time, who and where from", async () => {
    const user = userEvent.setup();
    const listAuditEvents = vi.fn((req: ListAuditEventsRequest) => {
      void req;
      return { events: events.slice(0, 1) };
    });
    renderAt("/settings/audit-log", { audit: { listAuditEvents } });
    await screen.findByRole("table", { name: "The audit log, newest first" });
    await user.type(screen.getByRole("textbox", { name: "Filter events" }), "203.0.113.9");
    await waitFor(() => expect(listAuditEvents.mock.lastCall?.[0].query).toBe("203.0.113.9"));
    await user.selectOptions(screen.getByRole("combobox", { name: "Actor" }), "Not signed in");
    await user.selectOptions(screen.getByRole("combobox", { name: "Source" }), "CLI");
    await user.selectOptions(screen.getByRole("combobox", { name: "Time range" }), "Everything");
    await waitFor(() =>
      expect(listAuditEvents.mock.lastCall?.[0]).toMatchObject({
        query: "203.0.113.9",
        actor: AuditActor.STRANGER,
        surface: Surface.CLI,
      }),
    );
    expect(listAuditEvents.mock.lastCall?.[0].since).toBeUndefined();
    expect(await screen.findByText("1 event, all there is.")).toBeVisible();
  });

  it("reads on a page at a time", async () => {
    const user = userEvent.setup();
    const listAuditEvents = vi.fn((req: ListAuditEventsRequest) =>
      req.pageToken === "" ? { events: events.slice(0, 2), nextPageToken: "more" } : { events: events.slice(2) },
    );
    renderAt("/settings/audit-log", { audit: { listAuditEvents } });
    expect(await screen.findByText("The newest 2 events. Show more for older ones.")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show more" }));
    expect(await screen.findByText("4 events in the last 24 hours. Pick a longer range to see more.")).toBeVisible();
    expect(listAuditEvents.mock.calls[1]?.[0].pageToken).toBe("more");
  });

  it("exports what the filters keep, every page, as CSV", async () => {
    const user = userEvent.setup();
    const saved: Blob[] = [];
    vi.stubGlobal(
      "URL",
      Object.assign(URL, { createObjectURL: (b: Blob) => (saved.push(b), "blob:x"), revokeObjectURL: () => {} }),
    );
    const listAuditEvents = vi.fn((req: ListAuditEventsRequest) =>
      req.pageToken === "" ? { events: events.slice(0, 2), nextPageToken: "more" } : { events: events.slice(2) },
    );
    const names: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.download);
    });
    renderAt("/settings/audit-log", { audit: { listAuditEvents } });
    await screen.findByRole("table", { name: "The audit log, newest first" });
    await user.click(screen.getByRole("button", { name: "Export CSV" }));
    expect(await screen.findByText("Saved 4 events.")).toBeVisible();
    // Dated as the screen shows times, in the browser's own zone.
    expect(names).toEqual([`dockwell-audit-${fileDay(new Date())}.csv`]);
    const exportCalls = listAuditEvents.mock.calls.filter((c) => c[0].pageSize === 500);
    expect(exportCalls.map((c) => c[0].pageToken)).toEqual(["", "more"]);
    const text = await saved[0]?.text();
    expect(text?.split("\n")[0]).toBe(
      "time,action,actor_kind,actor_id,actor_name,address,source,target_id,target_name,subject,detail,id",
    );
    expect(text?.split("\n")).toHaveLength(6);
    // Each detail as the screen says it.
    expect(text).toContain(",Created the API token GitHub Actions deploy,evt_4");
    expect(text).toContain(",Applied Deploy web,evt_2");
  });

  it("saves the newest 10,000 at most, and says so", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: () => "blob:x", revokeObjectURL: () => {} }));
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const page = Array.from({ length: 500 }, (_, i) => anEvent({ id: `evt_${i}`, action: "auth.sign_in" }));
    renderAt("/settings/audit-log", {
      audit: {
        listAuditEvents: (req) => ({ events: req.pageSize === 500 ? page : page.slice(0, 1), nextPageToken: "more" }),
      },
    });
    await screen.findByRole("table", { name: "The audit log, newest first" });
    await user.click(screen.getByRole("button", { name: "Export CSV" }));
    expect(
      await screen.findByText("Saved the newest 10,000 events. Narrow the filters to save the rest."),
    ).toBeVisible();
    expect(MOST_EXPORTED).toBe(10_000);
  });

  it("words what a setting replaced, as Activity does, rather than the JSON it records", async () => {
    const setting = anEvent({
      id: "evt_s",
      action: "settings.name",
      actor: mira,
      detail: '{"value":"Acme","previous":"Acme servers"}',
      event: {
        kind: EventKind.SETTINGS,
        action: "settings.name",
        setting: { value: "Acme", previous: "Acme servers" },
      },
    });
    const cleared = anEvent({
      id: "evt_c",
      action: "settings.time_zone",
      actor: mira,
      detail: '{"previous":"UTC"}',
      event: { kind: EventKind.SETTINGS, action: "settings.time_zone", setting: { previous: "UTC" } },
    });
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events: [setting, cleared] }) } });
    expect(await screen.findByText("Named this install Acme, which was Acme servers")).toBeVisible();
    expect(
      screen.getByText("Cleared the time zone for new schedules, which was UTC, so they start in UTC"),
    ).toBeVisible();
    expect(screen.queryByText(/"previous"/)).toBeNull();
  });

  it("words a call refused for its role, and ownership moving", async () => {
    const refused = anEvent({
      id: "evt_r",
      action: "role.refuse",
      actor: mira,
      subject: "usr_1",
      detail:
        "/dockwell.v1.AuditService/ListAuditEvents, which needs an admin, from 203.0.113.9: the caller is a viewer",
    });
    const moved = anEvent({
      id: "evt_o",
      action: "owner.transfer",
      actor: { kind: ActorKind.DOCKWELL, name: "Dockwell" },
      surface: Surface.CLI,
      subject: "usr_2",
      target: { id: "usr_2", name: "Ada Park" },
      detail: "made ada@example.com the owner, in place of mira@example.com, who is an admin now",
    });
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events: [refused, moved] }) } });
    expect(await screen.findByText("Refused a call their role doesn't cover")).toBeVisible();
    expect(screen.getByText("Made Ada Park the owner")).toBeVisible();
  });

  it("words who was invited and who joined, as what and where", async () => {
    const member = (id: string, action: string, m: Record<string, unknown>) =>
      anEvent({ id, action, actor: mira, event: { kind: EventKind.ACCESS, action, member: m } });
    renderAt("/settings/audit-log", {
      audit: {
        listAuditEvents: () => ({
          events: [
            member("evt_i", "member.invite", { email: "ada@example.com", role: Role.DEVELOPER, projectName: "shop" }),
            member("evt_a", "member.invite", { email: "sam@example.com", role: Role.ADMIN }),
            member("evt_r", "member.invite_revoke", { email: "lin@example.com", role: Role.VIEWER }),
            member("evt_j", "member.join", { email: "ada@example.com", name: "Ada Lovelace", role: Role.VIEWER }),
          ],
        }),
      },
    });
    expect(await screen.findByText("Invited ada@example.com as a developer of shop")).toBeVisible();
    expect(screen.getByText("Invited sam@example.com as an admin")).toBeVisible();
    expect(screen.getByText("Revoked the invite for lin@example.com")).toBeVisible();
    expect(screen.getByText("Ada Lovelace joined as a viewer of every project")).toBeVisible();
  });

  it("writes CSV that a spreadsheet reads back", () => {
    expect(csvLine(["plain", 'a "quote"', "a,comma", "two\nlines"])).toBe('plain,"a ""quote""","a,comma","two\nlines"');
    const said = (e: AuditEvent) => `said ${e.action}`;
    const [, first, second] = auditCSV(events.slice(0, 2), said).split("\n");
    expect(first).toMatch(
      /,token\.create,person,usr_1,Mira Chen,,panel,tok_1,GitHub Actions deploy,usr_1,said token\.create,evt_4$/,
    );
    expect(second).toMatch(
      /,auth\.sign_in_failed,stranger,,,203\.0\.113\.9,panel,,,usr_1,said auth\.sign_in_failed,evt_3$/,
    );
  });

  it("writes a field a spreadsheet would run as a formula as text", () => {
    // A token, a person or a server can be named so: a token's name is free text.
    expect(
      csvLine(['=HYPERLINK("https://example.com")', "+1", "-1", "@SUM(A1)", "\tx", "\rx", "a=b", "2026-11-07"]),
    ).toBe(
      [
        `"'=HYPERLINK(""https://example.com"")"`,
        `"'+1"`,
        `"'-1"`,
        `"'@SUM(A1)"`,
        `"'\tx"`,
        `"'\rx"`,
        "a=b",
        "2026-11-07",
      ].join(","),
    );
    // Excel where ; separates splits a field there, and on a tab or a line, whatever the quotes say.
    expect(csvLine(["x;=1+1", "a\t+b", "a\r@c", "a\n-d", "a;b", "1;2"])).toBe(
      [`"x;'=1+1"`, `"a\t'+b"`, `"a\r'@c"`, `"a\n'-d"`, `"a;b"`, `"1;2"`].join(","),
    );
    expect(csvLine(["=a;=b"])).toBe(`"'=a;'=b"`);
    const named = anEvent({
      id: "evt_f",
      action: "token.create",
      actor: { kind: ActorKind.TOKEN, id: "tok_f", name: "=cmd|' /C calc'!A0" },
      target: { id: "tok_g", name: "@SUM(1+1)" },
    });
    const [, line] = auditCSV([named], () => "-2+3").split("\n");
    expect(line).toContain(`,"'=cmd|' /C calc'!A0",`);
    expect(line).toContain(`,"'@SUM(1+1)",`);
    expect(line).toContain(`,"'-2+3",evt_f`);
  });

  it("says when nothing matches, and when it can't read the log", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt("/settings/audit-log", {
      audit: {
        listAuditEvents: () => {
          if (fail) throw new ConnectError("the store doesn't answer", Code.Unavailable);
          return { events: [] };
        },
      },
    });
    expect(await screen.findByText("Dockwell can't read the audit log")).toBeVisible();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Nothing happened in this range. Pick a longer one to see more.")).toBeVisible();
  });

  it("stacks each event on a phone, each opening to its detail", async () => {
    const user = userEvent.setup();
    onPhone();
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events }) } });
    const toggle = await screen.findByRole("button", { name: /^Show details for token\.create at / });
    expect(screen.queryByRole("table")).toBeNull();
    // A third line says what happened.
    expect(screen.getByText("Created the API token GitHub Actions deploy")).toBeVisible();
    await user.click(toggle);
    expect(screen.getByText('token tok_1 "GitHub Actions deploy", read_write, expires 5 Feb 2027')).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/settings");
    expect(screen.getByRole("button", { name: "Export CSV" })).toBeVisible();
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "审计日志" })).toBeVisible();
    expect(screen.getByRole("button", { name: "导出 CSV" })).toBeVisible();
    expect(await screen.findByText("创建了 API 令牌 GitHub Actions deploy")).toBeVisible();
  });
});
