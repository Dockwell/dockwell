// SPDX-License-Identifier: AGPL-3.0-only

import {
  ChangeState,
  ContainerState,
  DeploymentState,
  DockerContainerAction,
  DockerObjectKind,
  DockerOwnership,
  type PlanDockerMaintenanceRequest,
  type ProposeChangeRequest,
  Role,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { anApp, places, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { moved, webPlanned } from "../../test/changes";
import { feed, quietWatch, renderAt, renderWithApi } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { DockerComposes } from "./DockerComposes";
import { DockerContainers } from "./DockerContainers";

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
  expect(await within(bar).findByText(/Runs on 1 more server/)).toBeVisible();
  expect(within(bar).getByRole("button", { name: "Open shop" })).toBeVisible();
  await user.click(screen.getByRole("button", { name: "Actions for shop-1" }));
  await user.click(await screen.findByRole("menuitem", { name: "Stop shop on all 2 servers" }));
  await waitFor(() => expect(propose).toHaveBeenCalledOnce());
  expect(propose.mock.calls[0]?.[0]).toMatchObject({
    summary: "Stop shop on all 2 servers",
    edits: [{ resource: app.resource, baseRevision: app.revision }],
  });
});

it("ignores failed deployments on other servers, keeps matching selections while searching, hides checkboxes for viewers, and restricts Kill signals on paused containers (#919 N1/N3/N4, #925 N1)", async () => {
  const second = "srv_2";
  const app = anApp(
    "shop",
    "nginx",
    {},
    {
      deployments: [
        { serverId: SERVER, state: DeploymentState.LIVE },
        { serverId: second, state: DeploymentState.FAILED },
      ],
    },
  );
  const objects = [
    {
      key: { serverId: SERVER, engineId: "shop-1", kind: DockerObjectKind.CONTAINER },
      name: "shop-1",
      state: ContainerState.RUNNING,
      ownership: DockerOwnership.MANAGED,
      managedResource: app.resource,
      managedResourceId: app.id,
      managedRevision: app.revision,
      detail: { case: "container" as const, value: { image: "nginx" } },
    },
    {
      key: { serverId: SERVER, engineId: "paused-ext", kind: DockerObjectKind.CONTAINER },
      name: "paused-ext",
      state: ContainerState.PAUSED,
      ownership: DockerOwnership.EXTERNAL,
      detail: { case: "container" as const, value: { image: "alpine" } },
    },
  ];
  const makeServers = () => {
    const s = feed<WatchServersResponse>();
    s.push(
      changed(aServer({ id: SERVER, name: "first" })),
      changed(aServer({ id: second, name: "second" })),
      snapshotComplete,
    );
    return s.stream;
  };
  const plan = vi.fn((_req: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  const { unmount } = renderAt(`/servers/${SERVER}/docker/containers`, {
    resources: places(),
    services: { listServices: () => ({ services: [app] }) },
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: ({ filter }) => ({
        objects: objects.filter((o) => !filter?.search || o.name.includes(filter.search)),
      }),
      planDockerMaintenance: plan,
    },
  });
  const user = userEvent.setup();
  // Failed deployment on srv_2 does NOT block batch Stop on shop-1.
  await user.click(await screen.findByRole("checkbox", { name: "Select shop-1" }));
  const bar = screen.getByRole("toolbar");
  expect(within(bar).getByRole("button", { name: "Stop 1" })).toBeVisible();
  // Typing in search keeps shop-1 selected while it still matches.
  await user.type(screen.getByRole("textbox", { name: "Search containers" }), "shop");
  await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select paused-ext" })).toBeNull());
  expect(screen.getByRole("checkbox", { name: "Select shop-1" })).toBeChecked();
  await user.clear(screen.getByRole("textbox", { name: "Search containers" }));

  // Kill on a paused external container offers only default + SIGKILL with explanation.
  await user.click(await screen.findByRole("button", { name: "Actions for paused-ext" }));
  await user.click(await screen.findByRole("menuitem", { name: "Kill" }));
  const dialog = await screen.findByRole("dialog", { name: /Kill/ });
  const select = within(dialog).getByRole("combobox", { name: "Signal" });
  const options = within(select)
    .getAllByRole("option")
    .map((o) => o.textContent);
  expect(options).toEqual(["SIGTERM, then SIGKILL", "SIGKILL"]);
  expect(within(dialog).getByText(/A paused container takes only SIGKILL, or SIGTERM then SIGKILL/)).toBeVisible();
  unmount();

  // Viewer sees no selection checkboxes on Containers.
  const viewerRoute = createRootRoute({
    component: () => <DockerContainers servers={[aServer({ id: SERVER, name: "first" })]} fleet={false} />,
  });
  const viewerRouter = createRouter({
    routeTree: viewerRoute,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  renderWithApi(<RouterProvider router={viewerRouter} />, {
    auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
    resources: places(),
    services: { listServices: () => ({ services: [app] }) },
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects }),
    },
  });
  expect(await screen.findByText("paused-ext")).toBeVisible();
  expect(screen.queryByRole("checkbox")).toBeNull();
});

it("restricts Kill signals on a Compose project row when a member container is paused", async () => {
  const server = aServer({ id: SERVER, name: "first" });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  const route = createRootRoute({ component: () => <DockerComposes servers={[server]} serverId={server.id} /> });
  const router = createRouter({ routeTree: route, history: createMemoryHistory({ initialEntries: ["/"] }) });
  renderWithApi(<RouterProvider router={router} />, {
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: (ask) => {
        if (ask.kind === DockerObjectKind.COMPOSE) {
          return {
            objects: [
              {
                key: { serverId: server.id, engineId: "legacy-stack", kind: DockerObjectKind.COMPOSE },
                name: "legacy-stack",
                ownership: DockerOwnership.EXTERNAL,
                detail: {
                  case: "compose",
                  value: { engineProject: "legacy-stack", runningServices: 1, totalServices: 2 },
                },
              },
            ],
          };
        }
        return {
          objects: [
            {
              key: { serverId: server.id, engineId: "legacy-worker", kind: DockerObjectKind.CONTAINER },
              name: "legacy-worker",
              state: ContainerState.PAUSED,
              ownership: DockerOwnership.EXTERNAL,
              detail: { case: "container", value: { image: "alpine", composeProject: "legacy-stack" } },
            },
          ],
        };
      },
    },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Actions for legacy-stack" }));
  await user.click(await screen.findByRole("menuitem", { name: "Kill" }));
  const dialog = await screen.findByRole("dialog", { name: "Kill" });
  expect(
    await within(dialog).findByText(/A paused container takes only SIGKILL, or SIGTERM then SIGKILL/),
  ).toBeVisible();
  const options = within(within(dialog).getByRole("combobox", { name: "Signal" }))
    .getAllByRole("option")
    .map((o) => o.textContent);
  expect(options).toEqual(["SIGTERM, then SIGKILL", "SIGKILL"]);
});

it("unifies Stop/Pause across mixed managed and external containers, chains the external plan after apply, and keeps bar skip notes concise", async () => {
  const app = anApp("shop", "nginx", {}, { deployments: [{ serverId: SERVER, state: DeploymentState.LIVE }] });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(aServer({ id: SERVER, name: "first" })), snapshotComplete);
  const managedChange = webPlanned();
  const externalChange = moved(webPlanned(), { id: "chg_ext", summary: "Stop redis-1" });
  const changesStream = feed<WatchChangeResponse>();
  const propose = vi.fn((_request: ProposeChangeRequest) => ({ change: managedChange }));
  const apply = vi.fn(() => ({ change: moved(managedChange, { state: ChangeState.APPLIED }) }));
  const plan = vi.fn((_request: PlanDockerMaintenanceRequest) => ({ change: externalChange }));
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
          {
            key: { serverId: SERVER, engineId: "redis-1", kind: DockerObjectKind.CONTAINER },
            name: "redis-1",
            state: ContainerState.RUNNING,
            ownership: DockerOwnership.EXTERNAL,
            detail: { case: "container", value: { image: "redis:7" } },
          },
          {
            key: { serverId: SERVER, engineId: "dockwell-proxy", kind: DockerObjectKind.CONTAINER },
            name: "dockwell-proxy",
            state: ContainerState.RUNNING,
            ownership: DockerOwnership.SYSTEM,
            detail: { case: "container", value: { image: "caddy:2" } },
          },
        ],
      }),
      planDockerMaintenance: plan,
    },
    changes: {
      proposeChange: propose,
      applyChange: apply,
      watchChange: changesStream.stream,
      discardChange: () => ({}),
    },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: "Select shop-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select redis-1" }));
  const bar = screen.getByRole("toolbar");
  expect(within(bar).getAllByRole("button", { name: /^Stop / })).toHaveLength(1);
  expect(within(bar).getByRole("button", { name: "Stop 2" })).toBeVisible();
  expect(within(bar).getAllByRole("button", { name: /^Pause / })).toHaveLength(1);
  expect(within(bar).getByRole("button", { name: "Pause 2" })).toBeVisible();
  expect(within(bar).queryByRole("button", { name: /Rename/ })).toBeNull();

  // Unverified Running badges use the running tone, never green healthy.
  const badges = document.querySelectorAll(".dw-badge.dw-tone-healthy");
  expect([...badges].some((el) => el.textContent === "Running")).toBe(false);

  // Adding a system container shows one concise skip line without a contradictory multi-verb wall.
  await user.click(screen.getByRole("checkbox", { name: "Select dockwell-proxy" }));
  expect(within(bar).getByText("1 system container skipped")).toBeVisible();
  expect(within(bar).queryByText(/Already stopped/)).toBeNull();
  expect(within(bar).getByRole("button", { name: "Stop 2" })).toHaveAttribute(
    "title",
    expect.stringContaining("System container"),
  );

  // Clicking Stop 2 opens the managed plan with an upfront note about the follow-up external plan.
  await user.click(within(bar).getByRole("button", { name: "Stop 2" }));
  const dialog = await screen.findByRole("dialog", { name: "Review plan" });
  expect(propose).toHaveBeenCalledOnce();
  expect(plan).not.toHaveBeenCalled();
  expect(within(dialog).getByText("Then stops 1 container Dockwell didn't make, in its own plan.")).toBeVisible();

  // Applying and closing the managed plan immediately plans ONLY the external container with STOP.
  await user.click(within(dialog).getByRole("button", { name: "Apply plan" }));
  const done = await within(dialog).findByRole("button", { name: "Done" });
  await user.click(done);
  await waitFor(() => expect(plan).toHaveBeenCalledOnce());
  expect(plan.mock.calls[0]?.[0]).toMatchObject({
    operation: {
      case: "lifecycle",
      value: {
        action: DockerContainerAction.STOP,
        keys: [{ serverId: SERVER, engineId: "redis-1", kind: DockerObjectKind.CONTAINER }],
      },
    },
  });
});

it("does not plan external containers when the managed plan is cancelled or fails, and explains they weren't touched", async () => {
  const app = anApp("shop", "nginx", {}, { deployments: [{ serverId: SERVER, state: DeploymentState.LIVE }] });
  const makeServers = () => {
    const s = feed<WatchServersResponse>();
    s.push(changed(aServer({ id: SERVER, name: "first" })), snapshotComplete);
    return s.stream;
  };
  const objects = [
    {
      key: { serverId: SERVER, engineId: "shop-1", kind: DockerObjectKind.CONTAINER },
      name: "shop-1",
      state: ContainerState.RUNNING,
      ownership: DockerOwnership.MANAGED,
      managedResource: app.resource,
      managedResourceId: app.id,
      managedRevision: app.revision,
      detail: { case: "container" as const, value: { image: "nginx" } },
    },
    {
      key: { serverId: SERVER, engineId: "redis-1", kind: DockerObjectKind.CONTAINER },
      name: "redis-1",
      state: ContainerState.RUNNING,
      ownership: DockerOwnership.EXTERNAL,
      detail: { case: "container" as const, value: { image: "redis:7" } },
    },
  ];
  const managedChange = webPlanned();
  const plan = vi.fn((_request: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  const propose = vi.fn((_request: ProposeChangeRequest) => ({ change: managedChange }));
  const discard = vi.fn(() => ({}));
  const { unmount } = renderAt(`/servers/${SERVER}/docker/containers`, {
    resources: places(),
    services: { listServices: () => ({ services: [app] }) },
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects }),
      planDockerMaintenance: plan,
    },
    changes: { proposeChange: propose, discardChange: discard },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: "Select shop-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select redis-1" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Stop 2" }));
  const dialog = await screen.findByRole("dialog", { name: "Review plan" });
  await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(plan).not.toHaveBeenCalled();

  // Only the untouched external container stays selected with its own Stop 1 button and one-line explanation.
  const bar = screen.getByRole("toolbar");
  expect(
    within(bar).getByText("1 container Dockwell didn't make wasn't stopped because the managed plan didn't apply."),
  ).toBeVisible();
  expect(within(bar).getByRole("button", { name: "Stop 1" })).toBeVisible();
  expect(screen.getByRole("checkbox", { name: "Select redis-1" })).toBeChecked();
  expect(screen.getByRole("checkbox", { name: "Select shop-1" })).not.toBeChecked();
  unmount();

  // Failed managed plan also never triggers external planDockerMaintenance.
  const failStream = feed<WatchChangeResponse>();
  const failPlan = vi.fn((_request: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  renderAt(`/servers/${SERVER}/docker/containers`, {
    resources: places(),
    services: { listServices: () => ({ services: [app] }) },
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects }),
      planDockerMaintenance: failPlan,
    },
    changes: {
      proposeChange: () => ({ change: managedChange }),
      applyChange: () => ({ change: moved(managedChange, { state: ChangeState.FAILED, error: "agent rejected" }) }),
      watchChange: failStream.stream,
    },
  });
  await user.click(await screen.findByRole("checkbox", { name: "Select shop-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select redis-1" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Stop 2" }));
  const failDialog = await screen.findByRole("dialog", { name: "Review plan" });
  await user.click(within(failDialog).getByRole("button", { name: "Apply plan" }));
  await user.click(await within(failDialog).findByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(failPlan).not.toHaveBeenCalled();
  expect(
    within(screen.getByRole("toolbar")).getByText(
      "1 container Dockwell didn't make wasn't stopped because the managed plan didn't apply.",
    ),
  ).toBeVisible();
  cleanup();

  // Closed while the managed plan still applies: nothing says it failed, and the external row only waits.
  const applyingStream = feed<WatchChangeResponse>();
  const applyingPlan = vi.fn((_request: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  renderAt(`/servers/${SERVER}/docker/containers`, {
    resources: places(),
    services: { listServices: () => ({ services: [app] }) },
    servers: { watchServers: makeServers() },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({ objects }),
      planDockerMaintenance: applyingPlan,
    },
    changes: {
      proposeChange: () => ({ change: managedChange }),
      applyChange: () => ({ change: moved(managedChange, { state: ChangeState.APPLYING }) }),
      watchChange: applyingStream.stream,
    },
  });
  await user.click(await screen.findByRole("checkbox", { name: "Select shop-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select redis-1" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Stop 2" }));
  const applyingDialog = await screen.findByRole("dialog", { name: "Review plan" });
  await user.click(within(applyingDialog).getByRole("button", { name: "Apply plan" }));
  await within(applyingDialog).findByText(/keeps applying/i);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(applyingPlan).not.toHaveBeenCalled();
  const applyingBar = screen.getByRole("toolbar");
  expect(within(applyingBar).getByText("1 container Dockwell didn't make waits for its own plan.")).toBeVisible();
  expect(within(applyingBar).queryByText(/didn't apply/)).toBeNull();
});

it("sends external-only selections straight to planDockerMaintenance without proposeChange", async () => {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(aServer({ id: SERVER, name: "first" })), snapshotComplete);
  const propose = vi.fn((_request: ProposeChangeRequest) => ({ change: webPlanned() }));
  const plan = vi.fn((_request: PlanDockerMaintenanceRequest) => ({ change: webPlanned() }));
  renderAt(`/servers/${SERVER}/docker/containers`, {
    resources: places(),
    servers: { watchServers: servers.stream },
    docker: {
      watchDockerObjects: quietWatch,
      listDockerObjects: () => ({
        objects: [
          {
            key: { serverId: SERVER, engineId: "redis-1", kind: DockerObjectKind.CONTAINER },
            name: "redis-1",
            state: ContainerState.RUNNING,
            ownership: DockerOwnership.EXTERNAL,
            detail: { case: "container", value: { image: "redis:7" } },
          },
        ],
      }),
      planDockerMaintenance: plan,
    },
    changes: { proposeChange: propose },
  });
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: "Select redis-1" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Stop 1" }));
  await waitFor(() => expect(plan).toHaveBeenCalledOnce());
  expect(propose).not.toHaveBeenCalled();
  expect(plan.mock.calls[0]?.[0]).toMatchObject({
    operation: {
      case: "lifecycle",
      value: {
        action: DockerContainerAction.STOP,
        keys: [{ serverId: SERVER, engineId: "redis-1", kind: DockerObjectKind.CONTAINER }],
      },
    },
  });
});
