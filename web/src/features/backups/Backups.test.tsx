// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type Backup,
  BackupCopy,
  BackupDestinationProblem,
  BackupMethod,
  BackupPolicy_Drill,
  BackupPolicy_Every,
  BackupSchema,
  BackupState,
  ChangeSchema,
  ChangeState,
  type CreateBackupRequest,
  type DeleteBackupRequest,
  type DrillBackupRequest,
  DrillResult,
  type FetchBackupRequest,
  PlanAction,
  PreviewBackupResponseSchema,
  type ProposeChangeRequest,
  type RestorePlanSchema,
  type TestBackupDestinationRequest,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { SERVER } from "../../test/apps";
import { openHowItWorks } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";

function aBackup(init: MessageInitShape<typeof BackupSchema> = {}): Backup {
  return create(BackupSchema, {
    id: "bkp_1",
    environmentId: "env_1",
    volume: "orders",
    volumeId: "vol_1",
    serverId: SERVER,
    serverName: "hz-1",
    state: BackupState.DONE,
    onServer: true,
    method: BackupMethod.COPY,
    sizeBytes: 312n << 20n,
    startedAt: timestampFromDate(new Date("2026-11-06T03:00:00Z")),
    finishedAt: timestampFromDate(new Date("2026-11-06T03:00:40Z")),
    ...init,
  });
}

function api(backups: Backup[], more: Api = {}): Api {
  return {
    backups: { listBackups: () => ({ backups }) },
    resources: {
      listResources: (req: { kind: string }) =>
        req.kind === "volume"
          ? { resources: [{ id: "vol_1", resource: { kind: "volume", scope: "env_1", name: "orders" } }] }
          : { resources: [] },
    },
    ...more,
  };
}

/** The page with storage set up, and what a test adds to its calls. */
function copies(backups: Backup[], calls: Api["backups"] = {}, changes: Api["changes"] = {}): Api {
  return api(backups, {
    backups: {
      listBackups: () => ({ backups }),
      listBackupPolicies: () => ({}),
      listBackupDestinations: () => ({}),
      ...calls,
    },
    changes,
  });
}

describe("the backups page", () => {
  it("asks for the first backup, and says what one stops", async () => {
    renderAt("/backups", api([]));
    expect(await screen.findByRole("heading", { level: 2, name: "No backups yet" })).toBeVisible();
    expect(screen.getByText(/A database's backup stops nothing/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeVisible();
  });

  it("says there's nothing to back up, with no volume, and offers the two ways one comes", async () => {
    const empty = api([]);
    empty.resources = { listResources: () => ({ resources: [] }) };
    renderAt("/backups", empty);
    expect(await screen.findByRole("heading", { level: 2, name: "Nothing to back up yet" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Back up now" })).toBeNull();
    expect(screen.getByRole("link", { name: "Install a database" })).toHaveAttribute(
      "href",
      "/catalog?category=databases",
    );
    expect(screen.getByRole("link", { name: "New app" })).toHaveAttribute("href", "/apps/new");
  });

  it("offers one Add for schedules and for storage while there's none, in the empty state", async () => {
    renderAt("/backups", api([]));
    const schedules = await screen.findByRole("region", { name: "Schedules" });
    expect(within(schedules).getByRole("button", { name: "Add a schedule" })).toBeVisible();
    expect(within(schedules).queryByRole("button", { name: "Add" })).toBeNull();
    const storage = await screen.findByRole("region", { name: "Destinations" });
    expect(within(storage).getByRole("button", { name: "Add storage" })).toBeVisible();
    expect(within(storage).queryByRole("button", { name: "Add" })).toBeNull();
  });

  it("lists backups with how each was taken, and what a failure said", async () => {
    const { container } = renderAt(
      "/backups",
      api([
        aBackup(),
        aBackup({ id: "bkp_2", method: BackupMethod.DUMP, database: "PostgreSQL" }),
        aBackup({ id: "bkp_3", method: BackupMethod.COPY_RUNNING, filesChanged: 2n }),
        aBackup({
          id: "bkp_4",
          state: BackupState.FAILED,
          onServer: false,
          method: BackupMethod.UNSPECIFIED,
          sizeBytes: 0n,
          error: "The disk is full.",
        }),
      ]),
    );
    const table = await screen.findByRole("table", { name: "Backups, newest first" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(4);
    expect(rows[0]).toHaveTextContent("Exact copy");
    expect(rows[0]).toHaveTextContent("312 MB");
    expect(rows[1]).toHaveTextContent("PostgreSQL dump");
    expect(rows[2]).toHaveTextContent("Copy of a running app");
    expect(rows[2]).toHaveTextContent("2 files changed while it copied");
    expect(rows[3]).toHaveTextContent("The disk is full.");
    expect(within(rows[3] as HTMLElement).queryByRole("button", { name: "Restore" })).toBeNull();
    // How it was taken sits under its volume, so the status has the room, and a failure's reason keeps its whole
    // text on hover, as the list holds it to two lines.
    expect(within(table).queryByRole("columnheader", { name: "How" })).toBeNull();
    const volume = within(rows[1] as HTMLElement).getAllByRole("cell")[1];
    expect(volume).toHaveTextContent("PostgreSQL dump");
    expect(within(rows[3] as HTMLElement).getByText("The disk is full.")).toHaveAttribute("title", "The disk is full.");
    // Under a heading of their own, with how many and how many failed, as Backups has its runs.
    const runs = screen.getByRole("region", { name: "Recent runs" });
    expect(within(runs).getByText("4 backups · 1 failed")).toBeVisible();
    expect(runs).toContainElement(table);
    await expectAccessible(container);
  });

  it("says the runs are the newest, when there are more than a page holds", async () => {
    const page = Array.from({ length: 3 }, (_, i) => aBackup({ id: `bkp_${i}` }));
    renderAt("/backups", api([], { backups: { listBackups: () => ({ backups: page, nextPageToken: "more" }) } }));
    const runs = await screen.findByRole("region", { name: "Recent runs" });
    expect(within(runs).getByText("3 newest")).toBeVisible();
  });

  it("stacks on a phone", async () => {
    onPhone();
    renderAt("/backups", api([aBackup()]));
    expect(await screen.findByText("orders")).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getByRole("button", { name: "Restore" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/more");
    // With the count of what it took, as New app sits with the count on Apps.
    const runs = screen.getByRole("region", { name: "Recent runs" });
    expect(within(runs).getByRole("button", { name: "Back up now" })).toBeVisible();
    expect(screen.getByText(/unencrypted, so they go if the server does/)).toBeVisible();
  });

  it("says a backup on a server's disk isn't encrypted", async () => {
    renderAt("/backups", api([aBackup()]));
    expect(await screen.findByText(/unencrypted, so they go if the server does/)).toBeVisible();
  });

  it("says a word before a copy of a running app, and stops it unless told not to", async () => {
    const user = userEvent.setup();
    const made = vi.fn((_: CreateBackupRequest) => ({ backup: aBackup({ state: BackupState.RUNNING }) }));
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          previewBackup: () =>
            create(PreviewBackupResponseSchema, { method: BackupMethod.COPY_RUNNING, running: ["web", "worker"] }),
          createBackup: made,
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Back up now" }));
    expect(await screen.findByText("web and worker run on this volume")).toBeVisible();
    expect(screen.getByText(/can hold half a write/)).toBeVisible();
    const stopping = screen.getByRole("switch", { name: "Stop web and worker while it copies" });
    expect(stopping).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Back up" }));
    await waitFor(() => expect(made).toHaveBeenCalled());
    expect(made.mock.calls[0]?.[0]).toMatchObject({ volumeId: "vol_1", stop: true });
  });

  it("says a database's dump stops nothing", async () => {
    const user = userEvent.setup();
    const made = vi.fn((_: CreateBackupRequest) => ({ backup: aBackup({ state: BackupState.RUNNING }) }));
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          previewBackup: () =>
            create(PreviewBackupResponseSchema, { method: BackupMethod.DUMP, database: "PostgreSQL", running: ["db"] }),
          createBackup: made,
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Back up now" }));
    expect(await screen.findByText(/uses PostgreSQL's own dump/)).toBeVisible();
    expect(screen.queryByText(/MyISAM/)).toBeNull();
    expect(screen.queryByRole("switch")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Back up" }));
    await waitFor(() => expect(made).toHaveBeenCalled());
    expect(made.mock.calls[0]?.[0]).toMatchObject({ stop: false });
  });

  it("says MyISAM tables may not come out consistent in MySQL's dump", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          previewBackup: () =>
            create(PreviewBackupResponseSchema, { method: BackupMethod.DUMP, database: "MySQL", running: ["db"] }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Back up now" }));
    expect(await screen.findByText(/MyISAM table written to during the dump/)).toBeVisible();
  });

  it("says why a server can't answer, and takes nothing", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          previewBackup: () => {
            throw new ConnectError(
              "hz-1 isn't connected, and a backup runs on it. Try again once its agent reconnects",
              Code.Unavailable,
            );
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Back up now" }));
    expect(await screen.findByText(/isn't connected, and a backup runs on it/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Back up" })).toBeDisabled();
  });

  function restoring(restore: MessageInitShape<typeof RestorePlanSchema>) {
    return create(ChangeSchema, {
      id: "chg_restore",
      summary: "Restore orders",
      state: ChangeState.PLANNED,
      plan: {
        fingerprint: "abc",
        destructive: true,
        resources: [
          {
            id: "vol_1",
            resource: { kind: "volume", scope: "env_1", name: "orders" },
            action: PlanAction.REPLACE,
            servers: [SERVER],
          },
        ],
        servers: [
          {
            serverId: SERVER,
            serverName: "hz-1",
            steps: [
              {
                resourceId: "vol_1",
                resource: { kind: "volume", scope: "env_1", name: "orders" },
                action: PlanAction.REPLACE,
                destroysData: true,
                restore,
              },
            ],
          },
        ],
      },
    });
  }

  it("restores through the plan sheet, which says what it replaces and what stops", async () => {
    const user = userEvent.setup();
    const change = restoring({
      backupId: "bkp_1",
      method: BackupMethod.DUMP,
      database: "PostgreSQL",
      sizeBytes: 312n << 20n,
      takenAt: timestampFromDate(new Date("2026-11-06T03:00:00Z")),
      stops: ["db", "web"],
    });
    const watch = feed<WatchChangeResponse>();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: { listBackups: () => ({ backups: [aBackup()] }), restoreBackup: () => ({ change }) },
        changes: { getChange: () => ({ change }), watchChange: watch.stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    const risk = (await screen.findByRole("alert")).closest(".plan-risk") as HTMLElement;
    expect(risk.querySelector(".dw-callout-title")).toHaveTextContent(
      /^Everything in orders on hz-1 is replaced with the dump from/,
    );
    expect(within(risk).getAllByText("db and web stop while this happens.")[0]).toBeVisible();
    // How it goes is one step away.
    openHowItWorks(risk);
    expect(
      within(risk).getByText(/PostgreSQL starts anew on an empty volume and loads the dump, 312 MB/),
    ).toBeVisible();
    expect(screen.queryByText(/wrote these files/)).toBeNull();
  });

  it("warns in amber when a database's files go under another release", async () => {
    const user = userEvent.setup();
    const change = restoring({
      backupId: "bkp_1",
      method: BackupMethod.COPY,
      sizeBytes: 312n << 20n,
      takenAt: timestampFromDate(new Date("2026-11-06T03:00:00Z")),
      writtenBy: "postgres:16.4",
      runsOn: "postgres:17.11",
    });
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: { listBackups: () => ({ backups: [aBackup()] }), restoreBackup: () => ({ change }) },
        changes: { getChange: () => ({ change }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    const warning = (await screen.findByText(/wrote these files/)).closest(".plan-risk");
    expect(warning).toHaveClass("dw-tone-warning");
    expect(warning).toHaveTextContent("postgres:16.4 wrote these files, and postgres:17.11 runs on them now");
    expect(
      within(warning as HTMLElement).getByText("Deploy postgres:16.4 first, or restore a dump instead."),
    ).toBeVisible();
    expect(warning).toHaveTextContent("may not start on files from another release");
    expect(screen.getByRole("alert")).toHaveTextContent(/^Everything in orders on hz-1 is replaced/);
  });

  it("asks for a shared server's name typed before it plans restoring it whole", async () => {
    const user = userEvent.setup();
    const change = restoring({
      backupId: "bkp_1",
      method: BackupMethod.DUMP,
      database: "PostgreSQL",
      sizeBytes: 1n << 20n,
    });
    const asked: string[] = [];
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          restoreBackup: (req) => {
            asked.push(req.confirm);
            if (req.confirm !== "db") {
              throw new ConnectError(
                "Restoring db whole takes back what blog wrote to it since 2026-11-06 03:00 UTC. Type db to confirm, or restore each app's database alone from its page",
                Code.FailedPrecondition,
                { "Dockwell-Error": "confirm_name" },
              );
            }
            return { change };
          },
        },
        changes: { getChange: () => ({ change }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore db whole" });
    expect(dialog).toHaveTextContent("Restoring db whole takes back what blog wrote to it since");
    const review = within(dialog).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Type db to confirm"), "db");
    await user.click(review);
    await waitFor(() => expect(asked).toEqual(["", "db"]));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^Everything in orders on hz-1 is replaced/);
  });

  it("says what a schedule costs, and plans it", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return {
        change: create(ChangeSchema, {
          id: "chg_policy",
          summary: "Back up orders on a schedule",
          state: ChangeState.PLANNED,
          plan: { fingerprint: "abc", resources: [] },
        }),
      };
    });
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          previewBackup: () =>
            create(PreviewBackupResponseSchema, {
              method: BackupMethod.COPY_RUNNING,
              running: ["web"],
              estimateBytes: 100n << 20n,
              freeBytes: 10n << 30n,
              reserveBytes: 1n << 30n,
            }),
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    expect(await screen.findByText(/keeping 7, about 960 MB\. The disk has 9 GB to give\./)).toBeVisible();
    expect(screen.getByRole("switch", { name: "Stop web for each backup" })).toBeChecked();
    const keep = screen.getByLabelText("Backups to keep");
    await user.clear(keep);
    await user.type(keep, "100");
    expect(await screen.findByText("This may not fit")).toBeVisible();
    await user.clear(keep);
    await user.type(keep, "3");
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "backup.policy", scope: "env_1", name: "orders-daily" });
    expect(edit?.spec?.kind.value).toMatchObject({ volume: "orders", at: "03:00", keep: 3, stop: true });
    // It checks that its backups restore weekly unless someone chooses otherwise (ADR 0068).
    expect(edit?.spec?.kind.value).toMatchObject({ drill: BackupPolicy_Drill.WEEKLY });
    expect(
      screen.getByText(/Restores the newest into a throwaway copy, about 100 MB, an hour after its run/),
    ).toBeVisible();
  });

  it("says a schedule starts in the install's time zone, and leaves it for the control plane to set", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return {
        change: create(ChangeSchema, { id: "chg_policy", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
      };
    });
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          previewBackup: () => ({}),
        },
        settings: { getSettings: () => ({ settings: { timeZone: "Asia/Kolkata" } }) },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    expect(await screen.findByText("In Asia/Kolkata")).toBeVisible();
    // An hour's minutes depend on the zone too, in one half an hour off UTC.
    await user.click(screen.getByRole("radio", { name: "Hourly" }));
    expect(screen.getByText("In Asia/Kolkata")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value).toMatchObject({ timeZone: "" });
  });

  it("says a schedule starts in UTC where the install has no time zone, and where to set one", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          previewBackup: () => ({}),
        },
        settings: { getSettings: () => ({ settings: { timeZone: "" } }) },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    expect(await screen.findByText("In UTC. Set the install's time zone in Settings to use another.")).toBeVisible();
  });

  it("says when it can't tell what a schedule costs, rather than nothing", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          previewBackup: () =>
            create(PreviewBackupResponseSchema, {
              method: BackupMethod.COPY,
              estimateBytes: 0n,
              freeBytes: 10n << 30n,
              reserveBytes: 1n << 30n,
            }),
        },
      }),
    );
    await user.click(
      await within(await screen.findByRole("region", { name: "Schedules" })).findByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    expect(await screen.findByText(/can't size a backup yet.*The disk has 9 GB\./)).toBeVisible();
  });

  function schedulePreview() {
    return create(PreviewBackupResponseSchema, {
      method: BackupMethod.COPY_RUNNING,
      running: ["web"],
      estimateBytes: 100n << 20n,
      freeBytes: 10n << 30n,
      reserveBytes: 1n << 30n,
    });
  }

  it("keeps the schedule sheet on a phone's screen", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          previewBackup: schedulePreview,
        },
      }),
    );
    await user.click(
      await within(await screen.findByRole("region", { name: "Schedules" })).findByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    expect(await screen.findByRole("dialog", { name: "Back up on a schedule" })).toBeVisible();
    expect(await screen.findByText(/keeping 7, about 960 MB/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Review plan" })).toBeVisible();
    await expectAccessible(container);
  });

  it("reads in Chinese, with the time's zone named", async () => {
    await activateLocale("zh-CN");
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({
            policies: [
              {
                id: "bpl_1",
                name: "orders-daily",
                environmentId: "env_1",
                spec: {
                  volume: "orders",
                  every: BackupPolicy_Every.DAY,
                  at: "03:00",
                  keep: 7,
                  timeZone: "Asia/Shanghai",
                },
                kept: 3,
                keptBytes: 936n << 20n,
                nextRun: timestampFromDate(new Date("2026-11-07T19:00:00Z")),
              },
            ],
          }),
          previewBackup: schedulePreview,
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "备份计划" });
    expect(await within(panel).findByText(/每天 03:00.*Asia\/Shanghai/)).toBeVisible();
    await user.click(within(panel).getByRole("button", { name: "添加" }));
    expect(await screen.findByRole("dialog", { name: /按计划备份/ })).toBeVisible();
    await activateLocale("en");
  });

  it("offers weekly checks for schedules made before them, as one plan for a person to apply", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return {
        change: create(ChangeSchema, {
          id: "chg_checks",
          state: ChangeState.PLANNED,
          plan: { fingerprint: "abc", resources: [] },
        }),
      };
    });
    const policy = (name: string, drill: BackupPolicy_Drill) => ({
      id: `bpl_${name}`,
      name,
      environmentId: "env_1",
      spec: {
        volume: "orders",
        every: BackupPolicy_Every.DAY,
        at: "02:30",
        keep: 5,
        timeZone: "UTC",
        destination: "r2",
        drill,
      },
    });
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({
            policies: [
              policy("orders-daily", BackupPolicy_Drill.UNSPECIFIED),
              policy("logs-daily", BackupPolicy_Drill.UNSPECIFIED),
              policy("media-daily", BackupPolicy_Drill.NEVER),
              policy("db-daily", BackupPolicy_Drill.WEEKLY),
            ],
          }),
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    expect(await screen.findByText("2 schedules never check a restore")).toBeVisible();
    const panel = screen.getByRole("region", { name: "Schedules" });
    expect(within(panel).getByText("Checks a restore weekly")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Check weekly" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edits = proposed.mock.calls[0]?.[0].edits ?? [];
    expect(edits.map((e) => e.resource?.name)).toEqual(["orders-daily", "logs-daily"]);
    // Each whole as it is, but its check.
    expect(edits[0]?.spec?.kind.value).toMatchObject({
      at: "02:30",
      keep: 5,
      destination: "r2",
      drill: BackupPolicy_Drill.WEEKLY,
    });
  });

  it("shows each schedule, and a failed run where you'll see it", async () => {
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({
            policies: [
              {
                id: "bpl_1",
                name: "orders-daily",
                environmentId: "env_1",
                spec: {
                  volume: "orders",
                  every: BackupPolicy_Every.DAY,
                  at: "03:00",
                  keep: 7,
                  timeZone: "Asia/Shanghai",
                },
                last: aBackup({ state: BackupState.FAILED, error: "The disk is full." }),
                kept: 3,
                keptBytes: 936n << 20n,
                nextRun: timestampFromDate(new Date("2026-11-07T03:00:00Z")),
              },
            ],
          }),
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Schedules" });
    expect(await within(panel).findByText("Daily at 03:00, Asia/Shanghai")).toBeVisible();
    // In the schedule's zone, as its time is: 03:00 UTC is 11:00 there.
    expect(within(panel).getByText(/Next run .*11:00/)).toBeVisible();
    expect(within(panel).getByText("Keeps 7, holding 3 now (936 MB)")).toBeVisible();
    expect(within(panel).getByText("Doesn't check a restore")).toBeVisible();
    expect(within(panel).getByText("Last run failed")).toBeVisible();
    expect(within(panel).getByText("The disk is full.")).toBeVisible();
  });

  it("makes R2's endpoint from its account, finds and tests the bucket, and adds it once the passphrase is kept", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_: ProposeChangeRequest) => ({
      change: create(ChangeSchema, {
        id: "chg_d",
        summary: "Send backups to r2",
        state: ChangeState.PLANNED,
        plan: { fingerprint: "abc" },
      }),
    }));
    const tested = vi.fn((req: TestBackupDestinationRequest) =>
      req.destination?.bucket
        ? { signedIn: true, listed: true, buckets: ["acme-backups", "media"], wrote: true }
        : { signedIn: true, listed: true, buckets: ["acme-backups", "media"] },
    );
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          testBackupDestination: tested,
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Destinations" });
    await user.click(within(panel).getByRole("button", { name: /^Add( storage)?$/ }));
    const sheet = await screen.findByRole("dialog", { name: "Add a destination" });
    expect(within(sheet).getByLabelText("Storage")).toHaveValue("r2");
    // Only what can't be picked is typed: R2's account, and the keys.
    expect(within(sheet).queryByLabelText("Endpoint")).toBeNull();
    await user.type(within(sheet).getByLabelText("Account ID"), "abc123");
    await user.type(within(sheet).getByLabelText("Access key"), "AKIA");
    await user.type(within(sheet).getByLabelText("Secret key"), "the-secret");
    await user.click(within(sheet).getByRole("button", { name: "Find buckets" }));
    expect(await within(sheet).findByText("Signed in · 2 buckets")).toBeVisible();
    expect(within(sheet).getByLabelText("Bucket")).toHaveValue("acme-backups");
    await user.click(within(sheet).getByRole("button", { name: "Test" }));
    expect(await within(sheet).findByText("Wrote and deleted a test file in acme-backups")).toBeVisible();
    expect(tested.mock.calls.map((c) => c[0].destination?.bucket)).toEqual(["", "acme-backups"]);
    expect(tested.mock.calls[0]?.[0].destination).toMatchObject({
      endpoint: "https://abc123.r2.cloudflarestorage.com",
      region: "auto",
    });

    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.click(within(sheet).getByLabelText("I've kept a copy of the passphrase"));
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "backup.destination", name: "r2" });
    const value = edit?.spec?.kind.value as { endpoint: string; bucket: string; secretKey: string; passphrase: string };
    expect(value).toMatchObject({ endpoint: "https://abc123.r2.cloudflarestorage.com", bucket: "acme-backups" });
    expect(value.secretKey).toBe("the-secret");
    expect(value.passphrase.length).toBeGreaterThan(32);
  });

  it("says what the storage refused, in a word, with what it said", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          testBackupDestination: () => ({
            problem: BackupDestinationProblem.KEYS_REFUSED,
            said: "InvalidAccessKeyId: no",
          }),
        },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Destinations" })).getByRole("button", {
        name: /^Add( storage)?$/,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a destination" });
    await user.selectOptions(within(sheet).getByLabelText("Storage"), "b2");
    expect(within(sheet).getByLabelText("Region")).toHaveValue("us-west-001");
    await user.type(within(sheet).getByLabelText("Access key"), "AKIA");
    await user.type(within(sheet).getByLabelText("Secret key"), "wrong");
    await user.click(within(sheet).getByRole("button", { name: "Find buckets" }));
    expect(await within(sheet).findByText("The storage refused these keys")).toBeVisible();
    expect(within(sheet).getByText("InvalidAccessKeyId: no")).toBeVisible();
  });

  /** Opens Add a destination on Amazon S3 with keys typed, answering tests with answer. */
  async function s3Sheet(answer: (req: TestBackupDestinationRequest) => unknown) {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          testBackupDestination: answer as never,
        },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Destinations" })).getByRole("button", {
        name: /^Add( storage)?$/,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a destination" });
    await user.selectOptions(within(sheet).getByLabelText("Storage"), "s3");
    await user.type(within(sheet).getByLabelText("Access key"), "AKIA");
    await user.type(within(sheet).getByLabelText("Secret key"), "secret");
    return { user, sheet };
  }

  it("says a bucket is in another region, and picks that region in one click", async () => {
    const { user, sheet } = await s3Sheet((req) =>
      req.destination?.bucket
        ? {
            signedIn: true,
            problem: BackupDestinationProblem.WRONG_REGION,
            region: "eu-west-2",
            said: "PermanentRedirect",
          }
        : { signedIn: true, listed: true, buckets: ["acme"] },
    );
    await user.click(within(sheet).getByRole("button", { name: "Find buckets" }));
    await user.click(await within(sheet).findByRole("button", { name: "Test" }));
    expect(await within(sheet).findByText("acme is in eu-west-2")).toBeVisible();
    expect(within(sheet).queryByText(/Wrote and deleted/)).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: "Use eu-west-2" }));
    expect(within(sheet).getByLabelText("Region")).toHaveValue("eu-west-2");
  });

  it("says a test file it couldn't delete is still there, and never that it was deleted", async () => {
    const { user, sheet } = await s3Sheet((req) =>
      req.destination?.bucket
        ? {
            signedIn: true,
            wrote: true,
            problem: BackupDestinationProblem.CANT_DELETE,
            testFile: "dw/.dockwell-test-0a1b2c",
            said: "AccessDenied",
          }
        : { signedIn: true, listed: true, buckets: ["acme"] },
    );
    await user.click(within(sheet).getByRole("button", { name: "Find buckets" }));
    await user.click(await within(sheet).findByRole("button", { name: "Test" }));
    expect(
      await within(sheet).findByText("Wrote it, but couldn't delete the test file: dw/.dockwell-test-0a1b2c"),
    ).toBeVisible();
    expect(within(sheet).queryByText(/Wrote and deleted/)).toBeNull();
  });

  it("asks for the bucket's name when the keys sign in and may not list", async () => {
    const { user, sheet } = await s3Sheet(() => ({ signedIn: true, listed: false }));
    await user.click(within(sheet).getByRole("button", { name: "Find buckets" }));
    expect(
      await within(sheet).findByText("Signed in · these keys can't list buckets, so type the bucket's name"),
    ).toBeVisible();
  });

  it("drops an answer to keys that were changed while it was asked", async () => {
    let answer: (value: unknown) => void = () => {};
    const { user, sheet } = await s3Sheet(
      () =>
        new Promise((resolve) => {
          answer = resolve;
        }),
    );
    await user.click(within(sheet).getByRole("button", { name: "Find buckets" }));
    await user.type(within(sheet).getByLabelText("Secret key"), "-changed");
    answer({ signedIn: true, listed: true, buckets: ["acme"] });
    await new Promise((r) => setTimeout(r, 50));
    expect(within(sheet).queryByText(/Signed in/)).toBeNull();
  });

  it("fetches a backup its server lost, and offers no restore until then", async () => {
    const user = userEvent.setup();
    const fetched = vi.fn((_: FetchBackupRequest) => ({ backup: aBackup() }));
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({
            backups: [aBackup({ onServer: false, destination: "r2", copy: BackupCopy.STORED, fetchOnto: ["hz-2"] })],
          }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          fetchBackup: fetched,
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Fetch" }));
    await waitFor(() => expect(fetched).toHaveBeenCalled());
    expect(fetched.mock.calls[0]?.[0]).toMatchObject({ id: "bkp_1", server: "hz-2" });
    expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
    expect(screen.getByText("On r2")).toBeVisible();
  });

  it("asks which server to fetch onto, where its volume is on several", async () => {
    const user = userEvent.setup();
    const fetched = vi.fn((_: FetchBackupRequest) => ({ backup: aBackup() }));
    const b = aBackup({ onServer: false, destination: "r2", copy: BackupCopy.STORED, fetchOnto: ["hz-2", "hz-3"] });
    renderAt("/backups", copies([b], { fetchBackup: fetched }));
    await user.click(await screen.findByRole("button", { name: "Fetch" }));
    const sheet = await screen.findByRole("dialog", { name: /Fetch this backup/ });
    expect(fetched).not.toHaveBeenCalled();
    await user.selectOptions(within(sheet).getByLabelText("Onto"), "hz-3");
    await user.click(within(sheet).getByRole("button", { name: "Fetch" }));
    await waitFor(() => expect(fetched).toHaveBeenCalled());
    expect(fetched.mock.calls[0]?.[0]).toMatchObject({ id: "bkp_1", server: "hz-3" });
  });

  it("says what's needed when no server can take it", async () => {
    renderAt("/backups", copies([aBackup({ onServer: false, destination: "r2", copy: BackupCopy.STORED })]));
    expect(await screen.findByText("To fetch it, put orders on a connected server first")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Fetch" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
  });

  it("says when only a server that can't be reached has it", async () => {
    renderAt("/backups", copies([aBackup({ onServer: false })]));
    expect(await screen.findByText(/Its server can't restore this now/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
  });

  it("marks a copy only an earlier passphrase opens, and keeps the server's", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      copies([aBackup({ destination: "r2", copy: BackupCopy.STORED, sealedEarlier: true })], {
        deleteBackup: () => ({}),
      }),
    );
    expect(await screen.findByText("Sealed with an earlier passphrase")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Delete this backup" }));
    const sheet = await screen.findByRole("dialog", { name: /Delete this backup/ });
    expect(within(sheet).getByText(/where it was sealed with an earlier passphrase/)).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: "Remove from the server only" })).toBeNull();
  });

  it("checks that a backup restores from its row, and says how that went where you look", async () => {
    const user = userEvent.setup();
    const asked: DrillBackupRequest[] = [];
    const finished = timestampFromDate(new Date(Date.now() - 2 * 3600_000));
    const rows = [
      aBackup({ id: "bkp_never" }),
      aBackup({ id: "bkp_ok", drill: { result: DrillResult.PASSED, finishedAt: finished } }),
      aBackup({ id: "bkp_files", drill: { result: DrillResult.PASSED, unpackedOnly: true, finishedAt: finished } }),
      aBackup({
        id: "bkp_bad",
        drill: {
          result: DrillResult.FAILED,
          why: "orders.items has no rows, and the dump fills it",
          finishedAt: finished,
        },
      }),
      aBackup({
        id: "bkp_skip",
        drill: { result: DrillResult.NOT_CHECKED, why: "the disk it restores to has 1 GB free." },
      }),
      aBackup({ id: "bkp_now", drill: { result: DrillResult.RUNNING } }),
    ];
    const { container } = renderAt(
      "/backups",
      copies(rows, {
        drillBackup: (req) => {
          asked.push(req);
          return { drill: { id: "drl_1", result: DrillResult.RUNNING } };
        },
      }),
    );
    expect(await screen.findByText("Restored and checked 2 h ago")).toBeVisible();
    expect(screen.getByText("Unpacked whole 2 h ago")).toBeVisible();
    expect(
      screen.getByText("Restore check failed 2 h ago: orders.items has no rows, and the dump fills it"),
    ).toBeVisible();
    expect(screen.getByText("Not checked: the disk it restores to has 1 GB free")).toBeVisible();
    expect(screen.getByText("Checking it restores")).toBeVisible();
    expect(screen.getAllByRole("button", { name: "Check again" })).toHaveLength(4);
    await expectAccessible(container);
    await user.click(screen.getByRole("button", { name: "Check it restores" }));
    await waitFor(() => expect(asked).toEqual([expect.objectContaining({ id: "bkp_never" })]));
  });

  it("says why a backup can't be checked now, where you are", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      copies([aBackup()], {
        drillBackup: () => {
          throw new ConnectError(
            "a restore check is running on hz-1, which checks one at a time. Check this one once it ends",
            Code.FailedPrecondition,
          );
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Check it restores" }));
    const row = screen.getAllByRole("row")[1] as HTMLElement;
    expect(await within(row).findByRole("alert")).toHaveTextContent(/^Can't check it now: .*checks one at a time/);
  });

  it("keeps Check it restores busy while it asks, so a second click sends nothing", async () => {
    const user = userEvent.setup();
    let asked = 0;
    let answer: () => void = () => {};
    renderAt(
      "/backups",
      copies([aBackup()], {
        drillBackup: () => {
          asked++;
          return new Promise((resolve) => {
            answer = () => resolve({});
          });
        },
      }),
    );
    const check = await screen.findByRole("button", { name: "Check it restores" });
    await user.click(check);
    expect(check).toHaveAttribute("aria-busy", "true");
    await user.click(check);
    expect(asked).toBe(1);
    answer();
    await waitFor(() => expect(check).not.toHaveAttribute("aria-busy"));
  });

  it("offers a restore while its server has it, and no fetch", async () => {
    renderAt("/backups", copies([aBackup({ destination: "r2", copy: BackupCopy.STORED })]));
    expect(await screen.findByRole("button", { name: "Restore" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Fetch" })).toBeNull();
    expect(screen.getByText("Also on r2")).toBeVisible();
  });

  it("shows how a send is going, and why a fetch or a copy failed, on the backup's row", async () => {
    renderAt(
      "/backups",
      copies([
        aBackup({ destination: "r2", copy: BackupCopy.PENDING, progress: "Sending orders: 12 MB of 40 MB" }),
        aBackup({
          id: "bkp_2",
          destination: "r2",
          copy: BackupCopy.STORED,
          onServer: false,
          fetchError: "AccessDenied. Check the destination's keys and bucket",
        }),
        aBackup({
          id: "bkp_3",
          destination: "r2",
          copy: BackupCopy.FAILED,
          copyError: "the storage refused it. Check the destination's keys and bucket",
        }),
      ]),
    );
    expect(await screen.findByText("Sending orders: 12 MB of 40 MB")).toBeVisible();
    expect(screen.getByText(/It couldn't be fetched: AccessDenied/)).toBeVisible();
    expect(screen.getByText(/It didn't reach r2: the storage refused it/)).toBeVisible();
  });

  it("watches a backup until its copy lands, and a fetch until it's back", async () => {
    const sending = aBackup({ destination: "r2", copy: BackupCopy.PENDING });
    const stored = aBackup({ destination: "r2", copy: BackupCopy.STORED });
    const fetching = aBackup({
      destination: "r2",
      copy: BackupCopy.STORED,
      onServer: false,
      progress: "Fetching orders",
    });
    const answers = [sending, fetching, stored];
    renderAt("/backups", copies([], { listBackups: () => ({ backups: [answers.shift() ?? stored] }) }));
    expect(await screen.findByText("Sending to r2")).toBeVisible();
    expect(await screen.findByText("Fetching orders", undefined, { timeout: 8000 })).toBeVisible();
    expect(await screen.findByText("Also on r2", undefined, { timeout: 8000 })).toBeVisible();
  }, 20_000);

  it("says deleting takes it from both places, and can take it from the server alone", async () => {
    const user = userEvent.setup();
    const deleted = vi.fn((_: DeleteBackupRequest) => ({}));
    renderAt("/backups", copies([aBackup({ destination: "r2", copy: BackupCopy.STORED })], { deleteBackup: deleted }));
    await user.click(await screen.findByRole("button", { name: "Delete this backup" }));
    const sheet = await screen.findByRole("dialog", { name: /Delete this backup/ });
    expect(within(sheet).getByText(/from the server's disk and from r2/)).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Remove from the server only" }));
    await waitFor(() => expect(deleted).toHaveBeenCalledTimes(1));
    expect(deleted.mock.calls[0]?.[0]).toMatchObject({ id: "bkp_1", serverOnly: true });
    await waitFor(() => expect(sheet).not.toBeInTheDocument());

    await user.click(await screen.findByRole("button", { name: "Delete this backup" }));
    const again = await screen.findByRole("dialog", { name: /Delete this backup/ });
    await user.click(within(again).getByRole("button", { name: "Delete from both" }));
    await waitFor(() => expect(deleted).toHaveBeenCalledTimes(2));
    expect(deleted.mock.calls[1]?.[0]).toMatchObject({ id: "bkp_1", serverOnly: false });
  });

  it("says the destination has the last of it, with nothing to keep it by", async () => {
    const user = userEvent.setup();
    renderAt("/backups", copies([aBackup({ destination: "r2", copy: BackupCopy.STORED, onServer: false })]));
    await user.click(await screen.findByRole("button", { name: "Delete this backup" }));
    const sheet = await screen.findByRole("dialog", { name: /Delete this backup/ });
    expect(within(sheet).getByText(/from r2, the last place that has it/)).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: "Remove from the server only" })).toBeNull();
  });

  it("shows a failed copy on its schedule, though the run was good", async () => {
    renderAt(
      "/backups",
      copies([aBackup()], {
        listBackupPolicies: () => ({
          policies: [
            {
              id: "bpl_1",
              name: "orders-daily",
              environmentId: "env_1",
              spec: { volume: "orders", every: BackupPolicy_Every.DAY, at: "03:00", keep: 7, destination: "r2" },
              last: aBackup({
                destination: "r2",
                copy: BackupCopy.FAILED,
                copyError: "the storage refused it. Check the destination's keys and bucket",
              }),
              kept: 1,
              nextRun: timestampFromDate(new Date("2026-11-07T03:00:00Z")),
            },
          ],
        }),
      }),
    );
    const panel = await screen.findByRole("region", { name: "Schedules" });
    expect(await within(panel).findByText("Last copy failed")).toBeVisible();
    expect(within(panel).getByText(/the storage refused it/)).toBeVisible();
  });

  // With no destinations yet, Add is the empty state's Add storage.
  async function addDestination(user: ReturnType<typeof userEvent.setup>, add: string | RegExp) {
    const panel = await screen.findByRole("region", { name: /Destinations|目的地/ });
    await user.click(within(panel).getByRole("button", { name: add }));
  }

  it("takes a passphrase you already have, and holds it to the length the API does", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_: ProposeChangeRequest) => ({
      change: create(ChangeSchema, { id: "chg_d", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
    }));
    renderAt(
      "/backups",
      copies([aBackup()], {}, { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream }),
    );
    await addDestination(user, "Add storage");
    await user.selectOptions(await screen.findByLabelText("Storage"), "other");
    await user.type(screen.getByLabelText("Endpoint"), "https://storage.example.com");
    await user.type(screen.getByLabelText("Bucket"), "acme-backups");
    await user.type(screen.getByLabelText("Access key"), "AKIA");
    await user.type(screen.getByLabelText("Secret key"), "the-secret");
    // A passphrase of one's own is a step away, under Advanced.
    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.click(screen.getByRole("switch", { name: /Use a passphrase I already have/ }));
    expect(screen.queryByText("Keep this passphrase somewhere else")).toBeNull();
    const review = screen.getByRole("button", { name: "Review plan" });
    await user.type(screen.getByLabelText("Passphrase"), "too short");
    expect(review).toBeDisabled();
    await user.type(screen.getByLabelText("Passphrase"), " but now it's long enough");
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const value = proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value as { passphrase: string };
    expect(value.passphrase).toBe("too short but now it's long enough");
  });

  it("keeps the destination sheet on a phone's screen, with a passphrase you can copy", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = renderAt("/backups", copies([aBackup()]));
    await addDestination(user, "Add storage");
    expect(await screen.findByRole("dialog", { name: "Add a destination" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Copy the passphrase" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Review plan" })).toBeVisible();
    await expectAccessible(container);
  });

  it("reads the destination sheet in Chinese", async () => {
    await activateLocale("zh-CN");
    const user = userEvent.setup();
    renderAt("/backups", copies([aBackup()]));
    await addDestination(user, "添加存储");
    expect(await screen.findByRole("dialog", { name: "添加目的地" })).toBeVisible();
    expect(screen.getByRole("button", { name: "复制密码短语" })).toBeVisible();
    await activateLocale("en");
  });
});

describe("a backup's own page", () => {
  it("says what it's of and where it's kept, each name a link, with its actions", async () => {
    const backup = aBackup({ actor: { kind: 1, id: "usr_1", name: "Mira Chen" } });
    const { container } = renderAt("/backups/bkp_1", copies([backup], { getBackup: () => ({ backup }) }));
    expect(await screen.findByRole("heading", { level: 1, name: "Backup of orders" }, { timeout: 5000 })).toBeVisible();
    const facts = screen.getByRole("region", { name: "What it is" });
    expect(within(facts).getByRole("link", { name: "orders" })).toHaveAttribute("href", "/volumes/vol_1");
    expect(within(facts).getByRole("link", { name: "hz-1" })).toHaveAttribute("href", `/servers/${SERVER}`);
    expect(within(facts).getByText("Mira Chen")).toBeVisible();
    expect(within(facts).getByText("The server's disk")).toBeVisible();
    expect(screen.getByRole("button", { name: "Restore" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Delete this backup" })).toBeVisible();
    await expectAccessible(container);
  });

  it("is a step away from the list: its volume and its time are links", async () => {
    renderAt("/backups", copies([aBackup()]));
    const table = await screen.findByRole("table", { name: "Backups, newest first" }, { timeout: 5000 });
    expect(within(table).getByRole("link", { name: "orders" })).toHaveAttribute("href", "/volumes/vol_1");
    expect(
      within(table)
        .getAllByRole("link")
        .map((a) => a.getAttribute("href")),
    ).toContain("/backups/bkp_1");
  });
});
