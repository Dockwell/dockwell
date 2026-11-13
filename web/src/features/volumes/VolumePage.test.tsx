// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  BackupMethod,
  BackupSchema,
  BackupState,
  type ProposeChangeRequest,
  type VolumeInfo,
  VolumeInfoSchema,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { SERVER } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { expectNamesLinked } from "../../test/links";
import { type Api, onPhone, renderAt } from "../../test/render";

function aVolume(init: Partial<MessageInitShape<typeof VolumeInfoSchema>> = {}): VolumeInfo {
  return create(VolumeInfoSchema, {
    id: "vol_1",
    resource: { kind: "volume", scope: "env_1", name: "orders" },
    revision: 1n,
    driver: "local",
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        sizeBytes: 52_428_800n,
        path: "/var/lib/docker/volumes/shop-production_orders/_data",
        containers: 1,
      },
    ],
    mountedBy: [{ serviceId: "svc_shop", service: "shop", target: "/data" }],
    ...init,
  } as MessageInitShape<typeof VolumeInfoSchema>);
}

function api(volume: VolumeInfo, proposals: ProposeChangeRequest[] = []): Api {
  return {
    volumes: { getVolume: () => ({ volume }) },
    backups: {
      listBackups: () => ({
        backups: [
          create(BackupSchema, {
            id: "bkp_1",
            environmentId: "env_1",
            volume: "orders",
            volumeId: "vol_1",
            serverId: SERVER,
            serverName: "hz-1",
            state: BackupState.DONE,
            onServer: true,
            method: BackupMethod.COPY,
            sizeBytes: 1n << 20n,
            startedAt: timestampFromDate(new Date("2026-11-06T03:00:00Z")),
          }),
        ],
      }),
      listBackupPolicies: () => ({}),
      listBackupDestinations: () => ({}),
    },
    changes: {
      proposeChange: (req: ProposeChangeRequest) => {
        proposals.push(req);
        return { change: webPlanned() };
      },
    },
  };
}

describe("a volume's page", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("says what it holds on its server, with its files a step away, and who mounts it, each a link", async () => {
    const { container } = renderAt("/volumes/vol_1", api(aVolume()));
    expect(await screen.findByRole("heading", { level: 1, name: "orders" }, { timeout: 5000 })).toBeVisible();
    const servers = screen.getByRole("region", { name: "On its servers" });
    expect(within(servers).getByText("50 MB")).toBeVisible();
    expect(within(servers).getByRole("link", { name: "hz-1" })).toHaveAttribute("href", `/servers/${SERVER}`);
    expect(within(servers).getByRole("link", { name: "Open its files" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fvar%2Flib%2Fdocker%2Fvolumes%2Fshop-production_orders%2F_data`,
    );
    const mounts = screen.getByRole("region", { name: "Mounted by" });
    expect(within(mounts).getByRole("link", { name: "shop" })).toHaveAttribute("href", "/apps/svc_shop");
    expect(within(mounts).getByText("/data")).toBeVisible();
    expect(await screen.findByRole("table", { name: "Backups of orders, newest first" })).toBeVisible();
    expectNamesLinked(container.querySelector(".volume-page") as HTMLElement, ["hz-1", "shop"]);
    await expectAccessible(container);
  });

  it("deletes through a plan once no app mounts it, and says why not while one does", async () => {
    const user = userEvent.setup();
    const { unmount } = renderAt("/volumes/vol_1", api(aVolume()));
    const held = await screen.findByRole("button", { name: "Delete" }, { timeout: 5000 });
    expect(held).toHaveAttribute("aria-disabled", "true");
    expect(held).toHaveAccessibleDescription("shop mounts it. Take it off first, then delete it.");
    unmount();

    const proposals: ProposeChangeRequest[] = [];
    renderAt("/volumes/vol_1", api(aVolume({ mountedBy: [] }), proposals));
    expect(await screen.findByText("No app mounts it, so deleting it stops nothing.")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.summary).toBe("Delete the volume orders");
    expect(proposals[0]?.edits[0]?.resource).toEqual(
      expect.objectContaining({ kind: "volume", scope: "env_1", name: "orders" }),
    );
    expect(proposals[0]?.edits[0]?.delete).toBe(true);
  });

  it("keeps what it is and Delete on a phone, whose header has room for neither", async () => {
    onPhone();
    renderAt("/volumes/vol_1", api(aVolume({ mountedBy: [] })));
    expect(await screen.findByRole("button", { name: "Delete" }, { timeout: 5000 })).toBeVisible();
    expect(screen.getAllByText("No app mounts it").length).toBeGreaterThan(0);
  });

  it("says why it can't read what a server holds, and reads the rest", async () => {
    renderAt(
      "/volumes/vol_1",
      api(
        aVolume({
          servers: [
            {
              serverId: SERVER,
              serverName: "hz-1",
              unread: "hz-1 isn't connected, so it can't say what this holds there now",
            },
          ],
        }),
      ),
    );
    expect(
      await screen.findByText("hz-1 isn't connected, so it can't say what this holds there now", undefined, {
        timeout: 5000,
      }),
    ).toBeVisible();
    expect(screen.queryByRole("link", { name: "Open its files" })).toBeNull();
  });
});
