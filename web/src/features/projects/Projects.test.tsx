// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  type ProposeChangeRequest,
  type Resource,
  ResourceSchema,
  Role,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { shopPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

function resource(id: string, kind: string, scope: string, name: string, displayName = ""): Resource {
  return create(ResourceSchema, { id, resource: { kind, scope, name }, displayName });
}

const projects = [
  resource("prj_default", "project", "org_1", "default"),
  resource("prj_blog", "project", "org_1", "blog", "Blog"),
  resource("prj_old", "project", "org_1", "old-shop"),
];
const environments = [
  resource("env_prod", "environment", "prj_default", "production"),
  resource("env_blog", "environment", "prj_blog", "production"),
  resource("env_old", "environment", "prj_old", "production"),
];
const inside = [
  resource("svc_web", "service", "env_prod", "web"),
  resource("svc_ghost", "service", "env_blog", "ghost"),
  resource("vol_content", "volume", "env_blog", "content"),
  resource("var_title", "variable", "env_blog", "SITE_TITLE"),
  resource("sec_mail", "secret", "env_blog", "MAIL_PASSWORD"),
  resource("sec_stripe", "secret", "env_old", "STRIPE_KEY"),
  resource("var_mode", "variable", "env_old", "SHOP_MODE"),
];
const web = anApp(
  "web",
  "nginx:1.27",
  {},
  { id: "svc_web", resource: { kind: "service", scope: "env_prod", name: "web" } },
);
const ghost = anApp(
  "ghost",
  "ghost:5.101",
  {},
  { id: "svc_ghost", resource: { kind: "service", scope: "env_blog", name: "ghost" } },
);

function api(more: Api = {}, proposals: ProposeChangeRequest[] = []): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: "srv_1", name: "web-1", controlServer: true })), snapshotComplete);
  return {
    servers: { watchServers: watch.stream },
    resources: {
      listResources: (req) => ({
        resources:
          req.kind === "project"
            ? projects
            : req.kind === "environment"
              ? environments
              : req.scope
                ? inside.filter((r) => r.resource?.scope === req.scope)
                : req.kind === ""
                  ? [...projects, ...environments, ...inside]
                  : [],
      }),
    },
    services: { listServices: () => ({ services: [web, ghost] }) },
    changes: {
      proposeChange: (req) => {
        proposals.push(req);
        return { change: shopPlanned() };
      },
      watchChange: feed<WatchChangeResponse>().stream,
      discardChange: () => ({}),
    },
    ...more,
  };
}

const edits = (req: ProposeChangeRequest | undefined) =>
  req?.edits.map((e) => [e.resource?.kind, e.resource?.name, e.delete, e.withContents]) ?? [];

describe("the projects", () => {
  it("list what's in each, and mark the one with nothing running, with Delete beside it", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/projects", api({}, proposals));
    const table = await screen.findByRole("table", { name: "Projects" });
    const rows = await within(table).findAllByRole("row");
    expect(rows.slice(1).map((r) => r.textContent)).toEqual([
      expect.stringContaining("default"),
      expect.stringContaining("Blog"),
      expect.stringContaining("old-shop"),
    ]);
    expect(rows[1]).toHaveTextContent("Apps with no project");
    expect(rows[2]).toHaveTextContent("1 app · 1 variable · 1 secret · 1 volume");
    expect(rows[3]).toHaveTextContent("Nothing running");
    expect(rows[3]).toHaveTextContent("1 variable · 1 secret");
    expect(screen.getByText("3 projects · 1 with nothing running")).toBeVisible();
    // Only a leftover has Delete in the list; the rest are a click from theirs.
    expect(within(rows[1] as HTMLElement).queryByRole("button")).toBeNull();
    expect(within(rows[2] as HTMLElement).queryByRole("button")).toBeNull();
    await expectAccessible(document.body);

    await user.click(within(rows[3] as HTMLElement).getByRole("button", { name: "Delete the project old-shop" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Delete the project old-shop");
    // One edit, which the engine fills with what's in the project as it plans (ADR 0074).
    expect(edits(proposals[0])).toEqual([["project", "old-shop", true, true]]);
  });

  it("show only those with nothing running, when asked", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/projects", api());
    await user.click(await screen.findByRole("radio", { name: "Nothing running 1" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ show: "idle" }));
    const rows = within(screen.getByRole("table", { name: "Projects" })).getAllByRole("row");
    expect(rows).toHaveLength(2);
    expect(rows[1]).toHaveTextContent("old-shop");
  });

  it("are in the rail from the second, and not with one", async () => {
    renderAt("/projects", api());
    const rail = await screen.findByRole("navigation", { name: "Main" });
    expect(await within(rail).findByRole("link", { name: /Projects/ })).toHaveAttribute("href", "/projects");
  });

  it("aren't in the rail while the default is the only one", async () => {
    renderAt(
      "/",
      api({
        resources: {
          listResources: (req) => ({
            resources:
              req.kind === "project"
                ? [projects[0] as Resource]
                : req.kind === "environment"
                  ? [environments[0] as Resource]
                  : [],
          }),
        },
      }),
    );
    const rail = await screen.findByRole("navigation", { name: "Main" });
    await within(rail).findByRole("link", { name: /Apps/ });
    expect(within(rail).queryByRole("link", { name: /Projects/ })).toBeNull();
  });

  it("list on a phone, each with Delete beside a leftover", async () => {
    onPhone();
    renderAt("/projects", api());
    const old = await screen.findByRole("link", { name: /old-shop/ });
    expect(old).toHaveTextContent("Nothing running");
    expect(screen.getByRole("button", { name: "Delete the project old-shop" })).toBeVisible();
  });
});

describe("a project's page", () => {
  it("shows what's in it, its values a click from Variables, and renames it as a plan", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/projects/prj_blog", api({}, proposals));
    expect(await screen.findByRole("heading", { level: 1, name: "Blog" })).toBeVisible();
    const contents = screen.getByRole("region", { name: "What's in it" });
    expect(within(contents).getByRole("link", { name: /ghost/ })).toHaveAttribute("href", "/apps/svc_ghost");
    expect(contents).toHaveTextContent("content");
    const values = screen.getByRole("region", { name: "Variables and secrets" });
    expect(within(values).getByRole("link", { name: /1 variable · 1 secret/ })).toHaveAttribute(
      "href",
      "/variables?environment=env_blog",
    );
    // One environment is production, and says nothing.
    expect(screen.queryByRole("region", { name: "Environments" })).toBeNull();
    await expectAccessible(document.body);

    const field = screen.getByRole("textbox", { name: "Display name" });
    await user.clear(field);
    await user.type(field, "Company blog");
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Show blog as Company blog");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "project", name: "blog" });
    expect(proposals[0]?.edits[0]?.spec?.kind).toMatchObject({
      case: "project",
      value: { displayName: "Company blog" },
    });
  });

  it("deletes the project with everything in it", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/projects/prj_blog", api({}, proposals));
    const danger = await screen.findByRole("region", { name: "Danger zone" });
    expect(danger).toHaveTextContent("Takes 1 app, 1 variable, 1 secret and 1 volume in one plan");
    await user.click(within(danger).getByRole("button", { name: "Delete the project" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits(proposals[0])).toEqual([["project", "blog", true, true]]);
  });

  it("keeps the default project, where apps with no project go", async () => {
    renderAt("/projects/prj_default", api());
    const danger = await screen.findByRole("region", { name: "Danger zone" });
    expect(within(danger).getByRole("button", { name: "Delete the project" })).toHaveAccessibleDescription(
      "Apps with no project go here, so it stays.",
    );
  });

  it("leaves renaming and deleting to an admin, with why", async () => {
    renderAt(
      "/projects/prj_blog",
      api({
        auth: {
          getAuthState: () =>
            authState({
              user: { ...mira, role: Role.UNSPECIFIED },
              grants: [{ projectId: "prj_blog", role: Role.DEVELOPER }],
            }),
        },
      }),
    );
    const danger = await screen.findByRole("region", { name: "Danger zone" });
    expect(within(danger).getByRole("button", { name: "Delete the project" })).toHaveAccessibleDescription(
      "Only an admin or the owner can do this. Ask one of them.",
    );
  });

  it("says when there's no such project", async () => {
    renderAt("/projects/prj_gone", api());
    expect(await screen.findByText("No project with this ID")).toBeVisible();
    expect(screen.getByRole("link", { name: "Go to the projects" })).toHaveAttribute("href", "/projects");
  });
});
