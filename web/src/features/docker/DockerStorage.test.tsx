// SPDX-License-Identifier: AGPL-3.0-only

import {
  ContainerState,
  DockerCleanupKind,
  DockerCleanupReason,
  DockerObjectKind,
  DockerOwnership,
  type PlanDockerMaintenanceRequest,
  Role,
  type WatchServersResponse,
} from "@dockwell/api";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { feed, quietWatch, renderAt, renderWithApi } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { DockerStorage } from "./DockerStorage";

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

it("keeps matching selected volumes while searching and hides checkboxes for viewers (#919 N3/N4)", async () => {
  const server = aServer({ id: "srv_1" });
  const makeServers = () => {
    const s = feed<WatchServersResponse>();
    s.push(changed(server), snapshotComplete);
    return s.stream;
  };
  const objects = [
    {
      key: { serverId: server.id, engineId: "alpha.gen", kind: DockerObjectKind.VOLUME },
      name: "alpha",
      ownership: DockerOwnership.EXTERNAL,
      detail: { case: "volume" as const, value: { driver: "local", attachmentCount: 0 } },
    },
    {
      key: { serverId: server.id, engineId: "beta.gen", kind: DockerObjectKind.VOLUME },
      name: "beta",
      ownership: DockerOwnership.EXTERNAL,
      detail: { case: "volume" as const, value: { driver: "local", attachmentCount: 0 } },
    },
  ];
  const { unmount } = renderAt("/servers/srv_1/docker/volumes", {
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: ({ filter }) => ({
        objects: objects.filter((o) => !filter?.search || o.name.includes(filter.search)),
      }),
      previewDockerCleanup: () => ({ snapshot: "f".repeat(64), candidates: [], exclusions: [] }),
    },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: "Select alpha" }));
  await user.type(screen.getByRole("textbox", { name: "Find a volume" }), "alpha");
  await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select beta" })).toBeNull());
  expect(screen.getByRole("checkbox", { name: "Select alpha" })).toBeChecked();
  unmount();

  const viewerRoute = createRootRoute({
    component: () => <DockerStorage volumes servers={[server]} fleet={false} />,
  });
  const viewerRouter = createRouter({
    routeTree: viewerRoute,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  renderWithApi(<RouterProvider router={viewerRouter} />, {
    auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
    servers: { watchServers: makeServers() },
    docker: { watchDockerObjects: quietWatch, listDockerObjects: () => ({ objects }) },
  });
  expect(await screen.findByText("alpha")).toBeVisible();
  expect(screen.queryByRole("checkbox")).toBeNull();
});

it("shows a network detail page before container choices land and opens one live watch (#956)", async () => {
  const server = aServer({ id: "srv_1" });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  let resolveContainers!: (value: { objects: unknown[]; nextPageToken: string }) => void;
  const containersPending = new Promise<{ objects: unknown[]; nextPageToken: string }>((resolve) => {
    resolveContainers = resolve;
  });
  let watchCount = 0;
  renderAt("/servers/srv_1/docker/networks/net_1", {
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: async function* () {
        watchCount += 1;
        await new Promise(() => {});
      },
      getDockerObject: () => ({
        object: {
          key: { serverId: server.id, engineId: "net_1", kind: DockerObjectKind.NETWORK },
          name: "app-net",
          ownership: DockerOwnership.EXTERNAL,
          detail: {
            case: "network" as const,
            value: {
              driver: "bridge",
              scope: "local",
              attachmentCount: 1,
              attachments: [
                {
                  containerId: "ctr_1",
                  name: "web-1",
                  state: ContainerState.RUNNING,
                  ipv4: "172.20.0.2",
                },
              ],
            },
          },
        },
      }),
      listDockerObjects: () => containersPending as never,
    },
  });

  // Network heading and attached row land immediately before the container list resolves.
  expect(await screen.findByRole("heading", { name: "app-net" })).toBeVisible();
  expect(screen.getByRole("link", { name: "web-1" })).toBeVisible();
  const menuButton = screen.getByRole("button", { name: "Actions for web-1" });
  expect(menuButton).toBeDisabled();
  expect(menuButton).toHaveAttribute("aria-busy", "true");
  expect(watchCount).toBe(1);

  resolveContainers({
    objects: [
      {
        key: { serverId: server.id, engineId: "ctr_1", kind: DockerObjectKind.CONTAINER },
        name: "web-1",
        ownership: DockerOwnership.EXTERNAL,
        state: ContainerState.RUNNING,
      },
    ],
    nextPageToken: "",
  });

  const user = userEvent.setup();
  await waitFor(() => expect(menuButton).toBeEnabled());
  await user.click(menuButton);
  expect(screen.getByRole("menuitem", { name: "Take on" })).toBeVisible();
});
