// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  BackupDestinationInfoSchema,
  ChangeSchema,
  ChangeState,
  ConnectionInfoSchema,
  ConnectionReach,
  LinkKind,
  Role,
  type UseDestinationAsConnectionRequest,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authState, mira } from "../../test/auth";
import { feed, onPhone, renderAt } from "../../test/render";

const MiB = 1024 * 1024;

const r2 = create(BackupDestinationInfoSchema, {
  id: "bds_1",
  name: "r2",
  endpoint: "https://example.r2.cloudflarestorage.com",
  bucket: "acme-backups",
  backups: 12,
  usedBytes: BigInt(186 * MiB),
  lastUpload: timestampFromDate(new Date(Date.now() - 40 * 60_000)),
});
// It took backups, then stopped: its newest didn't get there.
const refused = create(BackupDestinationInfoSchema, {
  id: "bds_3",
  name: "b2",
  endpoint: "https://s3.us-west-004.backblazeb2.com",
  bucket: "acme-offsite",
  backups: 3,
  usedBytes: BigInt(40 * MiB),
  lastUpload: timestampFromDate(new Date(Date.now() - 2 * 3_600_000)),
  lastFailure: timestampFromDate(new Date(Date.now() - 5 * 60_000)),
  lastFailureError: "the bucket refused it: AccessDenied. Check the destination's access key and secret",
});
const fresh = create(BackupDestinationInfoSchema, {
  id: "bds_2",
  name: "minio",
  endpoint: "https://minio.example.com",
  bucket: "backups",
});

afterEach(async () => {
  await activateLocale("en");
});

describe("Integrations", () => {
  it("shows where backups go, how each is doing, and what routes sit behind", async () => {
    const { container } = renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [r2, fresh] }) },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Integrations" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Integrations" }),
    ).toHaveAttribute("aria-current", "page");
    expect(
      await screen.findByText(
        "2 connected · the services Dockwell builds from, deploys with, routes through and backs up to",
      ),
    ).toBeVisible();

    const bucket = screen.getByRole("article", { name: "r2" });
    expect(within(bucket).getByText("Received a backup 40 min ago")).toBeVisible();
    expect(bucket).toHaveTextContent("Bucket acme-backups at example.r2.cloudflarestorage.com");
    expect(bucket).toHaveTextContent("12 backups, 186 MB");
    expect(within(bucket).getByRole("link", { name: "Manage in Backups" })).toHaveAttribute("href", "/backups");
    // Nothing has proved the new one works, so it doesn't read as healthy.
    expect(within(screen.getByRole("article", { name: "minio" })).getByText("No backup yet")).toBeVisible();
    // With storage connected, more is connected from the section's head.
    expect(screen.queryByRole("article", { name: "S3-compatible storage" })).toBeNull();
    expect(screen.getByRole("button", { name: "Connect storage" })).toBeVisible();

    const cloudflare = screen.getByRole("article", { name: "Cloudflare" });
    expect(within(cloudflare).getByRole("link", { name: "How to route behind it" })).toHaveAttribute(
      "href",
      "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#behind-cloudflare-tunnel-or-another-proxy",
    );
    // What doesn't connect yet is a line, never a card or a panel of its own.
    expect(screen.queryByRole("region", { name: "Coming later" })).toBeNull();
    expect(screen.getByText("Cloud providers come later.")).toBeVisible();
    await expectAccessible(container);
  });

  it("lays every section on one board of columns, a column a card, each with a one-line header (#786)", async () => {
    const { container } = renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [r2, fresh, refused] }) },
    });
    const storage = await screen.findByRole("region", { name: "Object storage" });
    await within(storage).findByText("3 buckets");
    const board = container.querySelector(".integrations-board");
    const sections = [...(board?.querySelectorAll(":scope > .integrations-section") ?? [])];
    expect(sections.map((s) => s.querySelector("h2")?.textContent)).toEqual([
      "Git",
      "Object storage",
      "Registries",
      "DNS and proxies",
      "Sign-in",
    ]);
    // Git holds GitHub and GitLab, Storage holds three cards, Registries Docker Hub's and GitHub's, DNS Cloudflare's and Domains.
    expect(sections.map((s) => s.getAttribute("data-cards"))).toEqual(["2", "3", "2", "2", "1"]);
    // Each header is its name, how many, and its action, in one line: no sentence under it.
    const head = storage.querySelector(".integrations-head");
    expect(head?.children).toHaveLength(3);
    expect(head).toHaveTextContent("Object storage3 bucketsConnect storage");
  });

  it("says a destination is failing once its newest backup didn't get there, whatever got there before", async () => {
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [refused] }) } });
    const b2 = await screen.findByRole("article", { name: "b2" });
    expect(within(b2).getByText("A backup didn't get here 5 min ago")).toBeVisible();
    expect(within(b2).getByRole("alert")).toHaveTextContent("AccessDenied");
    expect(within(b2).queryByText(/Received a backup/)).toBeNull();
  });

  it("says it's healthy again once a backup gets there after one that didn't", async () => {
    const back = create(BackupDestinationInfoSchema, {
      ...refused,
      lastUpload: timestampFromDate(new Date(Date.now() - 60_000)),
    });
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [back] }) } });
    const b2 = await screen.findByRole("article", { name: "b2" });
    expect(within(b2).getByText("Received a backup 1 min ago")).toBeVisible();
    expect(within(b2).queryByRole("alert")).toBeNull();
  });

  it("counts what's connected once it knows, never none while it reads", async () => {
    let answer: (v: { destinations: (typeof r2)[] }) => void = () => {};
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => new Promise((resolve) => (answer = resolve)) },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Integrations" })).toBeVisible();
    expect(screen.queryByText(/connected · /)).toBeNull();
    expect(screen.queryByText(/buckets? · /)).toBeNull();
    expect(await screen.findByText("Loading where backups go")).toHaveClass("dw-sr");
    answer({ destinations: [r2] });
    expect(
      await screen.findByText(
        "1 connected · the services Dockwell builds from, deploys with, routes through and backs up to",
      ),
    ).toBeVisible();
  });

  it("connects S3-compatible storage through the connection sheet", async () => {
    const user = userEvent.setup();
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [] }) } });
    expect(
      await screen.findByText(
        "0 connected · the services Dockwell builds from, deploys with, routes through and backs up to",
      ),
    ).toBeVisible();
    await user.click(
      within(screen.getByRole("article", { name: "S3-compatible storage" })).getByRole("button", { name: "Connect" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    expect(within(sheet).getByRole("textbox", { name: "Bucket" })).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
  });

  it("counts an S3 connection paired with a backup destination once, alongside unpaired connections", async () => {
    const conn = create(ConnectionInfoSchema, {
      id: "cnx_1",
      name: "r2-backups",
      displayName: "Cloudflare R2 · acme-backups",
      kind: LinkKind.S3,
      endpoint: "https://example.r2.cloudflarestorage.com",
      region: "auto",
      bucket: "acme-backups",
      reach: ConnectionReach.UNSPECIFIED,
    });
    const appConn = create(ConnectionInfoSchema, {
      id: "cnx_2",
      name: "uploads",
      displayName: "Uploads",
      kind: LinkKind.S3,
      endpoint: "https://s3.us-east-1.amazonaws.com",
      region: "us-east-1",
      bucket: "site-uploads",
      reach: ConnectionReach.EVERY,
    });
    const pairedDest = create(BackupDestinationInfoSchema, {
      ...r2,
      name: "r2-backups",
      connection: "r2-backups",
    });
    renderAt("/settings/integrations", {
      connections: { listConnections: () => ({ connections: [conn, appConn] }) },
      backups: { listBackupDestinations: () => ({ destinations: [pairedDest] }) },
    });
    const storage = await screen.findByRole("region", { name: "Object storage" });
    expect(await within(storage).findByText("2 buckets")).toBeVisible();
    expect(
      screen.getByText("2 connected · the services Dockwell builds from, deploys with, routes through and backs up to"),
    ).toBeVisible();
    const backed = within(storage).getByRole("article", { name: "r2-backups" });
    expect(within(backed).getByText("12 backups, 186 MB")).toBeVisible();
    expect(within(backed).getByRole("link", { name: "Manage" })).toHaveAttribute("href", "/connections/r2-backups");
    expect(within(backed).queryByRole("button", { name: "Use as a connection" })).toBeNull();
  });

  it("offers Use as a connection on a standalone legacy destination, gated on install admin", async () => {
    const user = userEvent.setup();
    const asked: UseDestinationAsConnectionRequest[] = [];
    renderAt("/settings/integrations", {
      backups: {
        listBackupDestinations: () => ({ destinations: [r2] }),
        useDestinationAsConnection: (req) => {
          asked.push(req);
          return {
            connection: "r2",
            change: create(ChangeSchema, { id: "chg_use", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
          };
        },
      },
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    const card = await screen.findByRole("article", { name: "r2" });
    await user.click(within(card).getByRole("button", { name: "Use as a connection" }));
    await waitFor(() => expect(asked).toEqual([expect.objectContaining({ destination: "r2" })]));
  });

  it("keeps Use as a connection disabled below an install admin", async () => {
    renderAt("/settings/integrations", {
      auth: {
        getAuthState: () =>
          authState({
            user: { ...mira, role: Role.UNSPECIFIED },
            grants: [{ projectId: "prj_1", role: Role.DEVELOPER }],
          }),
      },
      backups: { listBackupDestinations: () => ({ destinations: [r2] }) },
    });
    const card = await screen.findByRole("article", { name: "r2" });
    expect(within(card).getByRole("button", { name: "Use as a connection" })).toHaveAttribute("aria-disabled", "true");
  });

  it("is a section of Settings on a phone", async () => {
    onPhone();
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [r2] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "Integrations" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/settings");
    expect(await screen.findByRole("article", { name: "r2" })).toBeVisible();
    // The phone's header says what the wide screen's lead and aside do.
    expect(
      screen.getByText("1 connected · the services Dockwell builds from, deploys with, routes through and backs up to"),
    ).toBeVisible();
    expect(screen.getByRole("link", { name: "Variables" })).toHaveAttribute("href", "/variables");
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [r2] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "集成" })).toBeVisible();
    expect(await screen.findByText("云服务商将在之后推出。")).toBeVisible();
  });
});
