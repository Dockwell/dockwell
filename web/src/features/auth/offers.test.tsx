// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import { ResourceSchema, Role, type WatchServersResponse } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { anApp, ENV } from "../../test/apps";
import { authError, authState, mira } from "../../test/auth";
import { type Api, feed, onPhone, renderAt, renderWithApi } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { ImageField } from "../apps/ImageField";

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

const ADMINS = "Only an admin or the owner can do this. Ask one of them.";

/**
 * A control that's off with a reason: still in the tab order, aria-disabled and not disabled, with the
 * reason as its description through aria-describedby, not a title, which neither a keyboard nor a phone shows.
 */
function expectOff(control: HTMLElement, reason: string) {
  expect(control).toHaveAttribute("aria-disabled", "true");
  expect(control).not.toBeDisabled();
  expect(control).not.toHaveAttribute("title");
  const described = control.getAttribute("aria-describedby") ?? "";
  expect(described).not.toBe("");
  expect(document.getElementById(described)).toHaveTextContent(reason);
  expect(control).toHaveAccessibleDescription(reason);
  expect(control.tabIndex).toBeGreaterThanOrEqual(0);
  control.focus();
  expect(control).toHaveFocus();
}
const DEVELOPERS = "Only a developer or more can do this. Ask an admin to change your role.";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("what the panel offers, by what you hold", () => {
  it("leaves an action on an app where it is for a viewer, off, with why", async () => {
    renderAt("/apps/svc_web", api(as(["prj_1", Role.VIEWER])));
    const deploy = await screen.findByRole("button", { name: "Deploy" });
    expectOff(deploy, DEVELOPERS);
    expectOff(screen.getByRole("button", { name: "Restart" }), DEVELOPERS);
    // Tab reaches it, and pressing it does nothing.
    document.body.focus();
    const user = userEvent.setup();
    for (let i = 0; i < 40 && document.activeElement !== deploy; i++) await user.tab();
    expect(deploy).toHaveFocus();
    await user.click(deploy);
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    // What a viewer may do stays on.
    expect(screen.getAllByRole("link", { name: "Logs" })[0]).toBeVisible();
  });

  it("says why once on a phone, where nothing hovers", async () => {
    onPhone();
    const { container } = renderAt("/apps/svc_web", api(as(["prj_1", Role.VIEWER])));
    expectOff(await screen.findByRole("button", { name: "Deploy" }), DEVELOPERS);
    const notes = container.querySelectorAll(".page-off-note");
    expect(notes).toHaveLength(1);
    expect(notes[0]).toHaveTextContent(DEVELOPERS);
    expect(notes[0]).toBeVisible();
  });

  it("says nothing on a phone to someone who may do it all", async () => {
    onPhone();
    const { container } = renderAt("/apps/svc_web", api(as(["", Role.ADMIN])));
    expect(await screen.findByRole("button", { name: "Deploy" })).toBeEnabled();
    expect(container.querySelector(".page-off-note")).toBeNull();
  });

  it("offers a developer their project's actions, in a protected environment too, where the plan then waits", async () => {
    renderAt("/apps/svc_web", api(as(["prj_1", Role.DEVELOPER]), true));
    expect(await screen.findByRole("button", { name: "Deploy" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Restart" })).toBeEnabled();
  });

  it("reads no image for someone who views this environment, whatever they are elsewhere", async () => {
    const asked: string[] = [];
    const tell = (what: string) => () => {
      asked.push(what);
      return {};
    };
    const images = { searchImages: tell("search"), listImageTags: tell("tags"), getImage: tell("image") };
    const field = <ImageField value="nginx:1.27" onChange={() => {}} environment={ENV} server="srv_1" />;
    const user = userEvent.setup();
    renderWithApi(field, { ...api(as(["prj_1", Role.VIEWER], ["prj_other", Role.DEVELOPER])), images });
    await user.click(await screen.findByRole("combobox", { name: "Image" }));
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(asked).toEqual([]);
  });

  it("offers nothing in a project its reader holds nothing in", async () => {
    renderAt("/apps/svc_web", api(as(["prj_other", Role.DEVELOPER])));
    expectOff(
      await screen.findByRole("button", { name: "Deploy" }),
      "You hold no role in this project. Ask an admin to add you.",
    );
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

  it("says a role doesn't cover a server's Updates opened by its address, as a refused read does, without asking", async () => {
    let asked = 0;
    renderAt("/servers/srv_1/updates", {
      ...api(as(["", Role.DEVELOPER])),
      updates: {
        getServerUpdates: () => {
          asked++;
          throw authError(Code.PermissionDenied, "needs_role", "only an admin or the owner can do this");
        },
      },
    });
    const why = (await screen.findByText("Your role doesn't cover this")).closest(".dw-callout");
    expect(why).toHaveTextContent("Your role doesn't let you do this here. Ask an admin to change your role.");
    expect(screen.queryByText(/Asking hz-1/)).not.toBeInTheDocument();
    expect(asked).toBe(0);
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
    expectOff(await screen.findByRole("button", { name: /Save/ }), ADMINS);
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
