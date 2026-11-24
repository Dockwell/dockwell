// SPDX-License-Identifier: AGPL-3.0-only

import {
  ContainerState,
  DeploymentState,
  DockerObjectKind,
  DockerOwnership,
  type ProposeChangeRequest,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { anApp, places, SERVER } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { feed, quietWatch, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

it("skips a partial service in a batch and names the whole service and server count in its row action", async () => {
  const second = "srv_2";
  const app = anApp(
    "shop",
    "nginx",
    {},
    {
      deployments: [SERVER, second].map((serverId) => ({ serverId, state: DeploymentState.LIVE })),
    },
  );
  const servers = feed<WatchServersResponse>();
  servers.push(
    changed(aServer({ id: SERVER, name: "first" })),
    changed(aServer({ id: second, name: "second" })),
    snapshotComplete,
  );
  const propose = vi.fn((_request: ProposeChangeRequest) => ({ change: webPlanned() }));
  renderAt(`/servers/${SERVER}/docker/containers`, {
    resources: places(),
    services: { listServices: () => ({ services: [app] }) },
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({
        objects: [
          {
            key: { serverId: SERVER, engineId: "shop-1", kind: DockerObjectKind.CONTAINER },
            name: "shop-1",
            state: ContainerState.RUNNING,
            ownership: DockerOwnership.MANAGED,
            managedResource: app.resource,
            managedResourceId: app.id,
            managedRevision: app.revision,
            detail: { case: "container", value: { image: "nginx" } },
          },
        ],
      }),
    },
    changes: { proposeChange: propose },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: "Select shop-1" }));
  const bar = screen.getByRole("toolbar");
  expect(within(bar).queryByRole("button", { name: /^Stop/ })).toBeNull();
  expect(await within(bar).findByText(/Select a matching replica on every server/)).toBeVisible();
  await user.click(screen.getByRole("button", { name: "Actions for shop-1" }));
  await user.click(await screen.findByRole("menuitem", { name: "Stop shop on all 2 servers" }));
  await waitFor(() => expect(propose).toHaveBeenCalledOnce());
  expect(propose.mock.calls[0]?.[0]).toMatchObject({
    summary: "Stop shop on all 2 servers",
    edits: [{ resource: app.resource, baseRevision: app.revision }],
  });
});
