// SPDX-License-Identifier: AGPL-3.0-only

import {
  DockerCleanupKind,
  DockerCleanupReason,
  DockerObjectKind,
  DockerOwnership,
  type PlanDockerMaintenanceRequest,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { webPlanned } from "../../test/changes";
import { feed, quietWatch, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

it("explains a backup hold and leaves out a volume that was replaced before the plan", async () => {
  const server = aServer({ id: "srv_1" });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  const storage = (name: string, engineId: string) => ({
    key: { serverId: server.id, engineId, kind: DockerObjectKind.VOLUME },
    name,
    ownership: DockerOwnership.EXTERNAL,
    detail: { case: "volume" as const, value: { driver: "local", attachmentCount: 0 } },
  });
  const cleanup = (name: string, engineId: string) => ({
    serverId: server.id,
    kind: DockerCleanupKind.VOLUME,
    engineId,
    name,
    evidence: "evidence",
  });
  const kept = cleanup("backup", "backup.generation");
  const stable = cleanup("stable", "stable.generation");
  const replaced = cleanup("same-name", "same-name.before");
  let recreated = false;
  const preview = vi.fn(() => ({
    snapshot: "d".repeat(64),
    candidates: [stable, recreated ? cleanup(replaced.name, "same-name.after") : replaced],
    exclusions: [{ ...kept, evidence: "", reason: DockerCleanupReason.BACKUP }],
  }));
  const plan = vi.fn((_request: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  renderAt("/servers/srv_1/docker/volumes", {
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects: [kept, stable, replaced].map((row) => storage(row.name, row.engineId)) }),
      previewDockerCleanup: preview,
      planDockerMaintenance: plan,
    },
  });
  const user = userEvent.setup();
  const menuButton = await screen.findByRole("button", { name: "Actions for backup" });
  expect(preview).not.toHaveBeenCalled();
  await user.click(menuButton);
  const menu = screen.getByRole("menu", { name: "Actions for backup" });
  const off = await within(menu).findByRole("menuitem", { name: /Held by a backup or backup schedule/ });
  expect(off).toHaveAttribute("aria-disabled", "true");
  await user.keyboard("{Escape}");
  for (const name of [kept.name, stable.name, replaced.name]) {
    await user.click(screen.getByRole("checkbox", { name: `Select ${name}` }));
  }
  const bar = screen.getByRole("toolbar");
  const remove = await within(bar).findByRole("button", { name: "Delete 2" });
  expect(bar).toHaveTextContent("1 skipped (Held by a backup or backup schedule)");
  recreated = true;
  await user.click(remove);
  await waitFor(() => expect(plan).toHaveBeenCalledOnce());
  expect(plan.mock.calls[0]?.[0].operation).toMatchObject({ case: "cleanup", value: { targets: [stable] } });
  const sheet = await screen.findByRole("dialog", { name: "Review plan" });
  expect(sheet).toHaveTextContent("1 skipped (This volume is gone or has been replaced)");
  expect(sheet).toHaveTextContent("1 skipped (Held by a backup or backup schedule)");
});
