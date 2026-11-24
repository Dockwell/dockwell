// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  PlanNoteSchema,
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
import { anApp, aWebsite, SITE_ENV } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { shopPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { projectNameOf } from "./ProjectSheets";

it("makes a URL name for a display name that starts with digits or uses non-Latin letters", () => {
  expect(projectNameOf("24 hour café")).toBe("project-24-hour-cafe");
  expect(projectNameOf("商店")).toBe("project-5546-5e97");
  expect(projectNameOf("!?")).toBe("");
});

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
    // Only a leftover has Delete beside it; every row has its menu, with the rest in it.
    expect(within(rows[1] as HTMLElement).queryByRole("button", { name: /Delete the project/ })).toBeNull();
    expect(within(rows[2] as HTMLElement).queryByRole("button", { name: /Delete the project/ })).toBeNull();
    expect(within(rows[1] as HTMLElement).getByRole("button", { name: "Actions for default" })).toBeVisible();
    await expectAccessible(document.body);

    await user.click(within(rows[3] as HTMLElement).getByRole("button", { name: "Delete the project old-shop" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Delete the project old-shop");
    // One edit, which the engine fills with what's in the project as it plans (ADR 0074).
    expect(edits(proposals[0])).toEqual([["project", "old-shop", true, true]]);
  });

  it("make an empty one from the list, by name only, as a plan", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/projects", api({}, proposals));
    await user.click(await screen.findByRole("button", { name: /New project/ }));
    const sheet = await screen.findByRole("dialog", { name: "New project" });
    const field = within(sheet).getByRole("textbox", { name: "Name" });
    // A taken name is said as you type, and Review plan stays off.
    await user.type(field, "Blog");
    expect(await within(sheet).findByText("There's a project named blog already")).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    await user.clear(field);
    await user.type(field, "Shop front");
    // What was typed is its display name; its name in URLs is written for you.
    expect(await within(sheet).findByText("Named shop-front in URLs and the CLI")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Create the project shop-front");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "project", name: "shop-front" });
    expect(proposals[0]?.edits[0]?.spec?.kind).toMatchObject({
      case: "project",
      value: { displayName: "Shop front" },
    });
  });

  it("lets you edit a derived URL name and validates it beside the field", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/projects", api({}, proposals));
    await user.click(await screen.findByRole("button", { name: /New project/ }));
    const sheet = await screen.findByRole("dialog", { name: "New project" });
    await user.type(within(sheet).getByRole("textbox", { name: "Name" }), "商店");
    await user.click(within(sheet).getByRole("button", { name: "Change name in URLs" }));
    const name = within(sheet).getByRole("textbox", { name: "Name in URLs" });
    expect(name).toHaveValue("project-5546-5e97");
    await user.clear(name);
    await user.type(name, "blog");
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    expect(within(sheet).getByText("There's a project named blog already")).toBeVisible();
    await user.clear(name);
    await user.type(name, "shop");
    await user.click(review);
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.edits[0]?.resource?.name).toBe("shop");
    expect(proposals[0]?.edits[0]?.spec?.kind).toMatchObject({ case: "project", value: { displayName: "商店" } });
  });

  it("rename and delete one from its row's menu, each as its plan", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/projects", api({}, proposals));
    const table = await screen.findByRole("table", { name: "Projects" });
    const blog = within(table).getByRole("row", { name: /Blog/ });
    // Right-click asks for the same menu its button has.
    await user.pointer({ keys: "[MouseRight]", target: blog });
    const menu = await screen.findByRole("menu", { name: "Actions for Blog" });
    await user.click(within(menu).getByRole("menuitem", { name: "Rename" }));
    const sheet = await screen.findByRole("dialog", { name: "Rename Blog" });
    const field = within(sheet).getByRole("textbox", { name: "Display name" });
    await user.clear(field);
    await user.type(field, "Company blog");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Show blog as Company blog");
    expect(proposals[0]?.edits[0]?.spec?.kind).toMatchObject({
      case: "project",
      value: { displayName: "Company blog" },
    });
    await user.keyboard("{Escape}");
    // A project with something running deletes from the list too, with what it holds in its plan.
    await user.click(within(blog).getByRole("button", { name: "Actions for Blog" }));
    await user.click(await screen.findByRole("menuitem", { name: "Delete" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits(proposals[1])).toEqual([["project", "blog", true, true]]);
  });

  it("keep the default project's Delete off in its menu, with why", async () => {
    const user = userEvent.setup();
    renderAt("/projects", api());
    const table = await screen.findByRole("table", { name: "Projects" });
    const first = within(table).getByRole("row", { name: /default/ });
    await user.click(within(first).getByRole("button", { name: "Actions for default" }));
    const item = await screen.findByRole("menuitem", { name: /Delete/ });
    expect(item).toHaveAttribute("aria-disabled", "true");
    expect(item).toHaveTextContent("Apps with no project go here, so it stays.");
  });

  it("filter by name from the address, and sort by the head", async () => {
    const user = userEvent.setup();
    // From six projects the list has its filter box.
    const many = {
      projects: [
        ...projects,
        resource("prj_shop", "project", "org_1", "shop"),
        resource("prj_docs", "project", "org_1", "docs"),
        resource("prj_api", "project", "org_1", "api"),
      ],
      environments: [
        ...environments,
        resource("env_shop", "environment", "prj_shop", "production"),
        resource("env_docs", "environment", "prj_docs", "production"),
        resource("env_api", "environment", "prj_api", "production"),
      ],
    };
    const { router } = renderAt(
      "/projects?q=sh",
      api({
        resources: {
          listResources: (req) => ({
            resources:
              req.kind === "project"
                ? many.projects
                : req.kind === "environment"
                  ? many.environments
                  : req.scope
                    ? inside.filter((r) => r.resource?.scope === req.scope)
                    : req.kind === ""
                      ? [...many.projects, ...many.environments, ...inside]
                      : [],
          }),
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Projects" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(3));
    expect(within(table).getByRole("row", { name: /old-shop/ })).toBeVisible();
    expect(within(table).getAllByRole("row", { name: /shop/ })).toHaveLength(2);
    await user.clear(screen.getByRole("searchbox", { name: "Filter projects" }));
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(7));
    await user.click(within(table).getByRole("button", { name: /Project/ }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ sort: "name" }));
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("api");
    expect(rows[6]).toHaveTextContent("shop");
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
    const values = screen.getByRole("region", { name: "Variables" });
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

  it("keeps what other projects' apps use by default, and deletes it too only with the project's name typed", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const keeps = () => {
      const change = shopPlanned();
      change.plan?.notes.push(
        create(PlanNoteSchema, {
          reason: "keeps_shared",
          message: "db moves to a project of its own, db, with its data",
        }),
      );
      return change;
    };
    renderAt(
      "/projects/prj_blog",
      api(
        {
          changes: {
            proposeChange: (req: ProposeChangeRequest) => {
              proposals.push(req);
              return { change: keeps() };
            },
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
          },
        },
        proposals,
      ),
    );
    const danger = await screen.findByRole("region", { name: "Danger zone" });
    await user.click(within(danger).getByRole("button", { name: "Delete the project" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.edits[0]?.deleteShared).toBe(false);
    expect(proposals[0]?.edits[0]?.keepSharedIn).toBe("");
    // Where it goes is a pick among the other projects' environments, which plans again.
    const where = await within(sheet).findByRole("combobox", { name: "Where it goes" });
    expect(
      within(where)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Projects of their own", "default", "old-shop"]);
    await user.selectOptions(where, "old-shop");
    await waitFor(() => expect(proposals).toHaveLength(2));
    expect(proposals[1]?.edits[0]?.keepSharedIn).toBe("env_old");
    expect(await within(sheet).findByText("It moves to old-shop")).toBeInTheDocument();
    const anyway = await within(sheet).findByRole("switch", { name: /Also delete what other apps use/ });
    expect(anyway).not.toBeChecked();
    await user.click(anyway);
    await waitFor(() => expect(proposals).toHaveLength(3));
    expect(proposals[2]?.edits[0]?.deleteShared).toBe(true);
    expect(proposals[2]?.edits[0]?.keepSharedIn).toBe("");
    expect(within(sheet).queryByRole("combobox", { name: "Where it goes" })).toBeNull();
    expect(await within(sheet).findByText(/to apply it/)).toHaveTextContent("Type blog to apply it");
  });

  it("says what a website and a database with its admin tool hold in the same words in the list and the delete", async () => {
    const site = aWebsite();
    const pg = anApp(
      "postgres",
      "postgres:17",
      {},
      { id: "svc_pg", resource: { kind: "service", scope: "env_pg", name: "postgres" } },
    );
    const adminer = anApp(
      "adminer",
      "adminer:5",
      {},
      { id: "svc_adminer", resource: { kind: "service", scope: "env_pg", name: "adminer" } },
    );
    const more = {
      projects: [
        resource("prj_site", "project", "org_1", site.site.name),
        resource("prj_pg", "project", "org_1", "pg"),
      ],
      environments: [
        resource(SITE_ENV, "environment", "prj_site", "production"),
        resource("env_pg", "environment", "prj_pg", "production"),
      ],
      inside: [
        resource(site.web.id, "service", SITE_ENV, "web"),
        resource("rt_site", "route", SITE_ENV, "example-com"),
        // A website brings its network, which nobody makes, so neither the list nor the delete names it.
        resource("net_site", "network", SITE_ENV, "site"),
        resource(pg.id, "service", "env_pg", "postgres"),
        resource(adminer.id, "service", "env_pg", "adminer"),
        resource("vol_pg", "volume", "env_pg", "pg-data"),
      ],
    };
    const all = {
      projects: [...projects, ...more.projects],
      environments: [...environments, ...more.environments],
      inside: [...inside, ...more.inside],
    };
    const endpoints = api({
      resources: {
        listResources: (req) => ({
          resources:
            req.kind === "project"
              ? all.projects
              : req.kind === "environment"
                ? all.environments
                : req.scope
                  ? all.inside.filter((r) => r.resource?.scope === req.scope)
                  : req.kind === ""
                    ? [...all.projects, ...all.environments, ...all.inside]
                    : [],
        }),
      },
      services: { listServices: () => ({ services: [web, ghost, site.web, pg, adminer] }) },
      websites: site.websites,
      databases: { listDatabases: () => ({ databases: [{ id: pg.id, name: "postgres", environmentId: "env_pg" }] }) },
      domains: {
        listDomains: () => ({
          domains: [{ name: "example.com", environmentId: SITE_ENV, routes: [{ service: "web" }] }],
        }),
      },
    });
    const { unmount } = renderAt("/projects", endpoints);
    const table = await screen.findByRole("table", { name: "Projects" });
    const row = (name: string) => within(table).getByRole("row", { name: new RegExp(name) });
    await waitFor(() => expect(row("example-com")).toHaveTextContent("a website · 1 domain"));
    await waitFor(() => expect(row("^pg")).toHaveTextContent("1 app · 1 database · 1 volume"));
    unmount();

    const first = renderAt("/projects/prj_site", endpoints);
    const siteDanger = await screen.findByRole("region", { name: "Danger zone" });
    await waitFor(() => expect(siteDanger).toHaveTextContent("Takes a website and 1 domain in one plan"));
    first.unmount();

    renderAt("/projects/prj_pg", endpoints);
    const pgDanger = await screen.findByRole("region", { name: "Danger zone" });
    await waitFor(() => expect(pgDanger).toHaveTextContent("Takes 1 app, 1 database and 1 volume in one plan"));
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
