// SPDX-License-Identifier: AGPL-3.0-only

import {
  DockerCleanupKind,
  DockerCleanupReason,
  DockerImageState,
  DockerObjectKind,
  type PlanDockerMaintenanceRequest,
  Role,
  type WatchServersResponse,
} from "@dockwell/api";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { feed, quietWatch, renderAt, renderWithApi } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { DockerImages } from "./DockerImages";

it("requests a fresh first page for the chosen state and shows usage still being measured", async () => {
  const server = aServer({ id: "srv_1", name: "example-server" });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  const request = vi.fn();
  renderAt("/servers/srv_1/docker/images", {
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: (ask) => {
        const state = ask.filter?.imageState ?? DockerImageState.UNSPECIFIED;
        request(state, ask.pageToken);
        return {
          nextPageToken: state === DockerImageState.UNSPECIFIED && !ask.pageToken ? "unfiltered-page" : "",
          objects: [
            {
              key: { kind: DockerObjectKind.IMAGE, serverId: server.id, engineId: `sha256:${"a".repeat(64)}` },
              observedAt: { seconds: 1n },
              detail: {
                case: "image",
                value: { tags: ["fixture/image:latest"], totalBytes: 64n, uniqueBytes: 64n },
              },
            },
          ],
        };
      },
    },
  });
  await userEvent.click(await screen.findByRole("button", { name: "Next" }));
  expect(request).toHaveBeenCalledWith(DockerImageState.UNSPECIFIED, "unfiltered-page");
  await userEvent.click(screen.getByRole("radio", { name: "Used" }));
  expect(await screen.findByRole("img", { name: "Measuring" })).toBeVisible();
  expect(request).toHaveBeenLastCalledWith(DockerImageState.USED, "");
  expect(screen.getByRole("link", { name: "fixture/image:latest" })).toBeVisible();
});

it("mounts 50 image menus without requesting cleanup, then reviews only the image chosen", async () => {
  const server = aServer({ id: "srv_1", name: "example-server" });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  const objects = Array.from({ length: 50 }, (_, index) => ({
    key: {
      kind: DockerObjectKind.IMAGE,
      serverId: server.id,
      engineId: `sha256:${index.toString(16).padStart(64, "0")}`,
    },
    observedAt: { seconds: 1n },
    detail: {
      case: "image" as const,
      value: { tags: [`fixture/row-${index}:1`], state: DockerImageState.UNUSED },
    },
  }));
  const chosen = {
    serverId: server.id,
    engineId: objects[0]?.key.engineId ?? "",
    kind: DockerCleanupKind.IMAGE,
    evidence: "chosen evidence",
  };
  const preview = vi.fn(() => ({
    snapshot: "d".repeat(64),
    candidates: [chosen],
  }));
  const plan = vi.fn((_input: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  renderAt("/servers/srv_1/docker/images", {
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects }),
      previewDockerCleanup: preview,
      planDockerMaintenance: plan,
    },
  });
  await waitFor(() => expect(screen.getAllByRole("button", { name: /^Actions for fixture\/row-/ })).toHaveLength(50));
  expect(preview).not.toHaveBeenCalled();
  expect(plan).not.toHaveBeenCalled();
  await userEvent.click(screen.getByRole("button", { name: "Actions for fixture/row-0:1" }));
  await waitFor(() => expect(preview).toHaveBeenCalledOnce());
  await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
  await waitFor(() => expect(plan).toHaveBeenCalledOnce());
  expect(preview).toHaveBeenCalledTimes(2);
  expect(plan.mock.calls[0]?.[0]).toMatchObject({
    serverIds: [server.id],
    expectedSnapshot: "d".repeat(64),
    operation: {
      case: "cleanup",
      value: { targets: [{ engineId: objects[0]?.key.engineId, evidence: "chosen evidence" }] },
    },
  });
});

it("hides image checkboxes for viewers and drops Review again on lasting refusals (#919 N4/N5)", async () => {
  const server = aServer({ id: "srv_1", name: "example-server" });
  const makeServers = () => {
    const s = feed<WatchServersResponse>();
    s.push(changed(server), snapshotComplete);
    return s.stream;
  };
  const imgId = `sha256:${"b".repeat(64)}`;
  const objects = [
    {
      key: { kind: DockerObjectKind.IMAGE, serverId: server.id, engineId: imgId },
      observedAt: { seconds: 1n },
      detail: { case: "image" as const, value: { tags: ["fixture/in-use:1"], state: DockerImageState.USED } },
    },
  ];
  const viewerRoute = createRootRoute({ component: () => <DockerImages servers={[server]} fleet={false} /> });
  const viewerRouter = createRouter({
    routeTree: viewerRoute,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  const { unmount } = renderWithApi(<RouterProvider router={viewerRouter} />, {
    auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
    servers: { watchServers: makeServers() },
    docker: { watchDockerObjects: quietWatch, listDockerObjects: () => ({ objects }) },
  });
  expect(await screen.findByText("fixture/in-use:1")).toBeVisible();
  expect(screen.queryByRole("checkbox")).toBeNull();
  unmount();

  // Admin clicking Delete on an image that becomes IN_USE gets a lasting error without "Review again."
  let inUse = false;
  renderAt("/servers/srv_1/docker/images", {
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects }),
      previewDockerCleanup: () => ({
        snapshot: "e".repeat(64),
        candidates: inUse
          ? []
          : [{ serverId: server.id, kind: DockerCleanupKind.IMAGE, engineId: imgId, evidence: "ev" }],
        exclusions: inUse
          ? [
              {
                serverId: server.id,
                kind: DockerCleanupKind.IMAGE,
                engineId: imgId,
                reason: DockerCleanupReason.IN_USE,
              },
            ]
          : [],
      }),
    },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Actions for fixture/in-use:1" }));
  const del = await screen.findByRole("menuitem", { name: "Delete" });
  inUse = true;
  await user.click(del);
  const dialog = await screen.findByRole("dialog");
  expect(dialog).toHaveTextContent("Containers still use this image");
  expect(dialog).not.toHaveTextContent("Review again");
});
