// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  type Change,
  ChangeSchema,
  ContainerSchema,
  DeploymentState,
  ServerInventorySchema,
  ServiceSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { anApp } from "../../test/apps";
import { feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

function server(id: string, name: string, architecture: string, control = false) {
  return aServer({
    id,
    name,
    controlServer: control,
    inventory: create(ServerInventorySchema, { osName: "Ubuntu 24.04.1 LTS", architecture }),
  });
}

const on = { deployments: [{ serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE }] };

/** An app on hz-2 that runs on the one architecture given (ADR 0102). */
function app(name: string, architecture: string) {
  const spec = create(ServiceSchema, {
    runs: { case: "app", value: create(ContainerSchema, { image: "nginx:1.27" }) },
    architectures: [architecture],
  });
  return anApp(name, "nginx:1.27", {}, { spec, ...on });
}

/** The fleet given, with web, which runs on amd64 alone, on hz-2, or the apps given. */
function api(fleet: ReturnType<typeof server>[], apps = [app("web", "amd64")]) {
  const servers = feed<WatchServersResponse>();
  servers.push(...fleet.map((s) => changed(s)), snapshotComplete);
  return {
    servers: { watchServers: servers.stream },
    changes: {
      planRemoveServer: () => ({ change: create(ChangeSchema, { id: "chg_remove" }) as Change }),
      watchChange: feed<WatchChangeResponse>().stream,
    },
    services: { listServices: () => ({ services: apps }) },
  };
}

describe("Removing a server whose apps run on one architecture", () => {
  it("offers to move them only to a server they run on, and says why another isn't", async () => {
    const user = userEvent.setup();
    renderAt(
      "/servers/srv_2",
      api([server("srv_1", "hz-1", "arm64", true), server("srv_2", "hz-2", "amd64"), server("srv_3", "hz-3", "amd64")]),
    );
    await user.click(await screen.findByRole("button", { name: /^Actions for / }));
    await user.click(screen.getByRole("menuitem", { name: "Remove from fleet" }));
    const sheet = await screen.findByRole("dialog", { name: "Remove hz-2" });
    await user.click(within(sheet).getByRole("radio", { name: /Move them to another server/ }));
    const to = within(sheet).getByRole("combobox", { name: "Move them to" });
    expect(to).toHaveValue("srv_3");
    expect(within(to).getByRole("option", { name: "hz-1 · arm64" })).toBeDisabled();
    expect(within(to).getByRole("option", { name: "hz-3" })).toBeEnabled();
    expect(sheet).toHaveTextContent("web runs on amd64 alone.");
  });

  it("says the apps can't move where no other server is one they run on", async () => {
    const user = userEvent.setup();
    renderAt("/servers/srv_2", api([server("srv_1", "hz-1", "arm64", true), server("srv_2", "hz-2", "amd64")]));
    await user.click(await screen.findByRole("button", { name: /^Actions for / }));
    await user.click(screen.getByRole("menuitem", { name: "Remove from fleet" }));
    const sheet = await screen.findByRole("dialog", { name: "Remove hz-2" });
    const move = within(sheet).getByRole("radio", { name: /Move them to hz-1/ });
    expect(move).toBeDisabled();
    expect(sheet).toHaveTextContent("web runs on amd64 alone, and no other server is amd64");
  });

  // #771's review, M3: each architecture is in the fleet, but no one server takes both.
  it("says when its apps run on different architectures, not that no server is one", async () => {
    const user = userEvent.setup();
    renderAt(
      "/servers/srv_2",
      api(
        [server("srv_1", "hz-1", "arm64", true), server("srv_2", "hz-2", "amd64"), server("srv_3", "hz-3", "amd64")],
        [app("web", "amd64"), app("api", "arm64")],
      ),
    );
    await user.click(await screen.findByRole("button", { name: /^Actions for / }));
    await user.click(screen.getByRole("menuitem", { name: "Remove from fleet" }));
    const sheet = await screen.findByRole("dialog", { name: "Remove hz-2" });
    expect(within(sheet).getByRole("radio", { name: /Move them to another server/ })).toBeDisabled();
    expect(sheet).toHaveTextContent("web and api run on different architectures, so no one server takes them all");
    expect(sheet).not.toHaveTextContent(/no other server is (arm64|amd64)/);
  });

  it("says a server hasn't reported yet rather than that none is the architecture", async () => {
    const user = userEvent.setup();
    renderAt(
      "/servers/srv_2",
      api([server("srv_1", "hz-1", "arm64", true), server("srv_2", "hz-2", "amd64"), server("srv_3", "hz-3", "")]),
    );
    await user.click(await screen.findByRole("button", { name: /^Actions for / }));
    await user.click(screen.getByRole("menuitem", { name: "Remove from fleet" }));
    const sheet = await screen.findByRole("dialog", { name: "Remove hz-2" });
    expect(within(sheet).getByRole("radio", { name: /Move them to another server/ })).toBeDisabled();
    expect(sheet).toHaveTextContent("web runs on amd64 alone, and no other server has said it's amd64 yet");
  });
});
