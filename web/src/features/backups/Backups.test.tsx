// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type Backup,
  BackupCopy,
  BackupDestinationInfoSchema,
  BackupMethod,
  BackupPolicy_Drill,
  BackupPolicy_Every,
  BackupSchema,
  BackupState,
  ChangeSchema,
  ChangeState,
  ConnectionInfoSchema,
  ConnectionReach,
  type CreateBackupRequest,
  type DeleteBackupRequest,
  type DrillBackupRequest,
  DrillResult,
  type FetchBackupRequest,
  LinkKind,
  LinkPart,
  PlanAction,
  PreviewBackupResponseSchema,
  type ProposeChangeRequest,
  type RestorePlanSchema,
  Role,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
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

  it("fetches the next cursor on demand and includes older backups in search", async () => {
    const user = userEvent.setup();
    const newest = Array.from({ length: 50 }, (_, i) => aBackup({ id: `bkp_${i}` }));
    const older = aBackup({
      id: "bkp_older",
      volume: "older-orders",
      startedAt: timestampFromDate(new Date("2026-11-05T03:00:00Z")),
    });
    const listBackups = vi.fn(({ pageToken }: { pageToken: string }) =>
      pageToken === "older" ? { backups: [older], nextPageToken: "" } : { backups: newest, nextPageToken: "older" },
    );
    renderAt("/backups", api([], { backups: { listBackups } }));
    const runs = await screen.findByRole("region", { name: "Recent runs" });
    expect(within(runs).getByText("1-50 of 50+")).toBeVisible();
    expect(listBackups).toHaveBeenCalledTimes(1);
    await user.click(within(runs).getByRole("button", { name: "Next" }));
    expect(await within(runs).findByText("51-51 of 51")).toBeVisible();
    expect(listBackups).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 50, pageToken: "older" }),
      expect.anything(),
    );
    await user.click(within(runs).getByRole("button", { name: "Previous" }));
    await user.type(within(runs).getByRole("searchbox", { name: "Filter backups" }), "older-orders");
    expect(await within(runs).findByText("older-orders")).toBeVisible();
  });

  it("shows matches immediately and reads beyond the loaded backups only on request", async () => {
    const user = userEvent.setup();
    const loaded = aBackup({ volume: "orders", destination: "archive" });
    const older = aBackup({
      id: "bkp_older",
      volume: "orders-previous",
      destination: "archive",
      startedAt: timestampFromDate(new Date("2026-11-05T03:00:00Z")),
    });
    const listBackups = vi.fn(({ pageToken }: { pageToken: string }) =>
      pageToken ? { backups: [older] } : { backups: [loaded], nextPageToken: "older" },
    );
    renderAt("/backups", api([], { backups: { listBackups } }));
    const runs = await screen.findByRole("region", { name: "Recent runs" });
    const search = within(runs).getByRole("searchbox", { name: "Filter backups" });
    await user.type(search, "archive");
    const all = await within(runs).findByRole("button", { name: "Search all backups" });
    expect(within(runs).getByText("orders", { exact: true })).toBeVisible();
    expect(listBackups).toHaveBeenCalledTimes(1);
    await user.click(all);
    expect(await within(runs).findByText("orders-previous")).toBeVisible();
    expect(listBackups).toHaveBeenCalledTimes(2);
  });

  it("polls the first cursor only while a backup is running", async () => {
    const user = userEvent.setup();
    const newest = Array.from({ length: 50 }, (_, i) =>
      aBackup({
        id: `bkp_${i}`,
        state: i === 0 ? BackupState.RUNNING : BackupState.DONE,
      }),
    );
    const older = aBackup({
      id: "bkp_older",
      volume: "older-orders",
      startedAt: timestampFromDate(new Date("2026-11-05T03:00:00Z")),
    });
    const listBackups = vi.fn(({ pageToken }: { pageToken: string }) =>
      pageToken ? { backups: [older] } : { backups: newest, nextPageToken: "older" },
    );
    renderAt("/backups", api([], { backups: { listBackups } }));
    const runs = await screen.findByRole("region", { name: "Recent runs" });
    await user.click(within(runs).getByRole("button", { name: "Next" }));
    expect(await within(runs).findByText("older-orders")).toBeVisible();
    const newestCalls = () => listBackups.mock.calls.filter(([req]) => !req.pageToken).length;
    const before = newestCalls();
    await waitFor(() => expect(newestCalls()).toBeGreaterThan(before), { timeout: 4000 });
    expect(listBackups.mock.calls.filter(([req]) => req.pageToken)).toHaveLength(1);
    expect(within(runs).getByText("older-orders")).toBeVisible();
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
    expect(screen.getByText(/On each server's disk alone, unencrypted/)).toBeVisible();
  });

  it("says a backup on a server's disk isn't encrypted", async () => {
    renderAt("/backups", api([aBackup()]));
    expect(await screen.findByText(/On each server's disk alone, unencrypted/)).toBeVisible();
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
    expect(risk.querySelector(".dw-callout-title")).toHaveTextContent(/^Replaces orders on hz-1 with the dump from/);
    expect(within(risk).getAllByText("db and web stop while it loads, and what was made since goes.")[0]).toBeVisible();
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
    expect(screen.getByRole("alert")).toHaveTextContent(/^Replaces orders on hz-1 with/);
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
    expect(await screen.findByRole("alert")).toHaveTextContent(/^Replaces orders on hz-1 with/);
  });

  /** A catalog PostgreSQL installed as warehouse, with its backup's calls, and the projects there are. */
  function aCatalogDatabase(
    restoreBackup: NonNullable<Api["backups"]>["restoreBackup"],
    projects = ["warehouse"],
    usedBefore: string[] = [],
    category = "databases",
  ): Api {
    const backup = aBackup({ volume: "data", method: BackupMethod.DUMP, database: "PostgreSQL" });
    const change = restoring({
      backupId: "bkp_1",
      method: BackupMethod.DUMP,
      database: "PostgreSQL",
      sizeBytes: 1n << 20n,
    });
    return api([backup], {
      backups: { listBackups: () => ({ backups: [backup] }), restoreBackup },
      resources: {
        listResources: (req: { kind: string }) =>
          req.kind === "project"
            ? {
                resources: projects.map((name, i) => ({
                  id: `prj_${i + 1}`,
                  resource: { kind: "project", scope: "org_1", name },
                })),
              }
            : req.kind === "environment"
              ? { resources: [{ id: "env_1", resource: { kind: "environment", scope: "prj_1", name: "production" } }] }
              : { resources: [] },
      },
      catalog: {
        listCatalogInstalls: () => ({
          installs: [{ projectId: "prj_1", project: "warehouse", environmentId: "env_1", pin: { entry: "postgres" } }],
        }),
        // What the install's entry is, as the catalog files it.
        listCatalogEntries: () => ({ entries: [{ id: "postgres", category }] }),
        // The control plane's free name, which knows the names used before.
        getCatalogEntry: (req: { project: string }) => ({
          project: usedBefore.includes(req.project) ? `${req.project}-2` : req.project,
        }),
      },
      changes: { getChange: () => ({ change }), watchChange: feed<WatchChangeResponse>().stream },
    });
  }

  it("restores a catalog database's backup into a new database first, with its name filled and checked", async () => {
    const user = userEvent.setup();
    const asked: { into: string; confirm: string }[] = [];
    const change = restoring({
      backupId: "bkp_1",
      method: BackupMethod.DUMP,
      database: "PostgreSQL",
      sizeBytes: 1n << 20n,
    });
    renderAt(
      "/backups",
      aCatalogDatabase(
        (req) => {
          asked.push({ into: req.into, confirm: req.confirm });
          return { change };
        },
        ["warehouse", "warehouse-restored"],
      ),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore warehouse" });
    const fresh = within(dialog).getByRole("radio", { name: /To a new database/ });
    expect(fresh).toBeChecked();
    expect(within(dialog).getByText("warehouse keeps running. Check the data, then switch apps over.")).toBeVisible();
    // warehouse-restored is taken, so the next free name is filled in.
    const name = within(dialog).getByLabelText("Name");
    expect(name).toHaveValue("warehouse-restored-2");
    const review = within(dialog).getByRole("button", { name: "Review plan" });
    await user.clear(name);
    await user.type(name, "warehouse-restored");
    expect(
      within(dialog).getByText("A project named warehouse-restored is here already. warehouse-restored-2 is free."),
    ).toBeVisible();
    expect(review).toBeDisabled();
    await user.clear(name);
    await user.type(name, "Not A Name");
    expect(within(dialog).getByText(/Use lowercase letters, digits and dashes/)).toBeVisible();
    expect(review).toBeDisabled();
    await user.clear(name);
    await user.type(name, "orders-check");
    // Review plan waits while the control plane says whether the name is free.
    expect(review).toBeDisabled();
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await waitFor(() => expect(asked).toEqual([{ into: "orders-check", confirm: "" }]));
    await expectAccessible(document.body);
  });

  it("says a name used before isn't free, as the control plane knows it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      aCatalogDatabase(
        () => ({ change: restoring({ backupId: "bkp_1", method: BackupMethod.DUMP }) }),
        ["warehouse"],
        ["warehouse-old"],
      ),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore warehouse" });
    const review = within(dialog).getByRole("button", { name: "Review plan" });
    await waitFor(() => expect(review).toBeEnabled());
    const name = within(dialog).getByLabelText("Name");
    await user.clear(name);
    await user.type(name, "warehouse-old");
    expect(
      await within(dialog).findByText(
        "A project named warehouse-old was here before, and its data may come back. warehouse-old-2 is free.",
      ),
    ).toBeVisible();
    expect(review).toBeDisabled();
  });

  it("replaces an app's own database straight away, as no new copy of an app is made", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    const change = restoring({ backupId: "bkp_1", method: BackupMethod.DUMP, database: "PostgreSQL" });
    renderAt(
      "/backups",
      aCatalogDatabase(
        (req) => {
          asked.push(req.into);
          return { change };
        },
        ["warehouse"],
        [],
        "productivity",
      ),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^Replaces orders on hz-1 with/);
    expect(asked).toEqual([""]);
    expect(screen.queryByRole("dialog", { name: "Restore warehouse" })).toBeNull();
  });

  it("replaces a catalog database's data straight away for a developer, who can't make a project", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    const change = restoring({ backupId: "bkp_1", method: BackupMethod.DUMP, database: "PostgreSQL" });
    renderAt("/backups", {
      ...aCatalogDatabase((req) => {
        asked.push(req.into);
        return { change };
      }),
      auth: {
        getAuthState: () =>
          authState({
            user: { ...mira, role: Role.UNSPECIFIED },
            grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
          }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^Replaces orders on hz-1 with/);
    expect(asked).toEqual([""]);
    expect(screen.queryByRole("dialog", { name: "Restore warehouse" })).toBeNull();
  });

  it("replaces a catalog database's data where you choose that, as before", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    const change = restoring({
      backupId: "bkp_1",
      method: BackupMethod.DUMP,
      database: "PostgreSQL",
      sizeBytes: 1n << 20n,
    });
    renderAt(
      "/backups",
      aCatalogDatabase((req) => {
        asked.push(req.into);
        return { change };
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore warehouse" });
    expect(within(dialog).getByLabelText("Name")).toHaveValue("warehouse-restored");
    await user.click(within(dialog).getByRole("radio", { name: /Replace warehouse/ }));
    expect(within(dialog).queryByLabelText("Name")).toBeNull();
    expect(within(dialog).getByText("What uses it stops while it loads, and what was made since goes.")).toBeVisible();
    await user.click(within(dialog).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(asked).toEqual([""]));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^Replaces orders on hz-1 with/);
  });

  it("says a restore into a new database replaces nothing", async () => {
    const user = userEvent.setup();
    const change = create(ChangeSchema, {
      id: "chg_restore_new",
      summary: "Restore warehouse as warehouse-restored",
      state: ChangeState.PLANNED,
      plan: {
        fingerprint: "abc",
        resources: [
          {
            id: "svc_new",
            resource: { kind: "service", scope: "env_2", name: "db" },
            action: PlanAction.CREATE,
            servers: [SERVER],
          },
        ],
        servers: [
          {
            serverId: SERVER,
            serverName: "hz-1",
            steps: [
              {
                resourceId: "svc_new",
                resource: { kind: "service", scope: "env_2", name: "db" },
                action: PlanAction.CREATE,
                restore: {
                  backupId: "bkp_1",
                  intoNew: true,
                  method: BackupMethod.DUMP,
                  database: "PostgreSQL",
                  sizeBytes: 1n << 20n,
                  takenAt: timestampFromDate(new Date("2026-11-06T03:00:00Z")),
                },
              },
            ],
          },
        ],
      },
    });
    renderAt(
      "/backups",
      aCatalogDatabase(() => ({ change })),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Restore warehouse" })).getByRole("button", {
        name: "Review plan",
      }),
    );
    const title = await screen.findByText(/^Loads the dump from .* into db on hz-1$/);
    const risk = title.closest(".plan-risk") as HTMLElement;
    expect(within(risk).getByText("The database it was taken of keeps running as it is.")).toBeVisible();
    expect(screen.queryByText(/^Replaces /)).toBeNull();
    expect(screen.queryByText(/replaced, and undoing this puts it back/)).toBeNull();
    expect(screen.queryByRole("textbox", { name: /fingerprint|Type/ })).toBeNull();
  });

  it("restores a backup straight away where no new database can be made of it", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    const change = restoring({ backupId: "bkp_1", method: BackupMethod.COPY, sizeBytes: 1n << 20n });
    // A volume of an app's own, not a catalog database's.
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          restoreBackup: (req) => {
            asked.push(req.into);
            return { change };
          },
        },
        catalog: {
          listCatalogInstalls: () => ({
            installs: [{ projectId: "prj_1", environmentId: "env_1", pin: { entry: "gitea" } }],
          }),
        },
        changes: { getChange: () => ({ change }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^Replaces orders on hz-1 with/);
    expect(asked).toEqual([""]);
    expect(screen.queryByRole("dialog", { name: /^Restore / })).toBeNull();
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

  it("starts a new backup schedule in the person's own time zone where they set one (#834)", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_req: ProposeChangeRequest) => ({
      change: create(ChangeSchema, { id: "chg_1", state: ChangeState.APPLIED }),
    }));
    renderAt(
      "/backups",
      api([aBackup()], {
        auth: { getAuthState: () => authState({ user: { ...mira, timeZone: "Asia/Tokyo" } }) },
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
    expect(await screen.findByText("In Asia/Tokyo")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value).toMatchObject({ timeZone: "Asia/Tokyo" });
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
    expect(await screen.findByText("2 schedules never test a restore")).toBeVisible();
    const panel = screen.getByRole("region", { name: "Schedules" });
    expect(within(panel).getByText("Tests a restore weekly")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Test weekly" }));
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
    expect(within(panel).getByText("Never tests a restore")).toBeVisible();
    expect(within(panel).getByText("Last run failed")).toBeVisible();
    expect(within(panel).getByText("The disk is full.")).toBeVisible();
  });

  it("opens Add a connection when Add storage is clicked on Destinations", async () => {
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Destinations" });
    await user.click(await within(panel).findByRole("button", { name: /^Add( storage)?$/ }));
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    expect(within(sheet).getByRole("textbox", { name: "Bucket" })).toBeVisible();
    // Storage alone: mail server tiles are not shown.
    expect(within(sheet).queryByRole("button", { name: "Resend" })).toBeNull();
  });

  it("offers S3 connections first in the schedule sheet, seals an unused one inline, and plans both edits", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_: ProposeChangeRequest) => ({
      change: create(ChangeSchema, {
        id: "chg_d",
        summary: "Back up orders on a schedule",
        state: ChangeState.PLANNED,
        plan: { fingerprint: "abc" },
      }),
    }));
    const spare = create(ConnectionInfoSchema, {
      id: "cnx_spare",
      name: "offsite",
      displayName: "Offsite",
      kind: LinkKind.S3,
      endpoint: "https://s3.us-east-1.amazonaws.com",
      region: "us-east-1",
      bucket: "acme-backups",
      reach: ConnectionReach.EVERY,
    });
    const appConn = create(ConnectionInfoSchema, {
      id: "cnx_app",
      name: "uploads",
      displayName: "Uploads",
      kind: LinkKind.S3,
      endpoint: "https://s3.eu-central-1.amazonaws.com",
      region: "eu-central-1",
      bucket: "site-uploads",
      reach: ConnectionReach.EVERY,
      uses: [
        {
          linkId: "lnk_1",
          link: "storage",
          environment: "env_1",
          project: "prj_1",
          prefix: "site/",
          sets: { S3_BUCKET: LinkPart.BUCKET },
          appIds: ["svc_site"],
          apps: ["site"],
        },
      ],
    });
    const heldConn = create(ConnectionInfoSchema, {
      id: "cnx_held",
      name: "old-app",
      displayName: "Old app bucket",
      kind: LinkKind.S3,
      endpoint: "https://minio.example.com",
      region: "auto",
      bucket: "old-bucket",
      held: true,
      reach: ConnectionReach.EVERY,
    });
    const { container } = renderAt(
      "/backups",
      api([aBackup()], {
        connections: { listConnections: () => ({ connections: [appConn, spare, heldConn] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          previewBackup: schedulePreview,
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Back up on a schedule" });
    const spareRadio = within(sheet).getByRole("radio", { name: /Use Offsite \(s3\.us-east-1\.amazonaws\.com\)/ });
    expect(spareRadio).toBeChecked();
    expect(
      within(sheet).getByRole("radio", { name: /Use Uploads \(s3\.eu-central-1\.amazonaws\.com\)/ }),
    ).toBeDisabled();
    expect(within(sheet).getByText("Serves site, whose key could delete the backups")).toBeVisible();
    expect(within(sheet).getByRole("radio", { name: /Use Old app bucket \(minio\.example\.com\)/ })).toBeDisabled();
    expect(within(sheet).getByText("An app received this connection's keys")).toBeVisible();

    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await expectAccessible(container);

    // New connection from an app-serving connection prefills endpoint/region with empty bucket & keys.
    await user.click(within(sheet).getByRole("button", { name: "New connection from it" }));
    const connSheet = await screen.findByRole("dialog", { name: "Add a connection" });
    expect(within(connSheet).getByRole("combobox", { name: "Region" })).toHaveValue("eu-central-1");
    expect(within(connSheet).getByRole("textbox", { name: "Bucket" })).toHaveValue("");
    expect(within(connSheet).getByRole("textbox", { name: "Access key ID" })).toHaveValue("");
    await user.click(within(connSheet).getByRole("button", { name: "Cancel" }));

    // Held connection offers Paste a key made for backups to rotate keys on that connection.
    await user.click(within(sheet).getByRole("button", { name: "Paste a key made for backups" }));
    const rotateSheet = await screen.findByRole("dialog", { name: "Connection old-app" });
    expect(within(rotateSheet).getByText(/Paste a key made for backups alone/)).toBeVisible();
    await user.click(within(rotateSheet).getByRole("button", { name: "Cancel" }));

    // Confirm passphrase and submit: proposes backup.destination + backup.policy in one plan.
    await user.click(within(sheet).getByLabelText("I've kept a copy of the passphrase"));
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edits = proposed.mock.calls[0]?.[0].edits ?? [];
    expect(edits).toHaveLength(2);
    expect(edits[0]?.resource).toMatchObject({ kind: "backup.destination", name: "offsite" });
    expect(edits[0]?.spec?.kind.value).toMatchObject({ connection: "offsite", prefix: "backups" });
    expect(edits[1]?.resource).toMatchObject({ kind: "backup.policy" });
    expect(edits[1]?.spec?.kind.value).toMatchObject({ destination: "offsite" });
  });

  it("falls back to Use <name> below admin and disables setting up a new backup destination with why", async () => {
    const user = userEvent.setup();
    const spare = create(ConnectionInfoSchema, {
      id: "cnx_spare",
      name: "offsite",
      kind: LinkKind.S3,
      reach: ConnectionReach.PROJECTS,
      projects: ["prj_1"],
    });
    renderAt(
      "/backups",
      api([aBackup()], {
        auth: {
          getAuthState: () =>
            authState({
              user: { ...mira, role: Role.UNSPECIFIED },
              grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
            }),
        },
        connections: { listConnections: () => ({ connections: [spare] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          previewBackup: schedulePreview,
        },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Back up on a schedule" });
    const radio = within(sheet).getByRole("radio", { name: /Use offsite/ });
    expect(radio).toBeDisabled();
    expect(within(sheet).getByRole("radio", { name: /Only this server/ })).toBeChecked();
  });

  it("deduplicates paired destinations and leaves out connections that can't take backups in Destinations", async () => {
    const conn = create(ConnectionInfoSchema, {
      id: "cnx_1",
      name: "offsite",
      displayName: "Offsite",
      kind: LinkKind.S3,
      endpoint: "https://s3.us-east-1.amazonaws.com",
      region: "us-east-1",
      bucket: "acme-backups",
      reach: ConnectionReach.EVERY,
    });
    const appConn = create(ConnectionInfoSchema, {
      id: "cnx_app",
      name: "uploads",
      displayName: "Uploads",
      kind: LinkKind.S3,
      endpoint: "https://s3.eu-central-1.amazonaws.com",
      region: "eu-central-1",
      bucket: "site-uploads",
      reach: ConnectionReach.EVERY,
      uses: [
        {
          linkId: "lnk_1",
          link: "storage",
          environment: "env_1",
          project: "prj_1",
          prefix: "site/",
          sets: { S3_BUCKET: LinkPart.BUCKET },
          appIds: ["svc_site"],
          apps: ["site"],
        },
      ],
    });
    const dest = create(BackupDestinationInfoSchema, {
      id: "bds_1",
      name: "offsite",
      endpoint: conn.endpoint,
      bucket: "acme-backups",
      backups: 5,
      usedBytes: 50n << 20n,
      connection: "offsite",
    });
    const legacy = create(BackupDestinationInfoSchema, {
      id: "bds_legacy",
      name: "legacy-r2",
      endpoint: "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com",
      bucket: "legacy-backups",
      prefix: "old",
    });
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        connections: { listConnections: () => ({ connections: [conn, appConn] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({ destinations: [dest, legacy] }),
          previewBackup: schedulePreview,
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Destinations" });
    expect(await within(panel).findByRole("link", { name: "offsite" })).toHaveAttribute("href", "/connections/offsite");
    expect(within(panel).getByText("legacy-r2")).toBeVisible();
    // App-serving connections are left out of Destinations; the schedule picker still shows them disabled.
    expect(within(panel).queryByText("uploads")).toBeNull();
    expect(within(panel).getAllByRole("listitem")).toHaveLength(2);
    expect(within(panel).getByText("5 backups, 50 MB")).toBeVisible();

    // Legacy destination in the schedule picker reads like connections do, with its bucket on the second line.
    await user.click(
      within(screen.getByRole("region", { name: "Schedules" })).getByRole("button", { name: /^Add( a schedule)?$/ }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Back up on a schedule" });
    expect(
      within(sheet).getByRole("radio", {
        name: /Use legacy-r2 \(0123456789abcdef0123456789abcdef\.r2\.cloudflarestorage\.com\)/,
      }),
    ).toBeVisible();
    expect(within(sheet).getByText("Bucket legacy-backups · encrypted under old/")).toBeVisible();
  });

  it("seals an unused S3 connection from Back up now, proposing the destination before backing up", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_: ProposeChangeRequest) => ({
      change: create(ChangeSchema, { id: "chg_dest", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
    }));
    const conn = create(ConnectionInfoSchema, {
      id: "cnx_1",
      name: "offsite",
      displayName: "Offsite",
      kind: LinkKind.S3,
      endpoint: "https://s3.us-east-1.amazonaws.com",
      region: "us-east-1",
      bucket: "acme-backups",
      reach: ConnectionReach.EVERY,
    });
    renderAt(
      "/backups",
      api([aBackup()], {
        connections: { listConnections: () => ({ connections: [conn] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          previewBackup: () => ({ method: BackupMethod.COPY, estimateBytes: 10n << 20n, freeBytes: 10n << 30n }),
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Back up now" }));
    const sheet = await screen.findByRole("dialog", { name: "Back up orders" });
    expect(within(sheet).getByRole("radio", { name: /Nowhere else/ })).toBeChecked();
    await user.click(within(sheet).getByRole("radio", { name: /Use Offsite \(s3\.us-east-1\.amazonaws\.com\)/ }));
    const submit = within(sheet).getByRole("button", { name: "Review plan and back up" });
    expect(submit).toBeDisabled();
    await user.click(within(sheet).getByLabelText("I've kept a copy of the passphrase"));
    await user.click(submit);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "backup.destination", name: "offsite" });
    expect(edit?.spec?.kind.value).toMatchObject({ connection: "offsite", prefix: "backups" });
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
    expect(await screen.findByText("Restore tested 2 h ago")).toBeVisible();
    expect(screen.getByText("Unpacked whole 2 h ago")).toBeVisible();
    expect(
      screen.getByText("Restore test failed 2 h ago: orders.items has no rows, and the dump fills it"),
    ).toBeVisible();
    expect(screen.getByText("Not tested: the disk it restores to has 1 GB free")).toBeVisible();
    expect(screen.getByText("Testing a restore")).toBeVisible();
    expect(screen.getAllByRole("button", { name: "Test again" })).toHaveLength(4);
    await expectAccessible(container);
    await user.click(screen.getByRole("button", { name: "Test restore" }));
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
    await user.click(await screen.findByRole("button", { name: "Test restore" }));
    const row = screen.getAllByRole("row")[1] as HTMLElement;
    expect(await within(row).findByRole("alert")).toHaveTextContent(/^Can't check it now: .*checks one at a time/);
  });

  it("keeps Test restore busy while it asks, so a second click sends nothing", async () => {
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
    const check = await screen.findByRole("button", { name: "Test restore" });
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

  const spareConn = () =>
    create(ConnectionInfoSchema, {
      id: "cnx_spare",
      name: "offsite",
      displayName: "Offsite",
      kind: LinkKind.S3,
      endpoint: "https://storage.example.com",
      region: "auto",
      bucket: "acme-backups",
      reach: ConnectionReach.EVERY,
    });

  it("takes a passphrase you already have when sealing an unused connection, and holds it to the API length", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_: ProposeChangeRequest) => ({
      change: create(ChangeSchema, { id: "chg_d", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
    }));
    renderAt(
      "/backups",
      api([aBackup()], {
        connections: { listConnections: () => ({ connections: [spareConn()] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          previewBackup: schedulePreview,
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Back up on a schedule" });
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    await user.click(within(sheet).getByRole("switch", { name: /Use a passphrase I already have/ }));
    expect(within(sheet).queryByText("Keep this passphrase somewhere else")).toBeNull();
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    await user.type(within(sheet).getByLabelText("Passphrase"), "too short");
    expect(review).toBeDisabled();
    await user.type(within(sheet).getByLabelText("Passphrase"), " but now it's long enough");
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const value = proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value as { passphrase: string; connection: string };
    expect(value.connection).toBe("offsite");
    expect(value.passphrase).toBe("too short but now it's long enough");
  });

  it("keeps the schedule sheet with a connection on a phone's screen, with a passphrase you can copy", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = renderAt(
      "/backups",
      api([aBackup()], {
        connections: { listConnections: () => ({ connections: [spareConn()] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          previewBackup: schedulePreview,
        },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", {
        name: /^Add( a schedule)?$/,
      }),
    );
    expect(await screen.findByRole("dialog", { name: "Back up on a schedule" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Copy the passphrase" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Review plan" })).toBeVisible();
    await expectAccessible(container);
  });

  it("reads the connection seal fields in Chinese", async () => {
    await activateLocale("zh-CN");
    const user = userEvent.setup();
    renderAt(
      "/backups",
      api([aBackup()], {
        connections: { listConnections: () => ({ connections: [spareConn()] }) },
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          previewBackup: schedulePreview,
        },
      }),
    );
    await user.click(
      within(await screen.findByRole("region", { name: "备份计划" })).getByRole("button", {
        name: /^添加(备份计划)?$/,
      }),
    );
    expect(await screen.findByRole("dialog", { name: "按计划备份" })).toBeVisible();
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

describe("the backups page, laid out as its mock (#793)", () => {
  it("puts restore tests beside where backups go, says a run's time within a day as how long ago, and edits a schedule", async () => {
    const user = userEvent.setup();
    const recent = aBackup({
      startedAt: timestampFromDate(new Date(Date.now() - 5 * 60_000)),
      finishedAt: timestampFromDate(new Date(Date.now() - 5 * 60_000 + 40_000)),
      drill: {
        result: DrillResult.PASSED,
        finishedAt: timestampFromDate(new Date(Date.now() - 2 * 3_600_000)),
      },
    });
    renderAt(
      "/backups",
      api([recent], {
        backups: {
          listBackups: () => ({ backups: [recent] }),
          listBackupDestinations: () => ({}),
          previewBackup: () => ({ method: BackupMethod.COPY }),
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
                  drill: BackupPolicy_Drill.WEEKLY,
                },
              },
            ],
          }),
        },
      }),
    );
    const tests = await screen.findByRole("region", { name: "Restore tests" });
    expect(await within(tests).findByText("Passed 2 h ago")).toBeVisible();
    expect(tests).toHaveTextContent("Weekly");
    const runs = screen.getByRole("region", { name: "Recent runs" });
    expect(within(runs).getByText("5 min ago")).toBeVisible();
    const schedules = screen.getByRole("region", { name: "Schedules" });
    expect(schedules).toHaveTextContent("Kept only on the server, unencrypted");
    await user.click(within(schedules).getByRole("button", { name: "Edit this schedule" }));
    expect(await screen.findByRole("dialog", { name: "Change the schedule" })).toBeVisible();
  });

  it("refreshes loaded older pages when a running backup finishes and shows searching feedback (#914 N3/N4/N6)", async () => {
    const user = userEvent.setup();
    let running = true;
    let pruned = false;
    const newest = () =>
      Array.from({ length: 50 }, (_, i) =>
        aBackup({
          id: `bkp_${i}`,
          volume: `vol-${i}`,
          state: i === 0 && running ? BackupState.RUNNING : BackupState.DONE,
        }),
      );
    const listBackups = vi.fn(async ({ pageToken }: { pageToken: string }) => {
      if (pageToken === "older") {
        return {
          backups: pruned
            ? []
            : [
                aBackup({
                  id: "bkp_older",
                  volume: "pruned-soon",
                  startedAt: timestampFromDate(new Date("2026-11-05T03:00:00Z")),
                }),
              ],
          nextPageToken: "",
        };
      }
      return { backups: newest(), nextPageToken: "older" };
    });
    renderAt("/backups", api([], { backups: { listBackups } }));
    const runs = await screen.findByRole("region", { name: "Recent runs" });
    // Partial window wording says "Searching the newest 50 backups." when filtering.
    const search = within(runs).getByRole("searchbox", { name: "Filter backups" });
    await user.type(search, "no-hit-yet");
    expect(await within(runs).findByText("Searching the newest 50 backups.")).toBeVisible();
    // Clicking Search all backups shows loading state while older page is pending, not Nothing matches.
    let unblock!: () => void;
    const hold = new Promise<void>((resolve) => {
      unblock = resolve;
    });
    listBackups.mockImplementationOnce(async ({ pageToken }: { pageToken: string }) => {
      if (pageToken === "older") {
        await hold;
        return {
          backups: [
            aBackup({
              id: "bkp_older",
              volume: "pruned-soon",
              startedAt: timestampFromDate(new Date("2026-11-05T03:00:00Z")),
            }),
          ],
          nextPageToken: "",
        };
      }
      return { backups: newest(), nextPageToken: "older" };
    });
    await user.click(within(runs).getByRole("button", { name: "Search all backups" }));
    expect(await within(runs).findByText("Searching older backups")).toBeVisible();
    unblock();
    await waitFor(() => expect(within(runs).queryByText("Searching older backups")).toBeNull());
    // Clear search and verify older backup is loaded on page 2, then finish the running backup with pruned=true.
    await user.clear(search);
    await user.click(within(runs).getByRole("button", { name: "Next" }));
    expect(await within(runs).findByText("pruned-soon")).toBeVisible();
    running = false;
    pruned = true;
    await waitFor(() => expect(within(runs).queryByText("pruned-soon")).toBeNull(), { timeout: 5000 });
  });

  it("filters and pages backup destinations when there are many (#914 Destinations)", async () => {
    const user = userEvent.setup();
    const destinations = Array.from({ length: 10 }, (_, i) =>
      create(BackupDestinationInfoSchema, {
        id: `dst_${i}`,
        name: `dest-${String(i).padStart(2, "0")}`,
        bucket: `bucket-${i}`,
        endpoint: "https://s3.example.com",
      }),
    );
    renderAt("/backups", api([aBackup()], { backups: { listBackupDestinations: () => ({ destinations }) } }));
    const section = await screen.findByRole("region", { name: "Destinations" });
    expect(await within(section).findByText("1-8 of 10")).toBeVisible();
    expect(within(section).getByText("dest-00")).toBeVisible();
    expect(within(section).queryByText("dest-09")).toBeNull();
    await user.click(within(section).getByRole("button", { name: "Next" }));
    expect(await within(section).findByText("dest-09")).toBeVisible();
    await user.type(within(section).getByRole("searchbox", { name: "Filter destinations" }), "dest-03");
    expect(await within(section).findByText("dest-03")).toBeVisible();
    expect(within(section).queryByText("dest-09")).toBeNull();
  });
});
