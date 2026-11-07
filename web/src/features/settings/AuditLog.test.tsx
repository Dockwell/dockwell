// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  AuditActor,
  type AuditEvent,
  AuditEventSchema,
  type ListAuditEventsRequest,
  Surface,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { onPhone, renderAt } from "../../test/render";
import { auditCSV, csvLine } from "./AuditLogPage";

const MINUTE = 60_000;
const ago = (minutes: number) => timestampFromDate(new Date(Date.now() - minutes * MINUTE));
const mira = { kind: ActorKind.USER, id: "usr_1", name: "Mira Chen" };

function anEvent(init: MessageInitShape<typeof AuditEventSchema>): AuditEvent {
  return create(AuditEventSchema, { surface: Surface.PANEL, time: ago(1), ...init });
}

const events = [
  anEvent({
    id: "evt_4",
    action: "token.create",
    actor: mira,
    target: { id: "tok_1", name: "GitHub Actions deploy" },
    subject: "usr_1",
    detail: 'token tok_1 "GitHub Actions deploy", read_write, expires 5 Feb 2027',
  }),
  anEvent({
    id: "evt_3",
    action: "auth.sign_in_failed",
    time: ago(5),
    address: "203.0.113.9",
    subject: "usr_1",
    detail: "authenticator code on localhost from 203.0.113.9: wrong code",
  }),
  anEvent({
    id: "evt_2",
    action: "change.applied",
    time: ago(10),
    actor: { kind: ActorKind.TOKEN, id: "tok_1", name: "GitHub Actions deploy" },
    surface: Surface.CLI,
    subject: "chg_1",
    detail: "Deploy web",
  }),
  anEvent({
    id: "evt_1",
    action: "install.create",
    time: ago(60 * 30),
    actor: { kind: ActorKind.DOCKWELL, name: "Dockwell" },
    surface: Surface.AUTOMATIC,
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
    expect(rows[2]).toHaveTextContent("GitHub Actions deploy");
    expect(rows[2]).toHaveTextContent("Panel");
    expect(rows[3]).toHaveTextContent("Not signed in");
    expect(rows[3]).toHaveTextContent("203.0.113.9");
    expect(rows[4]).toHaveTextContent("CLI");
    expect(screen.getByText("4 events in the last 24 hours. Pick a longer range to see more.")).toBeVisible();
    // The last 24 hours, from when the page opened.
    const since = listAuditEvents.mock.calls[0]?.[0].since;
    expect(since && Date.now() - timestampDate(since).getTime()).toBeGreaterThan(23 * 60 * MINUTE);
    await expectAccessible(container);
  });

  it("opens an event to all it recorded, and a change's to Activity", async () => {
    const user = userEvent.setup();
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events }) } });
    const table = await screen.findByRole("table", { name: "The audit log, newest first" });
    const toggle = within(table).getByRole("button", { name: /^Show details for change\.applied at / });
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(within(table).getByText("Deploy web", { selector: "dd" })).toBeVisible();
    // In the row, as what it's about, and again in its detail.
    expect(within(table).getAllByText("chg_1", { selector: "code" })).toHaveLength(2);
    expect(within(table).getByRole("link", { name: "Open in Activity" })).toHaveAttribute("href", "/changes/chg_1");
    await user.click(within(table).getByRole("button", { name: /^Show details for auth\.sign_in_failed/ }));
    expect(within(table).getByText("203.0.113.9", { selector: "dd code" })).toBeVisible();
    expect(within(table).queryByRole("link", { name: "Open in Activity" })).toBeNull();
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
    renderAt("/settings/audit-log", { audit: { listAuditEvents } });
    await screen.findByRole("table", { name: "The audit log, newest first" });
    await user.click(screen.getByRole("button", { name: "Export CSV" }));
    expect(await screen.findByText("Saved 4 events.")).toBeVisible();
    const exportCalls = listAuditEvents.mock.calls.filter((c) => c[0].pageSize === 500);
    expect(exportCalls.map((c) => c[0].pageToken)).toEqual(["", "more"]);
    const text = await saved[0]?.text();
    expect(text?.split("\n")[0]).toBe(
      "time,action,actor_kind,actor_id,actor_name,address,source,target_id,target_name,subject,detail,id",
    );
    expect(text?.split("\n")).toHaveLength(6);
  });

  it("words what a setting replaced, which it records as JSON", async () => {
    const setting = anEvent({
      id: "evt_s",
      action: "settings.name",
      actor: mira,
      detail: '{"value":"Acme","previous":"Acme servers"}',
    });
    const cleared = anEvent({ id: "evt_c", action: "settings.time_zone", actor: mira, detail: '{"previous":"UTC"}' });
    renderAt("/settings/audit-log", { audit: { listAuditEvents: () => ({ events: [setting, cleared] }) } });
    expect(await screen.findByText("Acme, which was Acme servers")).toBeVisible();
    expect(screen.getByText("Cleared, which was UTC")).toBeVisible();
    expect(auditCSV([setting])).toContain('"{""value"":""Acme"",""previous"":""Acme servers""}"');
  });

  it("writes CSV that a spreadsheet reads back", () => {
    expect(csvLine(["plain", 'a "quote"', "a,comma", "two\nlines"])).toBe('plain,"a ""quote""","a,comma","two\nlines"');
    const [, first, second] = auditCSV(events.slice(0, 2)).split("\n");
    expect(first).toMatch(
      /,token\.create,person,usr_1,Mira Chen,,panel,tok_1,GitHub Actions deploy,usr_1,"token tok_1 ""GitHub Actions deploy"", read_write, expires 5 Feb 2027",evt_4$/,
    );
    expect(second).toMatch(/,auth\.sign_in_failed,stranger,,,203\.0\.113\.9,panel,,,usr_1,/);
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
  });
});
