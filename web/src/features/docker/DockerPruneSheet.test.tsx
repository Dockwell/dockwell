// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  DockerCleanupKind,
  DockerCleanupReason,
  DockerCleanupServerLimitsSchema,
  type DockerCleanupTarget,
  DockerCleanupTargetSchema,
  DockerMaintenanceSchema,
  DockerReadState,
  DockerReceiptReason,
  DockerReceiptSchema,
  DockerReceiptState,
  DockerUnavailableSchema,
  PlanAction,
  PlannedResourceSchema,
  PlanSchema,
  PlanStepSchema,
  type PreviewDockerCleanupRequest,
  PreviewDockerCleanupResponseSchema,
  ServerPlanSchema,
  StepProgressSchema,
  StepState,
} from "@dockwell/api";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { renderWithApi } from "../../test/render";
import { track } from "../../test/routers";
import { ChangeSheetProvider, useChangeSheet } from "../changes/ChangeSheet";
import { cleanupTargetDisplayName, cleanupTargetFullName } from "./cleanupHelpers";
import { DockerCleanup } from "./DockerCleanup";
import { DockerCleanupReceipts } from "./DockerCleanupReceipts";
import { DockerPruneSheet } from "./DockerPruneSheet";
import { readFleetDockerSearch, readServerCleanupSearch } from "./search";

function withRouter(node: ReactNode) {
  const rootRoute = createRootRoute({ component: () => <ChangeSheetProvider>{node}</ChangeSheetProvider> });
  const router = track(createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: ["/"] }) }));
  return <RouterProvider router={router} />;
}

afterEach(() => {
  vi.resetAllMocks();
});

it("shows skeleton loading while Docker measures disk use", async () => {
  renderWithApi(
    <ChangeSheetProvider>
      <DockerPruneSheet kind={DockerCleanupKind.VOLUME} servers={[{ id: "srv_1", name: "alpha" }]} onClose={() => {}} />
    </ChangeSheetProvider>,
    {
      docker: {
        previewDockerCleanup: () =>
          create(PreviewDockerCleanupResponseSchema, {
            unavailable: [create(DockerUnavailableSchema, { serverId: "srv_1", state: DockerReadState.UNSPECIFIED })],
          }),
      },
    },
  );
  expect(screen.getByRole("dialog", { name: "Remove unused volumes" })).toBeVisible();
  expect(screen.getByRole("button", { name: "Review plan" })).toBeDisabled();
  expect(await screen.findByText("Checking Docker artifacts")).toBeInTheDocument();
  await waitFor(() => expect(document.querySelector(".dw-skeleton-row")).toBeInTheDocument());
});

it("preselects shortened anonymous volumes with copy and leaves named volumes unselected", async () => {
  const fullAnon = "a".repeat(64);
  const anon = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.VOLUME,
    engineId: "local/anon-key",
    name: fullAnon,
    reason: DockerCleanupReason.ANONYMOUS,
    evidence: "ev-anon",
    estimatedBytes: 16n * 1024n * 1024n,
  });
  const named = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.VOLUME,
    engineId: "local/named-key",
    name: "old-uploads",
    reason: DockerCleanupReason.UNUSED,
    evidence: "ev-named",
    estimatedBytes: 32n * 1024n * 1024n,
  });
  const composeProtected = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.VOLUME,
    engineId: "local/compose-key",
    name: "shop-db",
    reason: DockerCleanupReason.COMPOSE,
    composeProject: "shop",
    composeId: "outside:shop",
    estimatedBytes: 64n * 1024n * 1024n,
  });

  const plan = vi.fn();
  renderWithApi(
    withRouter(
      <DockerPruneSheet
        kind={DockerCleanupKind.VOLUME}
        servers={[{ id: "srv_1", name: "alpha" }]}
        onClose={() => {}}
      />,
    ),
    {
      docker: {
        previewDockerCleanup: () =>
          create(PreviewDockerCleanupResponseSchema, {
            snapshot: "snap-vol",
            candidates: [anon, named],
            exclusions: [composeProtected],
          }),
        planDockerMaintenance: plan,
      },
    },
  );

  const anonCheckbox = await screen.findByRole("checkbox", { name: new RegExp(fullAnon.slice(0, 12)) });
  const namedCheckbox = screen.getByRole("checkbox", { name: /old-uploads/ });
  expect(anonCheckbox).toBeChecked();
  expect(namedCheckbox).not.toBeChecked();
  expect(screen.getByTitle(fullAnon)).toHaveTextContent(fullAnon.slice(0, 12));
  expect(screen.getByRole("button", { name: "Copy volume name" })).toBeVisible();
  expect(screen.getByText("Data is deleted")).toBeVisible();
  expect(screen.getByText("Protected (1 item)")).toBeVisible();
  expect(screen.getByText("Belongs to Compose project shop")).toBeInTheDocument();
  expect(screen.getByText("Open Compose project")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Review plan" }));
  await waitFor(() => expect(plan).toHaveBeenCalledOnce());
  const req = plan.mock.calls[0]?.[0];
  expect(req?.expectedSnapshot).toBe("snap-vol");
  expect(req?.operation?.case).toBe("cleanup");
  if (req?.operation?.case === "cleanup") {
    expect(req.operation.value.targets.map((row: DockerCleanupTarget) => row.engineId)).toEqual(["local/anon-key"]);
  }
});

it("shows Close only when nothing can be removed", async () => {
  const swarmProtected = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.VOLUME,
    engineId: "local/swarm-key",
    name: "db-data",
    reason: DockerCleanupReason.SWARM_MANAGER,
  });
  renderWithApi(
    <ChangeSheetProvider>
      <DockerPruneSheet kind={DockerCleanupKind.VOLUME} servers={[{ id: "srv_1", name: "alpha" }]} onClose={() => {}} />
    </ChangeSheetProvider>,
    {
      docker: {
        previewDockerCleanup: () =>
          create(PreviewDockerCleanupResponseSchema, {
            snapshot: "snap-empty",
            candidates: [],
            exclusions: [swarmProtected],
          }),
      },
    },
  );
  expect(await screen.findByText("No unused volumes to remove")).toBeVisible();
  expect(screen.getByText("db-data")).toBeVisible();
  expect(screen.getByText("Swarm manager: remove volumes with docker volume rm")).toBeVisible();
  expect(screen.getAllByRole("button", { name: "Close" }).length).toBeGreaterThanOrEqual(2);
  expect(screen.queryByRole("button", { name: "Review plan" })).toBeNull();
});

it("bounds default selection to server limits and notes remaining items", async () => {
  const images = Array.from({ length: 14 }, (_, i) =>
    create(DockerCleanupTargetSchema, {
      serverId: "srv_old",
      kind: DockerCleanupKind.IMAGE,
      engineId: `sha256:${String(i).padStart(2, "0")}${"a".repeat(62)}`,
      reason: DockerCleanupReason.DANGLING,
      evidence: `ev-${i}`,
      estimatedBytes: BigInt((i + 1) * 1024 * 1024),
    }),
  );
  renderWithApi(
    <ChangeSheetProvider>
      <DockerPruneSheet
        kind={DockerCleanupKind.IMAGE}
        servers={[{ id: "srv_old", name: "legacy" }]}
        onClose={() => {}}
      />
    </ChangeSheetProvider>,
    {
      docker: {
        previewDockerCleanup: () =>
          create(PreviewDockerCleanupResponseSchema, {
            snapshot: "snap-legacy",
            candidates: images,
            maxTargetsTotal: 500,
            serverLimits: [
              create(DockerCleanupServerLimitsSchema, {
                serverId: "srv_old",
                maxTargets: 12,
                supportedKinds: [DockerCleanupKind.IMAGE, DockerCleanupKind.BUILD_CACHE, DockerCleanupKind.NETWORK],
              }),
            ],
          }),
      },
    },
  );
  expect(await screen.findByText(/12 selected/)).toBeVisible();
  expect(screen.getByText(/2 more after this cleanup/)).toBeVisible();
});

it("keeps arrived sections visible while another server is still measuring and holds Review plan", async () => {
  const arrived = create(DockerCleanupTargetSchema, {
    serverId: "srv_fast",
    kind: DockerCleanupKind.IMAGE,
    engineId: `sha256:${"b".repeat(64)}`,
    reason: DockerCleanupReason.DANGLING,
    evidence: "ev-fast",
    estimatedBytes: 10n * 1024n * 1024n,
  });
  renderWithApi(
    <ChangeSheetProvider>
      <DockerPruneSheet
        kind={DockerCleanupKind.IMAGE}
        servers={[
          { id: "srv_fast", name: "fast" },
          { id: "srv_slow", name: "slow" },
        ]}
        onClose={() => {}}
      />
    </ChangeSheetProvider>,
    {
      docker: {
        previewDockerCleanup: () =>
          create(PreviewDockerCleanupResponseSchema, {
            snapshot: "snap-partial",
            candidates: [arrived],
            unavailable: [
              create(DockerUnavailableSchema, { serverId: "srv_slow", state: DockerReadState.UNSPECIFIED }),
            ],
          }),
      },
    },
  );
  expect(await screen.findByText("bbbbbbbbbbbb")).toBeVisible();
  expect(screen.getByRole("button", { name: "Review plan" })).toBeDisabled();
});

it("keeps both cleanup pages visible until Refresh finishes", async () => {
  const candidate = (id: string) =>
    create(DockerCleanupTargetSchema, {
      serverId: "srv_1",
      kind: DockerCleanupKind.IMAGE,
      engineId: `sha256:${id.repeat(64)}`,
      reason: DockerCleanupReason.DANGLING,
      evidence: id,
      estimatedBytes: 1024n,
    });
  let finishRefresh = () => {};
  const waitForSecondPage = new Promise<void>((resolve) => {
    finishRefresh = resolve;
  });
  let refreshing = false;
  const previewDockerCleanup = vi.fn(async ({ pageToken }: PreviewDockerCleanupRequest) => {
    if (pageToken && refreshing) await waitForSecondPage;
    return create(PreviewDockerCleanupResponseSchema, {
      snapshot: refreshing ? "new-snapshot" : "old-snapshot",
      candidates: [candidate(pageToken ? (refreshing ? "c" : "b") : "a")],
      nextPageToken: pageToken ? "" : refreshing ? "new-page" : "old-page",
    });
  });
  renderWithApi(withRouter(<DockerCleanup servers={[{ id: "srv_1", name: "alpha" }]} />), {
    docker: { previewDockerCleanup },
  });
  expect(await screen.findByText("bbbbbbbbbbbb")).toBeVisible();
  const review = screen.getByRole("button", { name: "Review plan" });
  await waitFor(() => expect(review).toBeEnabled());

  refreshing = true;
  fireEvent.click(screen.getByRole("button", { name: "Refresh cleanup" }));
  try {
    await waitFor(() => expect(previewDockerCleanup.mock.calls.at(-1)?.[0].pageToken).toBe("new-page"));
    expect(screen.getByText("aaaaaaaaaaaa")).toBeVisible();
    expect(screen.getByText("bbbbbbbbbbbb")).toBeVisible();
    expect(review).toBeDisabled();
  } finally {
    finishRefresh();
  }
  expect(await screen.findByText("cccccccccccc")).toBeVisible();
  expect(screen.queryByText("bbbbbbbbbbbb")).toBeNull();
  expect(review).toBeEnabled();
});

it("states the consequence and offers Refresh when the cleanup snapshot is stale", async () => {
  renderWithApi(
    <ChangeSheetProvider>
      <DockerPruneSheet kind={DockerCleanupKind.IMAGE} servers={[{ id: "srv_1", name: "alpha" }]} onClose={() => {}} />
    </ChangeSheetProvider>,
    {
      docker: {
        previewDockerCleanup: () => {
          throw new ConnectError("the Docker cleanup candidates changed", Code.Aborted, {
            "Dockwell-Error": "docker_cleanup_changed",
          });
        },
      },
    },
  );
  expect(await screen.findByText("Cleanup candidates changed")).toBeVisible();
  expect(screen.getByText("This preview is out of date and can't be reviewed until you refresh.")).toBeVisible();
  expect(screen.getByRole("button", { name: "Refresh" })).toBeVisible();
});

it("shows pinned image name:tag instead of bare ID and shows changed reasons per row", () => {
  const pinnedId = `sha256:${"a".repeat(64)}`;
  const pinned = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.IMAGE,
    engineId: pinnedId,
    name: "busybox:1.36",
    reason: DockerCleanupReason.UNUSED,
    imageDigests: [`busybox@${pinnedId}`],
    estimatedBytes: 2048n,
  });
  const danglingId = `sha256:${"b".repeat(64)}`;
  const dangling = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.IMAGE,
    engineId: danglingId,
    name: danglingId,
    reason: DockerCleanupReason.DANGLING,
    estimatedBytes: 1024n,
  });
  const net = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.NETWORK,
    engineId: "c".repeat(64),
    name: "old-net",
    reason: DockerCleanupReason.UNUSED,
  });
  const cache = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.BUILD_CACHE,
    engineId: "d".repeat(25),
    name: "d".repeat(25),
    reason: DockerCleanupReason.IDLE,
  });

  const resizedCache = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.BUILD_CACHE,
    engineId: "e".repeat(25),
    name: "e".repeat(25),
    reason: DockerCleanupReason.IDLE,
  });

  expect(cleanupTargetDisplayName(pinned)).toBe("busybox:1.36");
  expect(cleanupTargetFullName(pinned)).toBe("busybox:1.36");
  expect(cleanupTargetDisplayName(dangling)).toBe("bbbbbbbbbbbb");
  expect(cleanupTargetFullName(dangling)).toBe(danglingId);

  renderWithApi(
    <DockerCleanupReceipts
      targets={[dangling, pinned, net, cache, resizedCache]}
      receipts={[
        create(DockerReceiptSchema, {
          target: dangling,
          state: DockerReceiptState.REMOVED,
          beforeBytes: 1024n,
          afterBytes: 0n,
        }),
        create(DockerReceiptSchema, {
          target: pinned,
          state: DockerReceiptState.CHANGED,
          reason: DockerReceiptReason.IN_USE,
        }),
        create(DockerReceiptSchema, {
          target: net,
          state: DockerReceiptState.CHANGED,
          reason: DockerReceiptReason.GONE,
        }),
        create(DockerReceiptSchema, {
          target: cache,
          state: DockerReceiptState.CHANGED,
          reason: DockerReceiptReason.NEWER,
        }),
        create(DockerReceiptSchema, {
          target: resizedCache,
          state: DockerReceiptState.CHANGED,
        }),
      ]}
    />,
    {},
  );

  expect(screen.getByText("busybox:1.36")).toBeVisible();
  expect(screen.getByText(/1 removed · 4 changed since you reviewed/)).toBeVisible();
  expect(screen.getByText("Removed")).toBeVisible();
  expect(screen.getByText("Now in use")).toBeVisible();
  expect(screen.getByText("Already gone")).toBeVisible();
  expect(screen.getByText("Newer than preview")).toBeVisible();
  expect(screen.getByText("Changed")).toBeVisible();
  expect(screen.queryByText("Review again")).toBeNull();
});

it("reopens a fresh preview with original scope on Review again from PruneSheet, Cleanup tab, and Activity", async () => {
  const idA = `sha256:${"a".repeat(64)}`;
  const idB = `sha256:${"b".repeat(64)}`;
  const targetA = create(DockerCleanupTargetSchema, {
    serverId: "srv_1",
    kind: DockerCleanupKind.IMAGE,
    engineId: idA,
    name: idA,
    reason: DockerCleanupReason.DANGLING,
    evidence: "ev-a",
    estimatedBytes: 4096n,
  });
  const targetB = create(DockerCleanupTargetSchema, {
    serverId: "srv_2",
    kind: DockerCleanupKind.IMAGE,
    engineId: idB,
    name: "busybox:1.36",
    reason: DockerCleanupReason.UNUSED,
    imageDigests: [`busybox@${idB}`],
    evidence: "ev-b",
    estimatedBytes: 2048n,
  });
  const makeChange = (
    state: ChangeState,
    servers: string[],
    receipts: ReturnType<typeof create<typeof DockerReceiptSchema>>[],
  ) =>
    create(ChangeSchema, {
      id: "chg_partial",
      summary: "Remove unused Docker images",
      state,
      plan: create(PlanSchema, {
        resources: [
          create(PlannedResourceSchema, {
            id: "host_1",
            action: PlanAction.UPDATE,
            dockerMaintenance: create(DockerMaintenanceSchema, {
              changeId: "chg_partial",
              targets: [targetA, targetB],
            }),
          }),
        ],
        servers: servers.map((serverId) =>
          create(ServerPlanSchema, {
            serverId,
            serverName: serverId,
            steps: [
              create(PlanStepSchema, {
                resourceId: "host_1",
                action: PlanAction.UPDATE,
                dockerMaintenance: create(DockerMaintenanceSchema, {
                  changeId: "chg_partial",
                  targets: [targetA, targetB].filter((row) => row.serverId === serverId || servers.length === 1),
                }),
              }),
            ],
          }),
        ),
      }),
      steps: servers.map((serverId) =>
        create(StepProgressSchema, {
          serverId,
          resourceId: "host_1",
          state: state === ChangeState.APPLIED ? StepState.DONE : StepState.WAITING,
          dockerReceipts: receipts.filter((row) => (row.target?.serverId || serverId) === serverId),
        }),
      ),
    });

  // 1. From Images page (DockerPruneSheet with onReviewAgain): reopens the prune sheet and fetches a fresh preview.
  const imgPreview = vi.fn((_req: PreviewDockerCleanupRequest) =>
    create(PreviewDockerCleanupResponseSchema, {
      snapshot: "snap-img",
      candidates: [targetA],
    }),
  );
  const singleApplied = makeChange(
    ChangeState.APPLIED,
    ["srv_1"],
    [
      create(DockerReceiptSchema, {
        target: targetA,
        state: DockerReceiptState.REMOVED,
        beforeBytes: 4096n,
        afterBytes: 0n,
      }),
      create(DockerReceiptSchema, {
        target: create(DockerCleanupTargetSchema, { ...targetB, serverId: "srv_1" }),
        state: DockerReceiptState.CHANGED,
        reason: DockerReceiptReason.PROTECTED,
      }),
    ],
  );
  const singlePlanned = makeChange(ChangeState.PLANNED, ["srv_1"], []);
  function ImagesHost() {
    const [pruning, setPruning] = useState(true);
    return pruning ? (
      <DockerPruneSheet
        kind={DockerCleanupKind.IMAGE}
        servers={[{ id: "srv_1", name: "alpha" }]}
        onClose={() => setPruning(false)}
        onReviewAgain={() => setPruning(true)}
      />
    ) : null;
  }
  const view1 = renderWithApi(withRouter(<ImagesHost />), {
    docker: {
      previewDockerCleanup: imgPreview,
      planDockerMaintenance: () => ({ change: singlePlanned }),
    },
    changes: {
      applyChange: () => ({ change: singleApplied }),
      getChange: () => ({ change: singleApplied }),
    },
  });
  expect(await screen.findByText("aaaaaaaaaaaa")).toBeVisible();
  expect(imgPreview).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "Review plan" }));
  fireEvent.click(await screen.findByRole("button", { name: /^Apply plan/ }));
  expect(await screen.findByRole("heading", { name: "Docker cleanup complete" })).toBeVisible();
  expect(screen.getByText("1 artifact removed")).toBeVisible();
  expect(screen.getByText("1 changed since you reviewed")).toBeVisible();
  expect(screen.getByText("Now protected")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Review again" }));
  expect(await screen.findByRole("dialog", { name: "Remove unused images" })).toBeVisible();
  await waitFor(() => expect(imgPreview.mock.calls.length).toBeGreaterThanOrEqual(2));
  expect(imgPreview.mock.calls.at(-1)?.[0]?.serverIds).toEqual(["srv_1"]);
  expect(imgPreview.mock.calls.at(-1)?.[0]?.filters?.kinds).toEqual([DockerCleanupKind.IMAGE]);
  view1.unmount();

  // 2. From Cleanup tab: refreshes the mounted cleanup preview with its original server and all-kinds scope.
  const tabPreview = vi.fn((_req: PreviewDockerCleanupRequest) =>
    create(PreviewDockerCleanupResponseSchema, {
      snapshot: "snap-tab",
      candidates: [targetA],
    }),
  );
  const view2 = renderWithApi(withRouter(<DockerCleanup servers={[{ id: "srv_1", name: "alpha" }]} />), {
    docker: {
      previewDockerCleanup: tabPreview,
      planDockerMaintenance: () => ({ change: singlePlanned }),
    },
    changes: {
      applyChange: () => ({ change: singleApplied }),
      getChange: () => ({ change: singleApplied }),
    },
  });
  expect(await screen.findByText("aaaaaaaaaaaa")).toBeVisible();
  const initialCalls = tabPreview.mock.calls.length;
  fireEvent.click(screen.getByRole("button", { name: "Review plan" }));
  fireEvent.click(await screen.findByRole("button", { name: /^Apply plan/ }));
  expect(await screen.findByRole("heading", { name: "Docker cleanup complete" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Review again" }));
  await waitFor(() => expect(tabPreview.mock.calls.length).toBeGreaterThan(initialCalls));
  expect(tabPreview.mock.calls.at(-1)?.[0]?.serverIds).toEqual(["srv_1"]);
  expect(tabPreview.mock.calls.at(-1)?.[0]?.filters?.kinds).toEqual([]);
  view2.unmount();

  // 3. From Activity: uses only the servers with Changed receipts and preserves the original kind scope.
  const activityPreview = vi.fn((_req: PreviewDockerCleanupRequest) =>
    create(PreviewDockerCleanupResponseSchema, {
      snapshot: "snap-act",
      candidates: [targetB],
    }),
  );
  const fleetChange = makeChange(
    ChangeState.APPLIED,
    ["srv_1", "srv_2"],
    [
      create(DockerReceiptSchema, {
        target: targetA,
        state: DockerReceiptState.REMOVED,
        beforeBytes: 4096n,
        afterBytes: 0n,
      }),
      create(DockerReceiptSchema, {
        target: targetB,
        state: DockerReceiptState.CHANGED,
        reason: DockerReceiptReason.IN_USE,
      }),
    ],
  );
  function ActivityOpener() {
    const sheet = useChangeSheet();
    return (
      <button type="button" onClick={() => sheet.open({ kind: "change", id: "chg_partial", witnessed: true })}>
        Open from Activity
      </button>
    );
  }
  const rootRoute = createRootRoute({
    component: () => (
      <ChangeSheetProvider>
        <Outlet />
      </ChangeSheetProvider>
    ),
  });
  const indexRoute = createRoute({ getParentRoute: () => rootRoute, path: "/", component: ActivityOpener });
  const serverCleanupRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/servers/$serverId/docker/cleanup",
    validateSearch: readServerCleanupSearch,
    component: function ServerCleanupDest() {
      const { serverId } = serverCleanupRoute.useParams();
      const { kinds } = serverCleanupRoute.useSearch();
      return <DockerCleanup servers={[{ id: serverId, name: serverId }]} kinds={kinds} />;
    },
  });
  const fleetDockerRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/servers/docker",
    validateSearch: readFleetDockerSearch,
    component: function FleetDest() {
      const search = readFleetDockerSearch(fleetDockerRoute.useSearch() as Record<string, unknown>);
      const servers = (search.servers ?? []).map((id) => ({ id, name: id }));
      return <DockerCleanup servers={servers} kinds={search.kinds} />;
    },
  });
  const router = track(
    createRouter({
      routeTree: rootRoute.addChildren([indexRoute, serverCleanupRoute, fleetDockerRoute]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    }),
  );
  const view3 = renderWithApi(<RouterProvider router={router} />, {
    docker: { previewDockerCleanup: activityPreview },
    changes: { getChange: () => ({ change: fleetChange }) },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Open from Activity" }));
  expect(await screen.findByRole("heading", { name: "Docker cleanup complete" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Review again" }));
  await waitFor(() => expect(router.state.location.pathname).toBe("/servers/srv_2/docker/cleanup"));
  expect(router.state.location.search).toEqual({ kinds: ["images"] });
  await waitFor(() => expect(activityPreview).toHaveBeenCalled());
  expect(activityPreview.mock.calls[0]?.[0]?.serverIds).toEqual(["srv_2"]);
  expect(activityPreview.mock.calls[0]?.[0]?.filters?.kinds).toEqual([DockerCleanupKind.IMAGE]);
  view3.unmount();

  // 4. From Activity when multiple servers had Changed receipts (srv_2 & srv_3 changed, srv_1 removed): opens fleet cleanup scoped to [srv_2, srv_3].
  const multiActivityPreview = vi.fn((_req: PreviewDockerCleanupRequest) =>
    create(PreviewDockerCleanupResponseSchema, {
      snapshot: "snap-multi",
      candidates: [],
    }),
  );
  const targetC = create(DockerCleanupTargetSchema, {
    ...targetB,
    serverId: "srv_3",
    engineId: `sha256:${"c".repeat(64)}`,
  });
  const multiChange = makeChange(
    ChangeState.APPLIED,
    ["srv_1", "srv_2", "srv_3"],
    [
      create(DockerReceiptSchema, { target: targetA, state: DockerReceiptState.REMOVED }),
      create(DockerReceiptSchema, {
        target: targetB,
        state: DockerReceiptState.CHANGED,
        reason: DockerReceiptReason.IN_USE,
      }),
      create(DockerReceiptSchema, {
        target: targetC,
        state: DockerReceiptState.CHANGED,
        reason: DockerReceiptReason.GONE,
      }),
    ],
  );
  const router2 = track(
    createRouter({
      routeTree: rootRoute.addChildren([indexRoute, serverCleanupRoute, fleetDockerRoute]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    }),
  );
  const view4 = renderWithApi(<RouterProvider router={router2} />, {
    docker: { previewDockerCleanup: multiActivityPreview },
    changes: { getChange: () => ({ change: multiChange }) },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Open from Activity" }));
  expect(await screen.findByRole("heading", { name: "Docker cleanup complete" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Review again" }));
  await waitFor(() => expect(router2.state.location.pathname).toBe("/servers/docker"));
  expect(router2.state.location.search).toEqual({ view: "cleanup", servers: ["srv_2", "srv_3"], kinds: ["images"] });
  await waitFor(() => expect(multiActivityPreview).toHaveBeenCalled());
  expect(multiActivityPreview.mock.calls[0]?.[0]?.serverIds).toEqual(["srv_2", "srv_3"]);
  expect(multiActivityPreview.mock.calls[0]?.[0]?.filters?.kinds).toEqual([DockerCleanupKind.IMAGE]);
  view4.unmount();

  // 5. From Activity with a multi-kind plan (images + volumes): preserves every planned kind.
  const multiKindPreview = vi.fn((_req: PreviewDockerCleanupRequest) =>
    create(PreviewDockerCleanupResponseSchema, {
      snapshot: "snap-multi-kind",
      candidates: [],
    }),
  );
  const volumeTarget = create(DockerCleanupTargetSchema, {
    serverId: "srv_2",
    kind: DockerCleanupKind.VOLUME,
    engineId: "vol:old-cache",
    name: "old-cache",
    reason: DockerCleanupReason.UNUSED,
    estimatedBytes: 2048n,
    evidence: "d".repeat(64),
  });
  const multiKindChange = makeChange(
    ChangeState.APPLIED,
    ["srv_2"],
    [
      create(DockerReceiptSchema, {
        target: targetB,
        state: DockerReceiptState.CHANGED,
        reason: DockerReceiptReason.IN_USE,
      }),
      create(DockerReceiptSchema, {
        target: volumeTarget,
        state: DockerReceiptState.REMOVED,
        beforeBytes: 2048n,
        afterBytes: 0n,
      }),
    ],
  );
  const router3 = track(
    createRouter({
      routeTree: rootRoute.addChildren([indexRoute, serverCleanupRoute, fleetDockerRoute]),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    }),
  );
  renderWithApi(<RouterProvider router={router3} />, {
    docker: { previewDockerCleanup: multiKindPreview },
    changes: { getChange: () => ({ change: multiKindChange }) },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Open from Activity" }));
  expect(await screen.findByRole("heading", { name: "Docker cleanup complete" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Review again" }));
  await waitFor(() => expect(router3.state.location.pathname).toBe("/servers/srv_2/docker/cleanup"));
  expect(router3.state.location.search).toEqual({ kinds: ["images", "volumes"] });
  await waitFor(() => expect(multiKindPreview).toHaveBeenCalled());
  expect(multiKindPreview.mock.calls[0]?.[0]?.serverIds).toEqual(["srv_2"]);
  expect(multiKindPreview.mock.calls[0]?.[0]?.filters?.kinds).toEqual([
    DockerCleanupKind.IMAGE,
    DockerCleanupKind.VOLUME,
  ]);
  expect(multiKindPreview.mock.calls[0]?.[0]?.filters?.taggedImages).toBe(true);
  expect(multiKindPreview.mock.calls[0]?.[0]?.filters?.volumes).toBe(true);
  expect(multiKindPreview.mock.calls[0]?.[0]?.filters?.networks).toBe(false);
});
