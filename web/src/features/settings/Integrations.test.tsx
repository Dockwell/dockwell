// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { BackupDestinationInfoSchema } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { onPhone, renderAt } from "../../test/render";

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
      await screen.findByText("2 connected · the services Dockwell sends backups to and routes behind"),
    ).toBeVisible();

    const bucket = screen.getByRole("article", { name: "r2" });
    expect(within(bucket).getByText("Received a backup 40 min ago")).toBeVisible();
    expect(bucket).toHaveTextContent("Bucket acme-backups at example.r2.cloudflarestorage.com");
    expect(bucket).toHaveTextContent("12 backups, 186 MB");
    expect(within(bucket).getByRole("link", { name: "Manage in Backups" })).toHaveAttribute("href", "/backups");
    // Nothing has proved the new one works, so it doesn't read as healthy.
    expect(within(screen.getByRole("article", { name: "minio" })).getByText("No backup yet")).toBeVisible();

    const cloudflare = screen.getByRole("article", { name: "Cloudflare" });
    expect(within(cloudflare).getByRole("link", { name: "How to route behind it" })).toHaveAttribute(
      "href",
      "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#behind-cloudflare-tunnel-or-another-proxy",
    );
    const later = screen.getByRole("region", { name: "Coming later" });
    for (const what of ["Git", "Registries", "Cloud providers"]) expect(later).toHaveTextContent(what);
    await expectAccessible(container);
  });

  it("connects S3-compatible storage through the same plan Backups makes", async () => {
    const user = userEvent.setup();
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [] }) } });
    expect(
      await screen.findByText("0 connected · the services Dockwell sends backups to and routes behind"),
    ).toBeVisible();
    await user.click(
      within(screen.getByRole("article", { name: "S3-compatible storage" })).getByRole("button", { name: "Connect" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a destination" });
    expect(within(sheet).getByRole("textbox", { name: "Bucket" })).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
  });

  it("is a section of Settings on a phone", async () => {
    onPhone();
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [r2] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "Integrations" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/settings");
    expect(await screen.findByRole("article", { name: "r2" })).toBeVisible();
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/settings/integrations", { backups: { listBackupDestinations: () => ({ destinations: [r2] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "集成" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "之后推出" })).toBeVisible();
  });
});
