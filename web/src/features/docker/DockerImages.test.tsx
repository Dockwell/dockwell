// SPDX-License-Identifier: AGPL-3.0-only

import {
  DockerCleanupKind,
  DockerImageState,
  DockerObjectKind,
  type PlanDockerMaintenanceRequest,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { webPlanned } from "../../test/changes";
import { feed, quietWatch, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

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
