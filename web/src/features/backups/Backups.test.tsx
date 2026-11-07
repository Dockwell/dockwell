// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type Backup,
  BackupCopy,
  BackupMethod,
  BackupPolicy_Every,
  BackupSchema,
  BackupState,
  ChangeSchema,
  ChangeState,
  type CreateBackupRequest,
  type DeleteBackupRequest,
  PlanAction,
  PreviewBackupResponseSchema,
  type ProposeChangeRequest,
  type RestorePlanSchema,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
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
    await user.click(
      within(await screen.findByRole("region", { name: "Schedules" })).getByRole("button", { name: "Add" }),
    );
    expect(
      await screen.findByText(/keeping 7 takes about 960 MB while the next is taken, and the disk has 9 GB to give/),
    ).toBeVisible();
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
    await user.click(
      await within(await screen.findByRole("region", { name: "Schedules" })).findByRole("button", { name: "Add" }),
    );
    expect(await screen.findByText(/can't say yet what each backup takes.*the disk has 9 GB to give/i)).toBeVisible();
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
      await within(await screen.findByRole("region", { name: "Schedules" })).findByRole("button", { name: "Add" }),
    );
    expect(await screen.findByRole("dialog", { name: "Back up on a schedule" })).toBeVisible();
    expect(await screen.findByText(/keeping 7 takes about 960 MB/)).toBeVisible();
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
    expect(within(panel).getByText("Last run failed")).toBeVisible();
    expect(within(panel).getByText("The disk is full.")).toBeVisible();
  });

  it("adds storage only once the passphrase is kept, and never shows what signs in again", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((_: ProposeChangeRequest) => ({
      change: create(ChangeSchema, {
        id: "chg_d",
        summary: "Send backups to r2",
        state: ChangeState.PLANNED,
        plan: { fingerprint: "abc" },
      }),
    }));
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({ backups: [aBackup()] }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
        },
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Destinations" });
    await user.click(within(panel).getByRole("button", { name: "Add" }));
    await user.type(await screen.findByLabelText("Name"), "r2");
    await user.type(screen.getByLabelText("Endpoint"), "https://storage.example.com");
    await user.type(screen.getByLabelText("Bucket"), "acme-backups");
    await user.type(screen.getByLabelText("Access key"), "AKIA");
    await user.type(screen.getByLabelText("Secret key"), "the-secret");
    const review = screen.getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.click(screen.getByLabelText("I've kept a copy of the passphrase"));
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const value = proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value as { passphrase: string; secretKey: string };
    expect(value.secretKey).toBe("the-secret");
    expect(value.passphrase.length).toBeGreaterThan(32);
  });

  it("fetches a backup its server lost, and offers no restore until then", async () => {
    const user = userEvent.setup();
    const fetched = vi.fn(() => ({ backup: aBackup() }));
    renderAt(
      "/backups",
      api([aBackup()], {
        backups: {
          listBackups: () => ({
            backups: [aBackup({ onServer: false, destination: "r2", copy: BackupCopy.STORED })],
          }),
          listBackupPolicies: () => ({}),
          listBackupDestinations: () => ({}),
          fetchBackup: fetched,
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Fetch" }));
    await waitFor(() => expect(fetched).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
    expect(screen.getByText("Only on r2")).toBeVisible();
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

  async function addDestination(user: ReturnType<typeof userEvent.setup>, add: string) {
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
    await addDestination(user, "Add");
    await user.type(await screen.findByLabelText("Name"), "r2");
    await user.type(screen.getByLabelText("Endpoint"), "https://storage.example.com");
    await user.type(screen.getByLabelText("Bucket"), "acme-backups");
    await user.type(screen.getByLabelText("Access key"), "AKIA");
    await user.type(screen.getByLabelText("Secret key"), "the-secret");
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
    await addDestination(user, "Add");
    expect(await screen.findByRole("dialog", { name: "Add a destination" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Copy the passphrase" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Review plan" })).toBeVisible();
    await expectAccessible(container);
  });

  it("reads the destination sheet in Chinese", async () => {
    await activateLocale("zh-CN");
    const user = userEvent.setup();
    renderAt("/backups", copies([aBackup()]));
    await addDestination(user, "添加");
    expect(await screen.findByRole("dialog", { name: "添加目的地" })).toBeVisible();
    expect(screen.getByRole("button", { name: "复制密码短语" })).toBeVisible();
    await activateLocale("en");
  });
});
