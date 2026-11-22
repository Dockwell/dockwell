// SPDX-License-Identifier: AGPL-3.0-only

import { DockerImageState, DockerObjectKind, type WatchServersResponse } from "@dockwell/api";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

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
