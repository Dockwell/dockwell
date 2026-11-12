// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeState,
  type ProposeChangeRequest,
  type Resource,
  ResourceSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp } from "../../test/apps";
import { applied, changeEvent, moved, shopPlanned, watched } from "../../test/changes";
import { type Api, type CacheOptions, feed, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1", controlServer: true, stats: sample(12) });

function resource(id: string, kind: string, scope: string, name: string, displayName = ""): Resource {
  return create(ResourceSchema, { id, resource: { kind, scope, name }, displayName });
}

const defaultProject = resource("prj_default", "project", "org_1", "default");
const shopProject = resource("prj_shop", "project", "org_1", "shop", "Shop");
const production = resource("env_prod", "environment", "prj_default", "production");
const staging = resource("env_staging", "environment", "prj_default", "staging");

async function openBar(api: Api, path = "/", cache?: CacheOptions) {
  const user = userEvent.setup();
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const rendered = renderAt(path, { ...api, servers: { watchServers: watch.stream } }, cache);
  await screen.findByRole("heading", { level: 1 });
  await user.keyboard("{Control>}k{/Control}");
  return { ...rendered, user, bar: await screen.findByRole("dialog", { name: "Command bar" }) };
}

describe("the command bar", () => {
  it("reaches Activity, and what happened on a server", async () => {
    const { user, bar, router } = await openBar({});
    await user.click(within(bar).getByRole("option", { name: "Open the activity" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/activity"));

    await user.keyboard("{Control>}k{/Control}");
    const again = await screen.findByRole("dialog", { name: "Command bar" });
    await user.click(within(again).getByRole("option", { name: "Show what happened on hz-1" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_1" }));
  });

  it("names projects and environments once there is a choice, and opens recent plans", async () => {
    const shop = applied(shopPlanned());
    const { user, bar, router } = await openBar({
      resources: {
        listResources: (req) => ({
          resources: req.kind === "project" ? [defaultProject, shopProject] : [production, staging],
        }),
      },
      events: { listEvents: () => ({ events: [changeEvent(shop)] }) },
      changes: { getChange: () => ({ change: shop }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const projects = await within(bar).findByRole("group", { name: "Projects" });
    expect(
      within(projects)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Show the activity of default", "Show the activity of shop Shop"]);
    const environments = within(bar).getByRole("group", { name: "Environments" });
    expect(
      within(environments)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual([
      "Show the activity of default / production",
      "Open the variables and secrets of default / production",
      "Show the activity of default / staging",
      "Open the variables and secrets of default / staging",
    ]);
    await expectAccessible(document.body);
    await user.click(within(bar).getByRole("option", { name: /Show the activity of shop/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ resource: "prj_shop" }));

    await user.keyboard("{Control>}k{/Control}");
    const again = await screen.findByRole("dialog", { name: "Command bar" });
    await user.click(await within(again).findByRole("option", { name: /Open the plan: Create the project shop/ }));
    expect(await screen.findByRole("dialog", { name: "Applied" })).toBeVisible();
  });

  it("leaves out the only project and environment", async () => {
    const { bar } = await openBar({
      resources: { listResources: (req) => ({ resources: req.kind === "project" ? [defaultProject] : [production] }) },
    });
    await within(bar).findByRole("option", { name: /Create a project/ });
    expect(within(bar).queryByRole("group", { name: "Projects" })).toBeNull();
    expect(within(bar).queryByRole("group", { name: "Environments" })).toBeNull();
    // The organization keeps a project, so the only one isn't offered for deleting.
    expect(within(bar).queryByRole("option", { name: /Delete a project/ })).toBeNull();
  });

  it("creates a project: it asks for the name, then opens the plan", async () => {
    const proposals: ProposeChangeRequest[] = [];
    const { user, bar } = await openBar({
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: shopPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await user.click(within(bar).getByRole("option", { name: "Create a project Opens a plan" }));
    const ask = screen.getByRole("dialog", { name: "Command bar" });
    const input = within(ask).getByRole("combobox", { name: "Name the new project, such as shop" });
    expect(input).toHaveValue("");
    await user.type(input, "Shop!");
    expect(within(ask).getByRole("option")).toHaveTextContent("Shop! can't be a name");
    await user.clear(input);
    await user.type(input, "shop");
    await user.click(within(ask).getByRole("option", { name: "Create the project shop Opens a plan" }));

    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(proposals).toHaveLength(1);
    expect(proposals[0]?.summary).toBe("Create the project shop");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "project", name: "shop" });
    expect(proposals[0]?.edits[0]?.spec?.kind.case).toBe("project");
  });

  it("deletes a project: it asks which, then opens the plan", async () => {
    const proposals: ProposeChangeRequest[] = [];
    const { user, bar } = await openBar({
      resources: {
        listResources: (req) => ({
          resources: req.kind === "project" ? [defaultProject, shopProject] : [production, staging],
        }),
      },
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: shopPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await user.click(await within(bar).findByRole("option", { name: "Delete a project Opens a plan" }));
    const ask = screen.getByRole("dialog", { name: "Command bar" });
    const input = within(ask).getByRole("combobox", { name: "Which project do you want to delete?" });
    expect(input).toHaveValue("");
    expect(
      within(ask)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Delete the project default Opens a plan", "Delete the project shop Opens a plan"]);
    await expectAccessible(document.body);
    await user.click(within(ask).getByRole("option", { name: "Delete the project shop Opens a plan" }));

    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(proposals).toHaveLength(1);
    expect(proposals[0]?.summary).toBe("Delete the project shop");
    expect(proposals[0]?.edits).toHaveLength(1);
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "project", name: "shop" });
    expect(proposals[0]?.edits[0]?.delete).toBe(true);
  });

  it("deletes everything a project still has, in the one plan", async () => {
    const proposals: ProposeChangeRequest[] = [];
    const shopProduction = resource("env_shop", "environment", "prj_shop", "production");
    const asked: string[] = [];
    const { user, bar } = await openBar({
      resources: {
        listResources: (req) => {
          if (req.scope) asked.push(req.scope);
          return {
            resources:
              req.kind === "project"
                ? [defaultProject, shopProject]
                : req.kind === "environment"
                  ? [production, shopProduction]
                  : req.scope === "env_shop"
                    ? [
                        resource("vol_data", "volume", "env_shop", "data"),
                        resource("net_default", "network", "env_shop", "default"),
                        resource("sec_db", "secret", "env_shop", "DB_PASSWORD"),
                        resource("var_mode", "variable", "env_shop", "MODE"),
                      ]
                    : [],
          };
        },
      },
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: shopPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await user.click(await within(bar).findByRole("option", { name: "Delete a project Opens a plan" }));
    await user.click(
      within(screen.getByRole("dialog", { name: "Command bar" })).getByRole("option", {
        name: "Delete the project shop Opens a plan",
      }),
    );
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    // Only the project's own environments are read, never another project's.
    expect(asked).toEqual(["env_shop"]);
    expect(proposals).toHaveLength(1);
    expect(proposals[0]?.summary).toBe("Delete the project shop");
    expect(proposals[0]?.edits.map((e) => [e.resource?.kind, e.resource?.scope, e.resource?.name, e.delete])).toEqual([
      ["volume", "env_shop", "data", true],
      ["network", "env_shop", "default", true],
      ["secret", "env_shop", "DB_PASSWORD", true],
      ["variable", "env_shop", "MODE", true],
      ["project", "", "shop", true],
    ]);
  });

  it("says what keeps a project, as the API does", async () => {
    const { user, bar } = await openBar({
      resources: {
        listResources: (req) => ({
          resources: req.kind === "project" ? [defaultProject, shopProject] : [production, staging],
        }),
      },
      changes: {
        proposeChange: () => {
          throw new ConnectError(
            "project shop still holds service web in production, so it stays. Delete them first, or in the same change",
            Code.InvalidArgument,
          );
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await user.click(await within(bar).findByRole("option", { name: "Delete a project Opens a plan" }));
    await user.click(
      within(screen.getByRole("dialog", { name: "Command bar" })).getByRole("option", {
        name: "Delete the project shop Opens a plan",
      }),
    );
    expect(await screen.findByText(/Project shop still holds service web in production/)).toBeVisible();
  });

  it("keeps what you typed when you choose the note that it can't be a name", async () => {
    const { user, bar } = await openBar({});
    await user.click(within(bar).getByRole("option", { name: "Create a project Opens a plan" }));
    const ask = screen.getByRole("dialog", { name: "Command bar" });
    const input = within(ask).getByRole("combobox", { name: "Name the new project, such as shop" });
    await user.type(input, "Shop!");
    await user.click(within(ask).getByRole("option", { name: /can't be a name/ }));
    expect(input).toHaveValue("Shop!");
    expect(within(ask).getByRole("option")).toHaveTextContent("Shop! can't be a name");
    await user.type(input, "{Backspace}");
    expect(within(ask).getByRole("option")).toHaveTextContent("Shop can't be a name");
  });

  it("waits for the projects before asking which, and asks nothing with only one", async () => {
    let loaded: () => void = () => {};
    const ready = new Promise<void>((resolve) => (loaded = resolve));
    const { user, bar } = await openBar({
      resources: {
        listResources: async (req) => {
          await ready;
          return { resources: req.kind === "project" ? [defaultProject] : [production] };
        },
      },
    });
    await user.click(within(bar).getByRole("option", { name: "Create an environment Opens a plan" }));
    const ask = screen.getByRole("dialog", { name: "Command bar" });
    expect(within(ask).getByRole("combobox", { name: "Finding your projects" })).toBeVisible();
    expect(within(ask).queryByRole("option")).toBeNull();
    act(() => loaded());
    const name = await screen.findByRole("combobox", { name: "Name the new environment, such as staging" });
    expect(name).toHaveValue("");
    await user.type(name, "qa");
    expect(screen.getByRole("option", { name: "Create the environment qa Opens a plan" })).toBeVisible();
  });

  it("reads the projects again once a change applies", async () => {
    const watch = feed<WatchChangeResponse>();
    let projects = [defaultProject];
    const shop = shopPlanned();
    const { user, bar } = await openBar(
      {
        resources: { listResources: (req) => ({ resources: req.kind === "project" ? projects : [] }) },
        changes: {
          proposeChange: () => ({ change: shop }),
          applyChange: () => ({ change: moved(shop, { state: ChangeState.APPLYING }) }),
          watchChange: watch.stream,
        },
      },
      "/",
      // As the panel keeps it: what was read a moment ago isn't read again on its own.
      { staleTime: 30_000 },
    );
    await within(bar).findByRole("option", { name: "Create a project Opens a plan" });
    await user.click(within(bar).getByRole("option", { name: "Create a project Opens a plan" }));
    await user.type(screen.getByRole("combobox", { name: "Name the new project, such as shop" }), "shop");
    await user.keyboard("{Enter}");
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    projects = [defaultProject, shopProject];
    act(() => watch.push(watched(applied(shop))));
    await user.click(
      await within(await screen.findByRole("dialog", { name: "Applied" })).findByRole("button", { name: "Done" }),
    );

    await user.keyboard("{Control>}k{/Control}");
    const again = await screen.findByRole("dialog", { name: "Command bar" });
    await user.click(await within(again).findByRole("option", { name: "Create an environment Opens a plan" }));
    const which = screen.getByRole("dialog", { name: "Command bar" });
    expect(await within(which).findByRole("option", { name: "In shop Shop" })).toBeVisible();
  });

  it("creates an environment in the project you choose, once there is a choice", async () => {
    const proposals: ProposeChangeRequest[] = [];
    const { user, bar } = await openBar({
      resources: {
        listResources: (req) => ({ resources: req.kind === "project" ? [defaultProject, shopProject] : [] }),
      },
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: shopPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await within(bar).findByRole("group", { name: "Projects" });
    await user.click(within(bar).getByRole("option", { name: "Create an environment Opens a plan" }));
    const which = screen.getByRole("dialog", { name: "Command bar" });
    expect(within(which).getByRole("combobox", { name: "Which project is it for?" })).toBeVisible();
    await user.click(within(which).getByRole("option", { name: "In shop Shop" }));
    const name = within(screen.getByRole("dialog", { name: "Command bar" })).getByRole("combobox", {
      name: "Name the new environment, such as staging",
    });
    await user.type(name, "staging");
    await user.keyboard("{Enter}");

    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Create the environment staging in shop");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "environment", name: "staging", scope: "prj_shop" });
  });

  it("reaches every app, and New app", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/", {
      services: {
        listServices: () => ({ services: [anApp("storefront", "ghcr.io/acme/storefront:1")] }),
        listServiceRevisions: () => ({}),
      },
    });
    await screen.findByRole("heading", { level: 1 });
    await user.keyboard("{Control>}k{/Control}");
    const bar = await screen.findByRole("dialog", { name: "Command bar" });
    const app = await within(bar).findByRole("option", { name: /Open storefront/ });
    expect(app).toHaveTextContent("ghcr.io/acme/storefront:1");
    expect(within(bar).getByRole("option", { name: /New app/ })).toBeVisible();
    await user.click(app);
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_storefront"));

    await user.keyboard("{Control>}k{/Control}");
    const again = await screen.findByRole("dialog", { name: "Command bar" });
    await user.click(within(again).getByRole("option", { name: /New app/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/new"));
  });
});
