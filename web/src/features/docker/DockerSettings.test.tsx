// SPDX-License-Identifier: AGPL-3.0-only

import {
  ChangeState,
  DockerDaemonAction,
  FieldOp,
  HostAccessKind,
  PlanAction,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { feed, quietWatch, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

it("reviews the exact local file separately and shows its diff before asking for a passkey", async () => {
  const server = aServer({ id: "srv_1", name: "example-server" });
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  const resource = { kind: "docker.daemon", scope: server.id, name: "daemon" };
  const impact = {
    action: DockerDaemonAction.RESTART,
    liveRestore: true,
    affectedWorkloads: ["example-api"],
  };
  const fileChanges = [
    {
      path: "registry-mirrors",
      op: FieldOp.MODIFIED,
      from: '["https://mirror.example"]',
      to: '["https://other.example"]',
    },
  ];
  const plan = vi.fn();
  renderAt("/servers/srv_1/docker/settings", {
    servers: { watchServers: servers.stream },
    docker: {
      getDockerSettings: () => ({ settings: { engineVersion: "28.5.2", pendingFileReview: true } }),
    },
    changes: {
      watchChange: quietWatch,
      proposeChange: (ask) => {
        plan(ask.edits[0]?.spec?.kind);
        return {
          change: {
            id: "chg_review",
            summary: "Activate the current Docker settings file",
            state: ChangeState.PLANNED,
            plan: {
              fingerprint: "12345678",
              summary: { change: 1 },
              hostAccess: true,
              resources: [
                {
                  id: "res_daemon",
                  resource,
                  action: PlanAction.UPDATE,
                  hostAccess: [{ kind: HostAccessKind.DOCKER_DAEMON_SETTINGS, sameAsRoot: true }],
                  dockerDaemon: { impact, fileChanges },
                },
              ],
              servers: [
                {
                  serverId: server.id,
                  serverName: server.name,
                  steps: [
                    {
                      resourceId: "res_daemon",
                      resource,
                      action: PlanAction.UPDATE,
                      dockerDaemon: { impact, fileChanges },
                    },
                  ],
                },
              ],
            },
          },
        };
      },
    },
  });
  const review = await screen.findByRole("button", { name: "Review the current file" });
  expect(screen.getByRole("link", { name: "Docker service and logs" })).toHaveClass("dw-btn");
  await userEvent.click(review);
  expect(await screen.findByText("Changes in daemon.json")).toBeVisible();
  expect(screen.getByText('["https://mirror.example"]')).toBeVisible();
  expect(screen.getByText('["https://other.example"]')).toBeVisible();
  expect(screen.getByText(/Containers affected by a restart.*example-api/)).toBeVisible();
  expect(screen.getByText("Applying this asks for your passkey")).toBeVisible();
  expect(plan).toHaveBeenCalledWith({
    case: "dockerDaemon",
    value: expect.objectContaining({ reviewCurrentFile: true }),
  });
});
