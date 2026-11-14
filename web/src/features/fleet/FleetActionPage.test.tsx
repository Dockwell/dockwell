// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  BuildInfoSchema,
  ChangeSchema,
  ChangeState,
  Connection,
  PlanAction,
  type PlanAgentUpgradeRequest,
  type PlanSystemServiceActionRequest,
  type Server,
  StepEffect,
  Surface,
  SystemServiceAction,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { places } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { agentRelease, waveSizes } from "./rollout";

const build = (version: string) => create(BuildInfoSchema, { version });
const control = aServer({ id: "srv_c", name: "control", controlServer: true, agentBuild: build("v0.9.4") });
const alpha = aServer({ id: "srv_a", name: "alpha", labels: { pool: "web" }, agentBuild: build("v0.9.3") });
const bravo = aServer({ id: "srv_b", name: "bravo", labels: { pool: "web" }, agentBuild: build("v0.9.3") });
const charlie = aServer({ id: "srv_d", name: "charlie", labels: { pool: "web" }, agentBuild: build("v0.9.4") });
const delta = aServer({ id: "srv_e", name: "delta", labels: { pool: "db" }, agentBuild: build("v0.9.3") });

function renderFleetAction(servers: Server[], url: string, extra: Partial<Api> = {}) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  return renderAt(url, {
    servers: { watchServers: watch.stream },
    resources: places(),
    events: { listEvents: () => ({ events: [] }) },
    ...extra,
  });
}

/** A planned change over alpha and bravo, in two waves, as the control plane returns it. */
function planned(summary: string) {
  const host = (scope: string) => ({ kind: "server.host", scope, name: "host" });
  const step = (scope: string) => ({
    resourceId: `res_${scope}`,
    resource: host(scope),
    action: PlanAction.UPDATE,
    effect: StepEffect.RESTART,
  });
  return create(ChangeSchema, {
    id: "chg_fleet",
    summary,
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    plan: {
      fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
      resources: ["srv_a", "srv_b"].map((s) => ({
        id: `res_${s}`,
        resource: host(s),
        action: PlanAction.UPDATE,
        servers: [s],
      })),
      servers: [
        { serverId: "srv_a", serverName: "alpha", steps: [step("srv_a")] },
        { serverId: "srv_b", serverName: "bravo", steps: [step("srv_b")] },
      ],
      rollout: {
        waves: [
          { serverIds: ["srv_a"], serverNames: ["alpha"] },
          { serverIds: ["srv_b"], serverNames: ["bravo"] },
        ],
        pause: { seconds: 300n },
      },
      notes: [
        {
          reason: "servers_unchanged",
          names: ["charlie"],
          named: true,
          message: "charlie needs nothing, so it takes no step",
        },
      ],
    },
  });
}

describe("rollout", () => {
  it("splits servers into waves as the control plane does", () => {
    expect(waveSizes(7, true, 0)).toEqual([1, 2, 2, 2]);
    expect(waveSizes(7, false, 0)).toEqual([2, 2, 2, 1]);
    expect(waveSizes(7, true, 3)).toEqual([1, 3, 3]);
    expect(waveSizes(1, true, 0)).toEqual([]);
  });

  it("upgrades agents to the control plane's version only when that's a release", () => {
    expect(agentRelease([control, alpha])).toBe("0.9.4");
    const dev = aServer({ ...control, agentBuild: build("v0.9.5-0.20261114083000-abcdef123456") });
    expect(agentRelease([dev, alpha])).toBeUndefined();
    expect(agentRelease([alpha])).toBeUndefined();
  });
});

describe("New fleet action", () => {
  it("targets servers by a selector, draws the waves, and plans the action with the rollout", async () => {
    const user = userEvent.setup();
    const asked: PlanSystemServiceActionRequest[] = [];
    const change = planned("Restart nginx.service on alpha, bravo and charlie");
    const { container } = renderFleetAction(
      [control, alpha, bravo, charlie, delta],
      "/fleet-actions?selector=pool%3Aweb",
      {
        changes: {
          planSystemServiceAction: (req: PlanSystemServiceActionRequest) => {
            asked.push(req);
            return { change };
          },
          getChange: () => ({ change }),
          watchChange: feed<WatchChangeResponse>().stream,
        },
      },
    );

    const target = await screen.findByRole("region", { name: "Target" });
    expect(within(target).getByText("3 servers match")).toBeVisible();
    const tiles = within(target).getByRole("region", { name: "Servers targeted" });
    expect(within(tiles).getAllByRole("button")).toHaveLength(3);

    await user.click(screen.getByRole("radio", { name: /Act on a system service/ }));
    const review = screen.getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.type(screen.getByRole("textbox", { name: "System service" }), "nginx.service");
    // A canary, then a quarter of three rounded up: one each.
    const waves = screen.getByRole("list", { name: "Waves" });
    expect(
      within(waves)
        .getAllByRole("listitem")
        .map((w) => w.textContent),
    ).toEqual(["Wave 1alpha", "Wave 21 server", "Wave 31 server"]);
    await user.type(screen.getByRole("textbox", { name: "Servers in each wave" }), "2");
    await user.type(screen.getByRole("textbox", { name: "Minutes between waves" }), "5");
    await user.click(screen.getByRole("switch", { name: /A step fails/ }));
    const summary = screen.getByRole("complementary", { name: "Summary" });
    expect(summary).toHaveTextContent("3 servers in 2 waves");
    expect(summary).toHaveTextContent("Restart nginx.service");
    expect(summary).toHaveTextContent("1 → 2 servers");
    expect(summary).toHaveTextContent("5 min between waves");
    expect(summary).toHaveTextContent("Never; fails at the end if any step did");
    await expectAccessible(container);

    await user.click(review);
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({
      selector: "pool:web",
      serverIds: [],
      name: "nginx.service",
      action: SystemServiceAction.RESTART,
      rollout: { noCanary: false, batchSize: 2, keepGoing: true, pause: { seconds: 300n } },
    });
    // The plan sheet has its steps wave by wave, with what the rollout does between them.
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(within(sheet).getByText("Steps by wave")).toBeVisible();
    expect(sheet).toHaveTextContent("2 waves, 5 min apart");
    expect(sheet).toHaveTextContent("Wave 1alpha");
    expect(sheet).toHaveTextContent("Wave 2bravo");
    expect(sheet).toHaveTextContent("charlie needs nothing, so it takes no step");
  });

  it("matches nothing while the selector has a term it can't read", async () => {
    renderFleetAction([control, alpha, bravo], "/fleet-actions?selector=pool%3Aweb+bad%21term");
    expect(await screen.findByText("No server matches while the selector has a term it can't read")).toBeVisible();
    expect(screen.getByRole("button", { name: "Review plan" })).toBeDisabled();
  });

  it("takes the servers picked on Servers, and says what an upgrade leaves out", async () => {
    const user = userEvent.setup();
    const asked: PlanAgentUpgradeRequest[] = [];
    const away = aServer({ ...delta, connection: Connection.UNREACHABLE });
    const change = planned("Upgrade the agents on alpha and bravo to 0.9.4");
    renderFleetAction([control, alpha, bravo, charlie, away], "/fleet-actions?servers=srv_a,srv_b,srv_c,srv_d,srv_e", {
      changes: {
        planAgentUpgrade: (req: PlanAgentUpgradeRequest) => {
          asked.push(req);
          return { change };
        },
        getChange: () => ({ change }),
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    expect(await screen.findByText("5 servers picked")).toBeVisible();
    await user.click(screen.getByRole("radio", { name: /Upgrade the agent/ }));
    const target = screen.getByRole("region", { name: "Target" });
    expect(target).toHaveTextContent("delta isn't connected, so it isn't in this change");
    expect(target).toHaveTextContent("control is the control server, which upgrades by the install script on it");
    expect(target).toHaveTextContent("charlie runs 0.9.4 already");
    expect(screen.getByRole("complementary", { name: "Summary" })).toHaveTextContent("2 servers in 2 waves");
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({ serverIds: ["srv_a", "srv_b", "srv_c", "srv_d", "srv_e"], selector: "" });

    // A selector instead.
    await user.click(screen.getByRole("button", { name: "Pick by labels instead" }));
    expect(await screen.findByRole("combobox", { name: "Add a label to the selector" })).toBeVisible();
  });

  it("offers no agent upgrade from a development build", async () => {
    const dev = aServer({ ...control, agentBuild: build("v0.9.5-0.20261114083000-abcdef123456") });
    renderFleetAction([dev, alpha, bravo], "/fleet-actions");
    const upgrade = await screen.findByRole("radio", { name: /Upgrade the agent/ });
    expect(upgrade).toBeDisabled();
    expect(
      screen.getByText("The control plane is a development build, so it has no release to upgrade agents to"),
    ).toBeVisible();
  });
});
