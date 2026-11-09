// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  ActorSchema,
  ChangeState,
  EventKind,
  FieldOp,
  HealthState,
  type ListEventsRequest,
  PlanAction,
  PlannedResourceSchema,
  ResourceSchema,
  StepProgressSchema,
  StepState,
  Surface,
  TerminalEndReason,
  type UndoChangeRequest,
  type WatchChangeResponse,
  type WatchEventsRequest,
  type WatchEventsResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterAll, afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import {
  applied,
  auditEvent,
  changeEvent,
  mira,
  moved,
  shopPlanned,
  since,
  snapshot,
  webPlanned,
} from "../../test/changes";
import { type Api, feed, onPhone, quietWatch, renderAt, untilAborted } from "../../test/render";

const HOUR = 3_600_000;
// Midday, so that what these say about "today" and "yesterday" holds at any hour:
// an hour ago is yesterday at 00:30, and 26 hours ago is two days back at 01:00.
vi.setSystemTime(new Date(2026, 10, 4, 12, 0));

async function* quietLogs(_req: unknown, ctx: { signal: AbortSignal }) {
  await untilAborted(ctx.signal);
  yield* [];
}

function feedOf(first: WatchEventsResponse) {
  return async function* watch(_req: unknown, ctx: { signal: AbortSignal }) {
    yield first;
    await untilAborted(ctx.signal);
  };
}

// Midday, so that what these say about "today" and "yesterday" holds at any hour:
// an hour ago is yesterday at 00:30, and 26 hours ago is two days back at 01:00.
vi.setSystemTime(new Date(2026, 10, 4, 12, 0));

const shop = applied(shopPlanned(), new Date(Date.now() - 2 * 60_000));
const shopEvent = changeEvent(shop);
const signIn = auditEvent({
  id: "evt_sign_in",
  action: "auth.sign_in",
  time: timestampFromDate(new Date(Date.now() - HOUR)),
});
const joined = auditEvent({
  id: "evt_join",
  kind: EventKind.SERVER,
  action: "server.join",
  time: timestampFromDate(new Date(Date.now() - 26 * HOUR)),
  actor: { kind: ActorKind.DOCKWELL, name: "Dockwell" },
  surface: Surface.AUTOMATIC,
  subject: { id: "srv_1", name: "hz-1" },
});
const token = auditEvent({
  id: "evt_token",
  action: "token.create",
  time: timestampFromDate(new Date(Date.now() - 27 * HOUR)),
  subject: { id: "tok_1", name: "GitHub Actions deploy" },
});

function renderActivity(watch: ReturnType<typeof feed<WatchEventsResponse>>, api: Api = {}, path = "/activity") {
  return renderAt(path, { ...api, events: { watchEvents: watch.stream, ...api.events } });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

afterAll(() => {
  vi.useRealTimers();
});

describe("Activity", () => {
  it("lists the timeline by day, newest first, and follows it live", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn, joined, token]));
    renderActivity(watch);

    expect(await screen.findByRole("heading", { level: 1, name: "Activity" })).toBeVisible();
    const today = await screen.findByRole("region", { name: "Today" });
    const rows = within(within(today).getByRole("table")).getAllByRole("row");
    expect(rows.map((r) => r.textContent)).toEqual([
      "EventByWhen",
      expect.stringMatching(/^Create the project shop2 to createMCMira Chen2 min ago/),
      expect.stringMatching(/^Signed inFrom the panelMCMira Chen1 h ago/),
    ]);
    const yesterday = screen.getByRole("region", { name: "Yesterday" });
    expect(yesterday).toHaveTextContent("hz-1 joined as the control server");
    expect(yesterday).toHaveTextContent("Created the API token GitHub Actions deploy");
    expect(within(yesterday).getByText(/· 2 events$/)).toBeVisible();
    await expectAccessible(document.body);

    // A new plan arrives, then goes when it is discarded.
    const planned = changeEvent(
      moved(shopPlanned(), {
        id: "chg_qa",
        summary: "Create the environment qa",
        createTime: timestampFromDate(new Date()),
      }),
    );
    act(() => watch.push(since([planned])));
    const row = await screen.findByRole("row", { name: /Create the environment qa/ });
    expect(row).toHaveTextContent("Planned");
    act(() => watch.push(since([], ["chg_qa"])));
    await waitFor(() => expect(screen.queryByRole("row", { name: /Create the environment qa/ })).toBeNull());
  });

  it("opens a change beside the list, with its plan, and undoes it with a plan of its own", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn]));
    const undos: UndoChangeRequest[] = [];
    const undo = moved(shopPlanned(), {
      id: "chg_undo",
      summary: "Undo Create the project shop",
      reverts: "chg_shop",
    });
    if (undo.plan?.summary) undo.plan.summary.create = 0;
    if (undo.plan?.summary) undo.plan.summary.destroy = 2;
    for (const r of undo.plan?.resources ?? []) r.action = 5;
    const { router } = renderActivity(watch, {
      changes: {
        getChange: () => ({ change: shop }),
        undoChange: (req) => {
          undos.push(req);
          return { change: undo };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    await user.click(await screen.findByRole("row", { name: /Create the project shop/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ event: "chg_shop" }));
    const detail = await screen.findByRole("region", { name: "Create the project shop" });
    expect(detail).toHaveTextContent("Mira Chen");
    expect(detail).toHaveTextContent("The panel");
    expect(detail).toHaveTextContent("3f9a1c2b");
    expect(await within(detail).findByText("production")).toBeVisible();
    await expectAccessible(document.body);

    await user.click(within(detail).getByRole("button", { name: "Undo this change" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(undos).toEqual([expect.objectContaining({ id: "chg_shop" })]);
    await within(sheet).findByText("to delete");
    expect(sheet).toHaveTextContent("Undo Create the project shop");
    expect(sheet).toHaveTextContent("2 to delete");
  });

  it("says why Dockwell rolled a change back, where a person reads the rollback", async () => {
    const user = userEvent.setup();
    const said =
      "web runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.";
    const watch = feed<WatchEventsResponse>();
    watch.push(
      snapshot([
        changeEvent(
          moved(shop, {
            id: "chg_back",
            summary: "Roll back Deploy web",
            rollback: true,
            reverts: "chg_shop",
            because: said,
          }),
        ),
      ]),
    );
    renderActivity(watch);
    await user.click(await screen.findByText("Roll back Deploy web"));
    const why = await screen.findByText(/Dockwell made this change because of what failed/);
    expect(why.closest(".plan-because")).toHaveTextContent(said);
  });

  it("leads from a change to a secret, and from a reveal, to the variables and secrets", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    const rotation = moved(shop, { id: "chg_rotate", summary: "Rotate the database password" });
    if (rotation.plan) {
      rotation.plan.resources = [
        create(PlannedResourceSchema, {
          id: "sec_db",
          resource: { kind: "secret", scope: "env_prod", name: "DB_PASSWORD" },
          action: PlanAction.UPDATE,
          changes: [{ path: "value", op: FieldOp.MODIFIED, sensitive: true }],
        }),
      ];
    }
    const reveal = auditEvent({
      id: "evt_reveal",
      action: "secret.reveal",
      time: timestampFromDate(new Date(Date.now() - 2 * HOUR)),
      subject: { id: "sec_db", name: "DB_PASSWORD", scope: "env_staging" },
    });
    watch.push(snapshot([changeEvent(rotation), reveal]));
    const { router } = renderActivity(watch, {
      changes: { getChange: () => ({ change: rotation }), watchChange: feed<WatchChangeResponse>().stream },
      variables: { listVariables: () => ({ environmentId: "env_prod" }) },
    });

    await user.click(await screen.findByRole("row", { name: /Revealed the secret DB_PASSWORD/ }));
    // One link, to the environment the secret lives in.
    expect(await screen.findAllByRole("link", { name: "Open the variables and secrets" })).toHaveLength(1);
    await user.click(screen.getByRole("link", { name: "Open the variables and secrets" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/variables"));
    expect(router.state.location.search).toEqual({ environment: "env_staging" });

    await act(() => router.navigate({ to: "/activity", search: { event: "chg_rotate" } }));
    const detail = await screen.findByRole("region", { name: "Rotate the database password" });
    expect(await within(detail).findByText("DB_PASSWORD")).toBeVisible();
    expect(within(detail).getAllByRole("link", { name: "Open the variables and secrets" })).toHaveLength(1);
    await user.click(within(detail).getByRole("link", { name: "Open the variables and secrets" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ environment: "env_prod" }));
  });

  it("says what a server's public address was set to, and what it was, so it can be put back", async () => {
    const on = { id: "srv_1", name: "hz-1" };
    const events = [
      auditEvent({
        id: "evt_clear",
        action: "server.public_address_clear",
        subject: on,
        publicAddress: { previous: "198.51.100.8" },
      }),
      auditEvent({
        id: "evt_change",
        action: "server.public_address",
        time: timestampFromDate(new Date(Date.now() - HOUR)),
        subject: on,
        publicAddress: { address: "198.51.100.8", previous: "198.51.100.7" },
      }),
      auditEvent({
        id: "evt_set",
        action: "server.public_address",
        time: timestampFromDate(new Date(Date.now() - 2 * HOUR)),
        subject: on,
        publicAddress: { address: "198.51.100.7" },
      }),
    ];
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot(events));
    renderActivity(watch);
    await screen.findByRole("row", { name: /Cleared the public address of hz-1, which was 198\.51\.100\.8/ });
    await screen.findByRole("row", {
      name: /Set the public address of hz-1 to 198\.51\.100\.8, which was 198\.51\.100\.7/,
    });
    await screen.findByRole("row", { name: /Set the public address of hz-1 to 198\.51\.100\.7(?!,)/ });
  });

  it("says what the install's settings were set to, and what they were, so they can be put back", async () => {
    const at = (hours: number) => timestampFromDate(new Date(Date.now() - hours * HOUR));
    const events = [
      auditEvent({ id: "evt_n3", action: "settings.name", setting: { setting: "name", previous: "Acme web" } }),
      auditEvent({
        id: "evt_n2",
        action: "settings.name",
        time: at(1),
        setting: { setting: "name", value: "Acme web", previous: "Acme" },
      }),
      auditEvent({ id: "evt_n1", action: "settings.name", time: at(2), setting: { setting: "name", value: "Acme" } }),
      auditEvent({
        id: "evt_z3",
        action: "settings.time_zone",
        time: at(3),
        setting: { setting: "time_zone", previous: "UTC" },
      }),
      auditEvent({
        id: "evt_z2",
        action: "settings.time_zone",
        time: at(4),
        setting: { setting: "time_zone", value: "UTC", previous: "Europe/Amsterdam" },
      }),
      auditEvent({
        id: "evt_z1",
        action: "settings.time_zone",
        time: at(5),
        setting: { setting: "time_zone", value: "Europe/Amsterdam" },
      }),
    ];
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot(events));
    renderActivity(watch);
    await screen.findByRole("row", { name: /Cleared this install's name, which was Acme web/ });
    await screen.findByRole("row", { name: /Named this install Acme web, which was Acme(?! web)/ });
    await screen.findByRole("row", { name: /Named this install Acme(?!,| web)/ });
    await screen.findByRole("row", {
      name: /Cleared the time zone for new schedules, which was UTC, so they start in UTC/,
    });
    await screen.findByRole("row", { name: /Set the time zone for new schedules to UTC, which was Europe\/Amsterdam/ });
    await screen.findByRole("row", { name: /Set the time zone for new schedules to Europe\/Amsterdam(?!,)/ });
  });

  it("says who had a terminal on a server, as whom, for how long and how it ended, and never what was typed", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    const on = { id: "srv_1", name: "hz-1" };
    const opened = new Date(Date.now() - 2 * HOUR);
    const opening = auditEvent({
      id: "evt_open",
      kind: EventKind.SERVER,
      action: "terminal.open",
      time: timestampFromDate(opened),
      subject: on,
      terminal: { id: "trm_1", title: "root@hz-1", openTime: timestampFromDate(opened) },
    });
    const closing = auditEvent({
      id: "evt_close",
      kind: EventKind.SERVER,
      action: "terminal.close",
      time: timestampFromDate(new Date(opened.getTime() + 12 * 60_000)),
      subject: on,
      terminal: {
        id: "trm_1",
        title: "root@hz-1",
        openTime: timestampFromDate(opened),
        end: { reason: TerminalEndReason.IDLE, time: timestampFromDate(new Date(opened.getTime() + 12 * 60_000)) },
      },
    });
    const unconfirmed = auditEvent({
      id: "evt_unconfirmed_terminal",
      kind: EventKind.SERVER,
      action: "terminal.open_unconfirmed",
      time: timestampFromDate(new Date(Date.now() - 3 * HOUR)),
      subject: on,
      terminal: { error: "asked for a passkey, and none came" },
    });
    const failed = auditEvent({
      id: "evt_open_failed",
      kind: EventKind.SERVER,
      action: "terminal.open_failed",
      time: timestampFromDate(new Date(Date.now() - 4 * HOUR)),
      subject: on,
      terminal: { id: "trm_0", error: "hz-1 has 8 terminals open" },
    });
    const inContainer = auditEvent({
      id: "evt_shell",
      kind: EventKind.SERVER,
      action: "terminal.open",
      time: timestampFromDate(new Date(Date.now() - 5 * HOUR)),
      subject: on,
      terminal: { id: "trm_2", title: "node@web-1", container: "web-1" },
    });
    watch.push(snapshot([closing, opening, unconfirmed, failed, inContainer]));
    renderActivity(watch);
    await screen.findByRole("row", { name: /Opened a shell in web-1 on hz-1/ });

    await screen.findByRole("row", { name: /Opened a terminal on hz-1, as root@hz-1/ });
    await screen.findByRole("row", { name: /Asked for a passkey to open a terminal on hz-1, and none came/ });
    await screen.findByRole("row", { name: /Couldn't open a terminal on hz-1/ });
    await user.click(await screen.findByRole("row", { name: /Closed a terminal on hz-1, open 12 min/ }));
    const detail = await screen.findByRole("region", { name: /Closed a terminal on hz-1/ });
    expect(within(detail).getByText("trm_1")).toBeInTheDocument();
    expect(within(detail).getByText("root@hz-1")).toBeInTheDocument();
    expect(
      within(detail).getByText(
        "Nothing was typed into it for an hour, so its shell hung up, rather than leave a root shell open.",
      ),
    ).toBeInTheDocument();
  });

  it("is one row for the same file read again and again, saying how often, and the day counts each", async () => {
    const user = userEvent.setup();
    const on = { id: "srv_1", name: "hz-1" };
    const read = (id: string, minutesAgo: number, path = "/etc/os-release", actor?: typeof signIn.actor) =>
      auditEvent({
        id,
        action: "file.read",
        time: timestampFromDate(new Date(Date.now() - minutesAgo * 60_000)),
        subject: { ...on, path },
        ...(actor ? { actor } : {}),
      });
    renderAt("/activity", {
      events: {
        watchEvents: feedOf(
          snapshot([
            read("evt_r1", 1),
            read("evt_r2", 2),
            read("evt_r3", 4),
            // Another file, then the same one again but long after: rows of their own.
            read("evt_other", 5, "/etc/hosts"),
            read("evt_r4", 6),
            read("evt_r5", 30),
          ]),
        ),
        listEvents: () => ({}),
      },
    });
    const rows = await screen.findAllByRole("row", { name: /Read \/etc\/os-release on hz-1/ });
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent("3 times");
    expect(rows[1]).not.toHaveTextContent("times");
    expect(screen.getByRole("row", { name: /Read \/etc\/hosts on hz-1/ })).toBeVisible();
    expect(screen.getByText(/· 6 events$/)).toBeVisible();
    await user.click(rows[0] as HTMLElement);
    const detail = await screen.findByRole("region", { name: /Read \/etc\/os-release on hz-1/ });
    expect(detail).toHaveTextContent("Times3, from 11:56 to 11:59");
  });

  it("keeps repeated reads on their own local calendar day", async () => {
    const today = new Date(Date.now());
    today.setHours(0, 2, 0, 0);
    const read = (id: string, at: Date) =>
      auditEvent({
        id,
        action: "file.read",
        time: timestampFromDate(at),
        subject: { id: "srv_1", name: "hz-1", path: "/etc/os-release" },
      });
    renderAt("/activity", {
      events: {
        watchEvents: feedOf(
          snapshot([read("evt_today", today), read("evt_yesterday", new Date(today.getTime() - 4 * 60_000))]),
        ),
        listEvents: () => ({}),
      },
    });
    expect(await screen.findAllByRole("row", { name: /Read \/etc\/os-release on hz-1/ })).toHaveLength(2);
    for (const day of ["Today", "Yesterday"]) {
      const section = screen.getByRole("region", { name: day });
      expect(within(section).getByText(/· 1 event$/)).toBeVisible();
      expect(within(section).getByRole("row", { name: /Read \/etc\/os-release on hz-1/ })).not.toHaveTextContent(
        "times",
      );
    }
  });

  it("keeps reads from different surfaces separate so their From label stays true", async () => {
    renderAt("/activity", {
      events: {
        watchEvents: feedOf(
          snapshot(
            [Surface.PANEL, Surface.CLI].map((surface, i) =>
              auditEvent({
                id: `evt_read_${i}`,
                action: "file.read",
                surface,
                time: timestampFromDate(new Date(Date.now() - i * 60_000)),
                subject: { id: "srv_1", name: "hz-1", path: "/etc/os-release" },
              }),
            ),
          ),
        ),
        listEvents: () => ({}),
      },
    });
    const rows = await screen.findAllByRole("row", { name: /Read \/etc\/os-release on hz-1/ });
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("From the panel");
    expect(rows[1]).toHaveTextContent("From the CLI");
    for (const row of rows) expect(row).not.toHaveTextContent("times");
  });

  it("shows what was read of a server and what wasn't, and leads to the file", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    const on = { id: "srv_1", name: "hz-1" };
    const read = auditEvent({
      id: "evt_read",
      action: "file.read",
      time: timestampFromDate(new Date(Date.now() - HOUR)),
      subject: { ...on, path: "/etc/nginx/nginx.conf" },
    });
    const refused = auditEvent({
      id: "evt_refuse",
      action: "file.refuse",
      time: timestampFromDate(new Date(Date.now() - 2 * HOUR)),
      subject: { ...on, path: "/root/.ssh/id_rsa" },
    });
    const failed = auditEvent({
      id: "evt_failed",
      action: "file.save_failed",
      time: timestampFromDate(new Date(Date.now() - 3 * HOUR)),
      subject: { ...on, path: "/etc/cron.d/job" },
    });
    const saved = auditEvent({
      id: "evt_saved",
      action: "file.save",
      time: timestampFromDate(new Date(Date.now() - 4 * HOUR)),
      subject: { ...on, path: "/etc/nginx/conf.d/app.conf" },
    });
    const unknown = auditEvent({
      id: "evt_unknown",
      action: "file.save_unknown",
      time: timestampFromDate(new Date(Date.now() - 5 * HOUR)),
      subject: { ...on, path: "/etc/hosts" },
    });
    const wayIn = auditEvent({
      id: "evt_way_in",
      action: "file.save_way_back_in",
      time: timestampFromDate(new Date(Date.now() - 6 * HOUR)),
      subject: { ...on, path: "/root/.ssh/authorized_keys" },
    });
    const unconfirmed = auditEvent({
      id: "evt_unconfirmed",
      action: "file.save_unconfirmed",
      time: timestampFromDate(new Date(Date.now() - 7 * HOUR)),
      subject: { ...on, path: "/etc/crontab" },
    });
    const notApplied = auditEvent({
      id: "evt_not_applied",
      action: "change.apply_unconfirmed",
      time: timestampFromDate(new Date(Date.now() - 8 * HOUR)),
      subject: { id: "chg_1" },
    });
    watch.push(snapshot([read, refused, failed, saved, unknown, wayIn, unconfirmed, notApplied]));
    const { router } = renderActivity(watch);

    await screen.findByRole("row", { name: /Couldn't open \/root\/\.ssh\/id_rsa on hz-1/ });
    await screen.findByRole("row", { name: /Couldn't save \/etc\/cron\.d\/job on hz-1/ });
    await screen.findByRole("row", { name: /May have saved \/etc\/hosts on hz-1/ });
    await screen.findByRole("row", { name: /Asked for a passkey to save \/etc\/crontab on hz-1, and none came/ });
    await screen.findByRole("row", { name: /Asked for a passkey to apply chg_1, and none came/ });
    await screen.findByRole("row", {
      name: /Saved \/root\/\.ssh\/authorized_keys on hz-1, which can let someone back in/,
    });
    await screen.findByRole("row", { name: /Saved \/etc\/nginx\/conf\.d\/app\.conf on hz-1/ });
    await user.click(await screen.findByRole("row", { name: /Read \/etc\/nginx\/nginx\.conf on hz-1/ }));
    await user.click(await screen.findByRole("link", { name: "Open the file" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/servers/srv_1/files"));
    expect(router.state.location.search).toEqual({ file: "/etc/nginx/nginx.conf" });
  });

  it("says what was uploaded and downloaded, how large, and leads to the folder", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    const on = { id: "srv_1", name: "hz-1" };
    const at = (hours: number) => timestampFromDate(new Date(Date.now() - hours * HOUR));
    watch.push(
      snapshot([
        auditEvent({
          id: "evt_up",
          action: "file.upload",
          time: at(1),
          subject: { ...on, path: "/var/www/backup.sql", sizeBytes: 3n << 20n },
        }),
        auditEvent({
          id: "evt_unpacked",
          action: "file.upload",
          time: at(2),
          subject: { ...on, path: "/var/www/theme.zip", sizeBytes: 2048n, toPath: "/var/www/theme" },
        }),
        auditEvent({
          id: "evt_up_way_in",
          action: "file.upload",
          time: at(3),
          subject: { ...on, path: "/root/.ssh/authorized_keys", sizeBytes: 100n, wayBackIn: true },
        }),
        auditEvent({
          id: "evt_up_none",
          action: "file.upload_unconfirmed",
          time: at(4),
          subject: { ...on, path: "/etc/cron.d/job" },
        }),
        auditEvent({ id: "evt_up_failed", action: "file.upload_failed", time: at(5), subject: { ...on, path: "/x" } }),
        auditEvent({
          id: "evt_down",
          action: "file.download",
          time: at(6),
          subject: { ...on, path: "/var/log/syslog", sizeBytes: 1536n },
        }),
        auditEvent({ id: "evt_down_dir", action: "file.download", time: at(7), subject: { ...on, path: "/srv/site" } }),
      ]),
    );
    const { router } = renderActivity(watch);
    await screen.findByRole("row", {
      name: /Uploaded \/var\/www\/theme\.zip to hz-1, 2 KB, and unpacked it into \/var\/www\/theme/,
    });
    await screen.findByRole("row", {
      name: /Uploaded \/root\/\.ssh\/authorized_keys to hz-1, which can let someone back in/,
    });
    await screen.findByRole("row", {
      name: /Asked for a passkey to upload \/etc\/cron\.d\/job to hz-1, and none came/,
    });
    await screen.findByRole("row", { name: /Couldn't upload \/x to hz-1/ });
    await screen.findByRole("row", { name: /Downloaded \/var\/log\/syslog from hz-1, 1\.5 KB/ });
    await screen.findByRole("row", { name: /Downloaded \/srv\/site from hz-1(?!,)/ });
    await user.click(await screen.findByRole("row", { name: /Uploaded \/var\/www\/backup\.sql to hz-1, 3 MB/ }));
    await user.click(await screen.findByRole("link", { name: "Open the folder" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/servers/srv_1/files"));
    expect(router.state.location.search).toEqual({ path: "/var/www" });
  });

  it("marks a setup that wrong codes ended as worth a look", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(
      snapshot([
        auditEvent({
          id: "evt_ended",
          action: "totp.setup_discarded",
          time: timestampFromDate(new Date(Date.now() - HOUR)),
        }),
      ]),
    );
    renderActivity(watch, {});
    const row = await screen.findByRole("row", { name: /Ended an authenticator app's setup after 5 wrong codes/ });
    expect(row.querySelector(".dw-tone-warning, [data-tone='warning']")).not.toBeNull();
  });

  it.each([
    ["passkey.add_with_recovery_code", "a recovery code"],
    ["passkey.add_with_authenticator_code", "an authenticator code"],
  ])("marks %s, a passkey added after %s, as worth a look", async (action, code) => {
    const watch = feed<WatchEventsResponse>();
    const added = auditEvent({
      id: "evt_recovered",
      action,
      time: timestampFromDate(new Date(Date.now() - HOUR)),
      subject: { id: "pky_new", name: "Unknown laptop" },
    });
    watch.push(snapshot([added]));
    renderActivity(watch, {});
    const row = await screen.findByRole("row", {
      name: new RegExp(`Added the passkey Unknown laptop after signing in with ${code}`),
    });
    expect(row.querySelector(".dw-tone-warning, [data-tone='warning']")).not.toBeNull();
  });

  it("says what became of a backup's copy in words of its own", async () => {
    const said = (action: string, id: string, error = "") =>
      auditEvent({
        id,
        kind: EventKind.BACKUP,
        action,
        time: timestampFromDate(new Date(Date.now() - HOUR)),
        subject: { id: "vol_1", name: "orders" },
        backup: { id: "bkp_1", volume: "orders", serverName: "hz-1", destination: "r2", error },
      });
    const watch = feed<WatchEventsResponse>();
    watch.push(
      snapshot([
        said("backup.fetch_failed", "evt_1", "AccessDenied"),
        said("backup.server_copy_removed", "evt_2"),
        said("backup.copy_kept", "evt_3", "AccessDenied"),
      ]),
    );
    renderActivity(watch);
    expect(await screen.findByRole("row", { name: /A backup of orders couldn't be fetched from r2/ })).toBeVisible();
    expect(
      screen.getByRole("row", { name: /Removed a backup of orders from its server, and kept it on r2/ }),
    ).toBeVisible();
    expect(screen.getByRole("row", { name: /r2 kept a backup of orders that was to go/ })).toBeVisible();
  });

  it("shows a backup that failed as worth a look, and says why", async () => {
    const user = userEvent.setup();
    const failed = auditEvent({
      id: "evt_backup",
      kind: EventKind.BACKUP,
      action: "backup.failed",
      time: timestampFromDate(new Date(Date.now() - HOUR)),
      subject: { id: "vol_1", name: "orders" },
      backup: {
        id: "bkp_1",
        volume: "orders",
        serverName: "hz-1",
        error: "The disk is full. Free some space, then try again.",
      },
    });
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([failed]));
    renderActivity(watch);
    const row = await screen.findByRole("row", { name: /The backup of orders failed/ });
    await user.click(row);
    expect(await screen.findByText("The disk is full. Free some space, then try again.")).toBeVisible();
    expect(screen.getByText("hz-1")).toBeVisible();
  });

  it("filters by kind and actor, in its address, reading each filter's own page", async () => {
    const user = userEvent.setup();
    const pages: ListEventsRequest[] = [];
    const { router } = renderAt("/activity", {
      events: {
        async *watchEvents(_req, ctx) {
          yield snapshot([shopEvent, signIn]);
          await new Promise((resolve) => ctx.signal.addEventListener("abort", resolve));
        },
        listEvents: (req) => {
          pages.push(req);
          return { events: req.kinds.length > 0 ? [shopEvent] : [shopEvent, signIn] };
        },
      },
    });
    await screen.findByRole("row", { name: /Signed in/ });

    await user.click(screen.getByRole("radio", { name: "Changes" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ kind: "changes" }));
    await waitFor(() => expect(pages.at(-1)?.kinds).toEqual([EventKind.CHANGE]));
    await waitFor(() => expect(screen.queryByRole("row", { name: /Signed in/ })).toBeNull());

    await user.selectOptions(screen.getByRole("combobox", { name: "Actor" }), "usr_1");
    await waitFor(() => expect(pages.at(-1)?.actorId).toBe("usr_1"));
    await user.selectOptions(screen.getByRole("combobox", { name: "Actor" }), "dockwell");
    await waitFor(() => expect(pages.at(-1)?.actorKind).toBe(ActorKind.DOCKWELL));
    expect(pages.at(-1)?.actorId).toBe("");
  });

  it("narrows to a server from its address, and clears it", async () => {
    const user = userEvent.setup();
    const pages: ListEventsRequest[] = [];
    const { router } = renderAt("/activity?server=srv_1", {
      events: {
        async *watchEvents(_req, ctx) {
          yield snapshot([joined, signIn]);
          await new Promise((resolve) => ctx.signal.addEventListener("abort", resolve));
        },
        listEvents: (req) => {
          pages.push(req);
          return { events: [joined] };
        },
      },
    });
    await screen.findByRole("row", { name: /hz-1 joined/ });
    expect(pages[0]?.serverId).toBe("srv_1");
    expect(screen.queryByRole("row", { name: /Signed in/ })).toBeNull();
    expect(screen.getByText("hz-1", { selector: ".dw-tag-value" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Remove the server filter" }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    // Without a filter, the timeline is the tab's own stream again, every event of it.
    expect(await screen.findByRole("row", { name: /Signed in/ })).toBeVisible();
  });

  it("shows earlier events from the page after the snapshot", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent], "page-2"));
    const pages: ListEventsRequest[] = [];
    renderActivity(watch, {
      events: {
        listEvents: (req) => {
          pages.push(req);
          return { events: [token] };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Show earlier events" }));
    expect(await screen.findByRole("row", { name: /Created the API token/ })).toBeVisible();
    expect(pages).toEqual([expect.objectContaining({ pageToken: "page-2" })]);
    expect(screen.queryByRole("button", { name: "Show earlier events" })).toBeNull();
  });

  it("leaves out an earlier page asked for under the filter before", async () => {
    const user = userEvent.setup();
    let answer: () => void = () => {};
    renderAt("/activity", {
      events: {
        async *watchEvents(_req, ctx) {
          yield snapshot([shopEvent], "page-2");
          await untilAborted(ctx.signal);
        },
        listEvents: async (req) => {
          if (req.pageToken === "page-2") await new Promise<void>((resolve) => (answer = resolve));
          // The changes' own first page.
          if (req.pageToken === "" && req.kinds.length > 0) return { events: [shopEvent], nextPageToken: "changes-2" };
          return { events: req.pageToken === "page-2" ? [token] : [], nextPageToken: "" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Show earlier events" }));
    await user.click(screen.getByRole("radio", { name: "Changes" }));
    await screen.findByRole("row", { name: /Create the project shop/ });
    // The page for every kind arrives once the list shows only changes.
    act(() => answer());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole("row", { name: /Created the API token/ })).toBeNull();
    // The changes carry on from their own snapshot.
    expect(screen.getByRole("button", { name: "Show earlier events" })).toBeEnabled();
  });

  it("keeps the earlier pages it loaded across a reconnect, and pages on below them", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    const user = userEvent.setup();
    const first = feed<WatchEventsResponse>();
    const second = feed<WatchEventsResponse>();
    first.push(snapshot([shopEvent], "page-2"));
    const watches = [first, second];
    const pages: string[] = [];
    renderAt("/activity", {
      events: {
        // The shell's own watch of changes, a page of one, keeps out of the timeline's turn.
        watchEvents: (req, ctx) =>
          req.pageSize === 1 ? quietWatch(req, ctx) : (watches.shift() ?? second).stream(req, ctx),
        listEvents: (req) => {
          pages.push(req.pageToken);
          return req.pageToken === "page-2" ? { events: [token], nextPageToken: "page-3" } : { events: [] };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Show earlier events" }));
    await screen.findByRole("row", { name: /Created the API token/ });

    // The watch drops, and its new snapshot reaches back to what was shown.
    const newer = auditEvent({ id: "evt_new", action: "auth.sign_in" });
    second.push(snapshot([newer, shopEvent], "page-2-again"));
    act(() => first.fail(new ConnectError("the control plane restarted", Code.Unavailable)));
    await waitFor(() => expect(screen.getAllByRole("row", { name: /Signed in/ })).toHaveLength(1), { timeout: 3000 });
    expect(screen.getByRole("row", { name: /Created the API token/ })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show earlier events" }));
    await waitFor(() => expect(pages).toEqual(["page-2", "page-3"]));
  });

  it("links a failed deploy to the logs of what it deployed", async () => {
    const planned = webPlanned();
    const service = planned.plan?.resources[0];
    if (service?.resource) service.resource.kind = "service";
    const failedChange = moved(applied(planned, new Date(Date.now() - 60_000)), {
      state: ChangeState.FAILED,
      error: "web didn't start",
    });
    const { router } = renderAt(`/activity?event=${failedChange.id}`, {
      events: { watchEvents: feedOf(snapshot([changeEvent(failedChange)])) },
      changes: { getChange: () => ({ change: failedChange }) },
      logs: {
        listLogSources: () => ({ sources: [{ id: "aaaaaaaaaaaa", changeId: failedChange.id }] }),
        watchLogs: quietLogs,
      },
    });
    const link = await screen.findByRole("link", { name: "Read this deploy's logs" });
    expect(link).toHaveAttribute("href", `/apps/${service?.id}/logs?change=${failedChange.id}`);
    await userEvent.setup().click(link);
    await waitFor(() => expect(router.state.location.pathname).toBe(`/apps/${service?.id}/logs`));
  });

  it("links a deploy that went well to the logs of what it started too, as Logs for r57 does", async () => {
    const planned = webPlanned();
    const service = planned.plan?.resources[0];
    if (service?.resource) service.resource.kind = "service";
    const done = applied(planned, new Date(Date.now() - 60_000));
    renderAt(`/activity?event=${done.id}`, {
      events: { watchEvents: feedOf(snapshot([changeEvent(done)])) },
      changes: { getChange: () => ({ change: done }) },
      logs: {
        listLogSources: () => ({ sources: [{ id: "aaaaaaaaaaaa", changeId: done.id }] }),
        watchLogs: quietLogs,
      },
    });
    const link = await screen.findByRole("link", { name: "Read this deploy's logs" });
    expect(link).toHaveAttribute("href", `/apps/${service?.id}/logs?change=${done.id}`);
  });

  it("finds a deploy's logs when it finishes after being selected before its container existed", async () => {
    const planned = webPlanned();
    const service = planned.plan?.resources[0];
    if (service?.resource) service.resource.kind = "service";
    const done = applied(planned, new Date(Date.now() - 60_000));
    const running = moved(done, { state: ChangeState.APPLYING });
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([changeEvent(running)]));
    let hasContainer = false;
    const listLogSources = vi.fn(() => ({
      sources: hasContainer ? [{ id: "aaaaaaaaaaaa", changeId: done.id }] : [],
    }));
    renderAt(
      `/activity?event=${running.id}`,
      {
        events: { watchEvents: watch.stream },
        changes: { getChange: () => ({ change: hasContainer ? done : running }) },
        logs: { listLogSources, watchLogs: quietLogs },
      },
      // Match the real panel: the empty answer is fresh for thirty seconds.
      { staleTime: 30_000 },
    );
    await waitFor(() => expect(listLogSources).toHaveBeenCalledTimes(1));
    await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    expect(screen.queryByRole("link", { name: "Read this deploy's logs" })).toBeNull();
    hasContainer = true;
    act(() => watch.push(since([changeEvent(done)])));
    const link = await screen.findByRole("link", { name: "Read this deploy's logs" });
    expect(link).toHaveAttribute("href", `/apps/${service?.id}/logs?change=${done.id}`);
    expect(listLogSources).toHaveBeenCalledTimes(2);
  });

  it("offers no logs of a deploy that never made a container", async () => {
    const planned = webPlanned();
    const service = planned.plan?.resources[0];
    if (service?.resource) service.resource.kind = "service";
    const failedChange = moved(applied(planned, new Date(Date.now() - 60_000)), {
      state: ChangeState.FAILED,
      error: "web didn't start",
    });
    const listLogSources = vi.fn(() => ({ sources: [{ id: "aaaaaaaaaaaa", changeId: "chg_another" }] }));
    renderAt(`/activity?event=${failedChange.id}`, {
      events: { watchEvents: feedOf(snapshot([changeEvent(failedChange)])) },
      changes: { getChange: () => ({ change: failedChange }) },
      logs: { listLogSources, watchLogs: quietLogs },
    });
    await waitFor(() => expect(listLogSources).toHaveBeenCalled());
    await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    expect(screen.queryByRole("link", { name: "Read this deploy's logs" })).toBeNull();
  });

  it("offers no logs of a deploy whose server went away", async () => {
    const planned = webPlanned();
    const service = planned.plan?.resources[0];
    if (service?.resource) service.resource.kind = "service";
    const failedChange = moved(applied(planned, new Date(Date.now() - 60_000)), {
      state: ChangeState.FAILED,
      error: "hz-1 went offline during this step.",
      steps: [create(StepProgressSchema, { state: StepState.FAILED, lost: true })],
    });
    const getChange = vi.fn(() => ({ change: failedChange }));
    renderAt(`/activity?event=${failedChange.id}`, {
      events: { watchEvents: feedOf(snapshot([changeEvent(failedChange)])) },
      changes: { getChange },
      logs: {
        listLogSources: () => ({ sources: [{ id: "aaaaaaaaaaaa", changeId: failedChange.id }] }),
        watchLogs: quietLogs,
      },
    });
    await waitFor(() => expect(getChange).toHaveBeenCalled());
    await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
    expect(screen.queryByRole("link", { name: "Read this deploy's logs" })).toBeNull();
  });

  it("still offers them while a server can't say", async () => {
    const planned = webPlanned();
    const service = planned.plan?.resources[0];
    if (service?.resource) service.resource.kind = "service";
    const failedChange = moved(applied(planned, new Date(Date.now() - 60_000)), { state: ChangeState.FAILED });
    renderAt(`/activity?event=${failedChange.id}`, {
      events: { watchEvents: feedOf(snapshot([changeEvent(failedChange)])) },
      changes: { getChange: () => ({ change: failedChange }) },
      logs: {
        listLogSources: () => ({ servers: [{ serverId: "srv_1", serverName: "hz-1", message: "no agent" }] }),
        watchLogs: quietLogs,
      },
    });
    expect(await screen.findByRole("link", { name: "Read this deploy's logs" })).toBeVisible();
  });

  it("counts only the steps that ran to the end", async () => {
    const watch = feed<WatchEventsResponse>();
    const steps = [StepState.DONE, StepState.FAILED, StepState.SKIPPED].map((state, i) =>
      create(StepProgressSchema, {
        serverId: "srv_1",
        resourceId: `res_${i}`,
        state,
        finishTime: timestampFromDate(new Date()),
      }),
    );
    watch.push(snapshot([changeEvent(moved(shopPlanned(), { state: ChangeState.APPLYING, steps }))]));
    renderActivity(watch);
    expect(await screen.findByRole("row", { name: /Create the project shop/ })).toHaveTextContent("1 of 3 steps done");
  });

  it("names the resource it narrows to, asking only for its kind", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent]));
    const kinds: string[] = [];
    renderActivity(
      watch,
      {
        resources: {
          listResources: (req) => {
            kinds.push(req.kind);
            return {
              resources: [create(ResourceSchema, { id: "prj_shop", resource: { kind: "project", name: "shop" } })],
            };
          },
        },
      },
      "/activity?resource=prj_shop",
    );
    expect(await screen.findByText("shop", { selector: ".dw-tag-value" })).toBeVisible();
    expect(kinds).toEqual(["project"]);
  });

  it("opens a change's own page, /changes/<id>, over Activity, to review and apply it", async () => {
    const user = userEvent.setup();
    // Proposed by the CLI's local access, which sends a person here for a destructive plan.
    const destructive = moved(webPlanned(), {
      proposedBy: create(ActorSchema, {
        kind: ActorKind.TOKEN,
        id: "tok_local",
        name: "Local access on hz-1, read-write",
      }),
      proposedVia: Surface.CLI,
    });
    if (destructive.plan) destructive.plan.destructive = true;
    const applying = moved(destructive, { state: ChangeState.APPLYING, appliedBy: mira, appliedVia: Surface.PANEL });
    // The tab's one watch starts from the change as it stands, and says so when it moves on.
    let now = destructive;
    let moved_on = () => {};
    const movedOn = new Promise<void>((resolve) => (moved_on = resolve));
    const applies: string[] = [];
    const { router } = renderAt("/changes/chg_web", {
      events: {
        async *watchEvents(_req, ctx) {
          yield snapshot([changeEvent(now)]);
          await movedOn;
          yield since([changeEvent(now)]);
          await untilAborted(ctx.signal);
        },
      },
      changes: {
        getChange: () => ({ change: now }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: (req) => {
          applies.push(req.fingerprint);
          now = applying;
          moved_on();
          return { change: applying };
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(apply).toBeDisabled();
    await user.type(within(sheet).getByLabelText("Type a1b2c3d4 to apply it"), "a1b2c3d4");
    await user.click(apply);
    await waitFor(() => expect(applies).toEqual(["a1b2c3d4e5f60718293a4b5c6d7e8f90"]));
    expect(screen.getByRole("link", { name: "Activity" })).toHaveAttribute("aria-current", "page");

    const sheetApplying = await screen.findByRole("dialog", { name: "Applying" });
    await user.click(within(sheetApplying).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);
    await waitFor(() => expect(router.state.location.pathname).toBe("/activity"));
    expect(router.state.location.search).toEqual({ event: "chg_web" });
    // Beside the list, the change names who applied it and from where, and
    // the CLI's token that proposed it.
    const detail = await screen.findByRole("region", { name: "Update web" });
    await waitFor(() => expect(detail).toHaveTextContent("Mira Chen"));
    expect(detail).toHaveTextContent("The panel");
    expect(detail).toHaveTextContent("Proposed byLocal access on hz-1, read-writeAPI token · From the CLI");
  });

  it("says when nothing has happened", async () => {
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([]));
    renderActivity(watch);
    expect(await screen.findByText("Nothing here yet")).toBeVisible();
    expect(screen.getByText("Changes, servers joining and sign-ins show up here as they happen.")).toBeVisible();
  });

  it("is a line of time on a phone, where a change opens its plan and undo is one tap", async () => {
    onPhone();
    const user = userEvent.setup();
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn]));
    const undos: string[] = [];
    renderActivity(watch, {
      changes: {
        getChange: () => ({ change: shop }),
        undoChange: (req) => {
          undos.push(req.id);
          throw new Error("not this time");
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    const today = await screen.findByRole("region", { name: "Today" });
    expect(within(today).queryByRole("table")).toBeNull();
    const items = within(today).getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Create the project shop");
    expect(items[0]).toHaveTextContent("Mira Chen2 to create");
    await expectAccessible(document.body);

    await user.click(within(items[0] as HTMLElement).getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(undos).toEqual(["chg_shop"]));
    const failed = await screen.findByRole("dialog", { name: "The plan didn't work" });
    await user.click(within(failed).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);

    await user.click(within(items[0] as HTMLElement).getByRole("button", { name: "Create the project shop" }));
    expect(await screen.findByRole("dialog", { name: "Applied" })).toBeVisible();
  });

  it("reads in Chinese", async () => {
    await activateLocale("zh-CN");
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([shopEvent, signIn]));
    renderActivity(watch);
    expect(await screen.findByRole("heading", { level: 1, name: "动态" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "今天" })).toBeVisible();
    expect(screen.getByRole("row", { name: /登录了/ })).toHaveTextContent("来自面板");
    // A change's summary is its author's words, and stays as written.
    expect(screen.getByRole("row", { name: /Create the project shop/ })).toHaveTextContent("2 项创建");
    expect(screen.getByRole("radiogroup", { name: "事件类型" })).toBeVisible();
  });
});

describe("Activity's restarts", () => {
  it("counts them in Chinese in the plan's words", async () => {
    await activateLocale("zh-CN");
    try {
      const restart = applied(shopPlanned(), new Date(Date.now() - 2 * 60_000));
      restart.id = "chg_restart";
      restart.summary = "Restart web";
      restart.restart = true;
      if (restart.plan?.summary) {
        restart.plan.summary.create = 0;
        restart.plan.summary.replace = 1;
      }
      renderAt("/activity", {
        events: { watchEvents: feedOf(snapshot([changeEvent(restart)])), listEvents: () => ({}) },
      });
      expect(await screen.findByRole("row", { name: /Restart web/ })).toHaveTextContent("1 项待重启");
    } finally {
      await activateLocale("en");
    }
  });

  it("counts what a restart replaces as restarted, not replaced", async () => {
    const restart = applied(shopPlanned(), new Date(Date.now() - 2 * 60_000));
    restart.id = "chg_restart";
    restart.summary = "Restart web";
    restart.restart = true;
    if (restart.plan?.summary) {
      restart.plan.summary.create = 0;
      restart.plan.summary.replace = 1;
    }
    renderAt("/activity", {
      events: { watchEvents: feedOf(snapshot([changeEvent(restart)])), listEvents: () => ({}) },
    });
    const row = await screen.findByRole("row", { name: /Restart web/ });
    expect(row).toHaveTextContent("1 to restart");
    expect(row).not.toHaveTextContent("to replace");
  });
});

describe("Activity's health", () => {
  const health = (id: string, state: HealthState, flips = 0, sinceMs?: number) =>
    auditEvent({
      id,
      kind: EventKind.HEALTH,
      action: "service.health",
      time: timestampFromDate(new Date(Date.now() - HOUR)),
      subject: { id: "svc_web", name: "web" },
      health: {
        serverId: "srv_1",
        serverName: "hz-1",
        state,
        previous: HealthState.HEALTHY,
        flips,
        output: "GET / answered 503",
        ...(sinceMs === undefined ? {} : { since: timestampFromDate(new Date(Date.now() - HOUR - sinceMs)) }),
      },
    });

  it.each([
    [HealthState.UNHEALTHY, "web turned unhealthy on hz-1", "Unhealthy"],
    [HealthState.NOT_RUNNING, "web stopped running on hz-1", "Not running"],
    [HealthState.PAUSED, "web was paused on hz-1", "Paused"],
    [HealthState.HEALTHY, "web is healthy again on hz-1", "Healthy"],
    [HealthState.NO_CHECK, "web is running again on hz-1", "Running"],
  ])("says what happened, in words of its own, for state %s", async (state, title, word) => {
    const { container } = renderAt("/activity", {
      events: { watchEvents: feedOf(snapshot([health("evt_1", state)])), listEvents: () => ({}) },
    });
    const row = await screen.findByRole("row", { name: new RegExp(title) });
    expect(row).toHaveTextContent(word);
    // Its word is the state, which a screen reader hears alone: not "Stopped: Paused".
    expect(row.querySelector(".dw-tl-meta .dw-sr")).toBeNull();
    await expectAccessible(container);
  });

  it("marks a pause as its state is, grey, and what went wrong in its tone", async () => {
    renderAt("/activity", {
      events: {
        watchEvents: feedOf(snapshot([health("evt_1", HealthState.PAUSED), health("evt_2", HealthState.NOT_RUNNING)])),
        listEvents: () => ({}),
      },
    });
    const paused = await screen.findByRole("row", { name: /web was paused/ });
    expect(paused.querySelector(".activity-mark")).not.toHaveClass("dw-tone-warning");
    expect(paused.querySelector(".dw-status")).toHaveClass("dw-tone-idle");
    const down = screen.getByRole("row", { name: /web stopped running/ });
    expect(down.querySelector(".activity-mark")).toHaveClass("dw-tone-critical");
  });

  it("is dated when it happened, which the server tells once it has lasted", async () => {
    renderAt("/activity", {
      events: {
        watchEvents: feedOf(snapshot([health("evt_1", HealthState.NOT_RUNNING, 0, 2 * HOUR)])),
        listEvents: () => ({}),
      },
    });
    const row = await screen.findByRole("row", { name: /web stopped running/ });
    // Two hours before it was told, an hour ago: a server tells within 35 s, so only a test is this far off.
    expect(row).toHaveTextContent("3 h ago");
  });

  it("is one line for a flapping app, saying how often it changed, and over how long", async () => {
    renderAt("/activity", {
      events: {
        watchEvents: feedOf(snapshot([health("evt_1", HealthState.UNHEALTHY, 12, 40 * 60_000)])),
        listEvents: () => ({}),
      },
    });
    const rows = await screen.findAllByRole("row", { name: /web keeps changing on hz-1/ });
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("It changed 12 times in 40 min.");
    expect(rows[0]).not.toHaveTextContent("since the last of these");
    // Its time is the last it was told, as it goes on.
    expect(rows[0]).toHaveTextContent("1 h ago");
    await expectAccessible(document.body);
  });

  it("reads in Chinese as the plan does, with no space before what ends the phrase", async () => {
    await activateLocale("zh-CN");
    try {
      renderAt("/activity", {
        events: {
          watchEvents: feedOf(snapshot([health("evt_1", HealthState.UNHEALTHY, 12, 54_000)])),
          listEvents: () => ({}),
        },
      });
      const row = (await screen.findAllByRole("row")).find((r) => r.textContent?.includes("次"));
      expect(row).toHaveTextContent("54 秒内变化了 12 次");
    } finally {
      await activateLocale("en");
    }
  });

  it("says what an event from before the server said since when counted", async () => {
    renderAt("/activity", {
      events: { watchEvents: feedOf(snapshot([health("evt_1", HealthState.UNHEALTHY, 12)])), listEvents: () => ({}) },
    });
    const row = await screen.findByRole("row", { name: /web turned unhealthy/ });
    expect(row).toHaveTextContent("It changed 12 more times since the last of these.");
  });

  it("filters to health, and asks for it", async () => {
    const asked: ListEventsRequest[] = [];
    const user = userEvent.setup();
    renderAt("/activity", {
      events: {
        watchEvents: async function* (_req: WatchEventsRequest, ctx: { signal: AbortSignal }) {
          yield snapshot([]);
          await untilAborted(ctx.signal);
        },
        listEvents: (req) => {
          asked.push(req);
          return {};
        },
      },
    });
    await user.click(await screen.findByRole("radio", { name: "Health" }));
    await waitFor(() => expect(asked.at(-1)?.kinds).toEqual([EventKind.HEALTH]));
  });

  it("says what its check said beside the event, and goes to the app", async () => {
    const user = userEvent.setup();
    renderAt("/activity", {
      events: { watchEvents: feedOf(snapshot([health("evt_1", HealthState.UNHEALTHY, 2)])), listEvents: () => ({}) },
    });
    await user.click(await screen.findByRole("row", { name: /web turned unhealthy/ }));
    expect(await screen.findByText("Its check said")).toBeVisible();
    expect(screen.getAllByText("GET / answered 503").length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: "Open the app" })).toHaveAttribute("href", "/apps/svc_web");
  });
});
