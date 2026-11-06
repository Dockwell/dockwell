// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type Backup,
  BackupMethod,
  BackupPolicy_Every,
  BackupSchema,
  BackupState,
  ChangeSchema,
  ChangeState,
  type CreateBackupRequest,
  PlanAction,
  PreviewBackupResponseSchema,
  type ProposeChangeRequest,
  type RestorePlanSchema,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { SERVER } from "../../test/apps";
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

describe("the backups page", () => {
  it("asks for the first backup, and says what one stops", async () => {
    renderAt("/backups", api([]));
    expect(await screen.findByRole("heading", { level: 2, name: "No backups yet" })).toBeVisible();
    expect(screen.getByText(/A database's backup stops nothing/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeVisible();
  });

  it("says there's nothing to back up, with no volume", async () => {
    const empty = api([]);
    empty.resources = { listResources: () => ({ resources: [] }) };
    renderAt("/backups", empty);
    expect(await screen.findByRole("heading", { level: 2, name: "Nothing to back up yet" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Back up now" })).toBeNull();
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
    await expectAccessible(container);
  });

  it("stacks on a phone", async () => {
    onPhone();
    renderAt("/backups", api([aBackup()]));
    expect(await screen.findByText("orders")).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getByRole("button", { name: "Restore" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/more");
    expect(screen.getByRole("button", { name: "Back up now" })).toBeVisible();
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
    expect(await screen.findByText(/Everything in orders on hz-1 is replaced with the dump from/)).toBeVisible();
    expect(screen.getByText(/PostgreSQL starts anew on an empty volume and loads the dump, 312 MB/)).toBeVisible();
    expect(screen.getByText(/db and web stop while this happens/)).toBeVisible();
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
    expect(screen.getByText(/may not start on files from another release. Deploy postgres:16.4 first/)).toBeVisible();
    expect(screen.getByText(/Everything in orders on hz-1 is replaced/).closest(".plan-risk")).not.toHaveClass(
      "dw-tone-warning",
    );
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
    await user.click(await screen.findByRole("button", { name: "Add" }));
    expect(await screen.findByText(/keeping 7 takes about 700 MB, and the disk has 9 GB to give/)).toBeVisible();
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
    await user.click(await screen.findByRole("button", { name: "Add" }));
    expect(await screen.findByText(/can't say yet what each backup takes.*the disk has 9 GB to give/i)).toBeVisible();
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
                spec: { volume: "orders", every: BackupPolicy_Every.DAY, at: "03:00", keep: 7 },
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
    expect(await within(panel).findByText("Daily at 03:00")).toBeVisible();
    expect(within(panel).getByText("Keeps 7, holding 3 now (936 MB)")).toBeVisible();
    expect(within(panel).getByText("Last run failed")).toBeVisible();
    expect(within(panel).getByText("The disk is full.")).toBeVisible();
  });
});
