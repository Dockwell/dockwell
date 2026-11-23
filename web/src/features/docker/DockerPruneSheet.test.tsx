// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  DockerCleanupKind,
  DockerCleanupReason,
  DockerCleanupServerLimitsSchema,
  type DockerCleanupTarget,
  DockerCleanupTargetSchema,
  DockerReadState,
  DockerUnavailableSchema,
  type PreviewDockerCleanupRequest,
  PreviewDockerCleanupResponseSchema,
} from "@dockwell/api";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { renderWithApi } from "../../test/render";
import { track } from "../../test/routers";
import { ChangeSheetProvider } from "../changes/ChangeSheet";
import { DockerCleanup } from "./DockerCleanup";
import { DockerPruneSheet } from "./DockerPruneSheet";

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
