// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ResourceSchema, Role, type WatchServersResponse } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { anApp, ENV } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const web = anApp("web", "nginx:1.4");
const hz1 = aServer({ id: "srv_1", name: "hz-1", controlServer: true });

/** Someone signed in who holds what grants say: a role on the install (""), or on the default project. */
function as(...grants: [string, Role][]): Api["auth"] {
  const onInstall = grants.find(([project]) => project === "")?.[1] ?? Role.UNSPECIFIED;
  return {
    getAuthState: () =>
      authState({
        user: { ...mira, role: onInstall },
        grants: grants.map(([projectId, role]) => ({ projectId, role })),
      }),
  };
}

/** One server, one app in the default project's production, protected or open. */
function api(auth: Api["auth"], isProtected = false): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const resource = (id: string, kind: string, scope: string, name: string, more = {}) =>
    create(ResourceSchema, { id, resource: { kind, scope, name }, ...more });
  return {
    auth,
    resources: {
      listResources: (req: { kind: string }) => ({
        resources:
          req.kind === "project"
            ? [resource("prj_1", "project", "org_1", "default")]
            : req.kind === "environment"
              ? [resource(ENV, "environment", "prj_1", "production", { protected: isProtected })]
              : [],
      }),
    },
    services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: [] }) },
    servers: { watchServers: watch.stream },
  };
}

const ADMINS = "Only an admin or the owner can do this.";
const DEVELOPERS = "Only a developer or more can do this. Ask an admin to change your role.";

describe("what the panel offers, by what you hold", () => {
  it("leaves an action on an app where it is for a viewer, off, with why", async () => {
    renderAt("/apps/svc_web", api(as(["prj_1", Role.VIEWER])));
    const deploy = await screen.findByRole("button", { name: "Deploy" });
    expect(deploy).toBeDisabled();
    expect(deploy).toHaveAccessibleDescription(DEVELOPERS);
    expect(screen.getByRole("button", { name: "Restart" })).toBeDisabled();
    // What a viewer may do stays on.
    expect(screen.getAllByRole("link", { name: "Logs" })[0]).toBeVisible();
  });

  it("offers a developer their project's actions, in a protected environment too, where the plan then waits", async () => {
    renderAt("/apps/svc_web", api(as(["prj_1", Role.DEVELOPER]), true));
    expect(await screen.findByRole("button", { name: "Deploy" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Restart" })).toBeEnabled();
  });

  it("offers nothing in a project its reader holds nothing in", async () => {
    renderAt("/apps/svc_web", api(as(["prj_other", Role.DEVELOPER])));
    const deploy = await screen.findByRole("button", { name: "Deploy" });
    expect(deploy).toBeDisabled();
    expect(deploy).toHaveAccessibleDescription("You hold no role in this project. Ask an admin to add you.");
  });

  it("leaves out a server's tabs that only an admin reads, and asks nothing for them", async () => {
    const asked: string[] = [];
    const tell = (what: string) => () => {
      asked.push(what);
      return {};
    };
    renderAt("/servers/srv_1", {
      ...api(as(["", Role.DEVELOPER])),
      updates: { getServerUpdates: tell("updates") },
      containers: { listContainers: tell("containers") },
    });
    const tabs = await screen.findByRole("navigation", { name: "Server hz-1" });
    expect(within(tabs).getByRole("link", { name: "Overview" })).toBeVisible();
    for (const name of ["Files", "Terminal", "Updates", "Firewall", "Processes"]) {
      expect(within(tabs).queryByRole("link", { name })).not.toBeInTheDocument();
    }
    expect(screen.queryByRole("region", { name: "Containers" })).not.toBeInTheDocument();
    expect(asked).toEqual([]);
  });

  it("shows an admin all of a server's tabs", async () => {
    renderAt("/servers/srv_1", api(as(["", Role.ADMIN])));
    const tabs = await screen.findByRole("navigation", { name: "Server hz-1" });
    for (const name of ["Overview", "Files", "Terminal", "Updates", "Firewall", "Processes"]) {
      expect(within(tabs).getByRole("link", { name })).toBeVisible();
    }
  });

  it("leaves the install's own pages out of Settings and the rail below an admin", async () => {
    renderAt("/settings/general", api(as(["", Role.DEVELOPER])));
    const settings = await screen.findByRole("navigation", { name: "Settings" });
    expect(within(settings).getByRole("link", { name: "General" })).toBeVisible();
    expect(within(settings).getByRole("link", { name: "API tokens" })).toBeVisible();
    for (const name of ["Members", "Audit log", "Control plane"]) {
      expect(within(settings).queryByRole("link", { name })).not.toBeInTheDocument();
    }
    const rail = screen.getByRole("navigation", { name: "Main" });
    expect(within(rail).queryByRole("link", { name: "Alerts" })).not.toBeInTheDocument();
    expect(within(rail).getByRole("link", { name: "Apps" })).toBeVisible();
    // The install's name and time zone are there to read, and an admin's to save.
    const save = await screen.findByRole("button", { name: /Save/ });
    expect(save).toBeDisabled();
    expect(save).toHaveAccessibleDescription(ADMINS);
  });

  it("shows an admin Settings and the rail whole", async () => {
    renderAt("/settings/general", api(as(["", Role.ADMIN])));
    const settings = await screen.findByRole("navigation", { name: "Settings" });
    for (const name of ["General", "Members", "API tokens", "Audit log", "Control plane"]) {
      expect(within(settings).getByRole("link", { name })).toBeVisible();
    }
    expect(
      within(screen.getByRole("navigation", { name: "Main" })).getByRole("link", { name: "Alerts" }),
    ).toBeVisible();
  });
});
